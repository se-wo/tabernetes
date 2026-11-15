package main

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"syscall/js"
	"testing"
	"time"
)

// udpPeer gives a fake pod worker in a pod with the IP ip, with the handlers
// of tabnode_wasi_tcp.go and "datagram". c is the container of the account,
// or nil.
func udpPeer(t *testing.T, ip string, c *ctr) *tcpWorker {
	t.Helper()
	h := wasiHandlers["datagram"]
	if h == nil {
		t.Fatal("no handler for datagram")
	}
	w := tcpPeer(t, ip, c, nil)
	wasiHandlers["datagram"] = h
	return w
}

// udpBind sends "bind" with the protocol udp and gives the "bound" answer.
func (w *tcpWorker) udpBind(id uint32, family, addr string, port int) js.Value {
	w.t.Helper()
	w.send(map[string]any{"type": "bind", "socket": id, "protocol": "udp", "family": family, "address": addr, "port": port})
	return w.wait("bound", id)
}

// udpBound binds 0.0.0.0 or another IPv4 address and fails the test on an
// error. It gives the port.
func (w *tcpWorker) udpBound(id uint32, addr string, port int) int {
	w.t.Helper()
	b := w.udpBind(id, "ipv4", addr, port)
	if e := errOf(b); e != "" {
		w.t.Fatalf("bind udp %s:%d: %s", addr, port, e)
	}
	return b.Get("port").Int()
}

func (w *tcpWorker) udpConnect(id uint32, addr string, port int) {
	w.send(map[string]any{"type": "connect", "socket": id, "protocol": "udp", "address": addr, "port": port})
}

func (w *tcpWorker) datagram(id uint32, addr string, port int, b []byte) {
	w.send(map[string]any{"type": "datagram", "socket": id, "address": addr, "port": port, "bytes": bytesBuf(b)})
}

// udpCredits waits for "credit" messages of the kind datagram-bytes for the
// socket id until they give n bytes back.
func (w *tcpWorker) udpCredits(id uint32, n int) {
	w.t.Helper()
	for got := 0; got < n; {
		v := w.wait("credit", id)
		if v.Get("kind").String() != "datagram-bytes" {
			w.t.Fatalf("credit %s", w.dump())
		}
		got += v.Get("n").Int()
	}
}

// udpSend sends size bytes in datagrams of 60,000 bytes from the socket id
// to addr and port. It keeps to the credit of the pod worker (rule 6), as
// sockets.js does. A helper for the AC1 tests (also through a Service).
func (w *tcpWorker) udpSend(id uint32, addr string, port, size int) {
	w.t.Helper()
	const chunk = 60000
	credit, sent := udpCredit, 0
	for sent < size {
		n := min(chunk, size-sent)
		for credit < n {
			v := w.wait("credit", id)
			credit += v.Get("n").Int()
		}
		w.datagram(id, addr, port, make([]byte, n))
		credit -= n
		sent += n
	}
}

// udpGot waits for a "datagram" for the socket id and gives its bytes and
// its source.
func (w *tcpWorker) udpGot(id uint32) ([]byte, string) {
	w.t.Helper()
	v := w.wait("datagram", id)
	return bytesOf(v), v.Get("address").String() + ":" + strconv.Itoa(v.Get("port").Int())
}

