package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"syscall/js"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

// tcpWorker is a fake pod worker for the handlers of tabnode_wasi_tcp.go.
type tcpWorker struct {
	t      *testing.T
	p      *wasiPeer
	posted *[]js.Value
	used   map[int]bool
}

// tcpPeer gives a fake pod worker in a pod with the IP ip. c is the
// container of the account, or nil. w is the pod worker of the pod, or nil.
func tcpPeer(t *testing.T, ip string, c *ctr, w *podWorker) *tcpWorker {
	t.Helper()
	handlers := map[string]wasiHandler{}
	for _, typ := range []string{"bind", "listen", "connect", "data", "credit", "shutdown", "closed", "lookup"} {
		if wasiHandlers[typ] == nil {
			t.Fatalf("no handler for %s", typ)
		}
		handlers[typ] = wasiHandlers[typ]
	}
	closers := map[string][]func(*wasiPeer, uint32){"socket": wasiClosers["socket"], "lookup": wasiClosers["lookup"]}
	if len(closers["socket"]) == 0 || len(closers["lookup"]) == 0 {
		t.Fatal("the closers of socket and lookup are not registered")
	}
	p, posted := testPeer(t, handlers, closers)
	if w == nil {
		w = &podWorker{namespace: "default"}
	}
	ctx, cancel := context.WithCancel(withProc(context.Background(), &procEnv{w: w, c: c, ip: ip}))
	t.Cleanup(cancel)
	p.State(wasiCtxState, func() any { return ctx })
	saved := theTabNode
	theTabNode = nil
	t.Cleanup(func() { theTabNode = saved })
	t.Cleanup(p.Close)
	return &tcpWorker{t: t, p: p, posted: posted, used: map[int]bool{}}
}

func (w *tcpWorker) send(fields map[string]any) { w.p.dispatch(msg(fields)) }

// find gives the first message of the type typ with the ID id in the field
// key that the test did not take yet.
func (w *tcpWorker) find(typ, key string, id uint32) (js.Value, bool) {
	for i, v := range *w.posted {
		if w.used[i] || v.Get("type").String() != typ || uint32(v.Get(key).Float()) != id {
			continue
		}
		w.used[i] = true
		return v, true
	}
	return js.Undefined(), false
}

// wait waits for a message of the type typ for the socket id.
func (w *tcpWorker) wait(typ string, id uint32) js.Value {
	w.t.Helper()
	return w.waitKey(typ, "socket", id)
}

func (w *tcpWorker) waitKey(typ, key string, id uint32) js.Value {
	w.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if v, ok := w.find(typ, key, id); ok {
			return v
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("no %s for %s %d; messages: %s", typ, key, id, w.dump())
		}
	}
}

// none fails if a message of the type typ for the socket id arrives in d.
func (w *tcpWorker) none(typ string, id uint32, d time.Duration) {
	w.t.Helper()
	time.Sleep(d)
	if v, ok := w.find(typ, "socket", id); ok {
		w.t.Fatalf("unexpected %s", js.Global().Get("JSON").Call("stringify", v).String())
	}
}

func (w *tcpWorker) dump() string {
	var b strings.Builder
	for _, v := range *w.posted {
		if v.Get("bytes").Type() == js.TypeObject {
			b.WriteString(v.Get("type").String() + " " + strconv.Itoa(v.Get("socket").Int()) + " (" + strconv.Itoa(v.Get("bytes").Get("byteLength").Int()) + " bytes); ")
			continue
		}
		b.WriteString(js.Global().Get("JSON").Call("stringify", v).String() + "; ")
	}
	return b.String()
}

func errOf(v js.Value) string {
	if e := v.Get("error"); e.Type() == js.TypeString {
		return e.String()
	}
	return ""
}

func bytesOf(v js.Value) []byte { return jsBytes(js.Global().Get("Uint8Array").New(v.Get("bytes"))) }

// bind sends "bind" and gives the "bound" answer.
func (w *tcpWorker) bind(id uint32, family, addr string, port int) js.Value {
	w.t.Helper()
	w.send(map[string]any{"type": "bind", "socket": id, "protocol": "tcp", "family": family, "address": addr, "port": port})
	return w.wait("bound", id)
}

