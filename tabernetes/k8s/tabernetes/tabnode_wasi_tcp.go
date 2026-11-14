package main

// The TCP and lookup messages of command components (docs/wasi-sockets.md,
// sections 7.4, 7.7 and 8.3.2). The pod worker (web/src/sockets.js) sends
// "bind", "listen", "connect", "data", "credit", "shutdown", "closed" and
// "lookup". The Go side answers with "bound", "listening", "accepted",
// "connected", "data", "credit", "error" and "addresses". "Rule N" in this
// file is rule N of section 8.3.2.
//
// Structure (a model for the UDP handlers of a later package):
//
//   - tcpSockets, in p.State(wasiTCPState), has the sockets of one pod
//     worker by socket ID. A tcpSock has the reservation of its port, its
//     listener or its connection, and its credits.
//   - The handlers run on the event loop and do not wait. A listener, a dial,
//     the read pump and the write pump of a connection run in goroutines.
//   - Each socket has a context from the context of the container
//     (wasiPeerCtx). Its end closes the listener and the connection of the
//     socket. So the stop of the container (rule 10), wasiOnClose (MS6) and
//     "closed" (rule 9) use the same path.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// wasiTCPState is the key of the sockets of a peer.
const wasiTCPState = "tcp"

// tcpBufSize is the size of one read of the read pump. A "data" message has
// at most wasiLimits["data"] bytes.
const tcpBufSize = 64 << 10

// tcpBindExclusive is true when a second bind to an address that a socket
// has bound (but without a listener) fails with address-in-use (TC18, rule
// 3). Package W3.3a records the result of runwasi. Until then, only a
// listener makes the address in use.
const tcpBindExclusive = false

// The ephemeral ports of an IPv6 socket. An IPv6 socket gets no reservation
// in the in-process network (TC7), so it gets its port from this counter.
var tcpV6Port atomic.Uint32

// tcpSockets are the sockets of one pod worker.
type tcpSockets struct {
	p       *wasiPeer
	mu      sync.Mutex
	socks   map[uint32]*tcpSock
	lookups map[uint32]context.CancelFunc
	cpu     atomic.Int64 // CPU time of handlers that is not charged yet
}

func tcpOf(p *wasiPeer) *tcpSockets {
	return p.State(wasiTCPState, func() any {
		return &tcpSockets{p: p, socks: map[uint32]*tcpSock{}, lookups: map[uint32]context.CancelFunc{}}
	}).(*tcpSockets)
}

func (s *tcpSockets) get(id uint32) *tcpSock {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.socks[id]
}

func (s *tcpSockets) put(k *tcpSock) {
	s.mu.Lock()
	s.socks[k.id] = k
	s.mu.Unlock()
}

func (s *tcpSockets) take(id uint32) *tcpSock {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.socks[id]
	delete(s.socks, id)
	return k
}

// charge charges the CPU time d of a handler to the container (AC1).
// acctFrom takes the lock of the pod, so a goroutine does it, off the event
// loop. Only one goroutine runs at a time for the time of many handlers.
func (s *tcpSockets) charge(d time.Duration) {
	if d <= 0 {
		return
	}
	if s.cpu.Add(int64(d)) != int64(d) {
		return // a goroutine charges it already
	}
	ctx := wasiPeerCtx(s.p)
	go func() {
		acctFrom(ctx).ChargeCPU(time.Duration(s.cpu.Swap(0)))
	}()
}

// The states of a socket.
const (
	tcpBound = iota + 1
	tcpListening
	tcpConnecting
	tcpConnected
	tcpFailed // the connect failed: only "closed" follows
)

