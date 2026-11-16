package main

import (
	"bytes"
	"encoding/hex"
	"net"
	"net/netip"
	"sync"
	"syscall/js"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

// udpLinkRec records the datagrams that the channel gives to udpFromNode.
type udpLinkRec struct {
	mu  sync.Mutex
	got []udpChanMsg
}

func (r *udpLinkRec) list() []udpChanMsg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]udpChanMsg(nil), r.got...)
}

// udpLinkSetup gives a cluster of four nodes: the page "tab", the node
// workers tab-worker-1 and tab-worker-2 and the Docker node docker-1. It
// sets udpFromNode to a recorder and restores everything at the end.
func udpLinkSetup(t *testing.T) *udpLinkRec {
	t.Helper()
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, n := range []struct{ name, cidr, ip string }{
		{"tab", "10.244.0.0/24", "10.0.0.1"},
		{"tab-worker-1", "10.244.1.0/24", "10.0.0.2"},
		{"docker-1", "10.244.2.0/24", "10.0.0.3"},
		{"tab-worker-2", "10.244.3.0/24", "10.0.0.4"},
	} {
		idx.Add(&v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: n.name},
			Spec:       v1.NodeSpec{PodCIDR: n.cidr, PodCIDRs: []string{n.cidr}},
			Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: n.ip}}},
		})
	}
	oldLister, oldName, oldFrom := udpLinkLister, tabNodeName, udpFromNode
	udpLinkLister = func() corelisters.NodeLister { return corelisters.NewNodeLister(idx) }
	udpLinkTable.mu.Lock()
	udpLinkTable.built = time.Time{}
	udpLinkTable.mu.Unlock()
	tabNodeName = "tab"
	rec := &udpLinkRec{}
	udpFromNode = func(src, dst netip.AddrPort, b []byte) {
		rec.mu.Lock()
		rec.got = append(rec.got, udpChanMsg{typ: udpChanTypeData, src: src, dst: dst, data: b})
		rec.mu.Unlock()
	}
	nodesMu.Lock()
	for _, n := range []struct {
		name   string
		worker bool
	}{{"tab-worker-1", true}, {"tab-worker-2", true}, {"docker-1", false}} {
		nodes[n.name] = &joinedNode{name: n.name, ln: udpLinkNoListener{}, worker: n.worker}
	}
	nodesMu.Unlock()
	t.Cleanup(func() {
		udpChanMu.Lock()
		chans, page := udpChans, udpChanPage
		udpChans, udpChanPage = map[string]*udpChan{}, nil
		udpChanMu.Unlock()
		for _, c := range chans {
			c.close()
		}
		if page != nil {
			page.close()
		}
		nodesMu.Lock()
		delete(nodes, "tab-worker-1")
		delete(nodes, "tab-worker-2")
		delete(nodes, "docker-1")
		nodesMu.Unlock()
		udpLinkLister, tabNodeName, udpFromNode = oldLister, oldName, oldFrom
		udpLinkTable.mu.Lock()
		udpLinkTable.built = time.Time{}
		udpLinkTable.mu.Unlock()
	})
	return rec
}

type udpLinkNoListener struct{ net.Listener }

func (udpLinkNoListener) Close() error { return nil }

// udpLinkFakeDC is a fake channel: a JavaScript object with the fields of
// an RTCDataChannel. send keeps a copy of each message in the array sent.
func udpLinkFakeDC(props map[string]any) js.Value {
	dc := js.Global().Get("Object").New()
	dc.Set("label", udpLinkLabel)
	dc.Set("readyState", "open")
	dc.Set("bufferedAmount", 0)
	dc.Set("sent", js.Global().Get("Array").New())
	dc.Set("send", js.Global().Get("Function").New("d", "this.sent.push(Uint8Array.from(d))"))
	dc.Set("close", js.Global().Get("Function").New("this.readyState = 'closed'"))
	for k, v := range props {
		dc.Set(k, v)
	}
	return dc
}

