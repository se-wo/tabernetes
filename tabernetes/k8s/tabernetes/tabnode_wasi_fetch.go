package main

// The client requests of proxy components (docs/wasi-sockets.md, sections
// 7.3 and 8.6): the pod worker sends "fetch", the Go side does the request
// with newComponentClient (tabnode_dial.go) and sends "fetched" with the
// response or an error code of wasmtime. "fetch-cancel" stops the request,
// and then no "fetched" goes to the pod worker (CR10).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"syscall/js"
	"time"
)

// wasiCtxState is the key of the context of the container in the state of
// the peer of a pod worker. The context has the procEnv of the pod (its IP
// for dialCluster, its container for acctFrom). startComponent puts it in
// before the pod worker sends messages.
const wasiCtxState = "ctx"

// wasiFetchState is the key of the client requests of a peer.
const wasiFetchState = "fetch"

// wasiPeerCtx gives the context of the container of a peer, or
// context.Background() if it has none.
func wasiPeerCtx(p *wasiPeer) context.Context {
	if ctx, ok := p.State(wasiCtxState, func() any { return nil }).(context.Context); ok && ctx != nil {
		return ctx
	}
	return context.Background()
}

// wasiPodCtx gives ctx with the procEnv of the pod of the container c.
func (c *ctr) wasiPodCtx(ctx context.Context) context.Context {
	_, ip := c.w.podInfo()
	return withProc(ctx, &procEnv{w: c.w, c: c, ip: ip})
}

// fetches are the client requests of one pod worker that have no "fetched"
// yet, by fetch ID.
type fetches struct {
	mu   sync.Mutex
	live map[uint32]context.CancelFunc
}

func fetchesOf(p *wasiPeer) *fetches {
	return p.State(wasiFetchState, func() any { return &fetches{live: map[uint32]context.CancelFunc{}} }).(*fetches)
}

func (f *fetches) add(id uint32, cancel context.CancelFunc) {
	f.mu.Lock()
	f.live[id] = cancel
	f.mu.Unlock()
}

// take removes the request with id. It gives nil if the request is not
// there: it has its "fetched", or the pod worker cancelled it.
func (f *fetches) take(id uint32) context.CancelFunc {
	f.mu.Lock()
	defer f.mu.Unlock()
	cancel := f.live[id]
	delete(f.live, id)
	return cancel
}

// stop cancels the request with id. It gets no "fetched".
func (f *fetches) stop(id uint32) {
	if cancel := f.take(id); cancel != nil {
		cancel()
	}
}

// fetchRequest is the copy of a "fetch" message in Go memory.
type fetchRequest struct {
	method  string
	url     string
	headers [][2]string
	body    []byte
}

// fetchForbidden are the 9 headers that wasmtime does not let a component
// set (fact 20). The pod worker does not send them. If one arrives, the Go
// side does not send it.
var fetchForbidden = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true,
	"proxy-connection": true, "transfer-encoding": true, "upgrade": true, "host": true, "http2-settings": true,
}

func init() {
	wasiHandle("fetch", func(p *wasiPeer, m wasiMsg) {
		start := time.Now()
		id := m.Uint("fetch")
		req := fetchRequest{method: m.Str("method"), url: m.Str("url"), body: m.Bytes("body")}
		hs := m.Value("headers")
		for i := 0; i < hs.Length(); i++ {
			h := hs.Index(i)
			req.headers = append(req.headers, [2]string{h.Index(0).String(), string(jsBytes(js.Global().Get("Uint8Array").New(h.Index(1))))})
		}
		ctx, cancel := context.WithCancel(wasiPeerCtx(p))
		f := fetchesOf(p)
		f.add(id, cancel)
		cost := time.Since(start)
		// The request waits for the network: not on the event loop.
		go func() {
			fields, cpu := doFetch(ctx, req)
			// After fetch-cancel or the stop of the pod worker, the request
			// is not there any more: no "fetched" (CR10).
			if f.take(id) != nil {
				cancel()
				s := time.Now()
				fields["fetch"] = id
				if err := p.send("fetched", fields); err != nil {
					log.Printf("tabnode: pod worker %s: fetched %d: %v", p.name, id, err)
				}
				cpu += time.Since(s)
			}
			// AC1. acctFrom takes the lock of the pod: not on the event
			// loop.
			acctFrom(ctx).ChargeCPU(cost + cpu)
		}()
	})
	wasiHandle("fetch-cancel", func(p *wasiPeer, m wasiMsg) {
		// Closes the connection: the context of the request ends (CR10).
		fetchesOf(p).stop(m.Uint("fetch"))
	})
	// A "fetch" message that failed the examination after its ID, and the
	// stop of the container.
	wasiOnClose("fetch", func(p *wasiPeer, id uint32) { fetchesOf(p).stop(id) })
}

// fetchErr gives the fields of a "fetched" with an error code.
func fetchErr(code string) map[string]any { return map[string]any{"error": code} }

