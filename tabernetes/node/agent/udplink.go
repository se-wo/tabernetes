package main

// The udp channel of the node agent (section 8.9.1 of docs/wasi-sockets.md):
// datagrams between the pods of this node and the tab addresses (tab pods,
// tab node IPs). The kernel part (nftables, the TPROXY socket and the
// IP_TRANSPARENT sockets of the tab addresses) is in udplink_net.go.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	udpLinkLabel = "udp"

	udpHeaderLen    = 14
	udpVersion      = 1
	udpTypeDatagram = 1
	udpTypeCredit   = 2
	udpMaxDatagram  = 65507                         // fact 13
	udpMaxMessage   = udpHeaderLen + udpMaxDatagram // 65,521 bytes (ND5)
	udpReadBuffer   = 65536

	udpMaxBuffered = 1 << 20 // channel rule 9 (ND12)
	udpMaxFlows    = 4096    // channel rule 11
	udpMaxPorts    = 1024    // channel rule 11
	// udpSendQueue is the number of messages that wait for the writer
	// goroutine. A write can wait in pion (EnableDataChannelBlockWrite), so
	// fromPod puts the message into this queue and a full queue drops.
	udpSendQueue = 64

	udpFlowTimeout       = 30 * time.Second  // FL2, FL4
	udpFlowStreamTimeout = 120 * time.Second // FL3
	udpFlowStreamAfter   = 2 * time.Second   // FL3: an answer, and a datagram 2 s after the first one
	udpFlowSweepEvery    = 10 * time.Second  // the timer that removes closed flows
	udpDropLogEvery      = 10 * time.Second  // channel rule 3
)

// tabPort is the socket of one tab address (IP and port of a tab pod or a tab node IP) on this node.
type tabPort interface {
	WriteTo(b []byte, dst netip.AddrPort) error // to a pod of this node
	Close() error
}

// openTabPort opens the socket of tab. W5.2b sets it in an init() (IP_TRANSPARENT, fsuid 65534).
// nil: each datagram that needs a port is a counted drop.
var openTabPort func(tab netip.AddrPort) (tabPort, error)

// startUDPNet starts the kernel part for l: the TPROXY socket. W5.2b sets it in an init().
// servePodLinks calls it once, after setupPodLinkNet succeeded. nil: no kernel part.
var startUDPNet func(ctx context.Context, l *udpLink) error

// udpFlowNow is the clock of the flows (plan rule 5).
var udpFlowNow = time.Now

// udpChannelInit gives the settings of the udp data channel (channel rule 1, ND3).
func udpChannelInit() *webrtc.DataChannelInit {
	ordered := false
	var retransmits uint16
	return &webrtc.DataChannelInit{Ordered: &ordered, MaxRetransmits: &retransmits}
}

// --- The header (section 8.9, channel rules 2 and 3) ---

var (
	errUDPShort    = errors.New("message shorter than 14 bytes")
	errUDPVersion  = errors.New("unknown version")
	errUDPType     = errors.New("unknown type")
	errUDPLong     = errors.New("datagram message longer than 65,521 bytes")
	errUDPPort     = errors.New("port 0")
	errUDPCredit   = errors.New("credit not of 14 bytes")
	errUDPNotIPv4  = errors.New("address not IPv4")
	errUDPDatagram = errors.New("datagram longer than 65,507 bytes")
)

// udpMessage is one examined message of the udp channel.
type udpMessage struct {
	typ      byte
	src, dst netip.AddrPort // type 1
	credit   uint32         // type 2
	payload  []byte         // type 1: a part of the message
}

// parseUDPMessage examines m with the conditions of channel rule 3, except
// the condition on the direction of a credit: the receiver does that.
func parseUDPMessage(m []byte) (udpMessage, error) {
	if len(m) < udpHeaderLen {
		return udpMessage{}, errUDPShort
	}
	if m[0] != udpVersion {
		return udpMessage{}, errUDPVersion
	}
	switch m[1] {
	case udpTypeDatagram:
		if len(m) > udpMaxMessage {
			return udpMessage{}, errUDPLong
		}
		src := netip.AddrPortFrom(netip.AddrFrom4([4]byte(m[2:6])), binary.BigEndian.Uint16(m[6:8]))
		dst := netip.AddrPortFrom(netip.AddrFrom4([4]byte(m[8:12])), binary.BigEndian.Uint16(m[12:14]))
		if src.Port() == 0 || dst.Port() == 0 {
			return udpMessage{}, errUDPPort
		}
		return udpMessage{typ: udpTypeDatagram, src: src, dst: dst, payload: m[udpHeaderLen:]}, nil
	case udpTypeCredit:
		if len(m) != udpHeaderLen {
			return udpMessage{}, errUDPCredit
		}
		return udpMessage{typ: udpTypeCredit, credit: binary.BigEndian.Uint32(m[2:6])}, nil
	default:
		return udpMessage{}, errUDPType
	}
}

