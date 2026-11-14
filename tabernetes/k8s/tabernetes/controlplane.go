package main

// The components of the control plane as boot() starts them: their command
// lines, start times and exits. The static pods of the tab node show them
// (tabnode_static.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/transport"

	"k8s.io/kubernetes/cmd/tabernetes/etcdshim"
)

// component is a control plane component that runs in this process.
type component struct {
	name  string
	args  []string // the command line without the program name
	start time.Time
	logs  *logBuf

	mu       sync.Mutex
	exitedAt time.Time // zero while it runs
	exitErr  error
}

func (c *component) setExited(err error) {
	c.mu.Lock()
	c.exitedAt, c.exitErr = time.Now(), err
	c.mu.Unlock()
}

// exited reports when the component ended, zero while it runs.
func (c *component) exited() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exitedAt
}

// controlPlane lists the components boot() started. It stays empty in a
// Web Worker node (nodeworker.go): such a node has no static pods.
var (
	controlPlaneMu sync.Mutex
	controlPlane   []*component
)

func addComponent(name string, args []string) *component {
	c := &component{name: name, args: args, start: time.Now(), logs: componentLogs(name)}
	controlPlaneMu.Lock()
	controlPlane = append(controlPlane, c)
	controlPlaneMu.Unlock()
	return c
}

func controlPlaneComponents() []*component {
	controlPlaneMu.Lock()
	defer controlPlaneMu.Unlock()
	return append([]*component(nil), controlPlane...)
}

// startComponent runs a component with run() and records it. The returned
// channel gets its error once it exits, like the one of run().
func startComponent(name string, cmd *cobra.Command, args []string) <-chan error {
	c := addComponent(name, args)
	exited := run(name, cmd, args)
	out := make(chan error, 1)
	go func() {
		err := <-exited
		fmt.Fprintln(c.logs.writer("stderr"), name, "exited:", err)
		c.setExited(err)
		out <- err
	}()
	return out
}

const (
	etcdListen        = "127.0.0.1:2379"
	etcdMetricsAddr   = "127.0.0.1:2381"
	etcdClientURL     = "http://" + etcdListen
	etcdHealthTimeout = 2 * time.Second
)

// etcdArgs is the command line of the etcd static pod. The shim takes no
// flags; these are the ones of a real etcd that it matches.
var etcdArgs = []string{
	"--name=tab",
	"--listen-client-urls=" + etcdClientURL,
	"--advertise-client-urls=" + etcdClientURL,
	"--listen-metrics-urls=http://" + etcdMetricsAddr,
}

// startEtcd serves the etcd shim on l and its health endpoints on
// 127.0.0.1:2381, like the metrics listener of etcd.
func startEtcd(l net.Listener) {
	l = countingListener{l, &cpBytes.etcd}
	c := addComponent("etcd", etcdArgs)
	etcdshim.Logf = func(level, msg string, kv ...any) {
		caller := "etcdshim"
		if _, file, line, ok := runtime.Caller(1); ok {
			caller = fmt.Sprintf("%s/%s:%d", filepath.Base(filepath.Dir(file)), filepath.Base(file), line)
		}
		b := etcdLogLine(time.Now(), level, caller, msg, kv...)
		c.logs.writer("stderr").Write(b)
		os.Stderr.Write(b)
	}
	go func() {
		labelGoroutine("etcd")
		err := etcdshim.Serve(l)
		c.setExited(err)
		must(err)
	}()
	hl, err := net.Listen("tcp", etcdMetricsAddr)
	must(err)
	go func() {
		labelGoroutine("etcd")
		if err := http.Serve(hl, etcdHealthHandler()); err != nil {
			fmt.Fprintln(os.Stderr, "etcd health:", err)
		}
	}()
}