// UD2, rules 3, 5 and 6: a datagram from a socket of another pod arrives as
// "datagram" with the source. The answer of the component arrives at the
// socket, and its credit comes back.
func TestWasiUDPEcho(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	b := w.udpBind(1, "ipv4", "0.0.0.0", 7)
	if errOf(b) != "" || b.Get("address").String() != ip || b.Get("port").Int() != 7 {
		t.Fatalf("bound %s", w.dump())
	}
	other := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	if _, err := other.WriteTo([]byte("ping"), udpTo(ip+":7")); err != nil {
		t.Fatal(err)
	}
	got, from := w.udpGot(1)
	if string(got) != "ping" || from != other.LocalAddr().String() {
		t.Fatalf("datagram %q from %s, want from %v", got, from, other.LocalAddr())
	}
	oa := other.LocalAddr().(*net.UDPAddr)
	w.datagram(1, oa.IP.String(), oa.Port, []byte("pong"))
	w.udpCredits(1, 4)
	if b, from := udpRecv(t, other); string(b) != "pong" || from.String() != ip+":7" {
		t.Fatalf("answer %q from %v", b, from)
	}
	// A datagram with 0 bytes is a datagram.
	other.WriteTo(nil, udpTo(ip+":7"))
	if got, _ := w.udpGot(1); len(got) != 0 {
		t.Errorf("empty datagram: %d bytes", len(got))
	}
	w.datagram(1, oa.IP.String(), oa.Port, nil)
	if b, _ := udpRecv(t, other); len(b) != 0 {
		t.Errorf("empty answer: %d bytes", len(b))
	}
	w.none("connected", 1, 0)
	if w.p.Drops() != 0 {
		t.Errorf("%d drops: %s", w.p.Drops(), w.dump())
	}
}

// UD3, rule 2: port 0 gives the first free port from 32768.
func TestWasiUDPEphemeralPort(t *testing.T) {
	w := udpPeer(t, nextLoopbackIP(), nil)
	if p := w.udpBound(1, "0.0.0.0", 0); p != 32768 {
		t.Errorf("port %d, want 32768", p)
	}
	if p := w.udpBound(2, "127.0.0.1", 0); p != 32769 {
		t.Errorf("port %d, want 32769", p)
	}
}

// Rule 2, UD1: a second UDP bind to the port gives address-in-use, also
// through 127.0.0.1. TCP and UDP bind the same port.
func TestWasiUDPAddressInUse(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	w.udpBound(1, "0.0.0.0", 53)
	for i, addr := range []string{"0.0.0.0", "127.0.0.1", ip} {
		id := uint32(2 + i)
		if e := errOf(w.udpBind(id, "ipv4", addr, 53)); e != "address-in-use" {
			t.Errorf("second bind to %s: %q, want address-in-use", addr, e)
		}
	}
	w.listenOn(10, "0.0.0.0", 53, 1)
	// The socket of a failed bind can bind again (MS3: no drop).
	if p := w.udpBound(2, "0.0.0.0", 54); p != 54 {
		t.Errorf("port %d", p)
	}
	if w.p.Drops() != 0 {
		t.Errorf("%d drops", w.p.Drops())
	}
}

// TC19 analog, MS3: an IP of another host gives address-not-bindable. The
// socket stays.
func TestWasiUDPNotBindable(t *testing.T) {
	w := udpPeer(t, nextLoopbackIP(), nil)
	if e := errOf(w.udpBind(1, "ipv4", "192.0.2.7", 9)); e != "address-not-bindable" {
		t.Fatalf("bind: %q", e)
	}
	if !w.p.Owns("socket", 1) || w.p.Drops() != 0 {
		t.Fatalf("owned %v, %d drops", w.p.Owns("socket", 1), w.p.Drops())
	}
	w.udpBound(1, "0.0.0.0", 9)
}

// Rule 2: an IPv6 socket on :: gets bound and no socket in the UDP table.
// Its datagrams are dropped, and their credit comes back.
func TestWasiUDPIPv6(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	b := w.udpBind(1, "ipv6", "::", 0)
	if errOf(b) != "" || b.Get("address").String() != "::" || b.Get("port").Int() < 32768 {
		t.Fatalf("bound %s", w.dump())
	}
	port := b.Get("port").Int()
	if e := errOf(w.udpBind(2, "ipv6", "::1", 5000)); e != "" {
		t.Fatalf("bind ::1: %s", e)
	}
	if e := errOf(w.udpBind(3, "ipv6", "2001:db8::1", 5000)); e != "address-not-bindable" {
		t.Errorf("bind 2001:db8::1: %q", e)
	}
	if portReserved("udp", ip, port) || portReserved("udp", ip, 5000) {
		t.Error("an IPv6 socket has a port of the table")
	}
	w.udpConnect(1, "::1", 5000)
	w.datagram(1, "::1", 5000, []byte("v6"))
	w.udpCredits(1, 2)
	w.none("datagram", 2, 20*time.Millisecond)
	// The other family cannot come from a correct pod worker.
	w.datagram(1, "10.0.0.1", 5000, []byte("v4"))
	if v := w.wait("closed", 1); errOf(v) != "invalid-argument" {
		t.Errorf("closed %q", errOf(v))
	}
}

