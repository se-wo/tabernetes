package main

import (
	"context"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// The UDP table of this tab node (docs/wasi-sockets.md, decision 9, section
// 8.7). The in-process network of Go for js/wasm has no usable UDP (fact
// 11), so the tab node keeps its UDP sockets in this table. Components
// (tabnode_wasi_udp.go), CoreDNS and the programs of the tab node (nc -u,
// nslookup) use it. Each Go thread is its own tab node, so the table is a
// global of the package, as the loopback table is.
//
// The ports of the table are in the table of the ports in use
// (tabnode_dial.go) with the protocol "udp": TCP and UDP can use the same
// port of an IP (UD1). A UDP bind is exclusive (section 8.3.3, rule 2).

// udpMaxDatagram is the largest datagram, the IPv4 limit (UD14).
const udpMaxDatagram = 65507

// udpQueueSize is the receive queue of a socket of the UDP table (UD5).
const udpQueueSize = 256 << 10

// errDatagramTooLarge is the error of a send with more than udpMaxDatagram
// bytes (UD14, wasi: datagram-too-large). errors.Is(err, syscall.EMSGSIZE)
// is true.
var errDatagramTooLarge error = os.NewSyscallError("sendto", syscall.EMSGSIZE)

// The points that the facts of runwasi (package W4.5a) can change. Each one
// has the expected value of section 8.3.3 and a test that names the fact
// (tabnode_udp_test.go).
const (
	// udpBindExclusive: a second UDP bind to the same IP and port gives
	// address-in-use (fact 1, rule 2). wasmtime sets SO_REUSEADDR only for
	// TCP. With false, two sockets could reserve the port as TCP binds do,
	// but the table keeps only one receiver for each IP and port.
	udpBindExclusive = true

	// udpPendingOnRead and udpPendingOnWrite: the calls that can take the
	// pending error of a refused datagram (fact 3, rule 7). On runwasi,
	// only the next send gets connection-refused; receive never gets it.
	udpPendingOnRead  = false
	udpPendingOnWrite = true

	// udpPendingWriteSends: a send that takes the pending error also sends
	// its datagram (fact 3). Linux does not send it.
	udpPendingWriteSends = false

	// udpRefuseUnconnected: a socket that is not connected also gets the
	// pending error (fact 3). Linux gives ICMP errors only to a connected
	// socket.
	udpRefuseUnconnected = false
)

// udpVerdict is the result of udpRoute for one datagram.
type udpVerdict int

const (
	udpDeliver udpVerdict = iota // deliver to the destination of udpRoute
	udpDrop                      // drop; the sender gets no error (UD12, NetworkPolicy)
	udpRefuse                    // drop; a connected sender gets connection-refused (UD10, UD11)
)

// udpRoute is the Service translation and the flow table of section 8.8 for
// one datagram from src to dst. ctx is the context of the socket that sends.
// W4.2a sets it in an init function of its own file. The default delivers
// to dst with no change.
//
// src is the IP of the table and the port of the sending socket. dst is the
// destination of the send after step 2 of WriteTo: 127.0.0.0/8 is already
// the IP of the sending socket. from is the source that the receiver sees,
// to is the socket that receives. udpRoute must not wait: WriteTo calls it
// for each datagram.
var udpRoute = func(ctx context.Context, src, dst netip.AddrPort) (from, to netip.AddrPort, v udpVerdict) {
	return src, dst, udpDeliver
}

var (
	udpMu sync.Mutex
	// udpSockets are the bound sockets of this tab node, by the IP of the
	// table and the port.
	udpSockets = map[netip.AddrPort]*udpConn{}
)

// udpDatagram is one datagram in the queue of a socket.
type udpDatagram struct {
	from netip.AddrPort
	data []byte
}

// udpConn is a socket of the UDP table (section 8.7). It is a
// net.PacketConn. After Connect, or from dialClusterUDP, Read and Write
// also work.
type udpConn struct {
	ctx      context.Context
	local    netip.AddrPort // the IP of the table and the port
	releases []func()       // the port and the loopback row
	cpu      atomic.Int64   // CPU time that a goroutine did not charge yet (AC1)

	mu      sync.Mutex
	queue   []udpDatagram
	qbytes  int            // bytes of payload in queue (UD5)
	remote  netip.AddrPort // the zero value: not connected
	pending error          // the pending error of a refused datagram, or nil
	closed  bool
	rdl     time.Time     // the read deadline
	wdl     time.Time     // the write deadline
	wake    chan struct{} // closed and replaced at each change for ReadFrom
}

// listenPodUDP binds a UDP socket for the pod of ctx. The hosts "",
// 0.0.0.0, :: and the pod IP give the pod IP. A loopback host (127.0.0.0/8,
// localhost, ::1) gives the pod IP and a row "udp" in the loopback table
// (UD20). Another IP gives an error that wraps syscall.EADDRNOTAVAIL. Port 0
// gives the first free UDP port of the IP from 32768 (UD3, UD4). An address
// in use gives an error that wraps errPortInUse (rule 2). Outside a pod
// (the terminal) and in a hostNetwork pod, the IP is the node IP. The socket
// closes at the end of ctx.
func listenPodUDP(ctx context.Context, addr string) (*udpConn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "udp", Err: err}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "udp", Err: &net.AddrError{Err: "invalid port", Addr: addr}}
	}
	ip, loopback, err := udpBindIP(ctx, host)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "udp", Err: err}
	}
	return udpBind(ctx, ip, int(port), loopback)
}