// listenOn binds and listens, and fails the test on an error.
func (w *tcpWorker) listenOn(id uint32, addr string, port, backlog int) int {
	w.t.Helper()
	b := w.bind(id, "ipv4", addr, port)
	if e := errOf(b); e != "" {
		w.t.Fatalf("bind %s:%d: %s", addr, port, e)
	}
	w.send(map[string]any{"type": "listen", "socket": id, "backlog": backlog})
	if e := errOf(w.wait("listening", id)); e != "" {
		w.t.Fatalf("listen %s:%d: %s", addr, port, e)
	}
	return b.Get("port").Int()
}

// connect sends "connect" and gives the "connected" answer.
func (w *tcpWorker) connect(id uint32, addr string, port int) js.Value {
	w.t.Helper()
	w.send(map[string]any{"type": "connect", "socket": id, "protocol": "tcp", "address": addr, "port": port})
	return w.wait("connected", id)
}

func (w *tcpWorker) data(id uint32, b []byte) {
	w.send(map[string]any{"type": "data", "socket": id, "bytes": bytesBuf(b)})
}

// read reads n bytes of "data" for the socket id. With credit, it gives the
// credit back after each message, as a component that reads does.
func (w *tcpWorker) read(id uint32, n int, credit bool) []byte {
	w.t.Helper()
	var out []byte
	for len(out) < n {
		b := bytesOf(w.wait("data", id))
		if len(b) == 0 {
			w.t.Fatalf("end of the stream after %d bytes, want %d", len(out), n)
		}
		out = append(out, b...)
		if credit {
			w.send(map[string]any{"type": "credit", "socket": id, "kind": "bytes", "n": len(b)})
		}
	}
	return out
}

// eof waits for "data" with 0 bytes (rule 7).
func (w *tcpWorker) eof(id uint32) {
	w.t.Helper()
	if b := bytesOf(w.wait("data", id)); len(b) != 0 {
		w.t.Fatalf("data with %d bytes, want the end of the stream", len(b))
	}
}

func dialT(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// readAll reads until the end of the stream with a limit of time.
func readAll(t *testing.T, c net.Conn) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read %d bytes, then %v", len(b), err)
	}
	return b
}

// TC1, rule 2, rule 4, rule 6, rule 7, rule 9: an echo through the
// messages. The listener on 0.0.0.0 accepts on the pod IP.
func TestWasiTCPEcho(t *testing.T) {
	w := tcpPeer(t, "10.244.252.1", nil, nil)
	b := w.bind(1, "ipv4", "0.0.0.0", 8080)
	if errOf(b) != "" || b.Get("address").String() != "10.244.252.1" || b.Get("port").Int() != 8080 {
		t.Fatalf("bound %s", w.dump())
	}
	w.send(map[string]any{"type": "listen", "socket": 1, "backlog": 4})
	if e := errOf(w.wait("listening", 1)); e != "" {
		t.Fatalf("listening: %s", e)
	}
	c := dialT(t, "10.244.252.1:8080")
	a := w.wait("accepted", 1)
	id := uint32(a.Get("accepted").Float())
	if id < wasiGoIDs || !w.p.Owns("socket", id) {
		t.Fatalf("accepted ID %d", id)
	}
	ca := c.LocalAddr().(*net.TCPAddr)
	if a.Get("localAddress").String() != "10.244.252.1" || a.Get("localPort").Int() != 8080 ||
		a.Get("remoteAddress").String() != ca.IP.String() || a.Get("remotePort").Int() != ca.Port {
		t.Errorf("accepted %s, client %s", w.dump(), ca)
	}
	c.Write([]byte("hello"))
	got := w.read(id, 5, true)
	if string(got) != "hello" {
		t.Fatalf("data %q", got)
	}
	w.data(id, []byte("HELLO"))
	if v := w.wait("credit", id); v.Get("kind").String() != "bytes" || v.Get("n").Int() != 5 {
		t.Errorf("credit %s", w.dump())
	}
	buf := make([]byte, 5)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "HELLO" {
		t.Fatalf("client read %q %v", buf, err)
	}
	// Rule 7: the half-close of the client.
	c.(interface{ CloseWrite() error }).CloseWrite()
	w.eof(id)
	// Rule 9: the end of the stream for the client, not a reset.
	w.send(map[string]any{"type": "closed", "socket": id})
	if rest := readAll(t, c); len(rest) != 0 {
		t.Errorf("client read %q after closed", rest)
	}
	// The end of the listener.
	w.send(map[string]any{"type": "closed", "socket": 1})
	time.Sleep(20 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", "10.244.252.1:8080", time.Second); err == nil {
		t.Error("the listener accepts after closed")
	}
	if w.p.Drops() != 0 {
		t.Errorf("%d drops", w.p.Drops())
	}
}