// Rule 4: connect sets the filter of the source, connect with 0.0.0.0:0
// removes it. connect before bind and an address that a correct pod worker
// does not send fail (MS6). No "connected" for UDP.
func TestWasiUDPConnect(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	port := w.udpBound(1, "0.0.0.0", 0)
	a := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	b := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	to := udpTo(ip + ":" + strconv.Itoa(port))
	aa := a.LocalAddr().(*net.UDPAddr)
	w.udpConnect(1, aa.IP.String(), aa.Port)
	b.WriteTo([]byte("from b"), to)
	a.WriteTo([]byte("from a"), to)
	if got, from := w.udpGot(1); string(got) != "from a" || from != aa.String() {
		t.Fatalf("datagram %q from %s", got, from)
	}
	w.none("datagram", 1, 20*time.Millisecond)
	w.udpConnect(1, "0.0.0.0", 0)
	b.WriteTo([]byte("from b"), to)
	if got, _ := w.udpGot(1); string(got) != "from b" {
		t.Fatalf("datagram %q after the disconnect", got)
	}
	w.none("connected", 1, 0)
	if w.p.Drops() != 0 {
		t.Fatalf("%d drops", w.p.Drops())
	}

	drops := 0
	for _, c := range []struct {
		id   uint32
		bind bool
		addr string
		port int
	}{
		{20, false, aa.IP.String(), aa.Port}, // before bind (rule 1)
		{21, true, "::", 0},                  // the other family
		{22, true, "::ffff:10.0.0.1", 9},     // IPv4-mapped
		{23, true, "0.0.0.0", 9},             // unspecified
		{24, true, "10.0.0.1", 0},            // port 0
	} {
		if c.bind {
			w.udpBound(c.id, "0.0.0.0", 0)
		}
		w.udpConnect(c.id, c.addr, c.port)
		drops++
		if v := w.wait("closed", c.id); errOf(v) != "invalid-argument" || w.p.Drops() != drops {
			t.Errorf("connect %d to %s:%d: closed %q, %d drops", c.id, c.addr, c.port, errOf(v), w.p.Drops())
		}
	}
}

// UD9, rule 7: a connected socket sends to a port without a socket. The read
// pump takes the pending error: exactly one "error" with
// connection-refused. The credit comes back, and the socket continues.
func TestWasiUDPRefused(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	udpTabNode(t)
	port := w.udpBound(1, "0.0.0.0", 0)
	dead := nextLoopbackIP()
	w.udpConnect(1, dead, 9)
	w.datagram(1, dead, 9, []byte("a"))
	w.udpCredits(1, 1)
	if v := w.wait("error", 1); errOf(v) != "connection-refused" {
		t.Fatalf("error %q", errOf(v))
	}
	w.none("error", 1, 20*time.Millisecond)

	// The socket continues: a new remote address in both directions.
	live := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	la := live.LocalAddr().(*net.UDPAddr)
	w.udpConnect(1, la.IP.String(), la.Port)
	w.datagram(1, la.IP.String(), la.Port, []byte("c"))
	if b, _ := udpRecv(t, live); string(b) != "c" {
		t.Fatalf("live got %q", b)
	}
	live.WriteTo([]byte("d"), udpTo(ip+":"+strconv.Itoa(port)))
	if got, _ := w.udpGot(1); string(got) != "d" {
		t.Fatalf("datagram %q", got)
	}
	// Two refusals give two errors.
	w.udpConnect(1, dead, 9)
	for range 2 {
		w.datagram(1, dead, 9, []byte("e"))
		w.udpCredits(1, 1)
		if v := w.wait("error", 1); errOf(v) != "connection-refused" {
			t.Fatalf("error %q", errOf(v))
		}
	}
	// A socket that is not connected gets no error.
	w.udpConnect(1, "0.0.0.0", 0)
	w.datagram(1, dead, 9, []byte("f"))
	w.udpCredits(1, 1)
	w.none("error", 1, 20*time.Millisecond)
	if w.p.Drops() != 0 {
		t.Errorf("%d drops", w.p.Drops())
	}
}