// tcpSock is one socket of a container.
type tcpSock struct {
	id  uint32
	set *tcpSockets
	p   *wasiPeer

	// ctx ends when the socket ends: "closed", MS6 or the stop of the
	// container. Its end closes ln and conn.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	state   int
	v6      bool
	host    string // the IP of "bind" as the component gave it
	port    int    // the local port after "bind", or 0
	release func() // the reservation of the port
	ln      net.Listener
	conns   int // the credit of connections of a listener (rule 4)
	conn    net.Conn

	// Credits of the connection (rule 6). out: the bytes that the Go side
	// can send. in: the bytes that the pod worker can send.
	out, in   int
	queue     [][]byte // bytes of "data" that are not written yet
	shutWrite bool     // "shutdown" of the send direction, after the queue
	shutRead  bool     // "shutdown" of the receive direction
	shutDone  bool     // the write pump closed the send direction
	closing   bool     // "closed": write the queue, then close
	errSent   bool     // "error" went out (rule 8)

	readWake, writeWake, acceptWake chan struct{}
}

func newTCPSock(set *tcpSockets, id uint32) *tcpSock {
	ctx, cancel := context.WithCancel(wasiPeerCtx(set.p))
	k := &tcpSock{
		id: id, set: set, p: set.p, ctx: ctx, cancel: cancel,
		readWake: make(chan struct{}, 1), writeWake: make(chan struct{}, 1), acceptWake: make(chan struct{}, 1),
	}
	context.AfterFunc(ctx, k.end)
	return k
}

// end closes the listener and the connection, and releases the port. It runs
// one time, when the context of the socket ends.
func (k *tcpSock) end() {
	k.mu.Lock()
	ln, conn, release := k.ln, k.conn, k.release
	k.release = nil
	k.mu.Unlock()
	if ln != nil {
		ln.Close() // also removes the row of the loopback table
	}
	if conn != nil {
		conn.Close()
	}
	if release != nil {
		release()
	}
}

