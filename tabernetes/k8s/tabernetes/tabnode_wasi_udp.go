package main

// The UDP messages of command components (docs/wasi-sockets.md, section
// 8.3.3). "Rule N" in this file is rule N of section 8.3.3; "TCP rule N" is
// rule N of section 8.3.2. The pod worker (web/src/sockets.js) sends "bind"
// and "connect" with the protocol "udp", "datagram", "credit" with the kind
// "datagram-bytes" and "closed". The Go side answers with "bound",
// "datagram", "credit" and "error". The sockets are sockets of the UDP table
// (tabnode_udp.go, section 8.7).
//
// Structure (as tabnode_wasi_tcp.go):
//
//   - wasiUDPSockets, in p.State(wasiUDPState), has the UDP sockets of one pod
//     worker by socket ID. One socket ID has one protocol: the routers
//     byProtocol and byUDPID give the messages that TCP and UDP share to
//     the correct handler (rule 1).
//   - The handlers run on the event loop and do not wait. Each socket has
//     two goroutines: the read pump calls ReadFrom and sends "datagram", the
//     write pump calls WriteTo for each "datagram" of the pod worker and
//     sends "credit".
//   - The pending error (rule 7). On runwasi only the next send of the
//     component gets connection-refused, and the UDP table gives the
//     pending error only to WriteTo (udpPendingOnRead). The pod worker must
//     know it before the next send of the component, so the write pump
//     takes it at once after the WriteTo that set it, and sends one
//     "error". The pod worker keeps it and gives it to the next send.
//   - Each socket has a context from the context of the container
//     (wasiPeerCtx). Its end closes the udpConn. "closed" (rule 8), MS6 and
//     the stop of the container use this path.
//   - Phase 5: datagrams from other nodes arrive through the same udpConn,
//     so the read pump does not change.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// wasiUDPState is the key of the UDP sockets of a peer.
const wasiUDPState = "udp"

// udpCredit is the credit of datagram bytes of each direction after "bound"
// (rule 6). It is also the size of the queue of a socket in the pod worker
// (UD5).
const udpCredit = 256 << 10

// The ephemeral ports of an IPv6 UDP socket. An IPv6 socket gets no socket
// in the UDP table (rule 2), so it gets its port from this counter.
var udpV6Port atomic.Uint32

// wasiUDPSockets are the UDP sockets of one pod worker.
type wasiUDPSockets struct {
	p     *wasiPeer
	mu    sync.Mutex
	socks map[uint32]*udpSock
}

func udpOf(p *wasiPeer) *wasiUDPSockets {
	return p.State(wasiUDPState, func() any {
		return &wasiUDPSockets{p: p, socks: map[uint32]*udpSock{}}
	}).(*wasiUDPSockets)
}

func (s *wasiUDPSockets) get(id uint32) *udpSock {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.socks[id]
}

func (s *wasiUDPSockets) put(k *udpSock) {
	s.mu.Lock()
	s.socks[k.id] = k
	s.mu.Unlock()
}

func (s *wasiUDPSockets) take(id uint32) *udpSock {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.socks[id]
	delete(s.socks, id)
	return k
}

// udpOut is a datagram of the pod worker that the write pump did not send
// yet.
type udpOut struct {
	to   *net.UDPAddr
	data []byte
}

// udpSock is one bound UDP socket of a container.
type udpSock struct {
	id uint32
	p  *wasiPeer

	// ctx ends when the socket ends. Its end closes conn.
	ctx    context.Context
	cancel context.CancelFunc

	v6   bool     // the family ipv6: no socket in the UDP table (rule 2)
	conn *udpConn // nil for v6

	mu sync.Mutex
	// Credits of datagram bytes (rule 6). out: the bytes that the Go side
	// can send. in: the bytes that the pod worker can send.
	out, in int
	queue   []udpOut // datagrams of the pod worker that are not sent yet
	closing bool     // "closed": send the queue, then close (rule 8)

	writeWake chan struct{}
}

func newUDPSock(p *wasiPeer, id uint32, v6 bool) *udpSock {
	ctx, cancel := context.WithCancel(wasiPeerCtx(p))
	return &udpSock{
		id: id, p: p, ctx: ctx, cancel: cancel, v6: v6,
		out: udpCredit, in: udpCredit, writeWake: make(chan struct{}, 1),
	}
}

// send sends a message of the socket. After "closed" the ID can belong to a
// new socket, so a socket that closes sends nothing. A send can still fail
// when the message crosses "closed" (TCP rule 9): it is discarded.
func (k *udpSock) send(typ string, fields map[string]any) {
	k.mu.Lock()
	closing := k.closing
	k.mu.Unlock()
	if closing || k.ctx.Err() != nil {
		return
	}
	fields["socket"] = k.id
	k.p.send(typ, fields)
}

// udpCharge charges the CPU time d of a pump to the container (AC1), off the
// event loop and with the time of other handlers (tcpSockets.charge).
func udpCharge(p *wasiPeer, d time.Duration) { tcpOf(p).charge(d) }