// UD9, rule 7: the write pump takes the pending error. The socket has no
// read pump here, so only the write pump can take it. The datagram of the
// send that takes it does not arrive, its credit comes back, and exactly one
// "error" goes out.
func TestWasiUDPRefusedOnWrite(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	udpTabNode(t)
	const id = 7
	w.p.mu.Lock()
	w.p.claim("socket", id)
	w.p.mu.Unlock()
	k := newUDPSock(w.p, id, false)
	conn, err := listenPodUDP(k.ctx, ":0")
	if err != nil {
		t.Fatal(err)
	}
	k.conn = conn
	udpOf(w.p).put(k)
	go k.writePump()

	dead := netip.MustParseAddrPort(nextLoopbackIP() + ":9")
	conn.Connect(dead)
	conn.Write([]byte("refused"))
	live := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	la := live.LocalAddr().(*net.UDPAddr)
	conn.Connect(la.AddrPort())
	w.datagram(id, la.IP.String(), la.Port, []byte("lost"))
	w.udpCredits(id, 4)
	if v := w.wait("error", id); errOf(v) != "connection-refused" {
		t.Fatalf("error %q", errOf(v))
	}
	udpNothing(t, live)
	w.datagram(id, la.IP.String(), la.Port, []byte("sent"))
	w.udpCredits(id, 4)
	if b, _ := udpRecv(t, live); string(b) != "sent" {
		t.Fatalf("live got %q", b)
	}
	w.none("error", id, 20*time.Millisecond)
}

// Rule 8: "closed" after "datagram" messages. The datagrams arrive, then the
// socket closes and its port is free.
func TestWasiUDPClosedDrains(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	port := w.udpBound(1, "0.0.0.0", 0)
	live := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	la := live.LocalAddr().(*net.UDPAddr)
	for i := range 20 {
		w.datagram(1, la.IP.String(), la.Port, []byte{byte(i)})
	}
	w.send(map[string]any{"type": "closed", "socket": 1})
	for i := range 20 {
		if b, _ := udpRecv(t, live); !bytes.Equal(b, []byte{byte(i)}) {
			t.Fatalf("datagram %d: %v", i, b)
		}
	}
	for deadline := time.Now().Add(5 * time.Second); portReserved("udp", ip, port); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the port is reserved after closed")
		}
	}
	if w.p.Owns("socket", 1) || w.p.Drops() != 0 {
		t.Errorf("owned %v, %d drops", w.p.Owns("socket", 1), w.p.Drops())
	}
}

// UD5, UD6, rule 6: 1,000 datagrams of 1,024 bytes to a socket whose pod
// worker sends no credit. The Go side sends at most 256 KiB, and the sender
// gets no error. A credit lets the next datagram pass.
func TestWasiUDPNoCredit(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	port := w.udpBound(1, "0.0.0.0", 0)
	to := udpTo(ip + ":" + strconv.Itoa(port))
	s := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	for range 1000 {
		if _, err := s.WriteTo(make([]byte, 1024), to); err != nil {
			t.Fatalf("sender: %v", err)
		}
		time.Sleep(0) // let the read pump run
	}
	time.Sleep(50 * time.Millisecond)
	n, total := 0, 0
	for {
		v, ok := w.find("datagram", "socket", 1)
		if !ok {
			break
		}
		n++
		total += v.Get("bytes").Get("byteLength").Int()
	}
	if n == 0 || total > udpCredit {
		t.Fatalf("%d datagrams with %d bytes, want at most %d bytes", n, total, udpCredit)
	}
	udpNothing(t, s) // no error for the sender
	w.send(map[string]any{"type": "credit", "socket": 1, "kind": "datagram-bytes", "n": udpCredit})
	s.WriteTo([]byte("next"), to)
	if got, _ := w.udpGot(1); string(got) != "next" {
		t.Fatalf("datagram %q after the credit", got)
	}
	if w.p.Drops() != 0 {
		t.Errorf("%d drops", w.p.Drops())
	}
}