func wake(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// send sends a message of the socket. A send fails for an ID that does not
// belong to the container any more: messages can cross "closed" (rule 9).
// Such a message is discarded without a log.
func (k *tcpSock) send(typ string, fields map[string]any) bool {
	if fields["socket"] == nil {
		fields["socket"] = k.id
	}
	if err := k.p.send(typ, fields); err != nil {
		if k.p.Owns("socket", fields["socket"].(uint32)) {
			log.Printf("tabnode: pod worker %s: %s %d: %v", k.p.name, typ, k.id, err)
		}
		return false
	}
	return true
}

// sendError sends "error" one time for a connection (rule 8).
func (k *tcpSock) sendError(code string) {
	k.mu.Lock()
	sent := k.errSent
	k.errSent = true
	k.mu.Unlock()
	if !sent {
		k.send("error", map[string]any{"error": code})
	}
}

func init() {
	wasiHandle("bind", timed(tcpBind))
	wasiHandle("listen", timed(tcpListen))
	wasiHandle("connect", timed(tcpConnect))
	wasiHandle("data", timed(tcpData))
	wasiHandle("credit", timed(tcpCredit))
	wasiHandle("shutdown", timed(tcpShutdown))
	wasiHandle("closed", timed(tcpClosed))
	wasiHandle("lookup", tcpLookup)
	// MS6 and the stop of the container (rule 10, TC14): close at once.
	wasiOnClose("socket", func(p *wasiPeer, id uint32) {
		if k := tcpOf(p).take(id); k != nil {
			k.cancel()
		}
	})
	wasiOnClose("lookup", func(p *wasiPeer, id uint32) {
		s := tcpOf(p)
		s.mu.Lock()
		cancel := s.lookups[id]
		delete(s.lookups, id)
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
}

// timed charges the time of a handler to the container (AC1).
func timed(h wasiHandler) wasiHandler {
	return func(p *wasiPeer, m wasiMsg) {
		start := time.Now()
		h(p, m)
		tcpOf(p).charge(time.Since(start))
	}
}

// podIPOf gives the pod IP of the container of a peer.
func podIPOf(p *wasiPeer) string {
	if pe := procOf(wasiPeerCtx(p)); pe != nil {
		return pe.ip
	}
	return ""
}

// tcpBindAddr examines the IP of "bind" (TC19, MS3, rule 3). It gives the
// error code for an IP that the socket cannot bind.
func tcpBindAddr(family, address, podIP string) (v6 bool, code string) {
	a, err := netip.ParseAddr(address)
	if err != nil {
		return false, "invalid-argument"
	}
	if family == "ipv6" {
		if !a.Is6() || a.Is4In6() {
			// The pod worker gives invalid-argument for an IPv4 address
			// first. IPv4-mapped addresses have no in-process network.
			return true, "address-not-bindable"
		}
		if a.IsUnspecified() || a.IsLoopback() {
			return true, ""
		}
		return true, "address-not-bindable"
	}
	if !a.Is4() {
		return false, "invalid-argument"
	}
	if a.IsUnspecified() || a.IsLoopback() || a.String() == podIP {
		return false, ""
	}
	return false, "address-not-bindable"
}

func tcpBind(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	set := tcpOf(p)
	bound := func(fields map[string]any) { fields["socket"] = id; p.send("bound", fields) }
	if m.Str("protocol") != "tcp" {
		// UDP is phase 4 (section 9).
		bound(map[string]any{"error": "not-supported"})
		return
	}
	if set.get(id) != nil {
		bound(map[string]any{"error": "invalid-state"})
		return
	}
	podIP := podIPOf(p)
	v6, code := tcpBindAddr(m.Str("family"), m.Str("address"), podIP)
	if code != "" {
		// MS3: an answer, not a drop. The socket stays.
		bound(map[string]any{"error": code})
		return
	}
	port := m.Int("port")
	k := newTCPSock(set, id)
	k.state, k.v6, k.host = tcpBound, v6, m.Str("address")
	if v6 {
		// TC6, TC7: no reservation and no listener in the in-process
		// network.
		if port == 0 {
			port = 32768 + int(tcpV6Port.Add(1)%(60999-32768+1))
		}
		k.port = port
		set.put(k)
		bound(map[string]any{"address": k.host, "port": port})
		return
	}
	if tcpBindExclusive && port != 0 && portReserved("tcp", podIP, port) {
		k.cancel()
		bound(map[string]any{"error": "address-in-use"})
		return
	}
	got, release, err := reservePort("tcp", podIP, port)
	if err != nil {
		k.cancel()
		// errPortInUse: a listener has the address (TC18). Port 0 without
		// a free ephemeral port gives EADDRINUSE on Linux too.
		bound(map[string]any{"error": "address-in-use"})
		return
	}
	k.port, k.release = got, release
	set.put(k)
	// Rule 2: the address of the in-process network.
	bound(map[string]any{"address": podIP, "port": got})
}

// tcpListenHost gives the host for listenPod. listenPod changes 0.0.0.0
// and 127.0.0.1 to the pod IP, and adds the row of the loopback table for
// 127.0.0.1 (section 8.11).
func tcpListenHost(host, podIP string) string {
	a, err := netip.ParseAddr(host)
	switch {
	case err != nil:
		return podIP
	case a.IsLoopback():
		return "127.0.0.1"
	case a.IsUnspecified():
		return "0.0.0.0"
	}
	return podIP
}

func tcpListen(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	k := tcpOf(p).get(id)
	listening := func(code string) {
		f := map[string]any{"socket": id}
		if code != "" {
			f["error"] = code
		}
		p.send("listening", f)
	}
	if k == nil {
		listening("invalid-state")
		return
	}
	k.mu.Lock()
	if k.state != tcpBound {
		k.mu.Unlock()
		listening("invalid-state")
		return
	}
	k.state = tcpListening
	k.conns = m.Int("backlog")
	v6, host, port := k.v6, k.host, k.port
	k.mu.Unlock()
	if v6 {
		// TC7: listening, but no listener in the in-process network.
		listening("")
		return
	}
	addr := net.JoinHostPort(tcpListenHost(host, podIPOf(p)), strconv.Itoa(port))
	go func() {
		ln, err := listenPod(k.ctx, addr)
		if err != nil {
			k.mu.Lock()
			k.state = tcpBound
			k.mu.Unlock()
			if k.ctx.Err() == nil {
				k.send("listening", map[string]any{"error": tcpErrorCode(err, "address-in-use")})
			}
			return
		}
		k.mu.Lock()
		k.ln = ln
		k.mu.Unlock()
		if k.ctx.Err() != nil {
			ln.Close()
			return
		}
		if k.send("listening", map[string]any{}) {
			k.acceptLoop(ln)
		}
	}()
}

// acceptLoop accepts connections while the credit of connections is more
// than 0 (rule 4). Other connections wait in the queue of the in-process
// network, as in the backlog of the kernel.
func (k *tcpSock) acceptLoop(ln net.Listener) {
	for {
		k.mu.Lock()
		n := k.conns
		k.mu.Unlock()
		if n <= 0 {
			select {
			case <-k.acceptWake:
				continue
			case <-k.ctx.Done():
				return
			}
		}
		c, err := ln.Accept()
		if err != nil {
			return
		}
		start := time.Now()
		nk := newTCPSock(k.set, k.p.NewID("socket"))
		nk.state, nk.conn, nk.out, nk.in = tcpConnected, c, wasiLimits["data"], wasiLimits["data"]
		k.set.put(nk)
		la, lp := tcpAddrParts(c.LocalAddr())
		ra, rp := tcpAddrParts(c.RemoteAddr())
		// The send fails when the listener does not belong to the
		// container any more (rule 9).
		ok := k.send("accepted", map[string]any{
			"accepted": nk.id, "localAddress": la, "localPort": lp, "remoteAddress": ra, "remotePort": rp,
		})
		if !ok {
			k.set.take(nk.id)
			nk.cancel()
			c.Close()
			return
		}
		k.mu.Lock()
		k.conns--
		k.mu.Unlock()
		nk.startPumps()
		acctFrom(k.ctx).ChargeCPU(time.Since(start))
	}
}

// tcpAddrParts gives the IP and the port of an address of the in-process
// network.
func tcpAddrParts(a net.Addr) (string, int) {
	if t, ok := a.(*net.TCPAddr); ok && t.IP != nil {
		if ip4 := t.IP.To4(); ip4 != nil {
			return ip4.String(), t.Port
		}
		return t.IP.String(), t.Port
	}
	if a == nil {
		return "0.0.0.0", 0
	}
	host, port, err := net.SplitHostPort(a.String())
	if err != nil || !wasiIP(host) {
		return "0.0.0.0", 0
	}
	n, _ := strconv.Atoi(port)
	return host, n
}

func tcpConnect(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	set := tcpOf(p)
	connected := func(code string) { p.send("connected", map[string]any{"socket": id, "error": code}) }
	if m.Str("protocol") != "tcp" {
		connected("not-supported")
		return
	}
	k := set.get(id)
	if k == nil {
		k = newTCPSock(set, id)
		set.put(k)
	} else {
		k.mu.Lock()
		state := k.state
		k.mu.Unlock()
		if state != tcpBound {
			connected("invalid-state")
			return
		}
	}
	a, err := netip.ParseAddr(m.Str("address"))
	if err != nil {
		connected("invalid-argument")
		return
	}
	k.mu.Lock()
	switch {
	case k.v6 && a.Is4(), a.Is4In6():
		// TC20. The pod worker gives this code first. runwasi also gives
		// invalid-argument for an IPv4-mapped IPv6 address: it is not an
		// IPv4 address.
		k.mu.Unlock()
		connected("invalid-argument")
		return
	case a.Is6():
		// Rule 5: the cluster has no IPv6 (section 2). Linux gives
		// ENETUNREACH for an address without a route.
		k.state = tcpFailed
		k.mu.Unlock()
		connected("remote-unreachable")
		return
	}
	k.state = tcpConnecting
	port := k.port
	k.mu.Unlock()
	addr := net.JoinHostPort(a.String(), strconv.Itoa(m.Int("port")))
	go func() {
		// Rule 5: the deadline of synTimeout. A SYN that NetworkPolicy
		// drops and a listener with a full queue give timeout.
		dctx, cancel := context.WithTimeout(k.ctx, synTimeout)
		c, err := dialClusterFrom(dctx, addr, port)
		cancel()
		if err != nil {
			k.mu.Lock()
			k.state = tcpFailed
			k.mu.Unlock()
			if k.ctx.Err() == nil {
				k.send("connected", map[string]any{"error": tcpErrorCode(err, "connection-refused")})
			}
			return
		}
		start := time.Now()
		k.mu.Lock()
		k.state, k.conn, k.out, k.in = tcpConnected, c, wasiLimits["data"], wasiLimits["data"]
		k.mu.Unlock()
		if k.ctx.Err() != nil {
			c.Close()
			return
		}
		la, lp := tcpAddrParts(c.LocalAddr())
		ra, rp := tcpAddrParts(c.RemoteAddr())
		// Rule 2: the pod worker keeps the remote address of the component.
		if k.send("connected", map[string]any{"localAddress": la, "localPort": lp, "remoteAddress": ra, "remotePort": rp}) {
			k.startPumps()
		}
		acctFrom(k.ctx).ChargeCPU(time.Since(start))
	}()
}

// tcpErrorCode maps an error of the in-process network to the error code of
// wasi:sockets (rule 5, rule 8). def is the code for other errors.
func tcpErrorCode(err error, def string) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		// No listener (TC11), a row of the loopback table (TC4, TC15) and
		// a ClusterIP without endpoints (refusedError).
		return "connection-refused"
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT), errors.Is(err, context.DeadlineExceeded):
		// A SYN that NetworkPolicy drops: dropped after synTimeout (TC10).
		// A full queue at the end of the deadline: ETIMEDOUT of the
		// in-process network. wasi:sockets gives ETIMEDOUT as timeout. It is
		// not the connection-refused of CR8, which is HTTP.
		return "timeout"
	case errors.Is(err, errPortInUse), errors.Is(err, syscall.EADDRINUSE):
		// The local port of a bind has a connection to the same address.
		return "address-in-use"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "address-not-bindable"
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH):
		return "remote-unreachable"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		// A reset after the connection (rule 8).
		return "connection-reset"
	case errors.Is(err, syscall.ECONNABORTED):
		return "connection-aborted"
	}
	// Other dial errors give connection-refused, as an unknown address of
	// the in-process network does. Other errors after the connection give
	// connection-reset.
	return def
}

