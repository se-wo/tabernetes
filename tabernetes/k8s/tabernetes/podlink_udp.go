package main

// The udp channel (docs/wasi-sockets.md, sections 8.9 and 8.9.1). A node
// opens one channel with the label "udp" to the page: a node worker with
// tabernetesOpen("udp"), the node agent of a Docker node with a WebRTC data
// channel (ordered: false, maxRetransmits: 0). Each message is a header of
// 14 bytes and, for a datagram, the datagram. The page is the center: it
// forwards each datagram to the node of the destination, or gives it to its
// own UDP table through udpFromNode. The page does this work for the
// control plane, not for a pod, so no pod pays for it.
//
// dcConn (tunnel.go) is a stream of bytes and waits for bufferedAmount, so
// it is not usable here. udpChan keeps the messages apart and never waits:
// a message that it cannot send at once is a counted drop.

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"syscall/js"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	corelisters "k8s.io/client-go/listers/core/v1"
)

const (
	udpLinkLabel = "udp"

	udpChanHeaderLen   = 14
	udpChanMaxMsg      = udpChanHeaderLen + 65507 // 65,521 bytes (ND5)
	udpChanVersion     = 1
	udpChanTypeData    = 1
	udpChanTypeCredit  = 2
	udpChanQueueMax    = 1 << 20 // channel rule 10
	udpChanCreditMax   = 1 << 20 // channel rule 8 (ND11)
	udpChanBufferedMax = 1 << 20 // channel rule 9 (ND12)
	udpChanCreditEvery = 65536   // channel rule 8: a credit before this many bytes
)

// udpChanCreditDelay is the longest time that a node worker keeps the bytes
// of a message without a credit (channel rule 8). A variable for tests.
var udpChanCreditDelay = 50 * time.Millisecond

// udpChanMsg is one parsed message of the channel.
type udpChanMsg struct {
	typ      byte
	src, dst netip.AddrPort // type 1
	data     []byte         // type 1: the datagram
	credit   uint32         // type 2: the number of bytes
}

// udpChanEncode makes a datagram message: the header and b.
func udpChanEncode(src, dst netip.AddrPort, b []byte) []byte {
	m := make([]byte, udpChanHeaderLen+len(b))
	m[0], m[1] = udpChanVersion, udpChanTypeData
	s, d := src.Addr().As4(), dst.Addr().As4()
	copy(m[2:6], s[:])
	binary.BigEndian.PutUint16(m[6:8], src.Port())
	copy(m[8:12], d[:])
	binary.BigEndian.PutUint16(m[12:14], dst.Port())
	copy(m[udpChanHeaderLen:], b)
	return m
}

// udpChanEncodeCredit makes a credit message for n bytes. Bytes 6 to 13
// are 0.
func udpChanEncodeCredit(n uint32) []byte {
	m := make([]byte, udpChanHeaderLen)
	m[0], m[1] = udpChanVersion, udpChanTypeCredit
	binary.BigEndian.PutUint32(m[2:6], n)
	return m
}

// udpChanParse does the examination of channel rule 3, except the sender of
// a credit, which only the caller knows. It gives the reason of a drop, or
// "". The datagram of the result shares the memory of m.
func udpChanParse(m []byte) (udpChanMsg, string) {
	if len(m) < udpChanHeaderLen {
		return udpChanMsg{}, "short"
	}
	if m[0] != udpChanVersion {
		return udpChanMsg{}, "version"
	}
	switch m[1] {
	case udpChanTypeData:
		if len(m) > udpChanMaxMsg {
			return udpChanMsg{}, "size"
		}
		src := netip.AddrPortFrom(netip.AddrFrom4([4]byte(m[2:6])), binary.BigEndian.Uint16(m[6:8]))
		dst := netip.AddrPortFrom(netip.AddrFrom4([4]byte(m[8:12])), binary.BigEndian.Uint16(m[12:14]))
		if src.Port() == 0 || dst.Port() == 0 {
			return udpChanMsg{}, "port"
		}
		return udpChanMsg{typ: udpChanTypeData, src: src, dst: dst, data: m[udpChanHeaderLen:]}, ""
	case udpChanTypeCredit:
		if len(m) != udpChanHeaderLen {
			return udpChanMsg{}, "credit"
		}
		return udpChanMsg{typ: udpChanTypeCredit, credit: binary.BigEndian.Uint32(m[2:6])}, ""
	default:
		return udpChanMsg{}, "type"
	}
}