// MS4: a "datagram" above the credit of the pod worker closes the socket.
func TestWasiUDPDatagramAboveCredit(t *testing.T) {
	w := udpPeer(t, nextLoopbackIP(), nil)
	w.udpBound(1, "0.0.0.0", 0)
	// No goroutine runs between the messages: the credit does not come
	// back in this time.
	for range 5 {
		w.datagram(1, "10.244.99.1", 9, make([]byte, 60000))
	}
	v := w.wait("closed", 1)
	if errOf(v) != "invalid-argument" || w.p.Drops() != 1 || w.p.Owns("socket", 1) {
		t.Fatalf("closed %q, %d drops, owned %v", errOf(v), w.p.Drops(), w.p.Owns("socket", 1))
	}
	if udpOf(w.p).get(1) != nil {
		t.Error("the socket stays after MS4")
	}
}

// MS2, rule 1: foreign IDs, the messages of TCP for a UDP socket, and one
// protocol for each socket ID.
func TestWasiUDPForeignID(t *testing.T) {
	w := udpPeer(t, nextLoopbackIP(), nil)
	drops := 0
	failed := func(name string, id uint32) {
		t.Helper()
		drops++
		if v := w.wait("closed", id); errOf(v) != "invalid-argument" || w.p.Drops() != drops {
			t.Errorf("%s: closed %q, %d drops", name, errOf(v), w.p.Drops())
		}
	}
	// MS2: IDs that do not belong to the container.
	w.datagram(100, "10.244.99.1", 9, []byte("x"))
	failed("datagram with a foreign ID", 100)
	w.send(map[string]any{"type": "credit", "socket": 101, "kind": "datagram-bytes", "n": 1})
	failed("credit with a foreign ID", 101)

	// Rule 1: data (also with 0 bytes) and shutdown for a UDP socket.
	for i, m := range []map[string]any{
		{"type": "data", "bytes": bytesBuf([]byte("x"))},
		{"type": "data", "bytes": bytesBuf(nil)},
		{"type": "shutdown", "how": "both"},
	} {
		id := uint32(1 + i)
		w.udpBound(id, "0.0.0.0", 0)
		m["socket"] = id
		w.send(m)
		failed(m["type"].(string), id)
		if udpOf(w.p).get(id) != nil {
			t.Errorf("%s: the socket stays", m["type"])
		}
	}
	// listen for a UDP socket: invalid-state, no drop.
	w.udpBound(4, "0.0.0.0", 0)
	w.send(map[string]any{"type": "listen", "socket": 4, "backlog": 1})
	if e := errOf(w.wait("listening", 4)); e != "invalid-state" {
		t.Errorf("listen: %q", e)
	}
	// A second UDP bind, and a TCP bind for the UDP socket: invalid-state.
	if e := errOf(w.udpBind(4, "ipv4", "0.0.0.0", 0)); e != "invalid-state" {
		t.Errorf("second bind: %q", e)
	}
	if e := errOf(w.bind(4, "ipv4", "0.0.0.0", 0)); e != "invalid-state" {
		t.Errorf("tcp bind of a UDP socket: %q", e)
	}
	// A UDP bind for a TCP socket.
	if e := errOf(w.bind(5, "ipv4", "0.0.0.0", 0)); e != "" {
		t.Fatalf("tcp bind: %q", e)
	}
	if e := errOf(w.udpBind(5, "ipv4", "0.0.0.0", 0)); e != "invalid-state" {
		t.Errorf("udp bind of a TCP socket: %q", e)
	}
	// datagram and UDP connect for a TCP socket.
	w.datagram(5, "10.244.99.1", 9, []byte("x"))
	failed("datagram for a TCP socket", 5)
	w.bind(6, "ipv4", "0.0.0.0", 0)
	w.udpConnect(6, "10.244.99.1", 9)
	failed("udp connect of a TCP socket", 6)
	// TCP connect for a UDP socket.
	w.send(map[string]any{"type": "connect", "socket": 4, "protocol": "tcp", "address": "10.244.99.1", "port": 9})
	failed("tcp connect of a UDP socket", 4)
	// datagram for a socket whose UDP bind failed.
	w.udpBind(7, "ipv4", "192.0.2.1", 0)
	w.datagram(7, "10.244.99.1", 9, []byte("x"))
	failed("datagram before bind", 7)
	if w.p.Drops() != drops {
		t.Errorf("%d drops, want %d", w.p.Drops(), drops)
	}
}

