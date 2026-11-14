package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall/js"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	npafake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
)

// fetchPeer gives a fake pod worker peer with the handlers of
// tabnode_wasi_fetch.go, in a pod with the IP ip. c is the container of the
// account, or nil.
func fetchPeer(t *testing.T, ip string, c *ctr) (*wasiPeer, *[]js.Value) {
	t.Helper()
	handlers := map[string]wasiHandler{"fetch": wasiHandlers["fetch"], "fetch-cancel": wasiHandlers["fetch-cancel"]}
	closers := map[string][]func(*wasiPeer, uint32){"fetch": wasiClosers["fetch"]}
	if handlers["fetch"] == nil || handlers["fetch-cancel"] == nil || len(closers["fetch"]) == 0 {
		t.Fatal("the handlers of fetch are not registered")
	}
	p, posted := testPeer(t, handlers, closers)
	ctx := withProc(context.Background(), &procEnv{w: &podWorker{namespace: "default"}, c: c, ip: ip})
	p.State(wasiCtxState, func() any { return ctx })
	saved := theTabNode
	theTabNode = nil
	t.Cleanup(func() { theTabNode = saved })
	return p, posted
}

// sendFetch dispatches a "fetch" message, as the pod worker sends it.
func sendFetch(p *wasiPeer, id int, method, url string, headers [][2]string, body []byte) {
	hs := js.Global().Get("Array").New()
	for _, h := range headers {
		hs.Call("push", js.Global().Get("Array").New(h[0], bytesBuf([]byte(h[1]))))
	}
	p.dispatch(msg(map[string]any{"type": "fetch", "fetch": id, "method": method, "url": url, "headers": hs, "body": bytesBuf(body)}))
}

func bytesBuf(b []byte) js.Value {
	a := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(a, b)
	return a.Get("buffer")
}

// waitPosted waits until the peer posted n messages.
func waitPosted(t *testing.T, posted *[]js.Value, n int, d time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(d); len(*posted) < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d messages after %v, want %d", len(*posted), d, n)
		}
	}
}

// fetched is a "fetched" message in Go values.
type fetched struct {
	id      uint32
	status  int
	error   string
	headers [][2]string
	body    []byte
}

func readFetched(t *testing.T, v js.Value) fetched {
	t.Helper()
	if typ := v.Get("type").String(); typ != "fetched" {
		t.Fatalf("message %s, want fetched", typ)
	}
	f := fetched{id: uint32(v.Get("fetch").Int())}
	if e := v.Get("error"); e.Type() == js.TypeString {
		f.error = e.String()
		if v.Get("status").Type() != js.TypeUndefined {
			t.Errorf("fetched has an error and a status")
		}
		return f
	}
	f.status = v.Get("status").Int()
	hs := v.Get("headers")
	for i := 0; i < hs.Length(); i++ {
		h := hs.Index(i)
		f.headers = append(f.headers, [2]string{h.Index(0).String(), string(jsBytes(js.Global().Get("Uint8Array").New(h.Index(1))))})
	}
	f.body = jsBytes(js.Global().Get("Uint8Array").New(v.Get("body")))
	return f
}

func (f fetched) header(name string) []string {
	var out []string
	for _, h := range f.headers {
		if h[0] == name {
			out = append(out, h[1])
		}
	}
	return out
}