// Drops: a count for each reason, and a log line at most each 10 seconds
// (channel rule 3). The line has the drops since the last line, for
// example "udp channel: dropped 3 messages (credit=2 source=1)".
var udpChanDrops struct {
	mu     sync.Mutex
	total  map[string]int // since the start, for tests
	recent map[string]int // since the last log line
	last   time.Time
}

const udpChanDropLogEvery = 10 * time.Second

func udpChanDrop(reason string) {
	d := &udpChanDrops
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.total == nil {
		d.total, d.recent = map[string]int{}, map[string]int{}
	}
	d.total[reason]++
	d.recent[reason]++
	now := time.Now()
	if now.Sub(d.last) < udpChanDropLogEvery {
		return
	}
	d.last = now
	keys := make([]string, 0, len(d.recent))
	n := 0
	for k, v := range d.recent {
		keys = append(keys, k)
		n += v
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%d", k, d.recent[k])
	}
	fmt.Fprintf(os.Stderr, "udp channel: dropped %d messages (%s)\n", n, strings.Join(parts, " "))
	clear(d.recent)
}

// udpChanDropCount is the number of drops for reason since the start.
func udpChanDropCount(reason string) int {
	udpChanDrops.mu.Lock()
	defer udpChanDrops.mu.Unlock()
	return udpChanDrops.total[reason]
}

// udpChan is one udp channel: on the page the channel of a node, in a node
// worker the channel to the page.
type udpChan struct {
	dc     js.Value
	node   string // the node at the other end ("" for the page)
	worker bool   // on the page: the channel of a node worker (credits)
	toPage bool   // in a node worker: the channel to the page

	mu     sync.Mutex
	q      [][]byte // channel rule 10
	qBytes int
	lost   int  // in a node worker: bytes that the callback dropped, for the credit
	sent   int  // on the page, worker channels: bytes without a credit
	closed bool // closed or replaced
	wake   chan struct{}
	funcs  []js.Func
}

func newUDPChan(dc js.Value, node string, worker, toPage bool) *udpChan {
	c := &udpChan{dc: dc, node: node, worker: worker, toPage: toPage, wake: make(chan struct{}, 1)}
	dc.Set("binaryType", "arraybuffer")
	onMsg := js.FuncOf(func(_ js.Value, a []js.Value) any {
		c.onMessage(a[0].Get("data"))
		return nil
	})
	onClose := js.FuncOf(func(js.Value, []js.Value) any {
		c.shut()
		return nil
	})
	c.funcs = []js.Func{onMsg, onClose}
	dc.Set("onmessage", onMsg)
	dc.Set("onclose", onClose)
	go c.run()
	return c
}

// onMessage runs in the callback of JavaScript: it copies the message into
// the queue and returns (channel rule 10).
func (c *udpChan) onMessage(data js.Value) {
	if data.Type() != js.TypeObject {
		c.dropIn(0, "short") // a string or nothing: not a message of rule 2
		return
	}
	arr := js.Global().Get(uint8ArrayCtr).New(data)
	n := arr.Get("length").Int()
	if n > udpChanMaxMsg {
		c.dropIn(n, "size")
		return
	}
	c.mu.Lock()
	if c.closed || c.qBytes+n > udpChanQueueMax {
		c.mu.Unlock()
		c.dropIn(n, "queue")
		return
	}
	c.mu.Unlock()
	b := make([]byte, n)
	js.CopyBytesToGo(b, arr)
	c.mu.Lock()
	c.q = append(c.q, b)
	c.qBytes += n
	c.mu.Unlock()
	c.poke()
}