// UD20, UD21: a component on 127.0.0.1 receives from a socket of its own pod
// IP. A connected socket of another pod gets ECONNREFUSED.
func TestWasiUDPLoopback(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	port := w.udpBound(1, "127.0.0.1", 0)
	own := udpListen(t, podCtx(t, ip), ":0")
	for _, to := range []string{"127.0.0.1", ip} {
		own.WriteTo([]byte("own"), udpTo(to+":"+strconv.Itoa(port)))
		if got, from := w.udpGot(1); string(got) != "own" || from != own.LocalAddr().String() {
			t.Errorf("to %s: %q from %s", to, got, from)
		}
	}
	c, err := dialClusterUDP(podCtx(t, nextLoopbackIP()), ip+":"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("other"))
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 8)); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("other pod: %v, want ECONNREFUSED", err)
	}
	w.none("datagram", 1, 20*time.Millisecond)
}

// TC14 for UDP: the stop of the container closes the socket and frees the
// port.
func TestWasiUDPStop(t *testing.T) {
	ip := nextLoopbackIP()
	w := udpPeer(t, ip, nil)
	w.udpBound(1, "0.0.0.0", 5353)
	w.udpBound(2, "127.0.0.1", 5354)
	w.p.Close()
	if n := len(udpOf(w.p).socks); n != 0 {
		t.Errorf("%d sockets after the stop", n)
	}
	// The end of the context closes the udpConn in a goroutine.
	for deadline := time.Now().Add(5 * time.Second); portReserved("udp", ip, 5353) || portReserved("udp", ip, 5354); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a port is reserved after the stop")
		}
	}
	if loopbackRefused("udp", nil, tcpAddr(ip+":5354")) {
		t.Error("the loopback row stays after the stop")
	}
	udpListen(t, podCtx(t, ip), "0.0.0.0:5353")
}

// AC1: 10 MB of datagrams from the component to a socket of another pod
// increase the CPU of the container.
func TestWasiUDPChargesCPU(t *testing.T) {
	c := &ctr{id: "udp-msg-cpu-test", wasm: true}
	t.Cleanup(func() { forgetAcct(c) })
	w := udpPeer(t, nextLoopbackIP(), c)
	w.udpBound(1, "0.0.0.0", 0)
	srv := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	go func() {
		buf := make([]byte, udpMaxDatagram)
		for {
			if _, _, err := srv.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	before := acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds
	sa := srv.LocalAddr().(*net.UDPAddr)
	w.udpSend(1, sa.IP.String(), sa.Port, 10<<20)
	var after uint64
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if after = acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds; after > before {
			break
		}
	}
	if after <= before {
		t.Fatalf("CPU of the container %d, before %d: nothing charged", after, before)
	}
}
