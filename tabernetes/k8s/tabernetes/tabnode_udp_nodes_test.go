package main

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// UDP between nodes on the side of the UDP table (docs/wasi-sockets.md,
// sections 8.7 to 8.9.1, package W5.1b): step 4 gives datagrams for other
// nodes to udpToNode, and udpNodeReceive (udpFromNode) gives datagrams of
// other nodes to the table. The pod CIDR of flowNode is 10.244.88.0/24, so
// 10.244.1.0/24 is on another node.

// udpNodeSent is one call of udpToNode.
type udpNodeSent struct {
	src, dst netip.AddrPort
	data     string
}

// udpNodeRecorder records the calls of udpToNode for one test.
type udpNodeRecorder struct {
	mu   sync.Mutex
	sent []udpNodeSent
}

func (r *udpNodeRecorder) calls() []udpNodeSent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]udpNodeSent(nil), r.sent...)
}

// udpNodeRecord sets udpToNode to a recorder for one test.
func udpNodeRecord(t *testing.T) *udpNodeRecorder {
	r := &udpNodeRecorder{}
	saved := udpToNode
	udpToNode = func(src, dst netip.AddrPort, b []byte) bool {
		r.mu.Lock()
		r.sent = append(r.sent, udpNodeSent{src, dst, string(b)})
		r.mu.Unlock()
		return true
	}
	t.Cleanup(func() { udpToNode = saved })
	return r
}

// udpNodeFrom gives a datagram from another node to the table, through the
// hook that the table set.
func udpNodeFrom(t *testing.T, src, dst string, b string) {
	t.Helper()
	if udpFromNode == nil {
		t.Fatal("udpFromNode is nil")
	}
	udpFromNode(netip.MustParseAddrPort(src), netip.MustParseAddrPort(dst), []byte(b))
}

