package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"testing"
	"testing/synctest"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// proxyTest is a proxy server with a fake pod worker: the messages that the
// Go side sends go into a channel, and the test answers with dispatch.
type proxyTest struct {
	t      *testing.T
	ctx    context.Context
	stop   context.CancelFunc
	peer   *wasiPeer
	px     *wasiProxy
	addr   string
	posted chan js.Value

	mu     sync.Mutex
	exits  []int
	cpu    time.Duration
	charge int
}

// proxyIPs gives each test a pod IP of its own on the in-process network.
var proxyIPs struct {
	sync.Mutex
	n int
}

func nextProxyIP() string {
	proxyIPs.Lock()
	defer proxyIPs.Unlock()
	proxyIPs.n++
	return "10.244.77." + strconv.Itoa(proxyIPs.n)
}

func newProxyTest(t *testing.T, env ...string) *proxyTest {
	t.Helper()
	ip := nextProxyIP()
	ctx, stop := context.WithCancel(context.Background())
	ctx = withProc(ctx, &procEnv{ip: ip})
	pt := &proxyTest{t: t, ctx: ctx, stop: stop, posted: make(chan js.Value, 64)}
	pt.peer = newWasiPeer("ns/pod/proxy", func(m, _ js.Value) { pt.posted <- m })
	ln, _, why := startProxy(ctx, env, ip)
	if why != "" {
		t.Fatalf("startProxy: %s", why)
	}
	pt.px = newWasiProxy(ctx, pt.peer, func(code int) {
		pt.mu.Lock()
		pt.exits = append(pt.exits, code)
		pt.mu.Unlock()
		stop()
	})
	pt.px.charge = func(d time.Duration) {
		pt.mu.Lock()
		pt.cpu += d
		pt.charge++
		pt.mu.Unlock()
	}
	pt.px.serve(ln)
	pt.addr = ln.Addr().String()
	t.Cleanup(stop)
	return pt
}

// next gives the next message to the pod worker.
func (pt *proxyTest) next(typ string) js.Value {
	pt.t.Helper()
	select {
	case m := <-pt.posted:
		if got := m.Get("type").String(); got != typ {
			pt.t.Fatalf("message %s, want %s", got, typ)
		}
		return m
	case <-time.After(10 * time.Second):
		pt.t.Fatalf("no %s message", typ)
	}
	return js.Undefined()
}

// respond sends a response message of the pod worker.
func (pt *proxyTest) respond(id js.Value, fields map[string]any) {
	f := map[string]any{"type": "response", "request": id}
	for k, v := range fields {
		f[k] = v
	}
	pt.peer.dispatch(msg(f))
}

func headerList(pairs ...string) js.Value {
	l := js.Global().Get("Array").New()
	for i := 0; i+1 < len(pairs); i += 2 {
		l.Call("push", jsArray(pairs[i], jsBuffer([]byte(pairs[i+1]))))
	}
	return l
}

func bufOf(s string) js.Value { return jsBuffer([]byte(s)) }

// rawResponse is a response as it arrives on the connection.
type rawResponse struct {
	status  string
	headers [][2]string
	body    string
	err     error
}

func (r rawResponse) names() []string {
	var n []string
	for _, h := range r.headers {
		n = append(n, strings.ToLower(h[0]))
	}
	sort.Strings(n)
	return n
}

func (r rawResponse) get(name string) []string {
	var v []string
	for _, h := range r.headers {
		if strings.EqualFold(h[0], name) {
			v = append(v, h[1])
		}
	}
	return v
}

// send writes a raw request on a new connection and reads the raw
// response, so that the test sees the header names and the framing.
func (pt *proxyTest) send(req string) <-chan rawResponse {
	out := make(chan rawResponse, 1)
	go func() {
		c, err := net.Dial("tcp", pt.addr)
		if err != nil {
			out <- rawResponse{err: err}
			return
		}
		defer c.Close()
		if _, err := io.WriteString(c, req); err != nil {
			out <- rawResponse{err: err}
			return
		}
		out <- readRaw(bufio.NewReader(c))
	}()
	return out
}