// serve starts an HTTP server of a test pod on addr in the in-process
// network.
func serve(t *testing.T, addr string, h http.HandlerFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// One fetch with its result.
func doOneFetch(t *testing.T, p *wasiPeer, posted *[]js.Value, id int, method, url string, headers [][2]string, body []byte) fetched {
	t.Helper()
	n := len(*posted)
	sendFetch(p, id, method, url, headers, body)
	waitPosted(t, posted, n+1, 10*time.Second)
	f := readFetched(t, (*posted)[n])
	if f.id != uint32(id) {
		t.Fatalf("fetched for %d, want %d", f.id, id)
	}
	return f
}

// CR1, CR3, CR4, CR5: the request goes through dialCluster with only the
// headers of the component, and the component gets the response as the
// server sends it.
func TestWasiFetchResponse(t *testing.T) {
	p, posted := fetchPeer(t, "10.244.251.3", nil)
	var got *http.Request
	var gotBody []byte
	serve(t, "10.244.251.3:8080", func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/gzip":
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			z.Write([]byte("compressed"))
			z.Close()
			w.Header().Set("Content-Encoding", "gzip")
			w.Write(b.Bytes())
		default:
			w.Header().Add("Set-Cookie", "a=1")
			w.Header().Add("Set-Cookie", "b=2")
			w.Header()["X-Mixed-Case"] = []string{"v"}
			w.WriteHeader(599)
			w.Write([]byte("hello"))
		}
	})

	// CR1: localhost of the pod is its pod IP, as dialCluster gives it.
	f := doOneFetch(t, p, posted, 1, "POST", "http://localhost:8080/path?q=1", [][2]string{
		{"x-custom", "one"}, {"x-custom", "two"}, {"cookie", "c=1"}, {"user-agent", "component/1"},
	}, []byte("request body"))
	if f.error != "" {
		t.Fatalf("fetch failed: %s", f.error)
	}
	if f.status != 599 || string(f.body) != "hello" {
		t.Errorf("status %d body %q, want 599 hello", f.status, f.body)
	}
	if c := f.header("set-cookie"); strings.Join(c, ";") != "a=1;b=2" {
		t.Errorf("set-cookie %q, want a=1 and b=2", c)
	}
	if v := f.header("x-mixed-case"); len(v) != 1 {
		t.Errorf("headers %q: names are not in lowercase", f.headers)
	}
	if got.Method != "POST" || got.URL.RequestURI() != "/path?q=1" || got.Host != "localhost:8080" || string(gotBody) != "request body" {
		t.Errorf("server got %s %s host %s body %q", got.Method, got.URL.RequestURI(), got.Host, gotBody)
	}
	// CR4: only the headers of the component, and Content-Length of the body.
	want := map[string]string{"X-Custom": "one,two", "Cookie": "c=1", "User-Agent": "component/1", "Content-Length": "12"}
	for k, vs := range got.Header {
		if want[k] != strings.Join(vs, ",") {
			t.Errorf("server got header %s: %q", k, vs)
		}
	}

	// CR4: no User-Agent, no Accept-Encoding without the component.
	f = doOneFetch(t, p, posted, 2, "GET", "http://10.244.251.3:8080/", nil, nil)
	if f.error != "" || len(got.Header) != 0 {
		t.Errorf("error %q, server got headers %v, want none", f.error, got.Header)
	}

	// CR3: the redirect goes to the component.
	f = doOneFetch(t, p, posted, 3, "GET", "http://10.244.251.3:8080/redirect", nil, nil)
	if f.status != http.StatusFound || strings.Join(f.header("location"), "") != "/elsewhere" {
		t.Errorf("redirect: status %d location %q, want 302 /elsewhere", f.status, f.header("location"))
	}

	// CR5: the body stays compressed.
	f = doOneFetch(t, p, posted, 4, "GET", "http://10.244.251.3:8080/gzip", [][2]string{{"accept-encoding", "gzip"}}, nil)
	if strings.Join(f.header("content-encoding"), "") != "gzip" || len(f.body) < 2 || f.body[0] != 0x1f || f.body[1] != 0x8b {
		t.Errorf("gzip: headers %q body %q, want the compressed body", f.headers, f.body)
	}
	if p.Owns("fetch", 1) || p.Owns("fetch", 4) {
		t.Error("the fetch IDs belong to the container after fetched")
	}
}

// A response body of more than 16 MiB gives an error code (section 8.3.1).
func TestWasiFetchBodyLimit(t *testing.T) {
	p, posted := fetchPeer(t, "10.244.251.4", nil)
	serve(t, "10.244.251.4:80", func(w http.ResponseWriter, r *http.Request) {
		n := 16 << 20
		if r.URL.Path == "/big" {
			n++
		}
		w.Write(make([]byte, n))
	})
	if f := doOneFetch(t, p, posted, 1, "GET", "http://10.244.251.4/", nil, nil); f.error != "" || len(f.body) != 16<<20 {
		t.Errorf("16 MiB: error %q, %d bytes", f.error, len(f.body))
	}
	if f := doOneFetch(t, p, posted, 2, "GET", "http://10.244.251.4/big", nil, nil); f.error != "HTTP-response-body-size" {
		t.Errorf("16 MiB + 1: error %q, want HTTP-response-body-size", f.error)
	}
}

// MS5, CR2, CR7: a URL that fails gets fetched with an error code, and no
// request occurs.
func TestWasiFetchURL(t *testing.T) {
	p, posted := fetchPeer(t, "10.244.251.5", nil)
	for i, c := range []struct{ url, want string }{
		{"http://example.com/", "DNS-error"},
		{"https://example.com:8443/x", "DNS-error"},
		{"http://192.0.2.1/", "connection-refused"},
		{"http://[2001:db8::1]:80/", "connection-refused"},
		{"/relative", "HTTP-request-URI-invalid"},
		{"ftp://10.244.251.5/", "HTTP-request-URI-invalid"},
		{"http://:80/", "HTTP-request-URI-invalid"},
		{"http://10.244.251.5:1/", "connection-refused"}, // in the cluster, no listener
	} {
		start := time.Now()
		f := doOneFetch(t, p, posted, i+1, "GET", c.url, nil, nil)
		if f.error != c.want {
			t.Errorf("%s: error %q, want %q", c.url, f.error, c.want)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: %v", c.url, d)
		}
	}
	// A header value that net/http does not send.
	if f := doOneFetch(t, p, posted, 100, "GET", "http://10.244.251.5:1/", [][2]string{{"x", "a\nb"}}, nil); f.error == "" {
		t.Error("a header value with a newline went out")
	}
}