// dialClusterUDP binds an ephemeral port as listenPodUDP does and connects
// the socket to addr. The host of addr resolves as clusterHost does.
func dialClusterUDP(ctx context.Context, addr string) (*udpConn, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "udp", Err: err}
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "udp", Err: &net.AddrError{Err: "invalid port", Addr: addr}}
	}
	ip, err := netip.ParseAddr(clusterHost(ctx, host))
	if err != nil {
		return nil, &net.OpError{Op: "dial", Net: "udp", Err: &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}}
	}
	c, err := listenPodUDP(ctx, ":0")
	if err != nil {
		return nil, err
	}
	if err := c.Connect(netip.AddrPortFrom(ip, uint16(port))); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// udpTableIP is the IP of the sockets of ctx: the pod IP, or the node IP
// outside a pod (the terminal). A hostNetwork pod has the node IP.
func udpTableIP(ctx context.Context) netip.Addr {
	if pe := procOf(ctx); pe != nil && pe.ip != "" {
		if a, err := netip.ParseAddr(pe.ip); err == nil {
			return a.Unmap()
		}
	}
	a, _ := netip.ParseAddr(tabNodeIP)
	return a.Unmap()
}

// udpBindIP gives the IP of the table for the host of a bind, and whether
// the socket gets a row in the loopback table.
func udpBindIP(ctx context.Context, host string) (netip.Addr, bool, error) {
	ip := udpTableIP(ctx)
	switch {
	case host == "", host == "0.0.0.0", host == "::":
		return ip, false, nil
	case isLoopbackHost(host):
		// A hostNetwork pod and the terminal have the node IP: the host of
		// the node reaches its 127.0.0.1 on Linux. They get no row, as in
		// listenPod.
		return ip, ip.String() != tabNodeIP, nil
	}
	if a, err := netip.ParseAddr(host); err == nil && a.Unmap() == ip {
		return ip, false, nil
	}
	return netip.Addr{}, false, os.NewSyscallError("bind", syscall.EADDRNOTAVAIL)
}

// udpClaimPort notes the socket on ip and port in the table of the ports in
// use. Port 0 gives the first free port from 32768 (UD3, UD4).
func udpClaimPort(ip string, port int) (int, func(), error) {
	if port == 0 {
		return claimFreePort("udp", ip)
	}
	if !udpBindExclusive {
		return reservePort("udp", ip, port)
	}
	release, err := claimListener("udp", ip, port)
	return port, release, err
}