// startPumps starts the read pump and the write pump of a connection.
func (k *tcpSock) startPumps() {
	go k.readPump()
	go k.writePump()
}

// readPump reads from the connection only up to the credit of the pod
// worker, and sends "data" (rule 6, TC12). The end of the stream of the
// peer gives "data" with 0 bytes (rule 7).
func (k *tcpSock) readPump() {
	buf := make([]byte, tcpBufSize)
	for {
		k.mu.Lock()
		n, discard, conn := min(k.out, len(buf)), k.shutRead, k.conn
		k.mu.Unlock()
		if discard {
			// "shutdown" of the receive direction: discard the bytes.
			// trackedConn has no CloseRead.
			n = len(buf)
		}
		if n <= 0 {
			select {
			case <-k.readWake:
				continue
			case <-k.ctx.Done():
				return
			}
		}
		got, err := conn.Read(buf[:n])
		start := time.Now()
		k.mu.Lock()
		discard = k.shutRead
		if !discard {
			k.out -= got
		}
		k.mu.Unlock()
		if got > 0 && !discard {
			if !k.send("data", map[string]any{"bytes": buf[:got]}) {
				return
			}
		}
		acctFrom(k.ctx).ChargeCPU(time.Since(start))
		if err != nil {
			switch {
			case k.ctx.Err() != nil:
			case errors.Is(err, io.EOF):
				if !discard {
					k.send("data", map[string]any{"bytes": []byte{}})
				}
			default:
				k.sendError(tcpErrorCode(err, "connection-reset"))
			}
			return
		}
	}
}

