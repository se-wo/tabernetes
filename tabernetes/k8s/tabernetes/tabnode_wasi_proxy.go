package main

// The proxy server of a proxy component (docs/wasi-sockets.md, section 8.6).
// runwasi serves a component that exports wasi:http/incoming-handler itself
// (fact 4); the tab node does the same with net/http on the in-process
// network. Each request goes to the pod worker as a request message, and its
// response message goes back to the client (section 8.3.1). The pod worker
// makes a new instance for each request, counts REQUEST_ID and keeps the
// queue of PX9 and PX10: the Go side has no queue and no timeout (PX23).

import (
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"time"
)

// The variables of runwasi for the address of the proxy server
// (http_proxy.rs). The component does not get them (PX6).
const (
	proxyAddrEnv    = "WASMTIME_HTTP_PROXY_SOCKET_ADDR"
	proxyBacklogEnv = "WASMTIME_HTTP_BACKLOG"
)

// proxyDefaultAddr is the address without the variable, or with a value
// that does not parse (PX2, PX3).
var proxyDefaultAddr = netip.MustParseAddrPort("0.0.0.0:8080")

// proxyBodyLimit is the limit of a request body and a response body
// (decision 4, PX18).
var proxyBodyLimit = wasiLimits["body"]

// The ephemeral ports of Linux (fact 16): port 0 gives one of them (PX32).
// TODO(W4.1): use the general allocator of ephemeral ports.
const (
	proxyEphemeralFirst = 32768
	proxyEphemeralLast  = 60999
)

// proxyDropHeaders are the headers that wasmtime removes from a request that
// arrives (fact 20, PX28).
var proxyDropHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"proxy-connection": true, "transfer-encoding": true, "upgrade": true, "host": true, "http2-settings": true,
}

// The texts of runwasi (musl) for a bind that fails (section 9.2, PX33).
const (
	proxyAddrNotAvailable = "Address not available (os error 99)"
	proxyAddrInUse        = "Address in use (os error 98)"
)

// proxyExitNoResponse is the exit code of runwasi after handle returns
// without a response: the panic aborts the shim (PX26, section 9.2).
const proxyExitNoResponse = 139

// proxyAddr gives the address of the proxy server from the environment of
// the container, and the environment without the variables of runwasi
// (PX1 to PX3, PX6). runwasi parses the value as a Rust SocketAddr: an IP
// and a port, with no host name.
func proxyAddr(env []string) (netip.AddrPort, []string) {
	addr := proxyDefaultAddr
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case proxyAddrEnv:
			if a, err := netip.ParseAddrPort(v); err == nil {
				addr = a
			} else {
				addr = proxyDefaultAddr
			}
		case proxyBacklogEnv:
		default:
			out = append(out, kv)
		}
	}
	return addr, out
}

// proxyListenHost gives the host for listenPod, or the text of runwasi for
// an IP that the pod cannot bind (PX33). podIP is the IP of the pod.
func proxyListenHost(a netip.Addr, podIP string) (string, string) {
	a = a.WithZone("")
	switch {
	case a.IsUnspecified():
		// PX4: listenPod listens on the pod IP. Linux also serves IPv4 on
		// [::] (fact 4).
		return "0.0.0.0", ""
	case a.IsLoopback():
		// TODO(W3.1): PX5. 127.0.0.1 must accept only connections from the
		// pod IP, with the loopback table (section 8.11). Until then, it
		// listens on the pod IP for all clients, as listenPod does for each
		// program of a container.
		return "127.0.0.1", ""
	case a.Unmap().String() == podIP:
		return podIP, ""
	}
	return "", proxyAddrNotAvailable
}

