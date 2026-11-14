package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// udpTabNode sets a tab node with the pod CIDR 10.244.0.0/16 for one test:
// a datagram to a pod IP without a socket is refused (UD8).
func udpTabNode(t *testing.T) {
	t.Helper()
	_, cidr, _ := net.ParseCIDR(clusterCIDR)
	withTabNode(t, &tabNode{podCIDR: cidr})
}

// udpListen binds addr for the pod with IP ip, or fails the test.
func udpListen(t *testing.T, ctx context.Context, addr string) *udpConn {
	t.Helper()
	c, err := listenPodUDP(ctx, addr)
	if err != nil {
		t.Fatalf("bind %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// udpRecv reads one datagram with a deadline of 5 seconds.
func udpRecv(t *testing.T, c *udpConn) ([]byte, net.Addr) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, udpMaxDatagram+1)
	n, from, err := c.ReadFrom(b)
	if err != nil {
		t.Fatalf("read on %v: %v", c.LocalAddr(), err)
	}
	return b[:n], from
}

// udpNothing checks that c has no datagram and no error to read.
func udpNothing(t *testing.T, c *udpConn) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	n, from, err := c.ReadFrom(make([]byte, 64))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read on %v: %d bytes from %v, %v; want nothing", c.LocalAddr(), n, from, err)
	}
	c.SetReadDeadline(time.Time{})
}

func udpTo(s string) *net.UDPAddr { return net.UDPAddrFromAddrPort(netip.MustParseAddrPort(s)) }

func udpPort(c *udpConn) int { return c.LocalAddr().(*net.UDPAddr).Port }

// UD1, phase 0 item 9: a TCP listener and a UDP socket on the same IP and
// port.
func TestUDPAndTCPSamePort(t *testing.T) {
	ip := nextLoopbackIP()
	ctx := podCtx(t, ip)
	ln, err := listenPod(ctx, ip+":53")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	u := udpListen(t, ctx, ip+":53")
	if got := u.LocalAddr().String(); got != ip+":53" {
		t.Errorf("udp local address %s", got)
	}
	cl := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	if _, err := cl.WriteTo([]byte("query"), udpTo(ip+":53")); err != nil {
		t.Fatal(err)
	}
	if b, _ := udpRecv(t, u); string(b) != "query" {
		t.Errorf("udp got %q", b)
	}
	serveEcho(ln)
	c, err := net.Dial("tcp", ip+":53")
	if err != nil {
		t.Fatalf("tcp dial: %v", err)
	}
	if got := firstLine(t, c); got == "" {
		t.Error("no tcp answer")
	}
}

// UD2: a socket on 0.0.0.0:P and a socket on the pod IP receive on the pod
// IP.
func TestUDPBindPodIP(t *testing.T) {
	ip := nextLoopbackIP()
	ctx := podCtx(t, ip)
	wild := udpListen(t, ctx, "0.0.0.0:7300")
	pod := udpListen(t, ctx, ip+":7301")
	v6 := udpListen(t, ctx, "[::]:7302")
	cl := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	for _, s := range []*udpConn{wild, pod, v6} {
		if s.LocalAddr().(*net.UDPAddr).IP.String() != ip {
			t.Errorf("local address %v", s.LocalAddr())
		}
		if _, err := cl.WriteTo([]byte("x"), s.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		if b, from := udpRecv(t, s); string(b) != "x" || from.String() != cl.LocalAddr().String() {
			t.Errorf("%v got %q from %v", s.LocalAddr(), b, from)
		}
	}
	if _, err := listenPodUDP(ctx, "10.244.77.1:7303"); !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Errorf("bind to another IP: %v", err)
	}
}