// writePump writes the bytes of "data" in their sequence and sends "credit"
// for them (rule 6). After "shutdown" of the send direction it closes the
// send direction. After "closed" it writes the queue and then closes the
// connection (rule 9).
func (k *tcpSock) writePump() {
	for {
		k.mu.Lock()
		var b []byte
		if len(k.queue) > 0 {
			b = k.queue[0]
			k.queue[0] = nil
			k.queue = k.queue[1:]
		}
		closing, shut, conn := k.closing, k.shutWrite && !k.shutDone, k.conn
		if b == nil && shut {
			k.shutDone = true
		}
		k.mu.Unlock()
		switch {
		case b != nil:
		case closing:
			// The peer gets the end of the stream, not a reset.
			k.cancel()
			return
		case shut:
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
			continue
		default:
			select {
			case <-k.writeWake:
			case <-k.ctx.Done():
				return
			}
			continue
		}
		_, err := conn.Write(b)
		if err != nil {
			if k.ctx.Err() != nil {
				return
			}
			k.sendError(tcpErrorCode(err, "connection-reset"))
			k.mu.Lock()
			k.queue = nil
			closing := k.closing
			k.mu.Unlock()
			if closing {
				k.cancel()
				return
			}
			continue
		}
		start := time.Now()
		k.mu.Lock()
		k.in += len(b)
		k.mu.Unlock()
		k.send("credit", map[string]any{"kind": "bytes", "n": len(b)})
		acctFrom(k.ctx).ChargeCPU(time.Since(start))
	}
}