func init() {
	wasiHandle("datagram", timed(wasiUDPDatagram))
	// MS6 and the stop of the container (TCP rule 10, TC14): close at once.
	wasiOnClose("socket", func(p *wasiPeer, id uint32) {
		if k := udpOf(p).take(id); k != nil {
			k.cancel()
		}
	})
}

// byProtocol routes "bind" and "connect" by the field protocol (rule 1). One
// socket ID has one protocol: a message with the other protocol for a
// socket gets "bound" with invalid-state (as a second bind), or MS6 for
// "connect".
func byProtocol(tcp, udp wasiHandler) wasiHandler {
	return func(p *wasiPeer, m wasiMsg) {
		id := m.Uint("socket")
		isUDP := m.Str("protocol") == "udp"
		var other bool
		if isUDP {
			other = tcpOf(p).get(id) != nil
		} else {
			other = udpOf(p).get(id) != nil
		}
		switch {
		case other && m.Type == "bind":
			p.send("bound", map[string]any{"socket": id, "error": "invalid-state"})
		case other:
			p.Fail("socket", id, m.Type+": the socket has the other protocol")
		case isUDP:
			udp(p, m)
		default:
			tcp(p, m)
		}
	}
}

// byUDPID routes "credit" and "closed": an ID of a UDP socket goes to udp,
// other IDs go to tcp.
func byUDPID(tcp, udp wasiHandler) wasiHandler {
	return func(p *wasiPeer, m wasiMsg) {
		if udpOf(p).get(m.Uint("socket")) != nil {
			udp(p, m)
			return
		}
		tcp(p, m)
	}
}

// wasiUDPBind answers "bind" with the protocol udp (rule 2; UD2, UD3, UD20).
func wasiUDPBind(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	set := udpOf(p)
	bound := func(fields map[string]any) { fields["socket"] = id; p.send("bound", fields) }
	if set.get(id) != nil {
		bound(map[string]any{"error": "invalid-state"})
		return
	}
	podIP := podIPOf(p)
	host := m.Str("address")
	v6, code := tcpBindAddr(m.Str("family"), host, podIP)
	if code != "" {
		// MS3: an answer, not a drop. The socket stays.
		bound(map[string]any{"error": code})
		return
	}
	port := m.Int("port")
	k := newUDPSock(p, id, v6)
	if v6 {
		// Rule 2: a port, but no socket in the UDP table.
		if port == 0 {
			port = 32768 + int(udpV6Port.Add(1)%(60999-32768+1))
		}
		set.put(k)
		bound(map[string]any{"address": host, "port": port})
		go k.writePump()
		return
	}
	conn, err := listenPodUDP(k.ctx, net.JoinHostPort(tcpListenHost(host, podIP), strconv.Itoa(port)))
	if err != nil {
		k.cancel()
		// errPortInUse gives address-in-use, EADDRNOTAVAIL gives
		// address-not-bindable. Port 0 without a free port gives
		// EADDRINUSE on Linux too.
		bound(map[string]any{"error": tcpErrorCode(err, "address-in-use")})
		return
	}
	k.conn = conn
	set.put(k)
	// Rule 3: the address of the UDP table, the pod IP.
	local := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	bound(map[string]any{"address": local.Addr().String(), "port": int(local.Port())})
	go k.readPump()
	go k.writePump()
}

// udpAddrOf gives the address of "connect" or "datagram" for the socket k.
// An address that a correct pod worker does not send gives ok false (rule
// 4): the wrong family, an IPv4-mapped address, and the unspecified address
// or the port 0. disconnect is true for the unspecified address of the
// family with the port 0, which only "connect" may send.
func (k *udpSock) udpAddrOf(m wasiMsg) (ap netip.AddrPort, disconnect, ok bool) {
	a, err := netip.ParseAddr(m.Str("address"))
	if err != nil || a.Is4In6() || a.Is6() != k.v6 {
		return ap, false, false
	}
	port := m.Int("port")
	if a.IsUnspecified() || port == 0 {
		return ap, a.IsUnspecified() && port == 0, false
	}
	return netip.AddrPortFrom(a, uint16(port)), false, true
}

// wasiUDPConnect does "connect" with the protocol udp (rule 4). It sends no
// answer.
func wasiUDPConnect(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	k := udpOf(p).get(id)
	if k == nil {
		// Rule 1: connect before bind.
		p.Fail("socket", id, "connect: the UDP socket is not bound")
		return
	}
	ap, disconnect, ok := k.udpAddrOf(m)
	if !ok && !disconnect {
		p.Fail("socket", id, "connect: the address "+m.Str("address")+" port "+strconv.Itoa(m.Int("port")))
		return
	}
	if k.conn == nil {
		return // IPv6: no socket in the UDP table
	}
	// The zero value removes the remote address.
	k.conn.Connect(ap)
}