// appendUDPDatagram appends the message of a datagram from src to dst to buf.
func appendUDPDatagram(buf []byte, src, dst netip.AddrPort, b []byte) ([]byte, error) {
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	if !s.Is4() || !d.Is4() {
		return buf, errUDPNotIPv4
	}
	if src.Port() == 0 || dst.Port() == 0 {
		return buf, errUDPPort
	}
	if len(b) > udpMaxDatagram {
		return buf, errUDPDatagram
	}
	s4, d4 := s.As4(), d.As4()
	buf = append(buf, udpVersion, udpTypeDatagram)
	buf = append(buf, s4[:]...)
	buf = binary.BigEndian.AppendUint16(buf, src.Port())
	buf = append(buf, d4[:]...)
	buf = binary.BigEndian.AppendUint16(buf, dst.Port())
	return append(buf, b...), nil
}

// appendUDPCredit appends the message of a credit of n bytes to buf.
func appendUDPCredit(buf []byte, n uint32) []byte {
	buf = append(buf, udpVersion, udpTypeCredit)
	buf = binary.BigEndian.AppendUint32(buf, n)
	return append(buf, make([]byte, 8)...)
}

// --- Counted drops (channel rule 3) ---

type udpDrops struct {
	mu      sync.Mutex
	count   map[string]uint64
	total   uint64
	logged  uint64
	lastLog time.Time
}

// drop counts one dropped datagram or message. It writes a line to the log
// at most one time each 10 seconds.
func (d *udpDrops) drop(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == nil {
		d.count = map[string]uint64{}
	}
	d.count[reason]++
	d.total++
	now := time.Now()
	if !d.lastLog.IsZero() && now.Sub(d.lastLog) < udpDropLogEvery {
		return
	}
	reasons := make([]string, 0, len(d.count))
	for r, n := range d.count {
		reasons = append(reasons, fmt.Sprintf("%s: %d", r, n))
	}
	sort.Strings(reasons)
	log.Printf("udp channel: dropped %d datagrams (%d in total; %s)", d.total-d.logged, d.total, strings.Join(reasons, ", "))
	d.logged, d.lastLog = d.total, now
}

func (d *udpDrops) get(reason string) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.count[reason]
}

// The reasons of the drops (the words in the log line).
const (
	udpDropInvalid    = "invalid message"
	udpDropCredit     = "credit"
	udpDropNotLocal   = "destination not a pod of this node"
	udpDropNoSubnet   = "no flannel subnet"
	udpDropFlows      = "flow limit"
	udpDropPorts      = "socket limit"
	udpDropNoSocket   = "no socket for tab addresses"
	udpDropOpen       = "socket open failed"
	udpDropPodWrite   = "write to pod failed"
	udpDropBadAddr    = "address not usable"
	udpDropNoChannel  = "no channel"
	udpDropBuffered   = "bufferedAmount above 1 MiB"
	udpDropQueue      = "send queue full"
	udpDropChanWrite  = "channel write failed"
	udpDropShortRead  = "message longer than 65,536 bytes"
	udpDropUnexpected = "unexpected"
)

// --- The channel ---

// udpChannel is the write side of the udp data channel.
type udpChannel interface {
	BufferedAmount() uint64
	Write(b []byte) (int, error)
}

// udpDC is a detached pion data channel.
type udpDC struct {
	dc  *webrtc.DataChannel
	raw io.ReadWriteCloser
}

func (c udpDC) BufferedAmount() uint64      { return c.dc.BufferedAmount() }
func (c udpDC) Write(b []byte) (int, error) { return c.raw.Write(b) }

// --- The flow table (section 8.8, FL2 to FL4, ND19) ---

// udpFlowKey is one tab address and one pod address of this node.
type udpFlowKey struct {
	tab, pod netip.AddrPort
}

type udpFlow struct {
	fromTab   bool      // the first datagram came from the tab side
	first     time.Time // the first datagram
	last      time.Time // the last datagram, in each direction
	lastReply time.Time // the last answer: a datagram against the first direction
	stream    bool
}