// fetchPolicyNode gives a tab node whose NetworkPolicy lets only role=client
// pods connect to 10.244.0.2:80. 10.244.0.4 is not a client.
func fetchPolicyNode(t *testing.T) {
	t.Helper()
	port80 := intstr.FromInt32(80)
	tcp := v1.ProtocolTCP
	client := fake.NewClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}},
		testPod("web", "10.244.0.2", "tab", map[string]string{"app": "web"}),
		testPod("other", "10.244.0.4", "tab", map[string]string{"role": "other"}),
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port80}},
				}},
			},
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pol := readyNetPolicy(ctx, t, client, npafake.NewSimpleClientset())
	theTabNode = &tabNode{policy: pol}
}

// setDurations sets synTimeout and componentConnectTimeout for a test: the
// clock that the test controls.
func setDurations(t *testing.T, syn, connect time.Duration) {
	savedSyn, savedConnect := synTimeout, componentConnectTimeout
	synTimeout, componentConnectTimeout = syn, connect
	t.Cleanup(func() { synTimeout, componentConnectTimeout = savedSyn, savedConnect })
}

// CR8: a request that NetworkPolicy denies fails after synTimeout with
// connection-refused, as on a Docker node (section 9.2, item 17).
func TestWasiFetchPolicyDenied(t *testing.T) {
	p, posted := fetchPeer(t, "10.244.0.4", nil)
	fetchPolicyNode(t)
	setDurations(t, 200*time.Millisecond, 10*time.Second)
	start := time.Now()
	f := doOneFetch(t, p, posted, 1, "GET", "http://10.244.0.2/", nil, nil)
	if f.error != "connection-refused" {
		t.Errorf("error %q, want connection-refused", f.error)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("failed after %v, before synTimeout", d)
	}
}

// CR11: only the connect timeout gives connection-timeout.
func TestWasiFetchConnectTimeout(t *testing.T) {
	p, posted := fetchPeer(t, "10.244.0.4", nil)
	fetchPolicyNode(t)
	setDurations(t, 10*time.Second, 200*time.Millisecond)
	start := time.Now()
	f := doOneFetch(t, p, posted, 1, "GET", "http://10.244.0.2/", nil, nil)
	if f.error != "connection-timeout" {
		t.Errorf("error %q, want connection-timeout", f.error)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 5*time.Second {
		t.Errorf("failed after %v, want the connect timeout", d)
	}
}

// hangingServer accepts connections and never answers. closed gets a value
// when the client closes its connection.
func hangingServer(t *testing.T, addr string) (accepted, closed chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted, closed = make(chan struct{}, 1), make(chan struct{}, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- struct{}{}
		buf := make([]byte, 4096)
		for {
			if _, err := c.Read(buf); err != nil {
				closed <- struct{}{}
				c.Close()
				return
			}
		}
	}()
	return accepted, closed
}

func waitCh(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s", what)
	}
}

// CR10: fetch-cancel closes the connection, and no fetched follows.
func TestWasiFetchCancel(t *testing.T) {
	p, posted := fetchPeer(t, "10.244.251.6", nil)
	accepted, closed := hangingServer(t, "10.244.251.6:80")
	sendFetch(p, 7, "GET", "http://10.244.251.6/", nil, nil)
	waitCh(t, accepted, "connection")
	p.dispatch(msg(map[string]any{"type": "fetch-cancel", "fetch": 7}))
	waitCh(t, closed, "close of the connection")
	time.Sleep(100 * time.Millisecond)
	if len(*posted) != 0 {
		t.Errorf("%d messages after fetch-cancel, want none", len(*posted))
	}
	if p.Owns("fetch", 7) {
		t.Error("the fetch ID belongs to the container after fetch-cancel")
	}
}

// The stop of the container closes the connections of its client requests
// (wasiOnClose).
func TestWasiFetchClose(t *testing.T) {
	p, posted := fetchPeer(t, "10.244.251.7", nil)
	accepted, closed := hangingServer(t, "10.244.251.7:80")
	sendFetch(p, 9, "GET", "http://10.244.251.7/", nil, nil)
	waitCh(t, accepted, "connection")
	p.Close()
	waitCh(t, closed, "close of the connection")
	time.Sleep(100 * time.Millisecond)
	if len(*posted) != 0 {
		t.Errorf("%d messages after the stop, want none", len(*posted))
	}
}

// AC1: the work of a client request counts for the container.
func TestWasiFetchChargesCPU(t *testing.T) {
	c := &ctr{id: "fetch-cpu-test", wasm: true}
	t.Cleanup(func() { forgetAcct(c) })
	p, posted := fetchPeer(t, "10.244.251.8", c)
	serve(t, "10.244.251.8:80", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 8<<20)) })
	before := acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds
	if f := doOneFetch(t, p, posted, 1, "PUT", "http://10.244.251.8/", nil, make([]byte, 4<<20)); f.error != "" {
		t.Fatalf("fetch failed: %s", f.error)
	}
	var after uint64
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if after = acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds; after > before {
			return
		}
	}
	t.Fatalf("CPU of the container %d, before %d: nothing charged", after, before)
}