// listenProxy listens on the address of the proxy server. It gives the
// listener, or the text of runwasi for stderr.
func listenProxy(ctx context.Context, addr netip.AddrPort, podIP string) (net.Listener, string) {
	host, why := proxyListenHost(addr.Addr(), podIP)
	if why != "" {
		return nil, why
	}
	if addr.Port() != 0 {
		ln, err := listenPod(ctx, net.JoinHostPort(host, strconv.Itoa(int(addr.Port()))))
		if err != nil {
			return nil, proxyListenText(err)
		}
		return ln, ""
	}
	// PX32: the in-process network gives port 0 a counter from 1 for all
	// IPs (fact 11). So try the ephemeral ports of Linux in sequence.
	for port := proxyEphemeralFirst; port <= proxyEphemeralLast; port++ {
		ln, err := listenPod(ctx, net.JoinHostPort(host, strconv.Itoa(port)))
		if err == nil {
			return ln, ""
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, proxyAddrInUse
}

func proxyListenText(err error) string {
	if strings.Contains(err.Error(), "in use") {
		return proxyAddrInUse
	}
	return err.Error()
}

// wasiProxyState is the key of the *wasiProxy of a pod worker in the state
// of its peer.
const wasiProxyState = "proxy"

// wasiProxy is the proxy server of one container: one pod worker.
type wasiProxy struct {
	// ctx is the context of the container, with its procEnv (the same as
	// wasiPeerCtx). It ends when the container stops.
	ctx  context.Context
	peer *wasiPeer
	// exit ends the container with an exit code (PX26).
	exit func(code int)
	// charge counts CPU time of the proxy server for the container (AC1).
	charge func(time.Duration)

	mu      sync.Mutex
	pending map[uint32]chan proxyResult
	srv     *http.Server
}

// proxyResult is the end of a request: the response message, or an abort.
type proxyResult struct {
	msg   js.Value // the response message, if not abort
	abort bool     // close the connection with no response
}

func proxyOf(p *wasiPeer) *wasiProxy {
	px, _ := p.State(wasiProxyState, func() any { return (*wasiProxy)(nil) }).(*wasiProxy)
	return px
}

// newWasiProxy makes the proxy server of peer and keeps it in the state of
// the peer. ctx must have the procEnv of the container.
func newWasiProxy(ctx context.Context, peer *wasiPeer, exit func(code int)) *wasiProxy {
	px := &wasiProxy{ctx: ctx, peer: peer, exit: exit, pending: map[uint32]chan proxyResult{}}
	px.charge = func(d time.Duration) { acctFrom(ctx).ChargeCPU(d) }
	peer.State(wasiProxyState, func() any { return px })
	return px
}

// serve serves ln until the context of the container ends. Then it closes
// the listener and all connections.
func (px *wasiProxy) serve(ln net.Listener) {
	srv := &http.Server{
		Handler:     px,
		BaseContext: func(net.Listener) context.Context { return px.ctx },
		// No timeouts: runwasi has none (PX23).
		ErrorLog: log.New(io.Discard, "", 0),
	}
	px.mu.Lock()
	px.srv = srv
	px.mu.Unlock()
	context.AfterFunc(px.ctx, func() {
		srv.Close()
		px.abortAll()
	})
	go srv.Serve(ln)
}

// abortAll ends all requests that wait for a response.
func (px *wasiProxy) abortAll() {
	px.mu.Lock()
	pending := px.pending
	px.pending = map[uint32]chan proxyResult{}
	px.mu.Unlock()
	for _, ch := range pending {
		ch <- proxyResult{abort: true}
	}
}

// finish gives the result of a request to the handler that waits for it.
// It does not wait. An ID without a handler is ignored: the client closed
// the connection, or the container stopped.
func (px *wasiProxy) finish(id uint32, r proxyResult) {
	px.mu.Lock()
	ch := px.pending[id]
	delete(px.pending, id)
	px.mu.Unlock()
	if ch != nil {
		ch <- r
	}
}

func init() {
	wasiHandle("response", func(p *wasiPeer, m wasiMsg) {
		if px := proxyOf(p); px != nil {
			px.finish(m.Uint("request"), proxyResult{msg: m.V})
		}
	})
	// A response that fails the examination (MS6), or the stop of the
	// container: close the connection of the client.
	wasiOnClose("request", func(p *wasiPeer, id uint32) {
		if px := proxyOf(p); px != nil {
			px.finish(id, proxyResult{abort: true})
		}
	})
}

// ServeHTTP does the steps of section 8.6 for one request.
func (px *wasiProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	busy := time.Duration(0)
	defer func() { px.charge(busy) }()

	// PX31: runwasi gives no response to a request without a host header
	// (HTTP/1.0), and the connection closes (the step wasi-proxy of the
	// Docker check). net/http of Go answers such a request of HTTP/1.1 with
	// 400 before it calls this handler.
	if r.Host == "" {
		busy += time.Since(start)
		panic(http.ErrAbortHandler)
	}
	// PX18, PX19: a request body of more than 16 MiB gets 413.
	if r.ContentLength > int64(proxyBodyLimit) {
		proxyTooLarge(w, r)
		busy += time.Since(start)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(proxyBodyLimit)+1))
	if err != nil {
		busy += time.Since(start)
		panic(http.ErrAbortHandler)
	}
	if len(body) > proxyBodyLimit {
		proxyTooLarge(w, r)
		busy += time.Since(start)
		return
	}
	headers, status := proxyRequestHeaders(r)
	url := proxyRequestURL(r)
	if status == 0 && len(url) > wasiLimits["url"] {
		status = http.StatusRequestURITooLong
	}
	if status == 0 && (len(r.Method) > wasiLimits["method"] || !methodRE.MatchString(r.Method)) {
		status = http.StatusNotImplemented
	}
	if status != 0 {
		proxyStatus(w, status)
		busy += time.Since(start)
		return
	}

	id := px.peer.NewID("request")
	ch := make(chan proxyResult, 1)
	px.mu.Lock()
	px.pending[id] = ch
	px.mu.Unlock()
	err = px.peer.send("request", map[string]any{
		"request": id, "method": r.Method, "url": url, "headers": headers, "body": body,
	})
	busy += time.Since(start)
	if err != nil {
		px.finish(id, proxyResult{})
		panic(http.ErrAbortHandler)
	}

	var res proxyResult
	select {
	case res = <-ch:
	case <-r.Context().Done():
		// The client closed the connection, or the container stopped.
		px.finish(id, proxyResult{})
		px.peer.send("cancel", map[string]any{"request": id})
		panic(http.ErrAbortHandler)
	}
	start = time.Now()
	if res.abort {
		busy += time.Since(start)
		panic(http.ErrAbortHandler)
	}
	m := wasiMsg{Type: "response", V: res.msg}
	switch {
	case m.Has("error"):
		// PX21, PX22, PX27: no response, and the container continues.
		busy += time.Since(start)
		panic(http.ErrAbortHandler)
	case !m.Has("status"):
		// PX26: runwasi aborts. The container exits with 139, and kubelet
		// starts it again.
		busy += time.Since(start)
		go px.exit(proxyExitNoResponse)
		panic(http.ErrAbortHandler)
	}
	proxyWriteResponse(w, r, m)
	busy += time.Since(start)
}

// proxyDiscardLimit is the number of bytes of a request body over the limit
// that the proxy server reads and discards before it sends 413.
const proxyDiscardLimit = 64 << 20

// proxyTooLarge sends 413 for a request body over the limit (PX19). A
// client that sends the complete body before it reads the response gets the
// 413 only if the server reads the body. So the server discards the rest of
// the body, to a maximum of proxyDiscardLimit. A client that waits for
// "100 Continue" gets the 413 at once.
func proxyTooLarge(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Expect"), "100-continue") {
		io.Copy(io.Discard, io.LimitReader(r.Body, proxyDiscardLimit))
	}
	proxyStatus(w, http.StatusRequestEntityTooLarge)
}