func (f *udpFlow) expired(now time.Time) bool {
	timeout := udpFlowTimeout
	if f.stream {
		timeout = udpFlowStreamTimeout
	}
	return now.Sub(f.last) >= timeout
}

func (f *udpFlow) touch(now time.Time, fromTab bool) {
	f.last = now
	if fromTab != f.fromTab {
		f.lastReply = now
	}
	if !f.lastReply.IsZero() && now.Sub(f.first) >= udpFlowStreamAfter {
		f.stream = true
	}
}

type udpTabPort struct {
	p     tabPort
	flows int
}

// udpLink is the udp channel of this node and its flow table.
type udpLink struct {
	drops udpDrops

	out   atomic.Pointer[udpChannel] // nil: no open channel
	queue chan []byte

	// subnet gives the flannel subnet of this node. The first result
	// without an error stays.
	subnet  func() (*net.IPNet, error)
	podNet  atomic.Pointer[netip.Prefix]
	mu      sync.Mutex
	flows   map[udpFlowKey]*udpFlow
	ports   map[netip.AddrPort]*udpTabPort
	stopped bool
}

func newUDPLink() *udpLink {
	return &udpLink{
		queue:  make(chan []byte, udpSendQueue),
		subnet: flannelSubnet,
		flows:  map[udpFlowKey]*udpFlow{},
		ports:  map[netip.AddrPort]*udpTabPort{},
	}
}

// openUDPLink opens the udp channel to the page (channel rule 1). Call it
// after the peer connection is connected.
func openUDPLink(ctx context.Context, pc *webrtc.PeerConnection) *udpLink {
	l := newUDPLink()
	go l.sweepLoop(ctx)
	dc, err := pc.CreateDataChannel(udpLinkLabel, udpChannelInit())
	if err != nil {
		log.Printf("udp channel: %v", err)
		return l
	}
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			log.Printf("udp channel: %v", err)
			return
		}
		l.serve(ctx, udpDC{dc: dc, raw: raw}, raw)
	})
	return l
}

// serve uses ch to send and r to receive until r ends. It does not wait.
func (l *udpLink) serve(ctx context.Context, ch udpChannel, r io.Reader) {
	l.out.Store(&ch)
	done := make(chan struct{})
	go l.writeLoop(ch, done)
	go func() {
		defer close(done)
		l.readLoop(ctx, r)
		l.out.CompareAndSwap(&ch, nil)
	}()
}

// readLoop gives each message from the page to fromPage. Each read gives
// one message.
func (l *udpLink) readLoop(ctx context.Context, r io.Reader) {
	buf := make([]byte, udpReadBuffer)
	for {
		n, err := r.Read(buf)
		switch {
		case errors.Is(err, io.ErrShortBuffer):
			l.drops.drop(udpDropShortRead)
			continue
		case err != nil:
			if ctx.Err() == nil {
				log.Printf("udp channel: closed (%v); the node agent does not open it again", err)
			}
			return
		}
		l.fromPage(buf[:n])
	}
}

// writeLoop writes the messages of the queue to ch until done is closed.
func (l *udpLink) writeLoop(ch udpChannel, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		case m := <-l.queue:
			if ch.BufferedAmount() > udpMaxBuffered {
				l.drops.drop(udpDropBuffered)
				continue
			}
			if _, err := ch.Write(m); err != nil {
				l.drops.drop(udpDropChanWrite)
			}
		}
	}
}

// send gives a message to the writer goroutine. It never waits (channel rule 9).
func (l *udpLink) send(m []byte) {
	ch := l.out.Load()
	if ch == nil {
		l.drops.drop(udpDropNoChannel)
		return
	}
	if (*ch).BufferedAmount() > udpMaxBuffered {
		l.drops.drop(udpDropBuffered)
		return
	}
	select {
	case l.queue <- m:
	default:
		l.drops.drop(udpDropQueue)
	}
}

// localPods gives the flannel subnet of this node.
func (l *udpLink) localPods() (netip.Prefix, bool) {
	if p := l.podNet.Load(); p != nil {
		return *p, true
	}
	n, err := l.subnet()
	if err != nil {
		return netip.Prefix{}, false
	}
	a, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	p := netip.PrefixFrom(a.Unmap(), ones).Masked()
	l.podNet.Store(&p)
	return p, true
}