// wasiUDPDatagram puts a "datagram" of the pod worker into the queue of the
// socket (rules 5 and 6; UD14, MS4).
func wasiUDPDatagram(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	k := udpOf(p).get(id)
	if k == nil {
		// Rule 1: a TCP socket, or a UDP socket that is not bound.
		p.Fail("socket", id, "datagram: no bound UDP socket")
		return
	}
	ap, _, ok := k.udpAddrOf(m)
	if !ok {
		p.Fail("socket", id, "datagram: the address "+m.Str("address")+" port "+strconv.Itoa(m.Int("port")))
		return
	}
	b := m.Bytes("bytes")
	k.mu.Lock()
	if len(b) > k.in {
		in := k.in
		k.mu.Unlock()
		// MS4 (rule 6).
		p.Fail("socket", id, "datagram: "+strconv.Itoa(len(b))+" bytes, credit "+strconv.Itoa(in))
		return
	}
	k.in -= len(b)
	k.queue = append(k.queue, udpOut{to: net.UDPAddrFromAddrPort(ap), data: b})
	k.mu.Unlock()
	wake(k.writeWake)
}

// wasiUDPCredit adds a credit of the kind datagram-bytes (rule 6). Other
// kinds do not apply to UDP and are ignored.
func wasiUDPCredit(p *wasiPeer, m wasiMsg) {
	k := udpOf(p).get(m.Uint("socket"))
	if k == nil || m.Str("kind") != "datagram-bytes" {
		return
	}
	k.mu.Lock()
	k.out += m.Int("n")
	k.mu.Unlock()
}

// wasiUDPClosed ends a socket ID (rule 8): the write pump first sends the
// datagrams of the queue, then closes the socket.
func wasiUDPClosed(p *wasiPeer, m wasiMsg) {
	k := udpOf(p).take(m.Uint("socket"))
	if k == nil {
		return
	}
	k.mu.Lock()
	k.closing = true
	k.mu.Unlock()
	wake(k.writeWake)
}

// readPump reads datagrams from the UDP table until the socket ends (rules
// 3, 6 and 7; UD5, UD6, UD9, UD15). It does not wait for credit: a datagram
// larger than the credit of the Go side is dropped (UD6), because the queue
// of the socket is in the pod worker.
func (k *udpSock) readPump() {
	buf := make([]byte, udpMaxDatagram)
	for {
		n, from, err := k.conn.ReadFrom(buf)
		start := time.Now()
		switch {
		case errors.Is(err, syscall.ECONNREFUSED):
			// Rule 7: this call took the pending error, so no other call
			// gives it. One "error" for each pending error. Only when the
			// table gives it to ReadFrom (udpPendingOnRead).
			k.send("error", map[string]any{"error": "connection-refused"})
		case err != nil:
			return // the socket closed
		default:
			k.deliver(buf[:n], from)
		}
		udpCharge(k.p, time.Since(start))
	}
}

// deliver sends one datagram from the UDP table to the pod worker, or drops
// it without credit (UD6).
func (k *udpSock) deliver(b []byte, from net.Addr) {
	src, ok := from.(*net.UDPAddr)
	if !ok {
		return
	}
	k.mu.Lock()
	if len(b) > k.out {
		k.mu.Unlock()
		return
	}
	k.out -= len(b)
	k.mu.Unlock()
	ap := src.AddrPort()
	k.send("datagram", map[string]any{
		"address": ap.Addr().Unmap().String(), "port": int(ap.Port()), "bytes": append([]byte(nil), b...),
	})
}

// writePump sends the datagrams of the pod worker in their sequence and
// gives their credit back (rule 6). After "closed" it sends the queue and
// then ends the socket (rule 8).
func (k *udpSock) writePump() {
	for {
		k.mu.Lock()
		var d udpOut
		have := len(k.queue) > 0
		if have {
			d = k.queue[0]
			k.queue[0] = udpOut{}
			k.queue = k.queue[1:]
		}
		closing := k.closing
		k.mu.Unlock()
		if !have {
			if closing {
				k.cancel()
				return
			}
			select {
			case <-k.writeWake:
			case <-k.ctx.Done():
				return
			}
			continue
		}
		var err error
		refused := false
		if k.conn != nil {
			// The UDP table charges the time of WriteTo (AC1). WriteTo
			// does not wait.
			_, err = k.conn.WriteTo(d.data, d.to)
			// A refusal happens in WriteTo: take the pending error now.
			refused = k.conn.takePending()
		}
		// IPv6 (no socket in the table): the datagram is dropped.
		start := time.Now()
		if k.ctx.Err() != nil {
			return
		}
		if errors.Is(err, syscall.ECONNREFUSED) || refused {
			// Rule 7: one "error" for each pending error. A WriteTo that
			// took a pending error (set outside this pump) did not send its
			// datagram; it cannot also set a new one.
			k.send("error", map[string]any{"error": "connection-refused"})
		}
		// The credit goes back also for a datagram that the table dropped
		// or did not send.
		k.mu.Lock()
		k.in += len(d.data)
		k.mu.Unlock()
		if len(d.data) > 0 {
			k.send("credit", map[string]any{"kind": "datagram-bytes", "n": len(d.data)})
		}
		udpCharge(k.p, time.Since(start))
	}
}

// takePending takes the pending error of the socket, if it has one, so that
// no later call gets it (rule 7). It reports whether the socket had one.
func (c *udpConn) takePending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	had := c.pending != nil
	c.pending = nil
	return had
}