// doFetch does one client request and gives the fields of its "fetched"
// message (without the ID), and the CPU time of its work. The time of the
// waits for the network is not in it.
func doFetch(ctx context.Context, r fetchRequest) (fields map[string]any, cpu time.Duration) {
	start := time.Now()
	fields = doFetchFields(ctx, r, &start, &cpu)
	cpu += time.Since(start)
	return fields, cpu
}

// fetchURL examines the URL of a "fetch" (MS5): an absolute http or https
// URL to a host of the cluster (CR1). It gives the error code for a URL
// that fails.
func fetchURL(ctx context.Context, raw string) (*url.URL, string) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Opaque != "" {
		return nil, "HTTP-request-URI-invalid"
	}
	host := u.Hostname()
	if host == "" {
		return nil, "HTTP-request-URI-invalid"
	}
	if !inCluster(ctx, host) {
		// The internet is out of scope (decision 8). A name outside the
		// cluster does not resolve (CR2). An IP outside the cluster gives
		// the connect error of wasmtime (CR7).
		if net.ParseIP(host) != nil {
			return nil, "connection-refused"
		}
		return nil, "DNS-error"
	}
	return u, ""
}

// doFetchFields does the steps of doFetch. *start is the start of the
// current part of the work: it moves past each wait, and the time before
// the wait goes into *cpu.
func doFetchFields(ctx context.Context, r fetchRequest, start *time.Time, cpu *time.Duration) map[string]any {
	wait := func() { *cpu += time.Since(*start) }
	resume := func() { *start = time.Now() }

	u, code := fetchURL(ctx, r.url)
	if code != "" {
		return fetchErr(code)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u.String(), nil)
	if err != nil {
		return fetchErr("HTTP-request-method-invalid")
	}
	if len(r.body) > 0 {
		req.Body = io.NopCloser(bytes.NewReader(r.body))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(r.body)), nil }
		req.ContentLength = int64(len(r.body))
	}
	// Only the headers of the component (CR4). Host comes from the URL.
	// An empty User-Agent stops the default of Go.
	req.Header = http.Header{}
	for _, h := range r.headers {
		name := strings.ToLower(h[0])
		if fetchForbidden[name] {
			continue
		}
		k := http.CanonicalHeaderKey(name)
		req.Header[k] = append(req.Header[k], h[1])
	}
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""}
	}

	client, tr := newComponentClient()
	defer tr.CloseIdleConnections()
	wait()
	resp, err := client.Do(req)
	resume()
	if err != nil {
		return fetchErr(fetchErrorCode(err))
	}
	defer resp.Body.Close()

	limit := wasiLimits["body"]
	wait()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	resume()
	switch {
	case len(body) > limit:
		// One buffer of 16 MiB at most (decision 4, section 8.3.1).
		return fetchErr("HTTP-response-body-size")
	case err != nil:
		return fetchErr("HTTP-response-incomplete")
	}

	headers, code := fetchHeaders(resp)
	if code != "" {
		return fetchErr(code)
	}
	return map[string]any{"status": resp.StatusCode, "headers": headers, "body": body}
}

// fetchHeaders gives the headers of a response in the format of section
// 8.3.1: [[name, ArrayBuffer]], names in lowercase. net/http keeps no
// sequence between names, so the names are in alphabetical sequence. The
// values of one name keep their sequence.
func fetchHeaders(resp *http.Response) (js.Value, string) {
	h := resp.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	// net/http moves Transfer-Encoding out of the header.
	if len(resp.TransferEncoding) > 0 {
		h["Transfer-Encoding"] = []string{strings.Join(resp.TransferEncoding, ", ")}
	}
	names := make([]string, 0, len(h))
	n := 0
	for k, vs := range h {
		names = append(names, k)
		n += len(vs)
	}
	if n > wasiLimits["headers"] {
		return js.Undefined(), "HTTP-response-header-section-size"
	}
	sort.Strings(names)
	list := jsArrayCtor.New()
	u8 := js.Global().Get("Uint8Array")
	for _, k := range names {
		name := strings.ToLower(k)
		for _, v := range h[k] {
			if len(name) > wasiLimits["headerName"] || len(v) > wasiLimits["headerValue"] {
				return js.Undefined(), "HTTP-response-header-size"
			}
			arr := u8.New(len(v))
			js.CopyBytesToJS(arr, []byte(v))
			list.Call("push", jsArrayCtor.New(name, arr.Get("buffer")))
		}
	}
	return list, ""
}

// fetchErrorCode maps the error of a client request to the error code of
// wasmtime (fact 20).
func fetchErrorCode(err error) string {
	var ce *connectError
	if errors.As(err, &ce) {
		if ce.timeout {
			// Only the connect timeout of wasmtime (CR11).
			return "connection-timeout"
		}
		// Each other connect error, also a SYN that NetworkPolicy drops
		// (dropped gives os.ErrDeadlineExceeded after synTimeout, CR8).
		return "connection-refused"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "x509:") || strings.Contains(msg, "certificate"):
		return "TLS-certificate-error"
	case strings.Contains(msg, "tls:"):
		return "TLS-protocol-error"
	}
	return "HTTP-protocol-error"
}