// connOf gives the socket of a "data" or "shutdown" message, or does MS6.
func connOf(p *wasiPeer, id uint32, typ string) *tcpSock {
	k := tcpOf(p).get(id)
	if k != nil {
		k.mu.Lock()
		ok := k.state == tcpConnected && !k.closing
		k.mu.Unlock()
		if ok {
			return k
		}
	}
	p.Fail("socket", id, typ+": the socket has no connection")
	return nil
}

func tcpData(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	b := m.Bytes("bytes")
	if len(b) == 0 {
		// Rule 7: the pod worker uses "shutdown". Ignore it.
		return
	}
	k := connOf(p, id, "data")
	if k == nil {
		return
	}
	k.mu.Lock()
	if k.shutWrite || len(b) > k.in {
		in := k.in
		k.mu.Unlock()
		// MS4 (rule 6).
		p.Fail("socket", id, "data: "+strconv.Itoa(len(b))+" bytes, credit "+strconv.Itoa(in))
		return
	}
	k.in -= len(b)
	k.queue = append(k.queue, b)
	k.mu.Unlock()
	wake(k.writeWake)
}

func tcpCredit(p *wasiPeer, m wasiMsg) {
	k := tcpOf(p).get(m.Uint("socket"))
	if k == nil {
		// A UDP socket of a later phase.
		return
	}
	n := m.Int("n")
	k.mu.Lock()
	switch m.Str("kind") {
	case "connections":
		k.conns += n
		k.mu.Unlock()
		wake(k.acceptWake)
	case "bytes":
		k.out += n
		k.mu.Unlock()
		wake(k.readWake)
	default:
		k.mu.Unlock()
	}
}

func tcpShutdown(p *wasiPeer, m wasiMsg) {
	id := m.Uint("socket")
	k := connOf(p, id, "shutdown")
	if k == nil {
		return
	}
	how := m.Str("how")
	k.mu.Lock()
	if how == "send" || how == "both" {
		k.shutWrite = true
	}
	if how == "receive" || how == "both" {
		k.shutRead = true
	}
	k.mu.Unlock()
	wake(k.writeWake)
	wake(k.readWake)
}

func tcpClosed(p *wasiPeer, m wasiMsg) {
	k := tcpOf(p).take(m.Uint("socket"))
	if k == nil {
		// bind or connect with an error code: nothing to release.
		return
	}
	k.mu.Lock()
	drain := k.state == tcpConnected
	k.closing = true
	k.mu.Unlock()
	if drain {
		// Rule 9: the write pump writes the queue and then ends the
		// socket.
		wake(k.writeWake)
		return
	}
	k.cancel()
}