func udpLinkDockerDC() js.Value {
	return udpLinkFakeDC(map[string]any{"ordered": false, "maxRetransmits": 0})
}

// udpLinkSent gives the messages that the fake channel got.
func udpLinkSent(dc js.Value) [][]byte {
	arr := dc.Get("sent")
	out := make([][]byte, arr.Length())
	for i := range out {
		a := arr.Index(i)
		out[i] = make([]byte, a.Length())
		js.CopyBytesToGo(out[i], a)
	}
	return out
}

// udpLinkDeliver gives m to the onmessage callback of dc, as JavaScript does.
func udpLinkDeliver(dc js.Value, m []byte) {
	arr := js.Global().Get("Uint8Array").New(len(m))
	js.CopyBytesToJS(arr, m)
	ev := js.Global().Get("Object").New()
	ev.Set("data", arr.Get("buffer"))
	dc.Get("onmessage").Invoke(ev)
}

// udpLinkWait waits at most 2 seconds for cond.
func udpLinkWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timeout: %s", what)
}

// udpLinkSettle lets the goroutines of the channels take their queues.
func udpLinkSettle() { time.Sleep(20 * time.Millisecond) }

func udpLinkAP(s string) netip.AddrPort { return netip.MustParseAddrPort(s) }

func udpLinkChan(node string) *udpChan {
	udpChanMu.Lock()
	defer udpChanMu.Unlock()
	return udpChans[node]
}

func (c *udpChan) sentCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent
}

// ND4, ND5: the test vectors of section 8.9.1 and the examination of
// channel rule 3.
func TestUDPChanHeader(t *testing.T) {
	vec, _ := hex.DecodeString("01010af4010514e90af400070035" + "6869")
	m := udpChanEncode(udpLinkAP("10.244.1.5:5353"), udpLinkAP("10.244.0.7:53"), []byte("hi"))
	if !bytes.Equal(m, vec) {
		t.Errorf("encode = %x, want %x", m, vec)
	}
	msg, reason := udpChanParse(vec)
	if reason != "" || msg.typ != udpChanTypeData || msg.src != udpLinkAP("10.244.1.5:5353") ||
		msg.dst != udpLinkAP("10.244.0.7:53") || string(msg.data) != "hi" {
		t.Errorf("parse = %+v, %q", msg, reason)
	}
	cred, _ := hex.DecodeString("0102" + "0000fff1" + "0000000000000000")
	if c := udpChanEncodeCredit(65521); !bytes.Equal(c, cred) {
		t.Errorf("credit = %x, want %x", c, cred)
	}
	if msg, reason := udpChanParse(cred); reason != "" || msg.typ != udpChanTypeCredit || msg.credit != 65521 {
		t.Errorf("parse credit = %+v, %q", msg, reason)
	}
	// The receiver ignores bytes 6 to 13 of a credit.
	odd := append([]byte(nil), cred...)
	copy(odd[6:], []byte{1, 2, 3, 4, 5, 6, 7, 8})
	if msg, reason := udpChanParse(odd); reason != "" || msg.credit != 65521 {
		t.Errorf("parse credit with bytes 6 to 13 = %+v, %q", msg, reason)
	}

	big := udpChanEncode(udpLinkAP("10.244.1.5:1"), udpLinkAP("10.244.0.7:2"), make([]byte, 65507))
	if len(big) != 65521 {
		t.Fatalf("len = %d", len(big))
	}
	if _, reason := udpChanParse(big); reason != "" {
		t.Errorf("65,521 bytes: %q", reason)
	}
	with := func(b []byte, i int, v byte) []byte {
		b = append([]byte(nil), b...)
		b[i] = v
		return b
	}
	for _, c := range []struct {
		name   string
		m      []byte
		reason string
	}{
		{"65,522 bytes", append(append([]byte(nil), big...), 0), "size"},
		{"13 bytes", vec[:13], "short"},
		{"empty", nil, "short"},
		{"version 2", with(vec, 0, 2), "version"},
		{"type 3", with(vec, 1, 3), "type"},
		{"type 0", with(vec, 1, 0), "type"},
		{"source port 0", with(with(vec, 6, 0), 7, 0), "port"},
		{"destination port 0", with(with(vec, 12, 0), 13, 0), "port"},
		{"credit of 15 bytes", append(append([]byte(nil), cred...), 0), "credit"},
	} {
		if _, reason := udpChanParse(c.m); reason != c.reason {
			t.Errorf("%s: reason %q, want %q", c.name, reason, c.reason)
		}
	}
}