// dropIn drops a message in the callback. A node worker still counts its
// bytes for the credit, else the count of the page never goes down.
func (c *udpChan) dropIn(n int, reason string) {
	udpChanDrop(reason)
	if c.toPage && n > 0 {
		c.mu.Lock()
		c.lost += n
		c.mu.Unlock()
		c.poke()
	}
}

func (c *udpChan) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// shut marks the channel closed and forgets it. It does not close the
// channel of JavaScript; close does.
func (c *udpChan) shut() {
	c.mu.Lock()
	c.closed = true
	c.q, c.qBytes = nil, 0
	c.mu.Unlock()
	udpChanMu.Lock()
	if udpChans[c.node] == c {
		delete(udpChans, c.node)
	}
	if udpChanPage == c {
		udpChanPage = nil
	}
	udpChanMu.Unlock()
	c.poke()
}

// close closes the channel of JavaScript and forgets it.
func (c *udpChan) close() {
	c.shut()
	udpChanCall(c.dc, "close")
}

// run is the goroutine of the channel: it takes the messages from the queue
// and does the work. A node worker sends the credits here (channel rule 8).
func (c *udpChan) run() {
	var (
		pending int // node worker: bytes without a credit
		timer   *time.Timer
		due     <-chan time.Time
	)
	credit := func() {
		if pending > 0 {
			c.send(udpChanEncodeCredit(uint32(pending)))
			pending = 0
		}
		if timer != nil {
			timer.Stop()
			timer, due = nil, nil
		}
	}
	count := func(n int) {
		if !c.toPage || n == 0 {
			return
		}
		if pending+n >= udpChanCreditEvery {
			credit()
		}
		if pending == 0 {
			timer = time.NewTimer(udpChanCreditDelay)
			due = timer.C
		}
		pending += n
	}
	for {
		c.mu.Lock()
		var m []byte
		if len(c.q) > 0 {
			m = c.q[0]
			c.q[0] = nil
			c.q = c.q[1:]
			c.qBytes -= len(m)
		}
		lost := c.lost
		c.lost = 0
		closed := c.closed
		c.mu.Unlock()
		count(lost)
		if m != nil {
			count(len(m))
			c.handle(m)
			continue
		}
		if closed {
			if timer != nil {
				timer.Stop()
			}
			c.release()
			return
		}
		select {
		case <-c.wake:
		case <-due:
			timer, due = nil, nil
			credit()
		}
	}
}

// release removes the callbacks after the channel closed.
func (c *udpChan) release() {
	func() {
		defer func() { _ = recover() }()
		c.dc.Set("onmessage", js.Null())
		c.dc.Set("onclose", js.Null())
	}()
	for _, f := range c.funcs {
		f.Release()
	}
}

// handle does the work for one message.
func (c *udpChan) handle(m []byte) {
	msg, reason := udpChanParse(m)
	if reason != "" {
		udpChanDrop(reason)
		return
	}
	if msg.typ == udpChanTypeCredit {
		if !c.worker { // only from a node worker to the page
			udpChanDrop("credit")
			return
		}
		c.mu.Lock()
		c.sent = max(c.sent-int(msg.credit), 0)
		c.mu.Unlock()
		return
	}
	if c.toPage { // in a node worker: the page routed it to this node
		udpChanDeliver(msg)
		return
	}
	// The page as the center (channel rule 4).
	if !udpLinkOwns(c.node, msg.src.Addr()) {
		udpChanDrop("source")
		return
	}
	switch node := udpLinkNodeOf(msg.dst.Addr()); {
	case node == "" || node == c.node:
		udpChanDrop("destination")
	case node == tabNodeName:
		udpChanDeliver(msg)
	default:
		udpChanSendTo(node, m)
	}
}

func udpChanDeliver(msg udpChanMsg) {
	if f := udpFromNode; f != nil {
		f(msg.src, msg.dst, msg.data)
		return
	}
	udpChanDrop("table")
}