// udpBind adds a socket on ip and port to the table.
func udpBind(ctx context.Context, ip netip.Addr, port int, loopback bool) (*udpConn, error) {
	got, release, err := udpClaimPort(ip.String(), port)
	if err != nil {
		return nil, &net.OpError{Op: "listen", Net: "udp", Addr: net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(port))), Err: err}
	}
	c := &udpConn{ctx: ctx, local: netip.AddrPortFrom(ip, uint16(got)), releases: []func(){release}, wake: make(chan struct{})}
	if loopback {
		c.releases = append(c.releases, addLoopback("udp", ip.String(), got))
	}
	udpMu.Lock()
	udpSockets[c.local] = c
	udpMu.Unlock()
	context.AfterFunc(ctx, func() { c.Close() })
	return c, nil
}

// udpLocalIP reports whether a datagram to ip and a port without a socket is
// refused (UD8): ip is a pod IP of this tab node or the node IP. WriteTo
// gives the datagrams to other IPs to udpToNode (section 8.9.1, rule 5).
func udpLocalIP(ip netip.Addr) bool {
	s := ip.String()
	return s == tabNodeIP || (theTabNode != nil && theTabNode.isTabPodIP(s))
}

// udpAddrPort gives the address of a net.Addr for WriteTo.
func udpAddrPort(addr net.Addr) (netip.AddrPort, error) {
	switch a := addr.(type) {
	case *net.UDPAddr:
		if a != nil {
			return a.AddrPort(), nil
		}
	case nil:
	default:
		if ap, err := netip.ParseAddrPort(a.String()); err == nil {
			return ap, nil
		}
		return netip.AddrPort{}, &net.AddrError{Err: "invalid address", Addr: a.String()}
	}
	return netip.AddrPort{}, syscall.EDESTADDRREQ
}

// localIP changes a destination in 127.0.0.0/8 to the IP of the socket.
func (c *udpConn) localIP(a netip.AddrPort) netip.AddrPort {
	ip := a.Addr().Unmap()
	if ip.Is4() && ip.IsLoopback() {
		ip = c.local.Addr()
	}
	return netip.AddrPortFrom(ip, a.Port())
}

// After a refused datagram of a connected socket (UD9, UD11, UD21), the
// socket has one pending error. Exactly one call takes it: the next
// WriteTo or Write gives an error that wraps syscall.ECONNREFUSED, and the
// pending error is then gone. It does not send its datagram. ReadFrom and
// Read never take it (fact 3 of runwasi, udpPendingOnRead). Two refused
// datagrams before the next send give one error.

// ReadFrom gives the next datagram and its source (a *net.UDPAddr), after
// udpRoute.
func (c *udpConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, from, err := c.read(b)
	if err != nil {
		return 0, nil, err
	}
	return n, net.UDPAddrFromAddrPort(from), nil
}

func (c *udpConn) read(b []byte) (int, netip.AddrPort, error) {
	for {
		c.mu.Lock()
		switch {
		case c.closed:
			c.mu.Unlock()
			return 0, netip.AddrPort{}, c.opError("read", nil, net.ErrClosed)
		case c.pending != nil && udpPendingOnRead:
			err := c.pending
			c.pending = nil
			c.mu.Unlock()
			return 0, netip.AddrPort{}, c.opError("read", nil, os.NewSyscallError("recvfrom", err))
		case len(c.queue) > 0:
			d := c.queue[0]
			c.queue[0] = udpDatagram{}
			c.queue = c.queue[1:]
			c.qbytes -= len(d.data)
			c.mu.Unlock()
			// A short buffer truncates the datagram, as recvfrom does.
			return copy(b, d.data), d.from, nil
		case !c.rdl.IsZero() && !time.Now().Before(c.rdl):
			c.mu.Unlock()
			return 0, netip.AddrPort{}, c.opError("read", nil, os.ErrDeadlineExceeded)
		}
		wake, dl := c.wake, c.rdl
		c.mu.Unlock()
		var timeout <-chan time.Time
		var t *time.Timer
		if !dl.IsZero() {
			t = time.NewTimer(time.Until(dl))
			timeout = t.C
		}
		select {
		case <-wake:
		case <-timeout:
		case <-c.ctx.Done():
			c.Close()
		}
		if t != nil {
			t.Stop()
		}
	}
}