// UD3, UD4: each pod IP gets port 32768 first; the next socket gets 32769.
func TestUDPEphemeralPorts(t *testing.T) {
	a, b := podCtx(t, nextLoopbackIP()), podCtx(t, nextLoopbackIP())
	a1, b1, a2 := udpListen(t, a, ":0"), udpListen(t, b, "0.0.0.0:0"), udpListen(t, a, ":0")
	if udpPort(a1) != 32768 || udpPort(b1) != 32768 || udpPort(a2) != 32769 {
		t.Errorf("ports %d %d %d, want 32768 32768 32769", udpPort(a1), udpPort(b1), udpPort(a2))
	}
	// A TCP reservation of 32768 does not take the UDP port (UD1).
	ip := nextLoopbackIP()
	_, release, err := reservePort("tcp", ip, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if c := udpListen(t, podCtx(t, ip), ":0"); udpPort(c) != 32768 {
		t.Errorf("udp port %d after a tcp port", udpPort(c))
	}
	// A closed socket frees its port.
	a1.Close()
	if c := udpListen(t, a, ":0"); udpPort(c) != 32768 {
		t.Errorf("port %d after close, want 32768", udpPort(c))
	}
}

// Fact 1 of W4.5a, rule 2: a UDP bind is exclusive. A second bind to the
// same port gives errPortInUse, also 127.0.0.1:P after 0.0.0.0:P. Port 0
// gives a port from 32768 to 60999.
func TestUDPFact1BindExclusive(t *testing.T) {
	if !udpBindExclusive {
		t.Fatal("udpBindExclusive: section 8.3.3, rule 2 expects an exclusive bind")
	}
	ip := nextLoopbackIP()
	ctx := podCtx(t, ip)
	udpListen(t, ctx, "0.0.0.0:7310")
	for _, addr := range []string{"0.0.0.0:7310", ip + ":7310", "127.0.0.1:7310", "localhost:7310", "[::1]:7310"} {
		if _, err := listenPodUDP(ctx, addr); !errors.Is(err, errPortInUse) {
			t.Errorf("second bind %s: %v", addr, err)
		}
	}
	udpListen(t, ctx, "127.0.0.1:7311")
	if _, err := listenPodUDP(ctx, ip+":7311"); !errors.Is(err, errPortInUse) {
		t.Errorf("pod IP after 127.0.0.1: %v", err)
	}
	if proxyEphemeralFirst != 32768 || proxyEphemeralLast != 60999 {
		t.Errorf("ephemeral ports %d to %d", proxyEphemeralFirst, proxyEphemeralLast)
	}
}

// UD5, UD6: 1,000 datagrams of 1,024 bytes to a socket that does not read:
// no error for the sender, 256 datagrams in the queue.
func TestUDPQueueFull(t *testing.T) {
	srv := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	cl := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	for i := range 1000 {
		if n, err := cl.WriteTo(make([]byte, 1024), srv.LocalAddr()); err != nil || n != 1024 {
			t.Fatalf("send %d: %d %v", i, n, err)
		}
	}
	got := 0
	for {
		srv.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, _, err := srv.ReadFrom(make([]byte, 2048)); err != nil {
			break
		}
		got++
	}
	if got != udpQueueSize/1024 {
		t.Errorf("%d datagrams in the queue, want %d", got, udpQueueSize/1024)
	}
}

// UD8, UD9, fact 3 of W4.5a: a connected socket that sends to a port
// without a socket gets ECONNREFUSED one time, from the next read or the
// next write. A write that takes it sends nothing. An unconnected socket
// gets no error. A datagram to an IP of another node is dropped.
func TestUDPFact3Refused(t *testing.T) {
	if !udpPendingOnRead || !udpPendingOnWrite || udpPendingWriteSends || udpRefuseUnconnected {
		t.Fatal("the pending error differs from section 8.3.3, rule 7")
	}
	udpTabNode(t)
	ip, dead := nextLoopbackIP(), nextLoopbackIP()
	ctx := podCtx(t, ip)

	// The next read takes it.
	c, err := dialClusterUDP(ctx, dead+":9")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n, err := c.Write([]byte("a")); err != nil || n != 1 {
		t.Fatalf("first write: %d %v", n, err)
	}
	if _, err := c.Read(make([]byte, 8)); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("read: %v, want ECONNREFUSED", err)
	}
	udpNothing(t, c) // one time only

	// A read that waits wakes up with it.
	got := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 8))
		got <- err
	}()
	time.Sleep(10 * time.Millisecond)
	c.Write([]byte("b"))
	select {
	case err := <-got:
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("waiting read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting read did not wake up")
	}

	// The next write takes it and sends nothing.
	srv := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	c.Write([]byte("c"))
	if n, err := c.WriteTo([]byte("lost"), srv.LocalAddr()); !errors.Is(err, syscall.ECONNREFUSED) || n != 0 {
		t.Fatalf("write: %d %v, want ECONNREFUSED", n, err)
	}
	udpNothing(t, srv)
	if _, err := c.WriteTo([]byte("sent"), srv.LocalAddr()); err != nil {
		t.Fatalf("write after the error: %v", err)
	}
	if b, _ := udpRecv(t, srv); string(b) != "sent" {
		t.Errorf("got %q", b)
	}
	udpNothing(t, c)

	// Not connected: no error.
	u := udpListen(t, ctx, ":0")
	if _, err := u.WriteTo([]byte("x"), udpTo(dead+":9")); err != nil {
		t.Fatal(err)
	}
	udpNothing(t, u)
	if _, err := u.WriteTo([]byte("x"), udpTo(dead+":9")); err != nil {
		t.Errorf("second write without connect: %v", err)
	}

	// Another node: a drop without an error, also for a connected socket.
	o, err := dialClusterUDP(ctx, "10.245.0.9:9")
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if _, err := o.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	udpNothing(t, o)
	if _, err := o.Write([]byte("x")); err != nil {
		t.Errorf("second write to another node: %v", err)
	}
	// The node IP is of this tab node: refused.
	n, err := dialClusterUDP(ctx, tabNodeIP+":9")
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.Write([]byte("x"))
	if _, err := n.Read(make([]byte, 8)); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("node IP: %v", err)
	}
}