// udpNodeWrite sends q from c, or fails the test.
func udpNodeWrite(t *testing.T, c *udpConn, q string) {
	t.Helper()
	if _, err := c.Write([]byte(q)); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// udpNodeTerminal is a context outside a pod (the terminal): its sockets
// have the node IP. It ends with the test.
func udpNodeTerminal(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

// Channel rule 5, ND2: a datagram to a pod IP of another node goes to
// udpToNode with the socket as its source. The terminal sends from the node
// IP. A connected socket gets no error (rule 7): its next send works.
func TestUDPNodeSendPodIP(t *testing.T) {
	flowNode(t, nil)
	rec := udpNodeRecord(t)
	cip := nextLoopbackIP()
	c := udpDial(t, podCtx(t, cip), "10.244.1.5:7000")
	udpNodeWrite(t, c, "hi")
	udpNodeWrite(t, c, "again")
	term := udpListen(t, udpNodeTerminal(t), ":0")
	if _, err := term.WriteTo([]byte("host"), udpTo("10.244.1.5:7000")); err != nil {
		t.Fatal(err)
	}
	want := []udpNodeSent{
		{c.local, netip.MustParseAddrPort("10.244.1.5:7000"), "hi"},
		{c.local, netip.MustParseAddrPort("10.244.1.5:7000"), "again"},
		{netip.MustParseAddrPort(net.JoinHostPort(tabNodeIP, strconv.Itoa(udpPort(term)))), netip.MustParseAddrPort("10.244.1.5:7000"), "host"},
	}
	got := rec.calls()
	if len(got) != len(want) {
		t.Fatalf("udpToNode got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: %v, want %v", i, got[i], want[i])
		}
	}
	// The answer of the other node arrives from the pod IP.
	udpNodeFrom(t, "10.244.1.5:7000", c.local.String(), "answer")
	if b, from := udpRecv(t, c); string(b) != "answer" || from.String() != "10.244.1.5:7000" {
		t.Errorf("client got %q from %v", b, from)
	}
}

// UD7, UD15 between nodes (section 8.8): a Service with only an endpoint on
// another node. udpToNode gets the endpoint, and the answer through
// udpFromNode reaches the client with the ClusterIP and port as its source.
func TestUDPNodeServiceRemote(t *testing.T) {
	es := udpSlice("far", udpEPPort("dns", 5353))
	es.Endpoints = []discoveryv1.Endpoint{{Addresses: []string{"10.244.1.5"}, NodeName: ptr.To("node-1"), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}
	s := flowNode(t, []*v1.Service{udpService("far", "10.96.88.30", udpSvcPort("dns", 53))}, es)
	rec := udpNodeRecord(t)
	c := udpDial(t, podCtx(t, nextLoopbackIP()), "10.96.88.30:53")
	udpNodeWrite(t, c, "q")
	got := rec.calls()
	ep := netip.MustParseAddrPort("10.244.1.5:5353")
	if len(got) != 1 || got[0] != (udpNodeSent{c.local, ep, "q"}) {
		t.Fatalf("udpToNode got %v", got)
	}
	if n := s.flowCount(); n != 1 {
		t.Errorf("%d flows, want 1", n)
	}
	udpNodeFrom(t, ep.String(), c.local.String(), "a")
	if b, from := udpRecv(t, c); string(b) != "a" || from.String() != "10.96.88.30:53" {
		t.Errorf("client got %q from %v, want the ClusterIP", b, from)
	}
}

// Channel rule 5: a datagram through udpFromNode to a socket on the node IP
// (the terminal, a hostNetwork pod) arrives. A socket on a pod IP also gets
// datagrams from another node.
func TestUDPNodeReceiveNodeIP(t *testing.T) {
	flowNode(t, nil)
	rec := udpNodeRecord(t)
	term := udpListen(t, udpNodeTerminal(t), ":0")
	udpNodeFrom(t, "10.244.1.5:999", net.JoinHostPort(tabNodeIP, strconv.Itoa(udpPort(term))), "to the node")
	if b, from := udpRecv(t, term); string(b) != "to the node" || from.String() != "10.244.1.5:999" {
		t.Errorf("terminal got %q from %v", b, from)
	}
	pip := nextLoopbackIP()
	pod := udpListen(t, podCtx(t, pip), ":7001")
	udpNodeFrom(t, "10.244.1.6:1000", net.JoinHostPort(pip, "7001"), "to the pod")
	if b, from := udpRecv(t, pod); string(b) != "to the pod" || from.String() != "10.244.1.6:1000" {
		t.Errorf("pod got %q from %v", b, from)
	}
	// The answer of the pod goes back to the other node.
	pod.WriteTo([]byte("back"), udpTo("10.244.1.6:1000"))
	if got := rec.calls(); len(got) != 1 || got[0] != (udpNodeSent{pod.local, netip.MustParseAddrPort("10.244.1.6:1000"), "back"}) {
		t.Errorf("udpToNode got %v", got)
	}
}

// FL11: with internalTrafficPolicy Local, the translation selects only the
// endpoint on this node. Without it, round robin selects the endpoint on
// node-1 too (section 8.8).
func TestUDPNodeLocalPolicy(t *testing.T) {
	eip := nextLoopbackIP()
	slice := func(svc string) *discoveryv1.EndpointSlice {
		es := udpSlice(svc, udpEPPort("u", 53), eip)
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.244.1.6"}, NodeName: ptr.To("node-1"), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
		return es
	}
	local := udpService("loc", "10.96.88.31", udpSvcPort("u", 53))
	local.Spec.InternalTrafficPolicy = ptr.To(v1.ServiceInternalTrafficPolicyLocal)
	cluster := udpService("all", "10.96.88.32", udpSvcPort("u", 53))
	flowNode(t, []*v1.Service{local, cluster}, slice("loc"), slice("all"))
	rec := udpNodeRecord(t)
	ep := udpListen(t, podCtx(t, eip), ":53")
	ctx := podCtx(t, nextLoopbackIP())

	for i := range 4 {
		udpAsk(t, udpDial(t, ctx, "10.96.88.31:53"), strconv.Itoa(i), ep)
	}
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("Local: udpToNode got %v", got)
	}

	here := 0
	for i := range 4 {
		udpNodeWrite(t, udpDial(t, ctx, "10.96.88.32:53"), strconv.Itoa(i))
	}
	for {
		ep.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, _, err := ep.ReadFrom(make([]byte, 16)); err != nil {
			break
		}
		here++
	}
	got := rec.calls()
	if here != 2 || len(got) != 2 {
		t.Fatalf("Cluster: %d datagrams on this node, udpToNode got %v; want 2 and 2", here, got)
	}
	for _, s := range got {
		if s.dst.String() != "10.244.1.6:53" {
			t.Errorf("udpToNode got %v", s)
		}
	}
}

// egressDenyPolicy selects app=name and denies all its egress.
func egressDenyPolicy(name string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-egress", Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
}

// ND6: an egress policy of the source pod denies the flow. udpToNode gets
// nothing, and the table makes no flow (FL12). Another pod still sends.
func TestUDPNodeEgressDenied(t *testing.T) {
	cip, oip := nextLoopbackIP(), nextLoopbackIP()
	s, _, _ := udpPolicyNode(t, nil,
		testPod("client", cip, "tab", map[string]string{"app": "client"}),
		testPod("other", oip, "tab", map[string]string{"app": "other"}),
		testPod("far", "10.244.1.5", "node-1", map[string]string{"app": "far"}),
		egressDenyPolicy("client"),
	)
	rec := udpNodeRecord(t)
	for range 3 {
		udpNodeWrite(t, udpDial(t, podCtx(t, cip), "10.244.1.5:7000"), "denied")
	}
	if got := rec.calls(); len(got) != 0 {
		t.Fatalf("udpToNode got %v", got)
	}
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows after denied datagrams", n)
	}
	udpNodeWrite(t, udpDial(t, podCtx(t, oip), "10.244.1.5:7000"), "allowed")
	if got := rec.calls(); len(got) != 1 || got[0].data != "allowed" {
		t.Errorf("udpToNode got %v", got)
	}
}