// TC8, rule 2, rule 5: connect uses dialCluster. The two sockets of one
// component talk through the messages.
func TestWasiTCPConnect(t *testing.T) {
	w := tcpPeer(t, "10.244.252.2", nil, nil)
	w.listenOn(1, "0.0.0.0", 9000, 1)
	cv := w.connect(2, "10.244.252.2", 9000)
	if e := errOf(cv); e != "" {
		t.Fatalf("connected: %s", e)
	}
	a := w.wait("accepted", 1)
	srv := uint32(a.Get("accepted").Float())
	if cv.Get("localAddress").String() != "10.244.252.2" || cv.Get("remoteAddress").String() != "10.244.252.2" ||
		cv.Get("remotePort").Int() != 9000 || cv.Get("localPort").Int() != a.Get("remotePort").Int() {
		t.Errorf("connected %s", w.dump())
	}
	w.data(2, []byte("ping"))
	if got := w.read(srv, 4, true); string(got) != "ping" {
		t.Errorf("server got %q", got)
	}
	w.data(srv, []byte("pong"))
	if got := w.read(2, 4, true); string(got) != "pong" {
		t.Errorf("client got %q", got)
	}
	// A connect on a socket that is connected.
	if e := errOf(w.connect(2, "10.244.252.2", 9000)); e != "invalid-state" {
		t.Errorf("second connect: %q, want invalid-state", e)
	}
}

// Rule 4: the backlog is the first credit of connections. Without credit,
// the Go side does not accept.
func TestWasiTCPBacklogCredit(t *testing.T) {
	w := tcpPeer(t, "10.244.252.3", nil, nil)
	w.listenOn(1, "10.244.252.3", 80, 1)
	dialT(t, "10.244.252.3:80")
	w.wait("accepted", 1)
	dialT(t, "10.244.252.3:80")
	w.none("accepted", 1, 100*time.Millisecond)
	w.send(map[string]any{"type": "credit", "socket": 1, "kind": "connections", "n": 1})
	w.wait("accepted", 1)
}