// udpHook sets udpRoute for one test.
func udpHook(t *testing.T, f func(ctx context.Context, src, dst netip.AddrPort) (netip.AddrPort, netip.AddrPort, udpVerdict)) {
	saved := udpRoute
	udpRoute = f
	t.Cleanup(func() { udpRoute = saved })
}

// UD11: udpRefuse of udpRoute refuses as UD8 does; udpDrop drops without an
// error.
func TestUDPRouteRefuse(t *testing.T) {
	cluster := netip.MustParseAddrPort("10.96.9.9:53")
	drop := netip.MustParseAddrPort("10.96.9.10:53")
	udpHook(t, func(ctx context.Context, src, dst netip.AddrPort) (netip.AddrPort, netip.AddrPort, udpVerdict) {
		switch dst {
		case cluster:
			return src, dst, udpRefuse
		case drop:
			return src, dst, udpDrop
		}
		return src, dst, udpDeliver
	})
	ctx := podCtx(t, nextLoopbackIP())
	c, err := dialClusterUDP(ctx, cluster.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("q"))
	if _, err := c.Write([]byte("q")); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("write after udpRefuse: %v", err)
	}
	d, err := dialClusterUDP(ctx, drop.String())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Write([]byte("q"))
	udpNothing(t, d)
	// Not connected to the destination: no error.
	u := udpListen(t, ctx, ":0")
	u.WriteTo([]byte("q"), net.UDPAddrFromAddrPort(cluster))
	udpNothing(t, u)
}