// etcdLogLine formats a log entry like the zap JSON logger of etcd.
func etcdLogLine(t time.Time, level, caller, msg string, kv ...any) []byte {
	var b strings.Builder
	field := func(k string, v any) {
		kj, _ := json.Marshal(k)
		vj, err := json.Marshal(v)
		if err != nil {
			vj, _ = json.Marshal(fmt.Sprint(v))
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.Write(kj)
		b.WriteByte(':')
		b.Write(vj)
	}
	b.WriteByte('{')
	field("level", level)
	field("ts", t.UTC().Format("2006-01-02T15:04:05.000000Z07:00"))
	field("caller", caller)
	field("msg", msg)
	for i := 0; i+1 < len(kv); i += 2 {
		field(fmt.Sprint(kv[i]), kv[i+1])
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

// etcdHealthHandler answers /health, /livez and /readyz with 200 only
// when the shim serves a read, like the QGET of "health" in etcd.
func etcdHealthHandler() http.Handler {
	var (
		mu   sync.Mutex
		conn *grpc.ClientConn
	)
	check := func(ctx context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if conn == nil {
			c, err := grpc.NewClient("passthrough:///"+etcdListen,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
				}))
			if err != nil {
				return err
			}
			conn = c
		}
		ctx, cancel := context.WithTimeout(ctx, etcdHealthTimeout)
		defer cancel()
		_, err := pb.NewKVClient(conn).Range(ctx, &pb.RangeRequest{Key: []byte("health"), Serializable: true})
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := check(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"health": "false", "reason": "QGET ERROR:" + err.Error()})
			return
		}
		io.WriteString(w, `{"health":"true","reason":""}`)
	})
	for _, path := range []string{"/livez", "/readyz"} {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			if err := check(r.Context()); err != nil {
				http.Error(w, "[-]serializable_read failed: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			io.WriteString(w, "ok")
		})
	}
	return mux
}

// cpBytes counts the bytes the control plane moves: in and out of the
// listener of etcd, and the request and response bodies of every client-go
// client, watch streams included, by the component of the goroutine that
// sends the request ("" for none: the tab node, the add-ons, kubectl in the
// terminal). The CPU of the node is split between the control plane pods by
// these bytes (tabnode_stats.go).
var cpBytes struct {
	etcd atomic.Int64

	mu   sync.Mutex
	rest map[string]int64
}

func addRESTBytes(component string, n int64) {
	cpBytes.mu.Lock()
	if cpBytes.rest == nil {
		cpBytes.rest = map[string]int64{}
	}
	cpBytes.rest[component] += n
	cpBytes.mu.Unlock()
}

// cpBytesNow reads the counters.
func cpBytesNow() (etcd int64, rest map[string]int64) {
	cpBytes.mu.Lock()
	defer cpBytes.mu.Unlock()
	rest = make(map[string]int64, len(cpBytes.rest))
	for k, v := range cpBytes.rest {
		rest[k] = v
	}
	return cpBytes.etcd.Load(), rest
}

// restBytes counts the bodies of a client. RoundTrip runs in the goroutine
// that sends the request, so its label names the component; the response
// body counts for the same component, whoever reads it.
type restBytes struct{ next http.RoundTripper }

func (r restBytes) RoundTrip(req *http.Request) (*http.Response, error) {
	comp := goroutineComponent()
	if req.ContentLength > 0 {
		addRESTBytes(comp, req.ContentLength)
	}
	resp, err := r.next.RoundTrip(req)
	// An upgraded connection (exec, port-forward) keeps its body: the
	// streams use it as the connection.
	if resp != nil && resp.Body != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body = countingBody{resp.Body, comp}
	}
	return resp, err
}

// WrappedRoundTripper gives the wrapped transport, as every client-go wrapper
// does. The SPDY round tripper looks through the wrappers for the TLS config
// of its UpgradeTransport: the apiserver translates the WebSocket of kubectl
// exec and attach into SPDY to the node with the transport of its kubelet
// client, and fails without the TLS config ("does not expose TLS client
// config").
func (r restBytes) WrappedRoundTripper() http.RoundTripper { return r.next }

type countingBody struct {
	io.ReadCloser
	comp string
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		addRESTBytes(b.comp, int64(n))
	}
	return n, err
}

// k8s/patch.go makes every client-go transport pass WrapRoundTripper.
func init() {
	transport.WrapRoundTripper = func(rt http.RoundTripper) http.RoundTripper { return restBytes{rt} }
}

// countingListener counts the bytes of every connection it accepts.
type countingListener struct {
	net.Listener
	n *atomic.Int64
}

func (l countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return countingConn{c, l.n}, nil
}

type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

func (c countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.n.Add(int64(n))
	return n, err
}