func readRaw(br *bufio.Reader) rawResponse {
	var r rawResponse
	line, err := br.ReadString('\n')
	if err != nil {
		r.err = err
		return r
	}
	r.status = strings.TrimRight(line, "\r\n")
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			r.err = err
			return r
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		k, v, _ := strings.Cut(line, ":")
		r.headers = append(r.headers, [2]string{k, strings.TrimSpace(v)})
	}
	var body []byte
	switch {
	case strings.Contains(r.status, " 204 ") || strings.Contains(r.status, " 304 "):
	case len(r.get("transfer-encoding")) > 0:
		for {
			sz, err := br.ReadString('\n')
			if err != nil {
				r.err = err
				return r
			}
			n, _ := strconv.ParseInt(strings.TrimSpace(sz), 16, 64)
			chunk := make([]byte, n+2)
			if _, err := io.ReadFull(br, chunk); err != nil {
				r.err = err
				return r
			}
			body = append(body, chunk[:n]...)
			if n == 0 {
				break
			}
		}
	case len(r.get("content-length")) > 0:
		n, _ := strconv.Atoi(r.get("content-length")[0])
		body = make([]byte, n)
		if _, err := io.ReadFull(br, body); err != nil {
			r.err = err
		}
	default:
		body, _ = io.ReadAll(br)
	}
	r.body = string(body)
	return r
}

func wait(t *testing.T, ch <-chan rawResponse) rawResponse {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("no response")
	}
	return rawResponse{}
}

func reqHeaders(m js.Value) [][2]string {
	var out [][2]string
	h := m.Get("headers")
	for i := 0; i < h.Length(); i++ {
		out = append(out, [2]string{h.Index(i).Index(0).String(), string(jsBytes(js.Global().Get("Uint8Array").New(h.Index(i).Index(1))))})
	}
	return out
}

func msgBody(m js.Value) string {
	return string(jsBytes(js.Global().Get("Uint8Array").New(m.Get("body"))))
}