// ND3, channel rule 1: the page closes a udp channel of a Docker node with
// other settings, a channel without a node name and one of an unknown node.
// A node worker channel has no WebRTC settings and stays.
func TestUDPLinkAttach(t *testing.T) {
	udpLinkSetup(t)
	for _, c := range []struct {
		name  string
		from  string
		props map[string]any
	}{
		{"ordered", "docker-1", map[string]any{"ordered": true, "maxRetransmits": 0}},
		{"maxRetransmits 1", "docker-1", map[string]any{"ordered": false, "maxRetransmits": 1}},
		{"no maxRetransmits", "docker-1", map[string]any{"ordered": false, "maxRetransmits": nil}},
		{"no node name", "", map[string]any{"ordered": false, "maxRetransmits": 0}},
		{"unknown node", "docker-9", map[string]any{"ordered": false, "maxRetransmits": 0}},
	} {
		dc := udpLinkFakeDC(c.props)
		attachChannel(dc, c.from)
		if dc.Get("readyState").String() != "closed" || udpLinkChan(c.from) != nil {
			t.Errorf("%s: the channel is not closed", c.name)
		}
	}
	dc := udpLinkDockerDC()
	attachChannel(dc, "docker-1")
	if dc.Get("readyState").String() != "open" || udpLinkChan("docker-1") == nil {
		t.Error("a Docker channel with ordered false and maxRetransmits 0 is not attached")
	}
	w := udpLinkFakeDC(nil) // a MessagePort of portChannel: no ordered
	attachChannel(w, "tab-worker-1")
	if w.Get("readyState").String() != "open" || udpLinkChan("tab-worker-1") == nil || !udpLinkChan("tab-worker-1").worker {
		t.Error("the node worker channel is not attached")
	}
	// A second channel of the same node replaces the first one.
	dc2 := udpLinkDockerDC()
	attachChannel(dc2, "docker-1")
	if dc.Get("readyState").String() != "closed" || udpLinkChan("docker-1") == nil || !udpLinkChan("docker-1").dc.Equal(dc2) {
		t.Error("the second channel did not replace the first one")
	}
	// onclose forgets the channel.
	dc2.Get("onclose").Invoke()
	if udpLinkChan("docker-1") != nil {
		t.Error("the closed channel is still known")
	}
	// forgetNode removes the channel: a terminated worker sends no null.
	forgetNode("tab-worker-1")
	if udpLinkChan("tab-worker-1") != nil || w.Get("readyState").String() != "closed" {
		t.Error("forgetNode did not remove the channel")
	}
}