// tcpLookup answers "lookup" (rule 11, NL1 to NL3). It runs in a goroutine:
// the resolver takes locks and charges CoreDNS (chargeDNS).
func tcpLookup(p *wasiPeer, m wasiMsg) {
	id, name := m.Uint("lookup"), m.Str("name")
	set := tcpOf(p)
	ctx, cancel := context.WithCancel(wasiPeerCtx(p))
	set.mu.Lock()
	set.lookups[id] = cancel
	set.mu.Unlock()
	go func() {
		start := time.Now()
		ips := lookupAddrs(ctx, name)
		set.mu.Lock()
		_, live := set.lookups[id]
		delete(set.lookups, id)
		set.mu.Unlock()
		cancel()
		if live {
			fields := map[string]any{"lookup": id}
			if len(ips) == 0 {
				fields["error"] = "name-unresolvable"
			} else {
				fields["addresses"] = ips[:min(len(ips), wasiLimits["ips"])]
			}
			if err := p.send("addresses", fields); err != nil && p.Owns("lookup", id) {
				log.Printf("tabnode: pod worker %s: addresses %d: %v", p.name, id, err)
			}
		}
		acctFrom(ctx).ChargeCPU(time.Since(start))
	}()
}

// lookupAddrs gives the addresses of a name: first the names of /etc/hosts
// of the pod, IPv6 first and else in the sequence of the file, then the resolver of nslookup
// with the search path of the pod (rule 11).
func lookupAddrs(ctx context.Context, name string) []string {
	if a, err := netip.ParseAddr(name); err == nil {
		// The pod worker answers an IP itself.
		return []string{a.WithZone("").String()}
	}
	if ips := hostsLookup(podHosts(ctx), name); len(ips) > 0 {
		// runwasi gives the order of getaddrinfo: IPv6 first, so
		// localhost gives ::1 and then 127.0.0.1.
		slices.SortStableFunc(ips, func(a, b string) int {
			return cmpBool(strings.Contains(b, ":"), strings.Contains(a, ":"))
		})
		return ips
	}
	a, ok := dnsLookup(ctx, name)
	if !ok {
		return nil
	}
	return a.ips
}

// podHosts gives /etc/hosts of the container (tabnode_ctr.go, rootFS). In a
// context without the file, it gives the lines that rootFS writes.
func podHosts(ctx context.Context) []byte {
	pe := procOf(ctx)
	if pe == nil {
		return []byte("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	}
	if pe.w != nil && pe.c != nil && pe.w.dir != "" {
		f := filepath.Join(pe.w.dir, "containers", pe.c.spec.Name, "rootfs", "etc", "hosts")
		if b, err := os.ReadFile(f); err == nil {
			return b
		}
	}
	var b bytes.Buffer
	b.WriteString("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n")
	if pe.w != nil && pe.ip != "" {
		if pod, _ := pe.w.podInfo(); pod != nil {
			b.WriteString(pe.ip + "\t" + specHostname(pod) + "\n")
		}
	}
	return b.Bytes()
}

// cmpBool orders true before false.
func cmpBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// hostsLookup gives the addresses of name in a hosts file, in the sequence
// of the file. Names do not depend on case.
func hostsLookup(hosts []byte, name string) []string {
	name = strings.TrimSuffix(strings.ToLower(name), ".")
	var ips []string
	sc := bufio.NewScanner(bytes.NewReader(hosts))
	for sc.Scan() {
		line, _, _ := strings.Cut(sc.Text(), "#")
		f := strings.Fields(line)
		if len(f) < 2 || !wasiIP(f[0]) {
			continue
		}
		for _, n := range f[1:] {
			if strings.ToLower(n) == name {
				ips = append(ips, f[0])
				break
			}
		}
	}
	return ips
}