// PX1 to PX3, PX6: the address of runwasi and the environment of the
// component.
func TestProxyAddr(t *testing.T) {
	for _, c := range []struct {
		env  []string
		want string
	}{
		{nil, "0.0.0.0:8080"},
		{[]string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=0.0.0.0:9000"}, "0.0.0.0:9000"},
		{[]string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=[::]:8081"}, "[::]:8081"},
		{[]string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=127.0.0.1:0"}, "127.0.0.1:0"},
		// PX3: no error for a value that does not parse.
		{[]string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=localhost:9000"}, "0.0.0.0:8080"},
		{[]string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=:9000"}, "0.0.0.0:8080"},
		{[]string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=nonsense"}, "0.0.0.0:8080"},
		{[]string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR="}, "0.0.0.0:8080"},
	} {
		env := append([]string{"PATH=/bin", "WASMTIME_HTTP_BACKLOG=10", "HOSTNAME=p"}, c.env...)
		addr, out := proxyAddr(env)
		if addr.String() != c.want {
			t.Errorf("%v: address %s, want %s", c.env, addr, c.want)
		}
		if strings.Join(out, " ") != "PATH=/bin HOSTNAME=p" {
			t.Errorf("%v: environment %v", c.env, out)
		}
	}
}

// PX4, PX33: the IPs that listen on the pod IP, and the text of runwasi
// for other IPs.
func TestProxyListenHost(t *testing.T) {
	pod := "10.244.0.9"
	for _, c := range []struct{ ip, host, why string }{
		{"0.0.0.0", "0.0.0.0", ""},
		{"::", "0.0.0.0", ""},
		{"127.0.0.1", "127.0.0.1", ""},
		{"::1", "127.0.0.1", ""},
		{"10.244.0.9", pod, ""},
		{"::ffff:10.244.0.9", pod, ""},
		{"192.0.2.7", "", proxyAddrNotAvailable},
		{"10.244.0.10", "", proxyAddrNotAvailable},
		{"2001:db8::1", "", proxyAddrNotAvailable},
	} {
		host, why := proxyListenHost(netip.MustParseAddr(c.ip), pod)
		if host != c.host || why != c.why {
			t.Errorf("%s: %q %q, want %q %q", c.ip, host, why, c.host, c.why)
		}
	}
	ctx := withProc(context.Background(), &procEnv{ip: nextProxyIP()})
	if _, _, why := startProxy(ctx, []string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=192.0.2.7:8080"}, procOf(ctx).ip); why != "Address not available (os error 99)" {
		t.Errorf("192.0.2.7: %q", why)
	}
}

// PX5: with 127.0.0.1:9000, the proxy server listens on the pod IP with a
// row of the loopback table. Another pod gets connection-refused, the pod
// itself connects, and a kubelet probe fails (section 8.11).
func TestProxyLoopback(t *testing.T) {
	withTabNode(t, &tabNode{})
	pt := newProxyTest(t, "WASMTIME_HTTP_PROXY_SOCKET_ADDR=127.0.0.1:9000")
	ip := procOf(pt.ctx).ip
	if pt.addr != ip+":9000" {
		t.Fatalf("listens on %s", pt.addr)
	}
	if _, err := dialCluster(podCtx(t, nextProxyIP()), "tcp", pt.addr); !isRefused(err) {
		t.Errorf("another pod: %v", err)
	}
	if _, err := dialCluster(context.Background(), "tcp", pt.addr); !isRefused(err) {
		t.Errorf("the terminal: %v", err)
	}
	c, err := dialCluster(pt.ctx, "tcp", "127.0.0.1:9000")
	if err != nil {
		t.Fatalf("the pod itself: %v", err)
	}
	c.Close()
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	w.mu.Lock()
	w.ip = ip
	w.mu.Unlock()
	pctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = w.ctrs[0].probeOnce(pctx, &v1.Probe{ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Port: intstr.FromInt32(9000)}}})
	if !isRefused(err) {
		t.Errorf("httpGet probe: %v", err)
	}
	// The row ends with the container.
	pt.stop()
	for i := 0; loopbackRefused("tcp", nil, tcpAddr(pt.addr)); i++ {
		if i == 100 {
			t.Fatal("the row stays after the end of the container")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// PX1, PX2, PX4, PX32: the listener is on the pod IP; port 0 gives an
// ephemeral port of Linux; a port in use gives the text of runwasi.
func TestProxyListen(t *testing.T) {
	ip := nextProxyIP()
	ctx, cancel := context.WithCancel(withProc(context.Background(), &procEnv{ip: ip}))
	defer cancel()
	ln, _, why := startProxy(ctx, nil, ip)
	if why != "" || ln.Addr().String() != ip+":8080" {
		t.Fatalf("default: %v %q", ln, why)
	}
	if _, _, why := startProxy(ctx, []string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=[::]:8080"}, ip); why != proxyAddrInUse {
		t.Errorf("second listener on 8080: %q", why)
	}
	ln2, _, why := startProxy(ctx, []string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=" + ip + ":9000"}, ip)
	if why != "" || ln2.Addr().String() != ip+":9000" {
		t.Fatalf("pod IP: %v %q", ln2, why)
	}
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		ln, _, why := startProxy(ctx, []string{"WASMTIME_HTTP_PROXY_SOCKET_ADDR=0.0.0.0:0"}, ip)
		if why != "" {
			t.Fatal(why)
		}
		_, p, _ := net.SplitHostPort(ln.Addr().String())
		port, _ := strconv.Atoi(p)
		if port < 32768 || port > 60999 || seen[port] {
			t.Errorf("port 0 gave %d", port)
		}
		seen[port] = true
	}
	// The listener closes with the container.
	cancel()
	for i := 0; ; i++ {
		c, err := net.Dial("tcp", ip+":8080")
		if err != nil {
			break
		}
		c.Close()
		if i == 100 {
			t.Fatal("the listener stays open after the end of the container")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// PX16, PX17, PX28, PX29, AC1: a request goes to the pod worker in the
// format of section 8.3.1, and the response goes to the client with all
// its headers and no added Content-Type.
func TestProxyRequestResponse(t *testing.T) {
	pt := newProxyTest(t)
	res := pt.send("PURGE /a/b?c=d%20e HTTP/1.1\r\nHost: web.default:8080\r\nCookie: a=1\r\nUser-Agent: test/1\r\n" +
		"Origin: http://o.example\r\nConnection: keep-alive\r\nKeep-Alive: timeout=5\r\nProxy-Authorization: x\r\n" +
		"Upgrade: h2c\r\nHttp2-Settings: AAA\r\nX-Two: 1\r\nX-Two: 2\r\nContent-Length: 5\r\n\r\nhello")
	m := pt.next("request")
	id := m.Get("request")
	if id.Float() < wasiGoIDs {
		t.Errorf("request ID %v is not an ID of the Go side", id)
	}
	if m.Get("method").String() != "PURGE" || m.Get("url").String() != "http://web.default:8080/a/b?c=d%20e" || msgBody(m) != "hello" {
		t.Errorf("request %s %s %q", m.Get("method"), m.Get("url"), msgBody(m))
	}
	got := map[string][]string{}
	for _, h := range reqHeaders(m) {
		if h[0] != strings.ToLower(h[0]) {
			t.Errorf("header name %q is not in lowercase", h[0])
		}
		got[h[0]] = append(got[h[0]], h[1])
	}
	for _, drop := range []string{"connection", "keep-alive", "proxy-authorization", "upgrade", "http2-settings", "host", "transfer-encoding"} {
		if got[drop] != nil {
			t.Errorf("header %s arrived (fact 20)", drop)
		}
	}
	for k, v := range map[string]string{"cookie": "a=1", "user-agent": "test/1", "origin": "http://o.example", "x-two": "1,2", "content-length": "5"} {
		if strings.Join(got[k], ",") != v {
			t.Errorf("header %s = %q, want %q", k, got[k], v)
		}
	}
	pt.respond(id, map[string]any{"status": 201, "headers": headerList("set-cookie", "a=1", "set-cookie", "b=2", "x-custom", "v"), "body": bufOf("<html>made</html>")})
	r := wait(t, res)
	if r.err != nil || r.status != "HTTP/1.1 201 Created" || r.body != "<html>made</html>" {
		t.Fatalf("response %q %q %v", r.status, r.body, r.err)
	}
	if strings.Join(r.get("set-cookie"), ",") != "a=1,b=2" || strings.Join(r.get("x-custom"), "") != "v" {
		t.Errorf("headers %v", r.headers)
	}
	if r.get("content-type") != nil {
		t.Errorf("PX17: Content-Type %v", r.get("content-type"))
	}
	pt.mu.Lock()
	if pt.charge == 0 || pt.cpu <= 0 {
		t.Errorf("AC1: %d charges, %v", pt.charge, pt.cpu)
	}
	pt.mu.Unlock()

	// PX29: TRACE and a GET with a body. A Content-Type of the component
	// arrives as it is.
	for _, req := range []string{
		"TRACE / HTTP/1.1\r\nHost: h\r\n\r\n",
		"GET /g HTTP/1.1\r\nHost: h\r\nContent-Length: 3\r\n\r\nabc",
	} {
		res := pt.send(req)
		m := pt.next("request")
		method, _, _ := strings.Cut(req, " ")
		if m.Get("method").String() != method {
			t.Errorf("method %s, want %s", m.Get("method"), method)
		}
		if method == "GET" && msgBody(m) != "abc" {
			t.Errorf("GET body %q", msgBody(m))
		}
		pt.respond(m.Get("request"), map[string]any{"status": 200, "headers": headerList("content-type", "text/x-own"), "body": bufOf("ok")})
		if r := wait(t, res); strings.Join(r.get("content-type"), ",") != "text/x-own" || r.body != "ok" {
			t.Errorf("%s: %v %q", method, r.headers, r.body)
		}
	}
}

// PX18, PX19: a request body of more than 16 MiB gets 413 and does not go
// to the pod worker. A body of 1 MiB arrives without a change.
func TestProxyBodyLimit(t *testing.T) {
	pt := newProxyTest(t)
	big := strings.Repeat("x", 16<<20+1)
	r := wait(t, pt.send("POST / HTTP/1.1\r\nHost: h\r\nContent-Length: "+strconv.Itoa(len(big))+"\r\n\r\n"+big))
	if !strings.Contains(r.status, " 413 ") {
		t.Errorf("Content-Length of 16 MiB + 1: %q %v", r.status, r.err)
	}
	r = wait(t, pt.send("POST / HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n"+strconv.FormatInt(int64(len(big)), 16)+"\r\n"+big+"\r\n0\r\n\r\n"))
	if !strings.Contains(r.status, " 413 ") {
		t.Errorf("chunked body of 16 MiB + 1: %q %v", r.status, r.err)
	}
	// A client that waits for 100 Continue gets the 413 before it sends the
	// body.
	r = wait(t, pt.send("POST / HTTP/1.1\r\nHost: h\r\nExpect: 100-continue\r\nContent-Length: "+strconv.Itoa(len(big))+"\r\n\r\n"))
	if !strings.Contains(r.status, " 413 ") {
		t.Errorf("Expect: 100-continue: %q %v", r.status, r.err)
	}
	select {
	case m := <-pt.posted:
		t.Fatalf("a body over the limit went to the pod worker: %s", m.Get("type"))
	default:
	}
	mib := strings.Repeat("0123456789abcdef", 1<<16)
	res := pt.send("POST / HTTP/1.1\r\nHost: h\r\nContent-Length: " + strconv.Itoa(len(mib)) + "\r\n\r\n" + mib)
	m := pt.next("request")
	if msgBody(m) != mib {
		t.Errorf("1 MiB body: %d bytes", len(msgBody(m)))
	}
	pt.respond(m.Get("request"), map[string]any{"status": 200, "headers": headerList(), "body": m.Get("body")})
	if r := wait(t, res); r.body != mib {
		t.Errorf("1 MiB response body: %d bytes", len(r.body))
	}
}

// PX30, PX31: status codes and the header names that hyper sends: date;
// transfer-encoding: chunked for a body without content-length; only date
// for 204.
func TestProxyStatusAndHeaderNames(t *testing.T) {
	pt := newProxyTest(t)
	for _, c := range []struct {
		proto   string
		status  int
		headers js.Value
		body    string
		line    string
		names   string
	}{
		{"HTTP/1.1", 200, headerList(), "body", "HTTP/1.1 200 OK", "date transfer-encoding"},
		{"HTTP/1.1", 200, headerList(), "", "HTTP/1.1 200 OK", "date transfer-encoding"},
		{"HTTP/1.1", 200, headerList("content-length", "4"), "body", "HTTP/1.1 200 OK", "content-length date"},
		{"HTTP/1.1", 204, headerList(), "", "HTTP/1.1 204 No Content", "date"},
		{"HTTP/1.1", 304, headerList("etag", `"e"`), "", "HTTP/1.1 304 Not Modified", "date etag"},
		{"HTTP/1.1", 599, headerList(), "x", "HTTP/1.1 599 status code 599", "date transfer-encoding"},
		{"HTTP/1.0", 200, headerList(), "body", "HTTP/1.0 200 OK", "date"},
		{"HTTP/1.0", 200, headerList("content-length", "4"), "body", "HTTP/1.0 200 OK", "content-length date"},
	} {
		res := pt.send("GET / " + c.proto + "\r\nHost: h\r\n\r\n")
		m := pt.next("request")
		pt.respond(m.Get("request"), map[string]any{"status": c.status, "headers": c.headers, "body": bufOf(c.body)})
		r := wait(t, res)
		if r.err != nil || r.status != c.line || strings.Join(r.names(), " ") != c.names || r.body != c.body {
			t.Errorf("%s %d: %q %v %q %v, want %q %s", c.proto, c.status, r.status, r.names(), r.body, r.err, c.line, c.names)
		}
		if te := r.get("transfer-encoding"); te != nil && te[0] != "chunked" {
			t.Errorf("transfer-encoding %v", te)
		}
	}
}

// PX31: as on runwasi, a request without a host header (HTTP/1.0) gets no
// response, and the connection closes. It does not go to the pod worker.
func TestProxyNoHost(t *testing.T) {
	pt := newProxyTest(t)
	if r := wait(t, pt.send("GET / HTTP/1.0\r\n\r\n")); r.err == nil || r.status != "" {
		t.Errorf("the client got %q %v", r.status, r.err)
	}
	select {
	case m := <-pt.posted:
		t.Errorf("message %s to the pod worker", m.Get("type"))
	default:
	}
	res := pt.send("GET / HTTP/1.0\r\nHost: h\r\n\r\n")
	pt.respond(pt.next("request").Get("request"), map[string]any{"status": 200, "headers": headerList(), "body": bufOf("ok")})
	if r := wait(t, res); r.status != "HTTP/1.0 200 OK" || r.body != "ok" {
		t.Errorf("with a host header: %q %q %v", r.status, r.body, r.err)
	}
}

// PX21, PX22, PX27: a response with an error code closes the connection
// with no response. The container continues, and the next request gets its
// response.
func TestProxyErrorCode(t *testing.T) {
	pt := newProxyTest(t)
	for _, code := range []string{"trap", "internal-error", "HTTP-response-incomplete", "cancelled"} {
		res := pt.send("GET / HTTP/1.1\r\nHost: h\r\n\r\n")
		pt.respond(pt.next("request").Get("request"), map[string]any{"error": code})
		if r := wait(t, res); r.err == nil || r.status != "" {
			t.Errorf("%s: the client got %q", code, r.status)
		}
	}
	res := pt.send("GET / HTTP/1.1\r\nHost: h\r\n\r\n")
	pt.respond(pt.next("request").Get("request"), map[string]any{"status": 200, "headers": headerList(), "body": bufOf("still here")})
	if r := wait(t, res); r.body != "still here" {
		t.Errorf("after the errors: %q %v", r.body, r.err)
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.exits) != 0 || pt.ctx.Err() != nil {
		t.Errorf("the container exits: %v", pt.exits)
	}
}

// PX26: a response with no status code and no error code closes the
// connection, and the container exits with 139.
func TestProxyNoResponse(t *testing.T) {
	pt := newProxyTest(t)
	res := pt.send("GET / HTTP/1.1\r\nHost: h\r\n\r\n")
	pt.respond(pt.next("request").Get("request"), nil)
	if r := wait(t, res); r.err == nil || r.status != "" {
		t.Errorf("the client got %q", r.status)
	}
	select {
	case <-pt.ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the container does not end")
	}
	pt.mu.Lock()
	defer pt.mu.Unlock()
	if len(pt.exits) != 1 || pt.exits[0] != 139 {
		t.Errorf("exits %v, want [139]", pt.exits)
	}
}

// Section 8.6, step 7: a client that closes the connection makes a cancel
// message. The response of the pod worker after it is ignored.
func TestProxyClientCloses(t *testing.T) {
	pt := newProxyTest(t)
	c, err := net.Dial("tcp", pt.addr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "GET /wait HTTP/1.1\r\nHost: h\r\n\r\n")
	id := pt.next("request").Get("request")
	c.Close()
	if cm := pt.next("cancel"); cm.Get("request").Float() != id.Float() {
		t.Errorf("cancel for %v, want %v", cm.Get("request"), id)
	}
	// The pod worker ends the ID with "cancelled" (section 8.3.1).
	pt.respond(id, map[string]any{"error": "cancelled"})
	if pt.peer.Owns("request", uint32(id.Float())) || pt.peer.Drops() != 0 {
		t.Errorf("owns %v, drops %d", pt.peer.Owns("request", uint32(id.Float())), pt.peer.Drops())
	}
	// The server serves the next request.
	res := pt.send("GET / HTTP/1.1\r\nHost: h\r\n\r\n")
	pt.respond(pt.next("request").Get("request"), map[string]any{"status": 200, "headers": headerList(), "body": bufOf("ok")})
	if r := wait(t, res); r.body != "ok" {
		t.Errorf("next request: %q %v", r.body, r.err)
	}
}

// MS6 and the stop of the container: a response that fails the
// examination, and the end of the context, close the connections.
func TestProxyCloseRequests(t *testing.T) {
	pt := newProxyTest(t)
	res := pt.send("GET / HTTP/1.1\r\nHost: h\r\n\r\n")
	id := pt.next("request").Get("request")
	pt.respond(id, map[string]any{"status": 200, "headers": "not a list", "body": bufOf("")})
	if r := wait(t, res); r.err == nil || r.status != "" {
		t.Errorf("a response that fails: the client got %q", r.status)
	}
	if pt.next("cancel").Get("request").Float() != id.Float() {
		t.Error("no cancel for the request of a message that fails")
	}
	res = pt.send("GET / HTTP/1.1\r\nHost: h\r\n\r\n")
	pt.next("request")
	pt.stop()
	pt.peer.Close()
	if r := wait(t, res); r.err == nil || r.status != "" {
		t.Errorf("stop: the client got %q", r.status)
	}
}

// PX23: the Go side sets no timeout. A response after 11 minutes of the
// clock of the test arrives.
func TestProxyNoTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pt := newProxyTest(t)
		res := pt.send("GET /wait/660000 HTTP/1.1\r\nHost: h\r\n\r\n")
		m := <-pt.posted
		start := time.Now()
		time.Sleep(11 * time.Minute)
		pt.respond(m.Get("request"), map[string]any{"status": 200, "headers": headerList(), "body": bufOf("late")})
		r := <-res
		if r.body != "late" || time.Since(start) < 11*time.Minute {
			t.Errorf("after %v: %q %v", time.Since(start), r.body, r.err)
		}
		pt.stop()
		synctest.Wait()
	})
}

// The proxy world of componentStatus: wasi:http/types and
// wasi:http/outgoing-handler in addition to the imports of the command
// world, and no function. An unknown import keeps the status of runwasi.
func TestComponentStatusProxy(t *testing.T) {
	imports := []string{"wasi:cli/environment@0.2.3", "wasi:io/streams@0.2.3", "wasi:http/types@0.2.12", "wasi:http/outgoing-handler@0.2.0", "wasi:sockets/tcp@0.2.3"}
	handler := [][2]string{{"wasi:http/incoming-handler@0.2.3", "instance"}}
	if got := componentStatus("proxy", imports, handler, ""); got != "" {
		t.Errorf("proxy: %q", got)
	}
	if got := componentStatus("proxy", imports, handler, "go"); got != "" {
		t.Errorf("proxy with a function of the entrypoint: %q", got)
	}
	for _, bad := range []string{"wasi:keyvalue/store@0.2.0", "wasi:http/incoming-handler@0.2.3", "wasi:http/types@0.3.0", "wasi:http/types"} {
		want := "component imports instance `" + bad + "`, but a matching implementation was not found in the linker"
		if got := componentStatus("proxy", append(imports, bad), handler, ""); got != want {
			t.Errorf("proxy with %s: %q", bad, got)
		}
	}
	// CR6: a command component keeps its refusal.
	if got := componentStatus("command", imports, [][2]string{{"wasi:cli/run@0.2.3", "instance"}}, ""); !strings.Contains(got, "wasi:http/types@0.2.12") {
		t.Errorf("command with wasi:http: %q", got)
	}
}