// ND7, ND8 and channel rule 4: the page examines the source and routes each
// datagram to the node of the destination.
func TestUDPLinkRoute(t *testing.T) {
	rec := udpLinkSetup(t)
	docker, w1 := udpLinkDockerDC(), udpLinkFakeDC(nil)
	attachChannel(docker, "docker-1")
	attachChannel(w1, "tab-worker-1")

	send := func(dc js.Value, src, dst string) []byte {
		m := udpChanEncode(udpLinkAP(src), udpLinkAP(dst), []byte("x"))
		udpLinkDeliver(dc, m)
		return m
	}
	// Sources that are not of docker-1: a pod of another node, another node IP.
	srcDrops := udpChanDropCount("source")
	send(docker, "10.244.1.5:5353", "10.244.0.7:53")
	send(docker, "10.0.0.2:5353", "10.244.0.7:53")
	udpLinkWait(t, "two source drops", func() bool { return udpChanDropCount("source") == srcDrops+2 })
	if got := rec.list(); len(got) != 0 {
		t.Fatalf("a datagram with a wrong source arrived: %+v", got)
	}
	// The correct sources: a pod of docker-1 and its node IP.
	send(docker, "10.244.2.5:5353", "10.244.0.7:53")
	send(docker, "10.0.0.3:5353", "10.0.0.1:53") // to the node IP of the page
	udpLinkWait(t, "two datagrams for the page", func() bool { return len(rec.list()) == 2 })
	got := rec.list()
	if got[0].src != udpLinkAP("10.244.2.5:5353") || got[0].dst != udpLinkAP("10.244.0.7:53") || string(got[0].data) != "x" ||
		got[1].src != udpLinkAP("10.0.0.3:5353") || got[1].dst != udpLinkAP("10.0.0.1:53") {
		t.Errorf("udpFromNode got %+v", got)
	}
	// To another node: the message goes there with no change, to its pod
	// CIDR and to its node IP.
	m1 := send(docker, "10.244.2.5:5353", "10.244.1.9:53")
	m2 := send(docker, "10.244.2.5:5353", "10.0.0.2:53")
	m3 := send(w1, "10.244.1.9:53", "10.244.2.5:5353")
	udpLinkWait(t, "forwarded", func() bool { return len(udpLinkSent(w1)) == 2 && len(udpLinkSent(docker)) == 1 })
	if s := udpLinkSent(w1); !bytes.Equal(s[0], m1) || !bytes.Equal(s[1], m2) {
		t.Errorf("tab-worker-1 got %x", s)
	}
	if s := udpLinkSent(docker); !bytes.Equal(s[0], m3) {
		t.Errorf("docker-1 got %x", s)
	}
	// Drops: the same node, a node without a channel, an IP of no node.
	dstDrops, noChan := udpChanDropCount("destination"), udpChanDropCount("no channel")
	send(docker, "10.244.2.5:5353", "10.244.2.6:53")
	send(docker, "10.244.2.5:5353", "192.168.9.9:53")
	send(docker, "10.244.2.5:5353", "10.244.3.1:53")
	udpLinkWait(t, "three drops", func() bool {
		return udpChanDropCount("destination") == dstDrops+2 && udpChanDropCount("no channel") == noChan+1
	})
	// A credit from a Docker node is dropped.
	credits := udpChanDropCount("credit")
	udpLinkDeliver(docker, udpChanEncodeCredit(100))
	udpLinkWait(t, "credit drop", func() bool { return udpChanDropCount("credit") == credits+1 })
	udpLinkSettle()
	if len(rec.list()) != 2 || len(udpLinkSent(w1)) != 2 || len(udpLinkSent(docker)) != 1 {
		t.Error("a dropped datagram arrived")
	}
}

// Channel rule 5 on the page: udpToNode sends to the channel of the node of
// the destination, else it drops and gives false.
func TestUDPLinkToNodePage(t *testing.T) {
	udpLinkSetup(t)
	docker := udpLinkDockerDC()
	attachChannel(docker, "docker-1")
	src := udpLinkAP("10.244.0.7:53")
	if !udpToNode(src, udpLinkAP("10.244.2.5:5353"), []byte("hi")) || !udpToNode(src, udpLinkAP("10.0.0.3:53"), nil) {
		t.Fatal("udpToNode dropped a datagram to docker-1")
	}
	s := udpLinkSent(docker)
	if len(s) != 2 || !bytes.Equal(s[0], udpChanEncode(src, udpLinkAP("10.244.2.5:5353"), []byte("hi"))) || len(s[1]) != 14 {
		t.Errorf("docker-1 got %x", s)
	}
	for _, dst := range []string{"10.244.0.9:53", "10.0.0.1:53", "10.244.3.1:53", "192.168.9.9:53", "[::ffff:10.244.2.5]:53", "[fd00::1]:53"} {
		if udpToNode(src, udpLinkAP(dst), []byte("x")) {
			t.Errorf("udpToNode to %s did not drop", dst)
		}
	}
	if udpToNode(src, udpLinkAP("10.244.2.5:5353"), make([]byte, 65508)) {
		t.Error("udpToNode sent a datagram of 65,508 bytes")
	}
	if len(udpLinkSent(docker)) != 2 {
		t.Error("a dropped datagram arrived")
	}
}