// TC7, TC6, rule 3, rule 5: an IPv6 socket binds to :: and listens, but
// has no listener in the in-process network. An IPv6 address gives
// remote-unreachable.
func TestWasiTCPIPv6(t *testing.T) {
	w := tcpPeer(t, "10.244.252.4", nil, nil)
	b := w.bind(1, "ipv6", "::", 7000)
	if errOf(b) != "" || b.Get("port").Int() != 7000 {
		t.Fatalf("bound %s", w.dump())
	}
	w.send(map[string]any{"type": "listen", "socket": 1, "backlog": 1})
	if e := errOf(w.wait("listening", 1)); e != "" {
		t.Fatalf("listening: %s", e)
	}
	if c, err := net.DialTimeout("tcp", "10.244.252.4:7000", time.Second); err == nil {
		c.Close()
		t.Error("TC7: the IPv6 socket listens on the pod IP")
	}
	// The address is free for IPv4.
	if e := errOf(w.bind(2, "ipv4", "0.0.0.0", 7000)); e != "" {
		t.Errorf("IPv4 bind after IPv6: %s", e)
	}
	if b := w.bind(3, "ipv6", "::1", 0); errOf(b) != "" || b.Get("port").Int() < 32768 || b.Get("port").Int() > 60999 {
		t.Errorf("bound ::1 port 0: %s", w.dump())
	}
	if e := errOf(w.bind(4, "ipv6", "2001:db8::1", 0)); e != "address-not-bindable" {
		t.Errorf("bind 2001:db8::1: %q", e)
	}
	if e := errOf(w.connect(5, "2001:db8::1", 80)); e != "remote-unreachable" {
		t.Errorf("connect to IPv6: %q, want remote-unreachable", e)
	}
	// TC20 on the Go side.
	if e := errOf(w.connect(1, "10.244.252.4", 80)); e != "invalid-state" {
		t.Errorf("connect on a listener: %q", e)
	}
	w.bind(6, "ipv6", "::", 0)
	if e := errOf(w.connect(6, "10.244.252.4", 80)); e != "invalid-argument" {
		t.Errorf("TC20: %q, want invalid-argument", e)
	}
	// An IPv4-mapped address is not IPv4 (runwasi), also for a socket that
	// is not bound.
	w.listenOn(7, "0.0.0.0", 8080, 1)
	for id, addr := range map[uint32]string{6: "::ffff:10.244.252.4", 8: "::ffff:10.244.252.4"} {
		if e := errOf(w.connect(id, addr, 8080)); e != "invalid-argument" {
			t.Errorf("connect %d to %s: %q, want invalid-argument", id, addr, e)
		}
	}
}

// TC11: a port without a listener gives connection-refused at once.
func TestWasiTCPRefused(t *testing.T) {
	w := tcpPeer(t, "10.244.252.5", nil, nil)
	start := time.Now()
	if e := errOf(w.connect(1, "10.244.252.5", 1)); e != "connection-refused" {
		t.Errorf("connect: %q, want connection-refused", e)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("connection-refused after %v", d)
	}
	// Rule 9: the ID belongs to the container until closed.
	if !w.p.Owns("socket", 1) {
		t.Error("the ID does not belong to the container after connected with an error")
	}
	w.send(map[string]any{"type": "closed", "socket": 1})
	if w.p.Owns("socket", 1) || w.p.Drops() != 0 {
		t.Errorf("after closed: owned %v, %d drops", w.p.Owns("socket", 1), w.p.Drops())
	}
}

// TC10: a connection that NetworkPolicy denies gives timeout after
// synTimeout (a short synTimeout: plan rule 5).
func TestWasiTCPPolicyTimeout(t *testing.T) {
	w := tcpPeer(t, "10.244.0.4", nil, nil)
	fetchPolicyNode(t)
	setDurations(t, 200*time.Millisecond, 10*time.Second)
	start := time.Now()
	if e := errOf(w.connect(1, "10.244.0.2", 80)); e != "timeout" {
		t.Errorf("connect: %q, want timeout", e)
	}
	if d := time.Since(start); d < 200*time.Millisecond {
		t.Errorf("timeout after %v, before synTimeout", d)
	}
}

// Rule 5: a listener with a full queue gives timeout at the end of
// synTimeout (ETIMEDOUT of the in-process network).
func TestWasiTCPFullQueue(t *testing.T) {
	w := tcpPeer(t, "10.244.252.6", nil, nil)
	ln, err := net.Listen("tcp", "10.244.252.7:80")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	// The in-process network keeps SOMAXCONN (128) connections.
	for i := 0; i < 128; i++ {
		dialT(t, "10.244.252.7:80")
	}
	setDurations(t, 200*time.Millisecond, 10*time.Second)
	if e := errOf(w.connect(1, "10.244.252.7", 80)); e != "timeout" {
		t.Errorf("connect: %q, want timeout", e)
	}
}