// udpRoute changes to and from, as a Service does: the endpoint gets the
// datagram from the client, the client gets the answer from the ClusterIP.
func TestUDPRouteService(t *testing.T) {
	eip, cip := nextLoopbackIP(), nextLoopbackIP()
	ep := udpListen(t, podCtx(t, eip), ":5353")
	svc := netip.MustParseAddrPort("10.96.9.11:53")
	udpHook(t, func(ctx context.Context, src, dst netip.AddrPort) (netip.AddrPort, netip.AddrPort, udpVerdict) {
		switch {
		case dst == svc:
			return src, ep.local, udpDeliver
		case src == ep.local:
			return svc, dst, udpDeliver
		}
		return src, dst, udpDeliver
	})
	c, err := dialClusterUDP(podCtx(t, cip), svc.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.RemoteAddr().String() != svc.String() {
		t.Errorf("remote address %v", c.RemoteAddr())
	}
	c.Write([]byte("query"))
	b, from := udpRecv(t, ep)
	if string(b) != "query" || from.String() != c.LocalAddr().String() {
		t.Fatalf("endpoint got %q from %v", b, from)
	}
	ep.WriteTo([]byte("answer"), from)
	// The connected client takes the answer: its source is the ClusterIP.
	if b, from := udpRecv(t, c); string(b) != "answer" || from.String() != svc.String() {
		t.Errorf("client got %q from %v", b, from)
	}
}

// UD14 and fact 2 of W4.5a: 65,507 bytes arrive; 65,508 bytes give
// errDatagramTooLarge at the send.
func TestUDPFact2DatagramSize(t *testing.T) {
	if udpMaxDatagram != 65507 {
		t.Fatalf("udpMaxDatagram %d", udpMaxDatagram)
	}
	srv := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	cl := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	big := bytes.Repeat([]byte{7}, udpMaxDatagram)
	if n, err := cl.WriteTo(big, srv.LocalAddr()); err != nil || n != len(big) {
		t.Fatalf("65,507 bytes: %d %v", n, err)
	}
	if b, _ := udpRecv(t, srv); !bytes.Equal(b, big) {
		t.Errorf("got %d bytes", len(b))
	}
	_, err := cl.WriteTo(append(big, 7), srv.LocalAddr())
	if !errors.Is(err, errDatagramTooLarge) || !errors.Is(err, syscall.EMSGSIZE) {
		t.Errorf("65,508 bytes: %v", err)
	}
	udpNothing(t, srv)
}

// UD15: the boundaries and the source of 100 datagrams of different sizes.
func TestUDPBoundaries(t *testing.T) {
	srv := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	a := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	b := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	for i := range 100 {
		from := a
		if i%3 == 0 {
			from = b
		}
		if _, err := from.WriteTo(bytes.Repeat([]byte{byte(i)}, i*37), srv.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 100 {
		want := a
		if i%3 == 0 {
			want = b
		}
		got, from := udpRecv(t, srv)
		if !bytes.Equal(got, bytes.Repeat([]byte{byte(i)}, i*37)) || from.String() != want.LocalAddr().String() {
			t.Fatalf("datagram %d: %d bytes from %v", i, len(got), from)
		}
	}
	// A short buffer truncates one datagram; the next one is whole.
	a.WriteTo([]byte("abcdef"), srv.LocalAddr())
	a.WriteTo([]byte("gh"), srv.LocalAddr())
	buf := make([]byte, 3)
	if n, _, _ := srv.ReadFrom(buf); string(buf[:n]) != "abc" {
		t.Errorf("short read %q", buf[:n])
	}
	if got, _ := udpRecv(t, srv); string(got) != "gh" {
		t.Errorf("next datagram %q", got)
	}
}

// UD20, UD21: a socket on 127.0.0.1 receives from its own pod; a datagram
// from another pod is dropped, and a connected sender gets ECONNREFUSED.
// The row goes away at Close.
func TestUDPLoopback(t *testing.T) {
	udpTabNode(t)
	ip := nextLoopbackIP()
	ctx := podCtx(t, ip)
	lo := udpListen(t, ctx, "127.0.0.1:7320")
	if lo.LocalAddr().String() != ip+":7320" {
		t.Errorf("local address %v", lo.LocalAddr())
	}
	dst := tcpAddr(ip + ":7320")
	if !loopbackRefused("udp", nil, dst) || loopbackRefused("tcp", nil, dst) {
		t.Error("no udp row in the loopback table")
	}
	// The same pod: to 127.0.0.1 and to the pod IP.
	own := udpListen(t, ctx, ":0")
	for _, to := range []string{"127.0.0.1:7320", "127.0.0.5:7320", ip + ":7320"} {
		if _, err := own.WriteTo([]byte("own"), udpTo(to)); err != nil {
			t.Fatal(err)
		}
		if b, from := udpRecv(t, lo); string(b) != "own" || from.String() != own.LocalAddr().String() {
			t.Errorf("to %s: %q from %v", to, b, from)
		}
	}
	// Another pod: dropped, and the connected sender gets ECONNREFUSED.
	c, err := dialClusterUDP(podCtx(t, nextLoopbackIP()), ip+":7320")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("other"))
	udpNothing(t, lo)
	if _, err := c.Read(make([]byte, 8)); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("connected sender: %v", err)
	}
	// The terminal (the node IP) is refused too.
	term := udpListen(t, context.Background(), ":0")
	term.WriteTo([]byte("node"), udpTo(ip+":7320"))
	udpNothing(t, lo)
	lo.Close()
	if loopbackRefused("udp", nil, dst) || portReserved("udp", ip, 7320) {
		t.Error("the row or the port stays after Close")
	}
}

// A connected socket receives only from its remote address. Connect with
// the zero value removes the filter.
func TestUDPConnectFilter(t *testing.T) {
	ctx := podCtx(t, nextLoopbackIP())
	a := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	b := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	c := udpListen(t, ctx, ":0")
	if c.RemoteAddr() != nil {
		t.Errorf("remote address %v before Connect", c.RemoteAddr())
	}
	if err := c.Connect(a.local); err != nil {
		t.Fatal(err)
	}
	b.WriteTo([]byte("b"), c.LocalAddr())
	a.WriteTo([]byte("a"), c.LocalAddr())
	if got, from := udpRecv(t, c); string(got) != "a" || from.String() != a.LocalAddr().String() {
		t.Errorf("got %q from %v", got, from)
	}
	if err := c.Connect(netip.AddrPort{}); err != nil {
		t.Fatal(err)
	}
	if c.RemoteAddr() != nil {
		t.Errorf("remote address %v after the zero Connect", c.RemoteAddr())
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, syscall.EDESTADDRREQ) {
		t.Errorf("Write without a remote address: %v", err)
	}
	b.WriteTo([]byte("b"), c.LocalAddr())
	if got, _ := udpRecv(t, c); string(got) != "b" {
		t.Errorf("got %q", got)
	}
	// 0.0.0.0:0 also removes it (rule 4).
	c.Connect(a.local)
	c.Connect(netip.MustParseAddrPort("0.0.0.0:0"))
	if c.RemoteAddr() != nil {
		t.Errorf("remote address %v after 0.0.0.0:0", c.RemoteAddr())
	}
	// 127.0.0.0/8 is the IP of the socket.
	c.Connect(netip.MustParseAddrPort("127.0.0.1:" + strconv.Itoa(udpPort(c))))
	if c.RemoteAddr().String() != c.LocalAddr().String() {
		t.Errorf("remote address %v", c.RemoteAddr())
	}
	c.Write([]byte("self"))
	if got, _ := udpRecv(t, c); string(got) != "self" {
		t.Errorf("got %q", got)
	}
}

// The end of the context closes the socket and frees the port; a waiting
// read ends.
func TestUDPContextEnd(t *testing.T) {
	ip := nextLoopbackIP()
	ctx, cancel := context.WithCancel(withProc(context.Background(), &procEnv{ip: ip}))
	c, err := listenPodUDP(ctx, "127.0.0.1:7330")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		_, _, err := c.ReadFrom(make([]byte, 8))
		got <- err
	}()
	cancel()
	select {
	case err := <-got:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not end")
	}
	for i := 0; portReserved("udp", ip, 7330) || loopbackRefused("udp", nil, tcpAddr(ip+":7330")); i++ {
		if i == 500 {
			t.Fatal("the port or the row stays after the end of the context")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := c.WriteTo([]byte("x"), udpTo(ip+":9")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("write after the end: %v", err)
	}
	if c2, err := listenPodUDP(podCtx(t, ip), ":7330"); err != nil {
		t.Errorf("bind after the end: %v", err)
	} else {
		c2.Close()
	}
}

// The terminal and a hostNetwork pod: the socket has the node IP, and a
// loopback host gets no row.
func TestUDPTerminal(t *testing.T) {
	c := udpListen(t, context.Background(), "127.0.0.1:0")
	if ip := c.LocalAddr().(*net.UDPAddr).IP.String(); ip != tabNodeIP {
		t.Errorf("terminal IP %s, want %s", ip, tabNodeIP)
	}
	if loopbackRefused("udp", nil, net.TCPAddrFromAddrPort(c.local)) {
		t.Error("the terminal got a loopback row")
	}
	h := udpListen(t, podCtx(t, tabNodeIP), "localhost:0")
	if ip := h.LocalAddr().(*net.UDPAddr).IP.String(); ip != tabNodeIP {
		t.Errorf("hostNetwork IP %s", ip)
	}
	// The terminal reaches a pod socket and gets the answer.
	srv := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	d, err := dialClusterUDP(context.Background(), srv.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	d.Write([]byte("ping"))
	_, from := udpRecv(t, srv)
	if from.(*net.UDPAddr).IP.String() != tabNodeIP {
		t.Errorf("source %v", from)
	}
	srv.WriteTo([]byte("pong"), from)
	if b, _ := udpRecv(t, d); string(b) != "pong" {
		t.Errorf("got %q", b)
	}
}

// AC1: 10 MB of datagrams from a pod increase the CPU of its container.
func TestUDPChargesCPU(t *testing.T) {
	c := &ctr{id: "udp-cpu-test", wasm: true}
	t.Cleanup(func() { forgetAcct(c) })
	ctx, cancel := context.WithCancel(withProc(context.Background(), &procEnv{ip: nextLoopbackIP(), c: c}))
	defer cancel()
	srv := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	cl := udpListen(t, ctx, ":0")
	before := acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds
	buf := make([]byte, 60000)
	for range (10 << 20) / len(buf) {
		if _, err := cl.WriteTo(make([]byte, len(buf)), srv.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		srv.ReadFrom(buf)
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
}

// Deadlines: a read deadline ends a waiting read; a write deadline in the
// past fails a write.
func TestUDPDeadlines(t *testing.T) {
	c := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	start := time.Now()
	if _, _, err := c.ReadFrom(make([]byte, 8)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("the read deadline was late")
	}
	c.SetDeadline(time.Now().Add(-time.Second))
	if _, err := c.WriteTo([]byte("x"), c.LocalAddr()); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("write: %v", err)
	}
	// A later deadline wakes a waiting read: it waits again.
	c.SetDeadline(time.Time{})
	got := make(chan error, 1)
	go func() {
		_, _, err := c.ReadFrom(make([]byte, 8))
		got <- err
	}()
	time.Sleep(10 * time.Millisecond)
	c.SetReadDeadline(time.Now().Add(time.Hour))
	c.SetReadDeadline(time.Now())
	select {
	case err := <-got:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Errorf("read: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SetReadDeadline did not wake the read")
	}
}