// ND11: the page sends at most 1 MiB to a node worker without a credit. A
// credit opens the channel again, and a new channel starts at 0.
func TestUDPLinkCredits(t *testing.T) {
	udpLinkSetup(t)
	w1 := udpLinkFakeDC(nil)
	attachChannel(w1, "tab-worker-1")
	src, dst := udpLinkAP("10.244.0.7:53"), udpLinkAP("10.244.1.5:5353")
	b := make([]byte, 65507) // a message of 65,521 bytes
	sendMiB := func() (n int) {
		for range 32 { // 2 MiB
			if udpToNode(src, dst, b) {
				n++
			}
		}
		return n
	}
	if n := sendMiB(); n != 16 { // 16 * 65,521 = 1,048,336 <= 1 MiB
		t.Fatalf("sent %d messages without a credit, want 16", n)
	}
	total := 0
	for _, m := range udpLinkSent(w1) {
		total += len(m)
	}
	if total > 1<<20 {
		t.Errorf("the worker got %d bytes without a credit", total)
	}
	c := udpLinkChan("tab-worker-1")
	udpLinkDeliver(w1, udpChanEncodeCredit(2*65521))
	udpLinkWait(t, "credit", func() bool { return c.sentCount() == 14*65521 })
	if n := sendMiB(); n != 2 {
		t.Errorf("sent %d messages after a credit of two, want 2", n)
	}
	// A credit larger than the count sets it to 0.
	udpLinkDeliver(w1, udpChanEncodeCredit(1<<31))
	udpLinkWait(t, "large credit", func() bool { return c.sentCount() == 0 })
	// A new channel starts at 0, also when the old count was full.
	sendMiB()
	w2 := udpLinkFakeDC(nil)
	attachChannel(w2, "tab-worker-1")
	if n := sendMiB(); n != 16 || len(udpLinkSent(w2)) != 16 {
		t.Errorf("the new channel sent %d messages, want 16", n)
	}
}

// ND20, ND2: the node worker side. Datagrams from the page go to
// udpFromNode, and the worker sends credits for each message it took, also
// for dropped ones: before 65,536 bytes and at most 50 ms later.
func TestUDPLinkWorker(t *testing.T) {
	rec := udpLinkSetup(t)
	tabNodeName = "tab-worker-1"
	page := udpLinkFakeDC(nil)
	udpLinkStartWorker(page)

	credits := func() (sum int, list []uint32) {
		for _, m := range udpLinkSent(page) {
			msg, reason := udpChanParse(m)
			if reason != "" || msg.typ != udpChanTypeCredit {
				continue
			}
			sum += int(msg.credit)
			list = append(list, msg.credit)
		}
		return sum, list
	}
	want := 0
	for i := range 3 {
		m := udpChanEncode(udpLinkAP("10.244.0.7:53"), udpLinkAP("10.244.1.5:5353"), make([]byte, 100+i))
		udpLinkDeliver(page, m)
		want += len(m)
	}
	bad := make([]byte, 20) // version 0: a counted drop, but credited
	udpLinkDeliver(page, bad)
	want += len(bad)
	udpLinkWait(t, "credit for small messages", func() bool { s, _ := credits(); return s == want })
	if got := rec.list(); len(got) != 3 || len(got[2].data) != 102 {
		t.Errorf("udpFromNode got %d datagrams", len(got))
	}
	// Large messages: a credit before 65,536 bytes without a credit.
	for range 4 {
		m := udpChanEncode(udpLinkAP("10.244.0.7:53"), udpLinkAP("10.244.1.5:5353"), make([]byte, 65507))
		udpLinkDeliver(page, m)
		want += len(m)
	}
	udpLinkWait(t, "credit for large messages", func() bool { s, _ := credits(); return s == want })
	_, list := credits()
	for _, c := range list {
		if c >= udpChanCreditEvery {
			t.Errorf("a credit of %d bytes", c)
		}
	}
	// A credit to a node worker is a counted drop. The worker counts it for
	// its credit as each message that it took.
	n := udpChanDropCount("credit")
	udpLinkDeliver(page, udpChanEncodeCredit(5))
	want += 14
	udpLinkWait(t, "credit drop", func() bool { s, _ := credits(); return udpChanDropCount("credit") == n+1 && s == want })

	// udpToNode in a node worker: to another node through the page.
	before := len(udpLinkSent(page))
	src := udpLinkAP("10.244.1.5:5353")
	if !udpToNode(src, udpLinkAP("10.244.2.5:53"), []byte("a")) || !udpToNode(src, udpLinkAP("10.244.0.7:53"), []byte("b")) {
		t.Fatal("udpToNode in a worker dropped a datagram to another node")
	}
	if udpToNode(src, udpLinkAP("10.244.1.6:53"), []byte("c")) || udpToNode(src, udpLinkAP("192.168.9.9:53"), []byte("d")) {
		t.Error("udpToNode in a worker sent to its own node or to no node")
	}
	if s := udpLinkSent(page); len(s) != before+2 || !bytes.Equal(s[before], udpChanEncode(src, udpLinkAP("10.244.2.5:53"), []byte("a"))) {
		t.Errorf("the page got %x", s[before:])
	}
}