// fromPage handles one message from the page: a datagram from a tab address
// to a pod of this node (ND16).
func (l *udpLink) fromPage(m []byte) {
	msg, err := parseUDPMessage(m)
	switch {
	case err != nil:
		l.drops.drop(udpDropInvalid)
		return
	case msg.typ == udpTypeCredit:
		// The agent receives no credits (channel rules 3 and 8).
		l.drops.drop(udpDropCredit)
		return
	}
	pods, ok := l.localPods()
	if !ok {
		l.drops.drop(udpDropNoSubnet)
		return
	}
	if !pods.Contains(msg.dst.Addr()) {
		l.drops.drop(udpDropNotLocal)
		return
	}
	p := l.flow(msg.src, msg.dst, true)
	if p == nil {
		return
	}
	if err := p.WriteTo(msg.payload, msg.dst); err != nil {
		// Also an ICMP error of an earlier datagram: no refusal to the
		// page (channel rule 7).
		l.drops.drop(udpDropPodWrite)
	}
}

// fromPod: a pod of this node (src) sent b to the tab address dst. The TPROXY
// socket and each tabPort of W5.2b call it. It must not wait. It does not keep b.
func (l *udpLink) fromPod(src, dst netip.AddrPort, b []byte) {
	src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
	dst = netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port())
	m, err := appendUDPDatagram(make([]byte, 0, udpHeaderLen+len(b)), src, dst, b)
	if err != nil {
		l.drops.drop(udpDropBadAddr)
		return
	}
	// The answers of the tab side leave from the socket of dst (ND15).
	if l.flow(dst, src, false) == nil {
		return
	}
	l.send(m)
}

// flow notes a datagram of the flow between tab and pod, and gives the
// socket of tab. It opens the socket for a new tab address. nil: a counted drop.
func (l *udpLink) flow(tab, pod netip.AddrPort, fromTab bool) tabPort {
	now := udpFlowNow()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		l.drops.drop(udpDropNoChannel)
		return nil
	}
	k := udpFlowKey{tab: tab, pod: pod}
	f := l.flows[k]
	if f != nil && f.expired(now) {
		l.removeFlow(k)
		f = nil
	}
	if f != nil {
		f.touch(now, fromTab)
		return l.ports[tab].p
	}
	if len(l.flows) >= udpMaxFlows {
		l.drops.drop(udpDropFlows)
		return nil
	}
	tp := l.ports[tab]
	if tp == nil {
		switch {
		case len(l.ports) >= udpMaxPorts:
			l.drops.drop(udpDropPorts)
			return nil
		case openTabPort == nil:
			l.drops.drop(udpDropNoSocket)
			return nil
		}
		p, err := openTabPort(tab)
		if err != nil {
			l.drops.drop(udpDropOpen)
			return nil
		}
		if p == nil {
			l.drops.drop(udpDropUnexpected)
			return nil
		}
		tp = &udpTabPort{p: p}
		l.ports[tab] = tp
	}
	tp.flows++
	l.flows[k] = &udpFlow{fromTab: fromTab, first: now, last: now}
	return tp.p
}

// removeFlow removes the flow k and closes the socket of its tab address
// after its last flow. The caller holds l.mu.
func (l *udpLink) removeFlow(k udpFlowKey) {
	delete(l.flows, k)
	tp := l.ports[k.tab]
	if tp == nil {
		return
	}
	if tp.flows--; tp.flows <= 0 {
		delete(l.ports, k.tab)
		tp.p.Close()
	}
}

// sweep removes the closed flows.
func (l *udpLink) sweep(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, f := range l.flows {
		if f.expired(now) {
			l.removeFlow(k)
		}
	}
}

// sweepLoop runs sweep until ctx ends. Then it closes all sockets.
func (l *udpLink) sweepLoop(ctx context.Context) {
	t := time.NewTicker(udpFlowSweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			l.mu.Lock()
			l.stopped = true
			for k := range l.flows {
				l.removeFlow(k)
			}
			l.mu.Unlock()
			return
		case <-t.C:
			l.sweep(udpFlowNow())
		}
	}
}

// startUDPLinkNet starts the kernel part of l, if there is one.
func startUDPLinkNet(ctx context.Context, l *udpLink) {
	if startUDPNet == nil || l == nil {
		return
	}
	if err := startUDPNet(ctx, l); err != nil {
		log.Printf("udp channel: pods of this node cannot send UDP to tab addresses: %v", err)
	}
}