// TC12, rule 6: without credit, the Go side sends at most 256 KiB. More
// credit gives more data.
func TestWasiTCPCredit(t *testing.T) {
	w := tcpPeer(t, "10.244.252.8", nil, nil)
	w.listenOn(1, "0.0.0.0", 80, 1)
	c := dialT(t, "10.244.252.8:80")
	id := uint32(w.wait("accepted", 1).Get("accepted").Float())
	go c.Write(make([]byte, 10<<20))
	time.Sleep(300 * time.Millisecond)
	sum := 0
	for {
		v, ok := w.find("data", "socket", id)
		if !ok {
			break
		}
		n := len(bytesOf(v))
		if n > wasiLimits["data"] {
			t.Errorf("data with %d bytes", n)
		}
		sum += n
	}
	if sum != 256<<10 {
		t.Fatalf("%d bytes without credit, want %d", sum, 256<<10)
	}
	w.send(map[string]any{"type": "credit", "socket": id, "kind": "bytes", "n": 1000})
	if got := w.read(id, 1000, false); len(got) != 1000 {
		t.Errorf("%d bytes after a credit of 1000", len(got))
	}
	w.none("data", id, 100*time.Millisecond)
}

// MS4: "data" above the credit is dropped, and the Go side closes the
// socket.
func TestWasiTCPDataAboveCredit(t *testing.T) {
	w := tcpPeer(t, "10.244.252.9", nil, nil)
	w.listenOn(1, "0.0.0.0", 80, 1)
	c := dialT(t, "10.244.252.9:80")
	id := uint32(w.wait("accepted", 1).Get("accepted").Float())
	// The client does not read: no credit comes back for a time.
	w.data(id, make([]byte, 200<<10))
	w.data(id, make([]byte, 100<<10))
	v := w.wait("closed", id)
	if errOf(v) != "invalid-argument" || w.p.Drops() != 1 || w.p.Owns("socket", id) {
		t.Fatalf("closed %q, %d drops, owned %v", errOf(v), w.p.Drops(), w.p.Owns("socket", id))
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	// The end of the connection: EOF, or a reset for the bytes that the
	// Go side did not read. Not the read deadline.
	if _, err := io.Copy(io.Discard, c); err != nil && strings.Contains(err.Error(), "timeout") {
		t.Errorf("client: %v, want the end of the connection", err)
	}
	// A data with 0 bytes is ignored (rule 7).
	w.data(1, nil)
	if w.p.Drops() != 1 {
		t.Errorf("%d drops after data with 0 bytes", w.p.Drops())
	}
}

// Rule 9: "closed" with pending data. The peer reads all bytes and then the
// end of the stream.
func TestWasiTCPClosedDrains(t *testing.T) {
	w := tcpPeer(t, "10.244.252.10", nil, nil)
	w.listenOn(1, "0.0.0.0", 80, 1)
	c := dialT(t, "10.244.252.10:80")
	id := uint32(w.wait("accepted", 1).Get("accepted").Float())
	want := bytes.Repeat([]byte("0123456789abcdef"), 16<<10) // 256 KiB
	for i := 0; i < 4; i++ {
		w.data(id, want[i*64<<10:(i+1)*64<<10])
	}
	w.send(map[string]any{"type": "closed", "socket": id})
	time.Sleep(50 * time.Millisecond)
	if got := readAll(t, c); !bytes.Equal(got, want) {
		t.Fatalf("client read %d bytes, want %d", len(got), len(want))
	}
}

// TC13 on the Go side, rule 7: shutdown of the send direction after the
// pending data. shutdown of the receive direction discards the bytes.
func TestWasiTCPShutdown(t *testing.T) {
	w := tcpPeer(t, "10.244.252.11", nil, nil)
	w.listenOn(1, "0.0.0.0", 80, 1)
	c := dialT(t, "10.244.252.11:80")
	id := uint32(w.wait("accepted", 1).Get("accepted").Float())
	w.data(id, []byte("answer"))
	w.send(map[string]any{"type": "shutdown", "socket": id, "how": "send"})
	if got := readAll(t, c); string(got) != "answer" {
		t.Errorf("client read %q", got)
	}
	// The receive direction is still open.
	c.Write([]byte("more"))
	if got := w.read(id, 4, true); string(got) != "more" {
		t.Errorf("server read %q", got)
	}
	w.send(map[string]any{"type": "shutdown", "socket": id, "how": "receive"})
	time.Sleep(20 * time.Millisecond)
	c.Write([]byte("discarded"))
	w.none("data", id, 100*time.Millisecond)
	// data after shutdown of the send direction fails the examination.
	w.data(id, []byte("x"))
	if errOf(w.wait("closed", id)) != "invalid-argument" {
		t.Error("data after shutdown is not dropped")
	}
}

// TC14, rule 10: the stop of the container closes the listener and the
// connections, and the client sees the end.
func TestWasiTCPStop(t *testing.T) {
	w := tcpPeer(t, "10.244.252.12", nil, nil)
	w.listenOn(1, "0.0.0.0", 80, 2)
	c := dialT(t, "10.244.252.12:80")
	w.wait("accepted", 1)
	if e := errOf(w.connect(2, "10.244.252.12", 80)); e != "" {
		t.Fatalf("connect: %s", e)
	}
	w.wait("accepted", 1)
	w.p.Close()
	if rest := readAll(t, c); len(rest) != 0 {
		t.Errorf("client read %q", rest)
	}
	time.Sleep(20 * time.Millisecond)
	if c2, err := net.DialTimeout("tcp", "10.244.252.12:80", time.Second); err == nil {
		c2.Close()
		t.Error("the listener accepts after the stop")
	}
	if n := len(tcpOf(w.p).socks); n != 0 {
		t.Errorf("%d sockets after the stop", n)
	}
	if portReserved("tcp", "10.244.252.12", 80) {
		t.Error("the port is reserved after the stop")
	}
}

// TC18, rule 3: a second bind fails with address-in-use when a listener has
// the address. Without a listener, it gives bound (until W3.3a records the
// result of runwasi).
func TestWasiTCPAddressInUse(t *testing.T) {
	w := tcpPeer(t, "10.244.252.13", nil, nil)
	if e := errOf(w.bind(1, "ipv4", "0.0.0.0", 7000)); e != "" {
		t.Fatal(e)
	}
	if e := errOf(w.bind(2, "ipv4", "0.0.0.0", 7000)); e != "" {
		t.Errorf("second bind without a listener: %q, want bound", e)
	}
	w.send(map[string]any{"type": "listen", "socket": 1, "backlog": 1})
	if e := errOf(w.wait("listening", 1)); e != "" {
		t.Fatal(e)
	}
	for i, addr := range []string{"0.0.0.0", "127.0.0.1", "10.244.252.13"} {
		if e := errOf(w.bind(uint32(10+i), "ipv4", addr, 7000)); e != "address-in-use" {
			t.Errorf("bind %s:7000 with a listener: %q, want address-in-use", addr, e)
		}
	}
	w.send(map[string]any{"type": "listen", "socket": 2, "backlog": 1})
	if e := errOf(w.wait("listening", 2)); e != "address-in-use" {
		t.Errorf("second listen: %q, want address-in-use", e)
	}
	// A second bind on the same socket.
	if e := errOf(w.bind(1, "ipv4", "0.0.0.0", 7001)); e != "invalid-state" {
		t.Errorf("bind on a bound socket: %q", e)
	}
	// Port 0 gives an ephemeral port (TC9 on the Go side).
	b := w.bind(3, "ipv4", "127.0.0.1", 0)
	if p := b.Get("port").Int(); errOf(b) != "" || p < 32768 || p > 60999 || b.Get("address").String() != "10.244.252.13" {
		t.Errorf("bind port 0: %s", w.dump())
	}
}

// TC19, MS3: a bind to a foreign IP gives address-not-bindable. It is not a
// drop, and the socket stays.
func TestWasiTCPNotBindable(t *testing.T) {
	w := tcpPeer(t, "10.244.252.14", nil, nil)
	for _, addr := range []string{"10.244.252.15", "192.0.2.1", "10.96.0.1"} {
		if e := errOf(w.bind(1, "ipv4", addr, 80)); e != "address-not-bindable" {
			t.Errorf("bind %s: %q, want address-not-bindable", addr, e)
		}
	}
	if w.p.Drops() != 0 || !w.p.Owns("socket", 1) {
		t.Fatalf("%d drops, owned %v", w.p.Drops(), w.p.Owns("socket", 1))
	}
	if _, ok := w.find("closed", "socket", 1); ok {
		t.Error("closed after address-not-bindable")
	}
	for _, addr := range []string{"127.0.0.5", "10.244.252.14"} {
		if e := errOf(w.bind(1, "ipv4", addr, 80)); e != "" {
			t.Fatalf("bind %s after the error: %s", addr, e)
		}
		w.send(map[string]any{"type": "closed", "socket": 1})
	}
}

// MS2: listen, data, credit, shutdown and closed with a socket ID that does
// not belong to the container are dropped, and the ID gets closed.
func TestWasiTCPForeignID(t *testing.T) {
	w := tcpPeer(t, "10.244.252.16", nil, nil)
	for i, m := range []map[string]any{
		{"type": "listen", "backlog": 1},
		{"type": "data", "bytes": bytesBuf([]byte("x"))},
		{"type": "credit", "kind": "bytes", "n": 1},
		{"type": "shutdown", "how": "both"},
		{"type": "closed"},
	} {
		id := uint32(100 + i)
		m["socket"] = id
		w.send(m)
		if v := w.wait("closed", id); errOf(v) != "invalid-argument" {
			t.Errorf("%s: closed %q", m["type"], errOf(v))
		}
		if w.p.Drops() != i+1 {
			t.Errorf("%s: %d drops", m["type"], w.p.Drops())
		}
	}
	// An ID of the Go side that the pod worker makes.
	w.send(map[string]any{"type": "bind", "socket": wasiGoIDs + 5, "protocol": "tcp", "family": "ipv4", "address": "0.0.0.0", "port": 0})
	w.wait("closed", wasiGoIDs+5)
	if _, ok := w.find("bound", "socket", wasiGoIDs+5); ok {
		t.Error("bound for a foreign ID")
	}
}

// lookupPeer gives a tab node with the Service web in default.
func lookupNode(t *testing.T) {
	theTabNode = &tabNode{net: testProxy(testService("default", "web", "10.96.5.5"), testService("other", "db", "10.96.5.6"))}
}

func (w *tcpWorker) lookup(id uint32, name string) ([]string, string) {
	w.t.Helper()
	w.send(map[string]any{"type": "lookup", "lookup": id, "name": name})
	v := w.waitKey("addresses", "lookup", id)
	if e := errOf(v); e != "" {
		return nil, e
	}
	var out []string
	a := v.Get("addresses")
	for i := 0; i < a.Length(); i++ {
		out = append(out, a.Index(i).String())
	}
	return out, ""
}

// NL1 to NL3, rule 11: /etc/hosts first, then the resolver of nslookup with
// the search path of the pod.
func TestWasiTCPLookup(t *testing.T) {
	pw := &podWorker{namespace: "default", pod: &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "client", Namespace: "default"}}}
	w := tcpPeer(t, "10.244.252.17", nil, pw)
	lookupNode(t)
	for i, c := range []struct{ name, want, err string }{
		{"localhost", "::1 127.0.0.1", ""}, // the order of runwasi
		{"LOCALHOST.", "::1 127.0.0.1", ""},
		{"client", "10.244.252.17", ""},
		{"web", "10.96.5.5", ""},                           // NL1: the search path
		{"web.default.svc.cluster.local", "10.96.5.5", ""}, // NL2
		{"db.other", "10.96.5.6", ""},
		{"db", "", "name-unresolvable"},
		{"example.com", "", "name-unresolvable"}, // NL3
	} {
		ips, e := w.lookup(uint32(i+1), c.name)
		if strings.Join(ips, " ") != c.want || e != c.err {
			t.Errorf("%s: %q %q, want %q %q", c.name, ips, e, c.want, c.err)
		}
		if w.p.Owns("lookup", uint32(i+1)) {
			t.Errorf("%s: the lookup ID belongs to the container after addresses", c.name)
		}
	}
	// The hosts file of the container, in the sequence of the file.
	if got := hostsLookup([]byte("# x\n10.0.0.1 a b\n::1 b # c\n10.0.0.2\tB\n"), "b"); strings.Join(got, " ") != "10.0.0.1 ::1 10.0.0.2" {
		t.Errorf("hostsLookup: %q", got)
	}
}