// ND9: an ingress policy of the destination pod drops the datagram of
// udpFromNode from a pod that it does not allow, and makes no flow. 10
// datagrams and 10 answers of an allowed flow cause one call of the engine
// (FL7).
func TestUDPNodeIngressPolicy(t *testing.T) {
	eip := nextLoopbackIP()
	s, _, _ := udpPolicyNode(t, nil,
		testPod("echo", eip, "tab", map[string]string{"app": "echo"}),
		testPod("client", "10.244.1.5", "node-1", map[string]string{"role": "client"}),
		testPod("other", "10.244.1.7", "node-1", map[string]string{"role": "other"}),
		ingressPolicy("echo", fromRole("client"), npPort(v1.ProtocolUDP, 7000)),
	)
	var calls atomic.Int32
	engine := udpPolicy
	setUDPPolicy(t, func(ctx context.Context, src, dst netip.AddrPort) bool {
		calls.Add(1)
		return engine(ctx, src, dst)
	})
	rec := udpNodeRecord(t)
	ep := udpListen(t, podCtx(t, eip), ":7000")
	dst := net.JoinHostPort(eip, "7000")

	udpNodeFrom(t, "10.244.1.7:4000", dst, "denied")
	udpNothing(t, ep)
	if n := s.flowCount(); n != 0 {
		t.Fatalf("%d flows after a denied datagram", n)
	}
	calls.Store(0)
	for i := range 10 {
		udpNodeFrom(t, "10.244.1.5:4000", dst, strconv.Itoa(i))
		b, from := udpRecv(t, ep)
		if string(b) != strconv.Itoa(i) || from.String() != "10.244.1.5:4000" {
			t.Fatalf("echo got %q from %v", b, from)
		}
		ep.WriteTo([]byte("a"), from)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the engine decided %d times, want 1", n)
	}
	if got := rec.calls(); len(got) != 10 || got[0] != (udpNodeSent{ep.local, netip.MustParseAddrPort("10.244.1.5:4000"), "a"}) {
		t.Errorf("udpToNode got %d answers: %v", len(got), got)
	}
}

// Channel rule 7: udpFromNode to a closed port, to a socket on 127.0.0.1 of
// a pod (UD21) and to another node loses the datagram. Nothing arrives, and
// nothing goes to udpToNode.
func TestUDPNodeReceiveDrops(t *testing.T) {
	flowNode(t, nil)
	rec := udpNodeRecord(t)
	pip := nextLoopbackIP()
	ctx := podCtx(t, pip)
	closed := udpListen(t, ctx, ":7002")
	closed.Close()
	lo := udpListen(t, ctx, "127.0.0.1:7003")
	other := udpListen(t, ctx, ":7004")

	udpNodeFrom(t, "10.244.1.5:4000", net.JoinHostPort(pip, "7002"), "closed")
	udpNodeFrom(t, "10.244.1.5:4000", net.JoinHostPort(pip, "7003"), "loopback")
	udpNodeFrom(t, "10.244.1.5:4000", "10.244.2.9:7004", "another node")
	udpNodeFrom(t, "10.244.1.5:4000", net.JoinHostPort(tabNodeIP, "7005"), "node IP without a socket")
	udpNothing(t, lo)
	udpNothing(t, other)
	if got := rec.calls(); len(got) != 0 {
		t.Errorf("udpToNode got %v", got)
	}
}
