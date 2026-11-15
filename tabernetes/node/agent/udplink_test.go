package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// --- Fakes ---

type fakeTabPort struct {
	tab    netip.AddrPort
	mu     sync.Mutex
	writes []fakeWrite
	closed bool
	err    error
}

type fakeWrite struct {
	dst netip.AddrPort
	b   []byte
}

func (p *fakeTabPort) WriteTo(b []byte, dst netip.AddrPort) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	p.writes = append(p.writes, fakeWrite{dst, bytes.Clone(b)})
	return nil
}

func (p *fakeTabPort) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

func (p *fakeTabPort) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// fakeOpener sets openTabPort for the test and records the opened ports.
type fakeOpener struct {
	mu     sync.Mutex
	opened []*fakeTabPort
	err    error
}

func useFakeOpener(t *testing.T) *fakeOpener {
	t.Helper()
	o := &fakeOpener{}
	old := openTabPort
	openTabPort = func(tab netip.AddrPort) (tabPort, error) {
		o.mu.Lock()
		defer o.mu.Unlock()
		if o.err != nil {
			return nil, o.err
		}
		p := &fakeTabPort{tab: tab}
		o.opened = append(o.opened, p)
		return p, nil
	}
	t.Cleanup(func() { openTabPort = old })
	return o
}

func (o *fakeOpener) ports() []*fakeTabPort {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*fakeTabPort(nil), o.opened...)
}

// fakeUDPChannel is the write side of a data channel.
type fakeUDPChannel struct {
	mu       sync.Mutex
	buffered uint64
	writes   [][]byte
	wrote    chan struct{}
}

func newFakeUDPChannel() *fakeUDPChannel {
	return &fakeUDPChannel{wrote: make(chan struct{}, 1024)}
}

func (c *fakeUDPChannel) BufferedAmount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buffered
}

func (c *fakeUDPChannel) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, bytes.Clone(b))
	c.mu.Unlock()
	c.wrote <- struct{}{}
	return len(b), nil
}

func (c *fakeUDPChannel) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.writes)
}