// send sends one message and never waits. It reports false for a drop.
func (c *udpChan) send(m []byte) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		udpChanDrop("closed")
		return false
	}
	if c.worker {
		if c.sent+len(m) > udpChanCreditMax { // ND11
			c.mu.Unlock()
			udpChanDrop("credit")
			return false
		}
		c.sent += len(m)
	}
	c.mu.Unlock()
	ok, reason := udpChanSendJS(c.dc, m)
	if !ok {
		if c.worker {
			c.mu.Lock()
			c.sent = max(c.sent-len(m), 0)
			c.mu.Unlock()
		}
		udpChanDrop(reason)
	}
	return ok
}

// udpChanSendJS gives m to the send function of dc as one Uint8Array. The
// channel must be open, and bufferedAmount must not be more than 1 MiB (ND12;
// it is always 0 for a MessagePort). A send that throws, for example on a
// MessagePort that closed or on a full WebRTC buffer, is a drop: the panic of
// js.Value.Call must not stop the control plane.
func udpChanSendJS(dc js.Value, m []byte) (ok bool, reason string) {
	defer func() {
		if recover() != nil {
			ok, reason = false, "send"
		}
	}()
	if dc.Get("readyState").String() != "open" {
		return false, "state"
	}
	if b := dc.Get("bufferedAmount"); b.Type() == js.TypeNumber && b.Float() > udpChanBufferedMax {
		return false, "buffered"
	}
	arr := js.Global().Get(uint8ArrayCtr).New(len(m))
	js.CopyBytesToJS(arr, m)
	dc.Call("send", arr)
	return true, ""
}

// udpChanCall calls a method of dc and ignores an exception.
func udpChanCall(dc js.Value, method string) {
	defer func() { _ = recover() }()
	dc.Call(method)
}

var (
	udpChanMu   sync.Mutex
	udpChans    = map[string]*udpChan{} // the page: the channel of each node
	udpChanPage *udpChan                // a node worker: the channel to the page
)

// udpChanSendTo sends a message to the channel of node.
func udpChanSendTo(node string, m []byte) bool {
	udpChanMu.Lock()
	c := udpChans[node]
	udpChanMu.Unlock()
	if c == nil {
		udpChanDrop("no channel")
		return false
	}
	return c.send(m)
}

// udpLinkAttach takes a udp channel on the page (channel rule 1). from is
// the node that opened it.
func udpLinkAttach(dc js.Value, from string) {
	if from == "" {
		fmt.Fprintln(os.Stderr, "udp channel: no sending node: closed")
		udpChanCall(dc, "close")
		return
	}
	nodesMu.Lock()
	jn := nodes[from]
	nodesMu.Unlock()
	if jn == nil {
		fmt.Fprintf(os.Stderr, "udp channel of %s: unknown node: closed\n", from)
		udpChanCall(dc, "close")
		return
	}
	if !jn.worker {
		// A Docker node: a data channel without order and retransmits (ND3).
		ordered, retr := dc.Get("ordered"), dc.Get("maxRetransmits")
		if ordered.Type() != js.TypeBoolean || ordered.Bool() || retr.Type() != js.TypeNumber || retr.Int() != 0 {
			fmt.Fprintf(os.Stderr, "udp channel of %s: ordered=%v maxRetransmits=%v, want false and 0: closed\n",
				from, js.Global().Call("String", ordered).String(), js.Global().Call("String", retr).String())
			udpChanCall(dc, "close")
			return
		}
	}
	c := newUDPChan(dc, from, jn.worker, false)
	udpChanMu.Lock()
	old := udpChans[from]
	udpChans[from] = c
	udpChanMu.Unlock()
	if old != nil {
		old.close() // the new channel replaces it; its credit count goes too
	}
}

// udpLinkForget closes the udp channel of a node that is gone (forgetNode).
func udpLinkForget(name string) {
	udpChanMu.Lock()
	c := udpChans[name]
	delete(udpChans, name)
	udpChanMu.Unlock()
	if c != nil {
		c.close()
	}
}

// udpLinkOpenWorker opens the udp channel of a node worker to the page,
// after its tab node started (ND2).
func udpLinkOpenWorker() {
	open := js.Global().Get("tabernetesOpen")
	if !open.Truthy() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dc, err := awaitPromise(ctx, open.Invoke(udpLinkLabel))
	if err != nil {
		fmt.Fprintln(os.Stderr, "udp channel: open:", err)
		return
	}
	udpLinkStartWorker(dc)
}