// ND12: the page does not send on a data channel with a bufferedAmount of
// more than 1 MiB.
func TestUDPLinkBuffered(t *testing.T) {
	udpLinkSetup(t)
	docker := udpLinkDockerDC()
	attachChannel(docker, "docker-1")
	docker.Set("bufferedAmount", 2<<20)
	n := udpChanDropCount("buffered")
	if udpToNode(udpLinkAP("10.244.0.7:53"), udpLinkAP("10.244.2.5:53"), []byte("x")) {
		t.Error("udpToNode sent with a bufferedAmount of 2 MiB")
	}
	if len(udpLinkSent(docker)) != 0 || udpChanDropCount("buffered") != n+1 {
		t.Error("no counted drop")
	}
	docker.Set("bufferedAmount", 1<<20)
	if !udpToNode(udpLinkAP("10.244.0.7:53"), udpLinkAP("10.244.2.5:53"), []byte("x")) {
		t.Error("udpToNode dropped with a bufferedAmount of 1 MiB")
	}
}

// A send that throws and a channel that is not open: counted drops, and the
// process continues.
func TestUDPLinkSendFails(t *testing.T) {
	udpLinkSetup(t)
	docker := udpLinkDockerDC()
	attachChannel(docker, "docker-1")
	docker.Set("send", js.Global().Get("Function").New("throw new Error('channel closed')"))
	n := udpChanDropCount("send")
	if udpToNode(udpLinkAP("10.244.0.7:53"), udpLinkAP("10.244.2.5:53"), []byte("x")) {
		t.Error("udpToNode reported a send that threw")
	}
	if udpChanDropCount("send") != n+1 {
		t.Error("the send that threw is not a counted drop")
	}

	w1 := udpLinkFakeDC(map[string]any{"readyState": "connecting"})
	attachChannel(w1, "tab-worker-1")
	n = udpChanDropCount("state")
	if udpToNode(udpLinkAP("10.244.0.7:53"), udpLinkAP("10.244.1.5:53"), []byte("x")) {
		t.Error("udpToNode sent on a connecting channel")
	}
	if udpChanDropCount("state") != n+1 || len(udpLinkSent(w1)) != 0 {
		t.Error("the send on a connecting channel is not a counted drop")
	}
	// The dropped message is not in the credit count.
	if c := udpLinkChan("tab-worker-1"); c.sentCount() != 0 {
		t.Errorf("credit count %d after a drop", c.sentCount())
	}
}