// proxyStatus sends a status code of the tab node itself.
func proxyStatus(w http.ResponseWriter, code int) {
	w.Header().Set("Connection", "close")
	http.Error(w, http.StatusText(code), code)
}

// proxyRequestHeaders gives the headers of a request as the list of
// section 8.3.1, without the headers of fact 20 (PX28). net/http keeps the
// sequence of the values of one name, but not the sequence of different
// names: the names are in alphabetical sequence (section 8.6). A status
// code other than 0 tells that the headers are more than the limits of
// section 8.3.1.
func proxyRequestHeaders(r *http.Request) (js.Value, int) {
	list := jsArrayCtor.New()
	n := 0
	for _, name := range proxyHeaderNames(r.Header) {
		lower := strings.ToLower(name)
		if proxyDropHeaders[lower] {
			continue
		}
		if len(lower) > wasiLimits["headerName"] {
			return js.Undefined(), http.StatusRequestHeaderFieldsTooLarge
		}
		for _, v := range r.Header[name] {
			n++
			if n > wasiLimits["headers"] || len(v) > wasiLimits["headerValue"] {
				return js.Undefined(), http.StatusRequestHeaderFieldsTooLarge
			}
			list.Call("push", jsArray(lower, jsBuffer([]byte(v))))
		}
	}
	return list, 0
}

// proxyHeaderNames gives the names of h in a fixed sequence.
func proxyHeaderNames(h http.Header) []string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// proxyRequestURL gives the url of the request message: http://, the value
// of the host header and the request target with no change (section
// 8.3.1). ServeHTTP sends no request without a host header.
func proxyRequestURL(r *http.Request) string {
	return "http://" + r.Host + r.RequestURI
}

// proxyWriteResponse sends a response message with a status code to the
// client: the status code, all headers and the body (PX16, PX30). It adds
// no Content-Type (PX17). net/http adds Date, as hyper does. Without a
// content-length header, the body goes with transfer-encoding: chunked, as
// hyper sends it (PX31).
func proxyWriteResponse(w http.ResponseWriter, r *http.Request, m wasiMsg) {
	h := w.Header()
	// Without this, net/http finds a Content-Type in the body.
	h["Content-Type"] = nil
	hasLength := false
	if hv := m.Value("headers"); hv.Type() == js.TypeObject {
		for i := 0; i < hv.Length(); i++ {
			pair := hv.Index(i)
			name := pair.Index(0).String()
			value := string(jsBytes(js.Global().Get("Uint8Array").New(pair.Index(1))))
			key := http.CanonicalHeaderKey(name)
			if key == "Content-Type" && h[key] == nil {
				delete(h, key)
			}
			if key == "Content-Length" {
				hasLength = true
			}
			h[key] = append(h[key], value)
		}
	}
	body := m.Bytes("body")
	w.WriteHeader(m.Int("status"))
	if !hasLength {
		// Without Flush, net/http gives a short body a Content-Length.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	if len(body) > 0 {
		w.Write(body)
	}
}

// jsBuffer gives a new ArrayBuffer with the bytes of b.
func jsBuffer(b []byte) js.Value {
	arr := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(arr, b)
	return arr.Get("buffer")
}

// startProxy starts the proxy server of a proxy component: it listens on
// the address of the environment before the pod worker starts. It gives the
// environment for the component (PX6), or the text of runwasi for stderr
// when the address cannot listen (PX33).
func startProxy(ctx context.Context, env []string, podIP string) (net.Listener, []string, string) {
	addr, env := proxyAddr(env)
	ln, why := listenProxy(ctx, addr, podIP)
	if why != "" {
		return nil, env, why
	}
	return ln, env, ""
}