// WriteTo sends one datagram, or gives the pending error and sends nothing.
// It never waits for the receiver: a full queue drops the datagram without
// an error (UD6). 127.0.0.0/8 is the IP of the socket.
func (c *udpConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	dst, err := udpAddrPort(addr)
	if err != nil {
		return 0, c.opError("write", addr, err)
	}
	return c.write(b, dst)
}

func (c *udpConn) Read(b []byte) (int, error) {
	n, _, err := c.read(b)
	return n, err
}

func (c *udpConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	remote := c.remote
	c.mu.Unlock()
	if !remote.IsValid() {
		return 0, c.opError("write", nil, syscall.EDESTADDRREQ)
	}
	return c.write(b, remote)
}

// write does the steps of section 8.7 for one datagram to dst. The time of
// the steps goes to the container of the socket (AC1).
func (c *udpConn) write(b []byte, dst netip.AddrPort) (int, error) {
	start := time.Now()
	defer func() { c.charge(time.Since(start)) }()
	addr := net.UDPAddrFromAddrPort(dst)
	// Step 1: the size (UD14).
	if len(b) > udpMaxDatagram {
		return 0, c.opError("write", addr, errDatagramTooLarge)
	}
	c.mu.Lock()
	switch {
	case c.closed:
		c.mu.Unlock()
		return 0, c.opError("write", addr, net.ErrClosed)
	case !c.wdl.IsZero() && !time.Now().Before(c.wdl):
		c.mu.Unlock()
		return 0, c.opError("write", addr, os.ErrDeadlineExceeded)
	case c.pending != nil && udpPendingOnWrite:
		err := c.pending
		c.pending = nil
		c.mu.Unlock()
		if udpPendingWriteSends {
			c.send(b, dst)
		}
		return 0, c.opError("write", addr, os.NewSyscallError("sendto", err))
	}
	c.mu.Unlock()
	c.send(b, dst)
	return len(b), nil
}

// send does steps 2 to 6 of section 8.7. It never gives an error to the
// caller: a refused datagram sets the pending error.
func (c *udpConn) send(b []byte, dst netip.AddrPort) {
	// Step 2: 127.0.0.0/8 is the IP of the socket.
	dst = c.localIP(dst)
	src := c.local
	// Step 3: the Service translation and the flow table (section 8.8).
	from, to, v := udpRoute(c.ctx, src, dst)
	switch v {
	case udpDrop:
		return
	case udpRefuse:
		c.refused(dst)
		return
	}
	// Step 4: the receiver.
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	udpMu.Lock()
	r := udpSockets[to]
	udpMu.Unlock()
	switch {
	case r == nil && udpLocalIP(to.Addr()):
		c.refused(dst) // UD8
		return
	case r == nil:
		// Another node (section 8.9.1, rule 5): udpToNode sends a datagram
		// to a pod CIDR or the node IP of another node and drops the
		// others. A nil hook drops. No refusal comes back (rule 7).
		if h := udpToNode; h != nil {
			h(netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), to, b)
		}
		return
	case loopbackRefused("udp", src.Addr().AsSlice(), net.TCPAddrFromAddrPort(to)):
		c.refused(dst) // UD21: the real sender, not from of udpRoute
		return
	}
	// Step 5: the queue of the receiver.
	r.deliver(from, b)
}

// deliver puts a copy of b with the source from into the queue. A connected
// socket takes only datagrams from its remote address. A full queue drops
// the datagram (UD6).
func (c *udpConn) deliver(from netip.AddrPort, b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || (c.remote.IsValid() && from != c.remote) || c.qbytes+len(b) > udpQueueSize {
		return
	}
	c.queue = append(c.queue, udpDatagram{from: from, data: append([]byte(nil), b...)})
	c.qbytes += len(b)
	c.wakeLocked()
}

// refused is step 6 of section 8.7: a connected socket gets the pending
// error if dst (before udpRoute) is its remote address (UD9, UD11). Linux
// gives this error after ICMP "port unreachable".
func (c *udpConn) refused(dst netip.AddrPort) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	if udpRefuseUnconnected || (c.remote.IsValid() && dst == c.remote) {
		c.pending = syscall.ECONNREFUSED
		if udpPendingOnRead {
			c.wakeLocked()
		}
	}
}