// tcpServiceNode gives a tab node with the Service echo (10.96.7.7:80) and
// the endpoint ip:port.
func tcpServiceNode(t *testing.T, ip string, port int32) {
	t.Helper()
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "echo"},
		Spec: v1.ServiceSpec{ClusterIP: "10.96.7.7", Ports: []v1.ServicePort{{Name: "tcp", Port: 80}}}}
	es := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "default", Name: "echo-abcde", Labels: map[string]string{discoveryv1.LabelServiceName: "echo"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{{Name: ptr.To("tcp"), Port: ptr.To(port)}},
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}},
	}
	s := newServiceProxy(nil)
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	idx.Add(svc)
	s.services = corelisters.NewServiceLister(idx)
	s.slices = cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{sliceIndex: func(obj any) ([]string, error) {
		e := obj.(*discoveryv1.EndpointSlice)
		return []string{e.Namespace + "/" + e.Labels[discoveryv1.LabelServiceName]}, nil
	}})
	s.slices.Add(es)
	s.synced.Store(true)
	s.sync()
	t.Cleanup(func() {
		s.mu.Lock()
		for _, l := range s.listeners {
			l.ln.Close()
		}
		s.mu.Unlock()
	})
	theTabNode = &tabNode{net: s}
}

// AC1 (TCP): 10 MB through a TCP Service. The CPU of the container
// increases. The time that the container gets does not go to the control
// plane pods (nodeSysModel.sample).
func TestWasiTCPChargesCPU(t *testing.T) {
	c := &ctr{id: "tcp-cpu-test", wasm: true}
	t.Cleanup(func() { forgetAcct(c) })
	w := tcpPeer(t, "10.244.252.18", c, nil)
	tcpServiceNode(t, "10.244.252.18", 8080)
	w.listenOn(1, "0.0.0.0", 8080, 1)

	var busy time.Duration
	measured := true
	var etcd int64
	rest := map[string]int64{compAPIServer: 0}
	fakeBusy(t, &busy, &measured)
	fakeBytes(t, &etcd, rest)
	var m nodeSysModel
	now := time.Now()
	m.sample(now, 1, true)
	m.sample(now.Add(time.Second), 1, true)
	before := acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds
	ctrsBefore, idleBefore := nodeCPU.get(), idleCPU.get()

	client := dialT(t, "10.96.7.7:80")
	id := uint32(w.wait("accepted", 1).Get("accepted").Float())
	const size = 10 << 20
	go client.Write(make([]byte, size))
	if got := w.read(id, size, true); len(got) != size {
		t.Fatalf("%d bytes", len(got))
	}
	var after uint64
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if after = acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds; after > before {
			break
		}
	}
	if after <= before {
		t.Fatalf("CPU of the container %d, before %d: nothing charged", after, before)
	}

	// The instance was busy for the charged time only, while the control
	// plane moved bytes: the control plane pods get none of it.
	charged := max(nodeCPU.get()-ctrsBefore-(idleCPU.get()-idleBefore), 0)
	busy += charged
	rest[compAPIServer] += 1000
	cp := m.sample(now.Add(2*time.Second), 1, true)
	for _, name := range compNames {
		if u := cp.comps[name].UsageCoreNanoSeconds; u != 0 {
			t.Errorf("control plane pod %s got %d ns of the charged time", name, u)
		}
	}
	// Without the charge, the same busy time goes to the control plane.
	var m2 nodeSysModel
	busy = 0
	m2.sample(now, 1, true)
	m2.sample(now.Add(time.Second), 1, true)
	busy += charged
	rest[compAPIServer] += 1000
	if u := m2.sample(now.Add(2*time.Second), 1, true).comps[compAPIServer].UsageCoreNanoSeconds; u == 0 {
		t.Error("the model gives no time to the control plane without a charge")
	}
}