// udpLinkStartWorker makes dc the channel of this node worker to the page.
func udpLinkStartWorker(dc js.Value) *udpChan {
	c := newUDPChan(dc, "", false, true)
	udpChanMu.Lock()
	old := udpChanPage
	udpChanPage = c
	udpChanMu.Unlock()
	if old != nil {
		old.close()
	}
	return c
}

func init() { udpToNode = udpLinkToNode }

// udpLinkToNode is udpToNode: a datagram of the UDP table of this tab node
// to another node (channel rule 5). The page sends it to the channel of the
// destination node, a node worker to the page.
func udpLinkToNode(src, dst netip.AddrPort, b []byte) bool {
	if !src.Addr().Is4() || !dst.Addr().Is4() {
		udpChanDrop("address")
		return false
	}
	if src.Port() == 0 || dst.Port() == 0 || udpChanHeaderLen+len(b) > udpChanMaxMsg {
		udpChanDrop("size")
		return false
	}
	node := udpLinkNodeOf(dst.Addr())
	if node == "" || node == tabNodeName {
		udpChanDrop("destination")
		return false
	}
	m := udpChanEncode(src, dst, b)
	udpChanMu.Lock()
	page := udpChanPage
	udpChanMu.Unlock()
	if page != nil {
		return page.send(m)
	}
	return udpChanSendTo(node, m)
}

// The node of an IP (channel rules 4 and 5): a parsed table of the pod CIDRs
// and node IPs, built again at most one time each second.
type udpLinkNode struct {
	name  string
	ips   []netip.Addr
	cidrs []netip.Prefix
}

var udpLinkTable struct {
	mu    sync.Mutex
	built time.Time
	nodes []udpLinkNode
}

// udpLinkLister gives the nodes of the cluster. A variable for tests.
var udpLinkLister = func() corelisters.NodeLister {
	if n := theTabNode; n != nil && n.policy != nil {
		return n.policy.nodes
	}
	return nil
}

func udpLinkNodes() []udpLinkNode {
	t := &udpLinkTable
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.built.IsZero() && time.Since(t.built) < time.Second {
		return t.nodes
	}
	l := udpLinkLister()
	if l == nil {
		return nil
	}
	list, err := l.List(labels.Everything())
	if err != nil {
		return t.nodes
	}
	nodes := make([]udpLinkNode, 0, len(list))
	for _, nd := range list {
		e := udpLinkNode{name: nd.Name}
		for _, a := range nd.Status.Addresses {
			if ip, err := netip.ParseAddr(a.Address); a.Type == v1.NodeInternalIP && err == nil {
				e.ips = append(e.ips, ip.Unmap())
			}
		}
		for _, c := range nodePodCIDRs(nd) {
			if p, err := netip.ParsePrefix(c); err == nil {
				e.cidrs = append(e.cidrs, p.Masked())
			}
		}
		nodes = append(nodes, e)
	}
	t.nodes, t.built = nodes, time.Now()
	return nodes
}

// udpLinkNodeOf is the node whose pod CIDR contains ip, else the node whose
// InternalIP is ip, else "".
func udpLinkNodeOf(ip netip.Addr) string {
	ip = ip.Unmap()
	nodes := udpLinkNodes()
	for _, n := range nodes {
		for _, c := range n.cidrs {
			if c.Contains(ip) {
				return n.name
			}
		}
	}
	for _, n := range nodes {
		for _, a := range n.ips {
			if a == ip {
				return n.name
			}
		}
	}
	return ""
}

// udpLinkOwns reports whether ip is a node IP or in a pod CIDR of node
// (ND7, ND8). It is nodeOwns of podlink.go on the parsed table.
func udpLinkOwns(node string, ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, n := range udpLinkNodes() {
		if n.name != node {
			continue
		}
		for _, a := range n.ips {
			if a == ip {
				return true
			}
		}
		for _, c := range n.cidrs {
			if c.Contains(ip) {
				return true
			}
		}
	}
	return false
}