// Connect sets the remote address (wasi: stream with an address). The zero
// netip.AddrPort removes it. A connected socket receives only from its
// remote address. 127.0.0.0/8 is the IP of the socket, as in WriteTo.
//
// The unspecified address with port 0 also removes it: the pod worker sends
// it for stream without an address (section 8.3.3, rule 4).
func (c *udpConn) Connect(raddr netip.AddrPort) error {
	if raddr.IsValid() && !(raddr.Addr().IsUnspecified() && raddr.Port() == 0) {
		raddr = c.localIP(raddr)
	} else {
		raddr = netip.AddrPort{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.opError("connect", nil, net.ErrClosed)
	}
	c.remote = raddr
	return nil
}

// LocalAddr gives a *net.UDPAddr: the IP of the table and the port.
func (c *udpConn) LocalAddr() net.Addr { return net.UDPAddrFromAddrPort(c.local) }

// RemoteAddr gives nil when the socket is not connected.
func (c *udpConn) RemoteAddr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.remote.IsValid() {
		return nil
	}
	return net.UDPAddrFromAddrPort(c.remote)
}

func (c *udpConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rdl, c.wdl = t, t
	c.wakeLocked()
	return nil
}

func (c *udpConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rdl = t
	c.wakeLocked()
	return nil
}

func (c *udpConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wdl = t
	return nil
}

// Close removes the socket, its port and its loopback row. The datagrams in
// its queue are gone.
func (c *udpConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return c.opError("close", nil, net.ErrClosed)
	}
	c.closed = true
	c.queue, c.qbytes = nil, 0
	c.wakeLocked()
	c.mu.Unlock()
	udpMu.Lock()
	if udpSockets[c.local] == c {
		delete(udpSockets, c.local)
	}
	udpMu.Unlock()
	for _, r := range c.releases {
		r()
	}
	return nil
}

func (c *udpConn) wakeLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}

func (c *udpConn) opError(op string, addr net.Addr, err error) error {
	return &net.OpError{Op: op, Net: "udp", Source: c.LocalAddr(), Addr: addr, Err: err}
}

// charge charges the CPU time d of WriteTo to the container of the socket
// (AC1). acctFrom takes the lock of the pod, so a goroutine does it, as
// tcpSockets.charge does. Only one goroutine runs at a time for the time of
// many datagrams.
func (c *udpConn) charge(d time.Duration) {
	if d <= 0 {
		return
	}
	if pe := procOf(c.ctx); pe == nil || pe.c == nil {
		return
	}
	if c.cpu.Add(int64(d)) != int64(d) {
		return // a goroutine charges it already
	}
	go func() {
		acctFrom(c.ctx).ChargeCPU(time.Duration(c.cpu.Swap(0)))
	}()
}

func init() {
	udpFromNode = udpNodeReceive
}

// udpNodeReceive is udpFromNode: steps 3 to 6 of section 8.7 for a datagram
// from src on another node to dst on this tab node. udpRoute examines the
// ingress of a new flow (ND9) and gives the answers of a flow their
// original source (UD15). A drop, a refusal of udpRoute, a destination
// without a socket (also on another node) and a refusal of the loopback
// table (UD21) lose the datagram without an answer (section 8.9.1, rule 7).
// The datagram never goes back to udpToNode. The time goes to the
// container of the receiving socket (AC1).
func udpNodeReceive(src, dst netip.AddrPort, b []byte) {
	start := time.Now()
	if len(b) > udpMaxDatagram {
		return
	}
	src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
	dst = netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port())
	from, to, v := udpRoute(context.Background(), src, dst)
	if v != udpDeliver {
		return
	}
	to = netip.AddrPortFrom(to.Addr().Unmap(), to.Port())
	udpMu.Lock()
	r := udpSockets[to]
	udpMu.Unlock()
	if r == nil || loopbackRefused("udp", src.Addr().AsSlice(), net.TCPAddrFromAddrPort(to)) {
		return
	}
	r.deliver(from, b)
	r.charge(time.Since(start))
}