// waitWrite waits for one write of the writer goroutine.
func (c *fakeUDPChannel) waitWrite(t *testing.T) []byte {
	t.Helper()
	select {
	case <-c.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("no write on the channel")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes[len(c.writes)-1]
}

var testPodNet = netip.MustParsePrefix("10.244.1.0/24")

// newTestUDPLink gives a link with the flannel subnet 10.244.1.0/24 and a
// fake channel. The reader of the channel blocks until the test ends.
func newTestUDPLink(t *testing.T) (*udpLink, *fakeUDPChannel) {
	t.Helper()
	l := newUDPLink()
	l.subnet = func() (*net.IPNet, error) {
		_, n, err := net.ParseCIDR(testPodNet.String())
		return n, err
	}
	ch := newFakeUDPChannel()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	t.Cleanup(func() { cancel(); pw.Close() })
	l.serve(ctx, ch, pr)
	return l, ch
}

// setFlowClock sets udpFlowNow to a clock that the test moves.
func setFlowClock(t *testing.T) *time.Time {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	old := udpFlowNow
	udpFlowNow = func() time.Time { return now }
	t.Cleanup(func() { udpFlowNow = old })
	return &now
}

func ap(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func datagramMsg(t *testing.T, src, dst netip.AddrPort, b []byte) []byte {
	t.Helper()
	m, err := appendUDPDatagram(nil, src, dst, b)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// --- ND3: the settings of the channel ---

func TestUDPLinkChannelSettings(t *testing.T) {
	init := udpChannelInit()
	if init.Ordered == nil || *init.Ordered {
		t.Errorf("Ordered = %v, want false", init.Ordered)
	}
	if init.MaxRetransmits == nil || *init.MaxRetransmits != 0 {
		t.Errorf("MaxRetransmits = %v, want 0", init.MaxRetransmits)
	}
	if init.MaxPacketLifeTime != nil {
		t.Errorf("MaxPacketLifeTime = %v, want nil", *init.MaxPacketLifeTime)
	}
}

// TestUDPLinkChannelPeers connects two pion peers in this process. The
// other side reads the settings of the channel, and each side sends a
// message of 65,521 bytes (section 9.2, item 10).
func TestUDPLinkChannelPeers(t *testing.T) {
	newAPI := func() *webrtc.API {
		se := webrtc.SettingEngine{}
		se.DetachDataChannels()
		se.EnableDataChannelBlockWrite(true)
		se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
		se.SetIncludeLoopbackCandidate(true)
		se.SetInterfaceFilter(func(name string) bool { return strings.HasPrefix(name, "lo") })
		return webrtc.NewAPI(webrtc.WithSettingEngine(se))
	}
	agent, err := newAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	page, err := newAPI().NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()

	big := bytes.Repeat([]byte{0xab}, udpMaxDatagram)
	msg := datagramMsg(t, ap("10.244.1.5:5353"), ap("10.244.0.7:53"), big)
	if len(msg) != 65521 {
		t.Fatalf("message has %d bytes, want 65,521", len(msg))
	}

	type pageSide struct {
		ordered bool
		retr    *uint16
		got     []byte
		err     error
	}
	pageDone := make(chan pageSide, 1)
	page.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			res := pageSide{ordered: dc.Ordered(), retr: dc.MaxRetransmits()}
			raw, err := dc.Detach()
			if err != nil {
				res.err = err
				pageDone <- res
				return
			}
			buf := make([]byte, udpReadBuffer)
			n, err := raw.Read(buf)
			if err != nil {
				res.err = err
				pageDone <- res
				return
			}
			res.got = buf[:n]
			_, res.err = raw.Write(msg)
			pageDone <- res
		})
	})

	agentGot := make(chan []byte, 1)
	dc, err := agent.CreateDataChannel(udpLinkLabel, udpChannelInit())
	if err != nil {
		t.Fatal(err)
	}
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			agentGot <- nil
			return
		}
		if _, err := raw.Write(msg); err != nil {
			agentGot <- nil
			return
		}
		buf := make([]byte, udpReadBuffer)
		n, err := raw.Read(buf)
		if err != nil {
			agentGot <- nil
			return
		}
		agentGot <- buf[:n]
	})

	offer, err := agent.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := webrtc.GatheringCompletePromise(agent)
	if err := agent.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := page.SetRemoteDescription(*agent.LocalDescription()); err != nil {
		t.Fatal(err)
	}
	answer, err := page.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered = webrtc.GatheringCompletePromise(page)
	if err := page.SetLocalDescription(answer); err != nil {
		t.Fatal(err)
	}
	<-gathered
	if err := agent.SetRemoteDescription(*page.LocalDescription()); err != nil {
		t.Fatal(err)
	}

	select {
	case res := <-pageDone:
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.ordered {
			t.Error("the page sees ordered: true, want false")
		}
		if res.retr == nil || *res.retr != 0 {
			t.Errorf("the page sees maxRetransmits %v, want 0", res.retr)
		}
		if !bytes.Equal(res.got, msg) {
			t.Errorf("the page got %d bytes, want the message of %d bytes", len(res.got), len(msg))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the channel did not open")
	}
	select {
	case got := <-agentGot:
		if !bytes.Equal(got, msg) {
			t.Errorf("the agent got %d bytes, want the message of %d bytes", len(got), len(msg))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the agent got no message")
	}
}

// --- ND4, ND5: the header ---

func TestUDPLinkTestVectors(t *testing.T) {
	// The test vectors of section 8.9.1.
	wantDatagram, _ := hex.DecodeString("01010af4010514e90af40007003568" + "69")
	got, err := appendUDPDatagram(nil, ap("10.244.1.5:5353"), ap("10.244.0.7:53"), []byte("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, wantDatagram) || len(got) != 16 {
		t.Errorf("datagram = % x, want % x", got, wantDatagram)
	}
	m, err := parseUDPMessage(wantDatagram)
	if err != nil {
		t.Fatal(err)
	}
	if m.typ != udpTypeDatagram || m.src != ap("10.244.1.5:5353") || m.dst != ap("10.244.0.7:53") || string(m.payload) != "hi" {
		t.Errorf("parsed datagram = %+v", m)
	}

	wantCredit, _ := hex.DecodeString("01020000fff10000000000000000")
	if got := appendUDPCredit(nil, 65521); !bytes.Equal(got, wantCredit) {
		t.Errorf("credit = % x, want % x", got, wantCredit)
	}
	m, err = parseUDPMessage(wantCredit)
	if err != nil {
		t.Fatal(err)
	}
	if m.typ != udpTypeCredit || m.credit != 65521 {
		t.Errorf("parsed credit = %+v", m)
	}

	// IPv4-in-IPv6 addresses are written as IPv4; IPv6 is not possible.
	mapped := netip.AddrPortFrom(netip.AddrFrom16(netip.MustParseAddr("::ffff:10.244.1.5").As16()), 5353)
	if got, _ := appendUDPDatagram(nil, mapped, ap("10.244.0.7:53"), []byte("hi")); !bytes.Equal(got, wantDatagram) {
		t.Errorf("mapped source: % x", got)
	}
	if _, err := appendUDPDatagram(nil, ap("[fd00::1]:53"), ap("10.244.0.7:53"), nil); err == nil {
		t.Error("an IPv6 source gave no error")
	}
}

func TestUDPLinkExamination(t *testing.T) {
	src, dst := ap("10.0.0.1:5353"), ap("10.244.1.7:53")
	valid := func(n int) []byte { return datagramMsg(t, src, dst, make([]byte, n)) }
	if _, err := parseUDPMessage(valid(udpMaxDatagram)); err != nil {
		t.Errorf("65,521 bytes: %v", err)
	}
	if _, err := parseUDPMessage(valid(0)); err != nil {
		t.Errorf("14 bytes: %v", err)
	}
	long := append(valid(udpMaxDatagram), 0)
	version2 := valid(2)
	version2[0] = 2
	type3 := valid(2)
	type3[1] = 3
	srcPort0 := valid(2)
	srcPort0[6], srcPort0[7] = 0, 0
	dstPort0 := valid(2)
	dstPort0[12], dstPort0[13] = 0, 0
	longCredit := append(appendUDPCredit(nil, 1), 0)
	for name, tc := range map[string]struct {
		m    []byte
		want error
	}{
		"65,522 bytes": {long, errUDPLong},
		"13 bytes":     {valid(0)[:13], errUDPShort},
		"0 bytes":      {nil, errUDPShort},
		"version 2":    {version2, errUDPVersion},
		"type 3":       {type3, errUDPType},
		"source port":  {srcPort0, errUDPPort},
		"dest port":    {dstPort0, errUDPPort},
		"long credit":  {longCredit, errUDPCredit},
	} {
		if _, err := parseUDPMessage(tc.m); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

// TestUDPLinkFromPageDrops: the agent drops the invalid messages and each
// credit, and counts them (channel rule 3).
func TestUDPLinkFromPageDrops(t *testing.T) {
	o := useFakeOpener(t)
	l, _ := newTestUDPLink(t)
	src, dst := ap("10.0.0.1:5353"), ap("10.244.1.7:53")
	long := datagramMsg(t, src, dst, make([]byte, udpMaxDatagram))
	long = append(long, 0)
	short := datagramMsg(t, src, dst, nil)[:13]
	version2 := datagramMsg(t, src, dst, []byte("x"))
	version2[0] = 2
	type3 := datagramMsg(t, src, dst, []byte("x"))
	type3[1] = 3
	port0 := datagramMsg(t, src, dst, []byte("x"))
	port0[13] = 0
	for _, m := range [][]byte{long, short, version2, type3, port0} {
		l.fromPage(m)
	}
	l.fromPage(appendUDPCredit(nil, 65521))
	if n := l.drops.get(udpDropInvalid); n != 5 {
		t.Errorf("invalid drops = %d, want 5", n)
	}
	if n := l.drops.get(udpDropCredit); n != 1 {
		t.Errorf("credit drops = %d, want 1", n)
	}
	// A destination outside the flannel subnet of this node.
	l.fromPage(datagramMsg(t, src, ap("10.244.2.7:53"), []byte("x")))
	if n := l.drops.get(udpDropNotLocal); n != 1 {
		t.Errorf("not-local drops = %d, want 1", n)
	}
	if p := o.ports(); len(p) != 0 {
		t.Errorf("%d sockets opened, want 0", len(p))
	}
}

// --- ND12: sends never wait ---

func TestUDPLinkBufferedAmount(t *testing.T) {
	useFakeOpener(t)
	l, ch := newTestUDPLink(t)
	ch.mu.Lock()
	ch.buffered = 2 << 20
	ch.mu.Unlock()
	start := time.Now()
	for range 10 {
		l.fromPod(ap("10.244.1.7:53"), ap("10.0.0.1:5353"), []byte("answer"))
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("fromPod took %v", d)
	}
	if n := l.drops.get(udpDropBuffered); n != 10 {
		t.Errorf("bufferedAmount drops = %d, want 10", n)
	}
	if n := ch.count(); n != 0 {
		t.Errorf("%d writes on a channel with 2 MiB buffered, want 0", n)
	}
}

// blockingChannel is a channel whose writes wait, as pion with
// EnableDataChannelBlockWrite.
type blockingChannel struct {
	release chan struct{}
}

func (c *blockingChannel) BufferedAmount() uint64 { return 0 }
func (c *blockingChannel) Write(b []byte) (int, error) {
	<-c.release
	return len(b), nil
}

func TestUDPLinkWriteWaitsQueueDrops(t *testing.T) {
	useFakeOpener(t)
	l := newUDPLink()
	ch := &blockingChannel{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	defer func() { close(ch.release); cancel(); pw.Close() }()
	l.serve(ctx, ch, pr)
	start := time.Now()
	n := udpSendQueue + 100
	for range n {
		l.fromPod(ap("10.244.1.7:53"), ap("10.0.0.1:5353"), []byte("answer"))
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("fromPod took %v", d)
	}
	// The writer goroutine holds at most one message; the queue holds
	// udpSendQueue messages; the others are dropped.
	if got := l.drops.get(udpDropQueue); got < 99 {
		t.Errorf("queue drops = %d, want at least 99", got)
	}
}

func TestUDPLinkNoChannel(t *testing.T) {
	useFakeOpener(t)
	l := newUDPLink()
	l.fromPod(ap("10.244.1.7:53"), ap("10.0.0.1:5353"), []byte("x"))
	if n := l.drops.get(udpDropNoChannel); n != 1 {
		t.Errorf("no-channel drops = %d, want 1", n)
	}
}

// --- ND15, ND16, ND18: the flows ---

func TestUDPLinkFlowBothDirections(t *testing.T) {
	o := useFakeOpener(t)
	setFlowClock(t)
	l, ch := newTestUDPLink(t)
	tab, pod := ap("10.244.0.5:40000"), ap("10.244.1.7:53")

	// ND16: a datagram from the page opens the socket of its source and
	// writes to the pod.
	l.fromPage(datagramMsg(t, tab, pod, []byte("query")))
	ports := o.ports()
	if len(ports) != 1 || ports[0].tab != tab {
		t.Fatalf("opened sockets = %v, want one for %v", ports, tab)
	}
	if w := ports[0].writes; len(w) != 1 || w[0].dst != pod || string(w[0].b) != "query" {
		t.Fatalf("writes = %v", w)
	}

	// ND15, ND18: the answer goes to the channel with the tab address as
	// its destination, from the same socket.
	l.fromPod(pod, tab, []byte("answer"))
	got := ch.waitWrite(t)
	if want := datagramMsg(t, pod, tab, []byte("answer")); !bytes.Equal(got, want) {
		t.Errorf("message = % x, want % x", got, want)
	}
	if len(o.ports()) != 1 {
		t.Errorf("the answer opened another socket")
	}

	// Two pods that send to one tab address share one socket.
	l.fromPod(ap("10.244.1.8:9999"), tab, []byte("other"))
	ch.waitWrite(t)
	if n := len(o.ports()); n != 1 {
		t.Errorf("%d sockets for one tab address, want 1", n)
	}
	if n := len(l.flows); n != 2 {
		t.Errorf("%d flows, want 2", n)
	}
}

func TestUDPLinkNodeIPSource(t *testing.T) {
	o := useFakeOpener(t)
	l, _ := newTestUDPLink(t)
	// The terminal or a hostNetwork pod of a tab node: the node IP.
	nodeIP := ap("192.168.65.10:41000")
	l.fromPage(datagramMsg(t, nodeIP, ap("10.244.1.7:53"), []byte("q")))
	ports := o.ports()
	if len(ports) != 1 || ports[0].tab != nodeIP {
		t.Fatalf("opened sockets = %v, want one on %v", ports, nodeIP)
	}
}

func TestUDPLinkNoOpener(t *testing.T) {
	old := openTabPort
	openTabPort = nil
	t.Cleanup(func() { openTabPort = old })
	l, ch := newTestUDPLink(t)
	l.fromPage(datagramMsg(t, ap("10.244.0.5:40000"), ap("10.244.1.7:53"), []byte("q")))
	l.fromPod(ap("10.244.1.7:53"), ap("10.244.0.5:40000"), []byte("a"))
	if n := l.drops.get(udpDropNoSocket); n != 2 {
		t.Errorf("no-socket drops = %d, want 2", n)
	}
	if ch.count() != 0 || len(l.flows) != 0 {
		t.Errorf("writes = %d, flows = %d, want 0 and 0", ch.count(), len(l.flows))
	}
}

func TestUDPLinkSocketErrors(t *testing.T) {
	o := useFakeOpener(t)
	l, ch := newTestUDPLink(t)
	o.err = errors.New("bind failed")
	l.fromPage(datagramMsg(t, ap("10.244.0.5:40000"), ap("10.244.1.7:53"), []byte("q")))
	if n := l.drops.get(udpDropOpen); n != 1 {
		t.Errorf("open drops = %d, want 1", n)
	}
	o.err = nil
	l.fromPage(datagramMsg(t, ap("10.244.0.5:40000"), ap("10.244.1.7:53"), []byte("q")))
	// Channel rule 7: an ICMP error comes back as an error of WriteTo. It
	// is a counted drop and sends nothing to the page.
	p := o.ports()[0]
	p.mu.Lock()
	p.err = errors.New("connection refused")
	p.mu.Unlock()
	l.fromPage(datagramMsg(t, ap("10.244.0.5:40000"), ap("10.244.1.7:53"), []byte("q")))
	if n := l.drops.get(udpDropPodWrite); n != 1 {
		t.Errorf("write drops = %d, want 1", n)
	}
	time.Sleep(50 * time.Millisecond)
	if n := ch.count(); n != 0 {
		t.Errorf("%d messages to the page, want 0", n)
	}
}

// --- ND19: the timeouts of FL2 to FL4 ---

func TestUDPLinkFlowTimeouts(t *testing.T) {
	o := useFakeOpener(t)
	now := setFlowClock(t)
	l, _ := newTestUDPLink(t)
	pod := ap("10.244.1.7:53")

	// FL2: a flow without answers is gone after 30 seconds, and its socket
	// is closed.
	noAnswer := ap("10.244.0.5:40001")
	l.fromPage(datagramMsg(t, noAnswer, pod, []byte("q")))
	*now = now.Add(29 * time.Second)
	l.sweep(*now)
	if len(l.flows) != 1 {
		t.Fatal("FL2: the flow is gone before 30 seconds")
	}
	*now = now.Add(time.Second)
	l.sweep(*now)
	if len(l.flows) != 0 || len(l.ports) != 0 {
		t.Errorf("FL2: flows = %d, sockets = %d after 30 seconds, want 0", len(l.flows), len(l.ports))
	}
	if !o.ports()[0].isClosed() {
		t.Error("FL2: the socket is not closed")
	}

	// FL3: a flow with answers for 5 seconds stays after 100 seconds
	// without datagrams and is gone after 120 seconds.
	stream := ap("10.244.0.5:40002")
	for range 6 {
		l.fromPage(datagramMsg(t, stream, pod, []byte("q")))
		l.fromPod(pod, stream, []byte("a"))
		*now = now.Add(time.Second)
	}
	*now = now.Add(-time.Second) // the last datagram was at 5 seconds
	*now = now.Add(100 * time.Second)
	l.sweep(*now)
	if len(l.flows) != 1 {
		t.Fatal("FL3: the flow is gone after 100 seconds")
	}
	*now = now.Add(20 * time.Second)
	l.sweep(*now)
	if len(l.flows) != 0 || len(l.ports) != 0 {
		t.Errorf("FL3: flows = %d, sockets = %d after 120 seconds, want 0", len(l.flows), len(l.ports))
	}

	// FL4: one query and one answer: gone after 30 seconds.
	dns := ap("10.244.0.5:40003")
	l.fromPage(datagramMsg(t, dns, pod, []byte("q")))
	*now = now.Add(10 * time.Millisecond)
	l.fromPod(pod, dns, []byte("a"))
	*now = now.Add(30 * time.Second)
	l.sweep(*now)
	if len(l.flows) != 0 {
		t.Error("FL4: the flow is not gone after 30 seconds")
	}

	// A flow from the pod side without answers: also 30 seconds.
	l.fromPod(pod, ap("10.244.0.5:40004"), []byte("q"))
	*now = now.Add(30 * time.Second)
	l.sweep(*now)
	if len(l.flows) != 0 {
		t.Error("a pod-side flow without answers is not gone after 30 seconds")
	}
}

// TestUDPLinkSharedSocketLastFlow: the socket of a tab address closes only
// after its last flow.
func TestUDPLinkSharedSocketLastFlow(t *testing.T) {
	o := useFakeOpener(t)
	now := setFlowClock(t)
	l, _ := newTestUDPLink(t)
	tab := ap("10.244.0.5:40000")
	l.fromPage(datagramMsg(t, tab, ap("10.244.1.7:53"), []byte("q")))
	*now = now.Add(20 * time.Second)
	l.fromPage(datagramMsg(t, tab, ap("10.244.1.8:53"), []byte("q")))
	*now = now.Add(10 * time.Second)
	l.sweep(*now)
	if len(l.flows) != 1 || o.ports()[0].isClosed() {
		t.Fatal("the socket closed with one flow left")
	}
	*now = now.Add(20 * time.Second)
	l.sweep(*now)
	if !o.ports()[0].isClosed() || len(l.ports) != 0 {
		t.Error("the socket is not closed after its last flow")
	}
}

// --- Channel rule 11: the limits ---

func TestUDPLinkFlowLimit(t *testing.T) {
	useFakeOpener(t)
	setFlowClock(t)
	l, _ := newTestUDPLink(t)
	tab := ap("10.244.0.5:40000")
	for i := range udpMaxFlows {
		pod := netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 244, 1, byte(1 + i/250)}), uint16(1000+i))
		l.fromPage(datagramMsg(t, tab, pod, []byte("q")))
	}
	if n := len(l.flows); n != udpMaxFlows {
		t.Fatalf("%d flows, want %d", n, udpMaxFlows)
	}
	l.fromPage(datagramMsg(t, tab, ap("10.244.1.200:9"), []byte("q")))
	if n := l.drops.get(udpDropFlows); n != 1 {
		t.Errorf("flow 4,097: flow drops = %d, want 1", n)
	}
	if n := len(l.flows); n != udpMaxFlows {
		t.Errorf("%d flows, want %d", n, udpMaxFlows)
	}
}

func TestUDPLinkPortLimit(t *testing.T) {
	o := useFakeOpener(t)
	setFlowClock(t)
	l, _ := newTestUDPLink(t)
	pod := ap("10.244.1.7:53")
	for i := range udpMaxPorts {
		l.fromPage(datagramMsg(t, netip.AddrPortFrom(netip.MustParseAddr("10.244.0.5"), uint16(1000+i)), pod, []byte("q")))
	}
	if n := len(o.ports()); n != udpMaxPorts {
		t.Fatalf("%d sockets, want %d", n, udpMaxPorts)
	}
	l.fromPod(pod, ap("10.244.0.6:1000"), []byte("a"))
	if n := l.drops.get(udpDropPorts); n != 1 {
		t.Errorf("socket 1,025: socket drops = %d, want 1", n)
	}
	if n := len(o.ports()); n != udpMaxPorts {
		t.Errorf("%d sockets, want %d", n, udpMaxPorts)
	}
}

// TestUDPLinkChannelClosed: the read side ends; the link writes a log line,
// stops sending and does not panic.
func TestUDPLinkChannelClosed(t *testing.T) {
	useFakeOpener(t)
	l := newUDPLink()
	ch := newFakeUDPChannel()
	pr, pw := io.Pipe()
	l.serve(context.Background(), ch, pr)
	pw.Close()
	deadline := time.Now().Add(5 * time.Second)
	for l.out.Load() != nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	l.fromPod(ap("10.244.1.7:53"), ap("10.0.0.1:5353"), []byte("x"))
	if n := l.drops.get(udpDropNoChannel); n != 1 {
		t.Errorf("no-channel drops = %d, want 1", n)
	}
}

// TestUDPLinkReadLoop: messages from the reader go to the pods; a message
// longer than the buffer is a counted drop and the loop goes on.
func TestUDPLinkReadLoop(t *testing.T) {
	o := useFakeOpener(t)
	l := newUDPLink()
	l.subnet = func() (*net.IPNet, error) {
		_, n, err := net.ParseCIDR(testPodNet.String())
		return n, err
	}
	msgs := []any{io.ErrShortBuffer, datagramMsg(t, ap("10.244.0.5:40000"), ap("10.244.1.7:53"), []byte("q"))}
	r := &scriptReader{items: msgs}
	l.readLoop(context.Background(), r)
	if n := l.drops.get(udpDropShortRead); n != 1 {
		t.Errorf("short-buffer drops = %d, want 1", n)
	}
	if p := o.ports(); len(p) != 1 || len(p[0].writes) != 1 {
		t.Errorf("the datagram did not reach the pod")
	}
}

type scriptReader struct{ items []any }

func (r *scriptReader) Read(b []byte) (int, error) {
	if len(r.items) == 0 {
		return 0, io.EOF
	}
	it := r.items[0]
	r.items = r.items[1:]
	if err, ok := it.(error); ok {
		return 0, err
	}
	return copy(b, it.([]byte)), nil
}
