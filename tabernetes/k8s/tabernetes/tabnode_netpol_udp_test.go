package main

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	npafake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
)

// The NetworkPolicy of UDP flows (docs/wasi-sockets.md, section 8.8, FL6
// to FL9 and FL12): the real engine of kube-network-policies decides the
// first datagram of each flow of the flow table.

// udpPolicyNode sets a tab node with a service proxy and a policy engine
// with objs (pods, NetworkPolicies) in the namespace default.
func udpPolicyNode(t *testing.T, svcs []*v1.Service, objs ...runtime.Object) (*serviceProxy, *netPolicy, *fake.Clientset) {
	t.Helper()
	s := flowNode(t, svcs)
	objs = append([]runtime.Object{&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}}}, objs...)
	client := fake.NewClientset(objs...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := readyNetPolicy(ctx, t, client, npafake.NewSimpleClientset())
	theTabNode.policy = p
	return s, p, client
}

// ingressPolicy selects app=name and allows the peers on the ports. No
// peers allows nothing.
func ingressPolicy(name string, from []networkingv1.NetworkPolicyPeer, ports ...networkingv1.NetworkPolicyPort) *networkingv1.NetworkPolicy {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if from != nil || ports != nil {
		np.Spec.Ingress = []networkingv1.NetworkPolicyIngressRule{{From: from, Ports: ports}}
	}
	return np
}

func npPort(proto v1.Protocol, port int32) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(port)
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

func fromRole(role string) []networkingv1.NetworkPolicyPeer {
	return []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": role}}}}
}

// waitFor polls cond every 10 ms for at most 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
	}
}

// FL6: an ingress policy allows only UDP port 53 from role=client. The
// datagram of another pod does not arrive, the datagram of the client
// arrives, also through a Service. A datagram of the client to another port
// does not arrive.
func TestUDPPolicyIngress(t *testing.T) {
	dip, cip, oip := nextLoopbackIP(), nextLoopbackIP(), nextLoopbackIP()
	svc := udpService("dns", "10.96.88.53", udpSvcPort("dns", 53))
	s, _, _ := udpPolicyNode(t, []*v1.Service{svc},
		testPod("dns", dip, "tab", map[string]string{"app": "dns"}),
		testPod("client", cip, "tab", map[string]string{"role": "client"}),
		testPod("other", oip, "tab", map[string]string{"role": "other"}),
		ingressPolicy("dns", fromRole("client"), npPort(v1.ProtocolUDP, 53)),
	)
	s.slices.Add(udpSlice("dns", udpEPPort("dns", 53), dip))
	s.sync()
	dns := udpListen(t, podCtx(t, dip), ":53")
	dns54 := udpListen(t, podCtx(t, dip), ":54")

	udpAsk(t, udpDial(t, podCtx(t, cip), net.JoinHostPort(dip, "53")), "direct", dns)
	udpAsk(t, udpDial(t, podCtx(t, cip), "10.96.88.53:53"), "service", dns)

	o := udpDial(t, podCtx(t, oip), net.JoinHostPort(dip, "53"))
	o.Write([]byte("denied"))
	o2 := udpDial(t, podCtx(t, oip), "10.96.88.53:53")
	o2.Write([]byte("denied"))
	c54 := udpDial(t, podCtx(t, cip), net.JoinHostPort(dip, "54"))
	c54.Write([]byte("denied"))
	udpNothing(t, dns)
	udpNothing(t, dns54)
	if n := s.flowCount(); n != 2 {
		t.Errorf("%d flows, want 2 (the allowed ones)", n)
	}
}

// FL6: a port of the protocol TCP does not allow UDP, and a port of the
// protocol UDP does not allow TCP.
func TestUDPPolicyProtocol(t *testing.T) {
	tip, uip, cip := nextLoopbackIP(), nextLoopbackIP(), nextLoopbackIP()
	_, p, _ := udpPolicyNode(t, nil,
		testPod("tcp7", tip, "tab", map[string]string{"app": "tcp7"}),
		testPod("udp7", uip, "tab", map[string]string{"app": "udp7"}),
		testPod("client", cip, "tab", map[string]string{"role": "client"}),
		ingressPolicy("tcp7", fromRole("client"), npPort(v1.ProtocolTCP, 7)),
		ingressPolicy("udp7", fromRole("client"), npPort(v1.ProtocolUDP, 7)),
	)
	ctx := context.Background()
	src := netip.MustParseAddrPort(cip + ":40000")
	for _, c := range []struct {
		dst      string
		tcp, udp bool
	}{
		{tip, true, false},
		{uip, false, true},
	} {
		if got := p.allowed(ctx, tcpAddr(src.String()), tcpAddr(c.dst+":7")); got != c.tcp {
			t.Errorf("TCP to %s:7: allowed %v, want %v", c.dst, got, c.tcp)
		}
		if got := p.allowedUDP(ctx, src, netip.MustParseAddrPort(c.dst+":7")); got != c.udp {
			t.Errorf("UDP to %s:7: allowed %v, want %v", c.dst, got, c.udp)
		}
	}
	// The same through the UDP table.
	ep := udpListen(t, podCtx(t, tip), ":7")
	udpDial(t, podCtx(t, cip), net.JoinHostPort(tip, "7")).Write([]byte("denied"))
	udpNothing(t, ep)
	ep = udpListen(t, podCtx(t, uip), ":7")
	udpAsk(t, udpDial(t, podCtx(t, cip), net.JoinHostPort(uip, "7")), "allowed", ep)
}

// FL7: 10 datagrams and 10 answers of one flow cause one call of the
// engine.
func TestUDPPolicyOneDecision(t *testing.T) {
	eip, cip := nextLoopbackIP(), nextLoopbackIP()
	udpPolicyNode(t, nil,
		testPod("echo", eip, "tab", map[string]string{"app": "echo"}),
		testPod("client", cip, "tab", map[string]string{"role": "client"}),
		ingressPolicy("echo", fromRole("client"), npPort(v1.ProtocolUDP, 7000)),
	)
	var calls atomic.Int32
	engine := udpPolicy
	setUDPPolicy(t, func(ctx context.Context, src, dst netip.AddrPort) bool {
		calls.Add(1)
		return engine(ctx, src, dst)
	})
	ep := udpListen(t, podCtx(t, eip), ":7000")
	c := udpDial(t, podCtx(t, cip), net.JoinHostPort(eip, "7000"))
	for range 10 {
		from := udpAsk(t, c, "q", ep)
		ep.WriteTo([]byte("a"), from)
		if b, _ := udpRecv(t, c); string(b) != "a" {
			t.Fatalf("client got %q", b)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("the engine decided %d times, want 1", n)
	}
}

// FL8, FL9: a new policy denies an open flow. After recheckDelay the flow
// is gone (strict mode), and the next datagram does not arrive. A flow that the policy
// still allows stays.
func TestUDPPolicyStrictMode(t *testing.T) {
	eip, cip, kip := nextLoopbackIP(), nextLoopbackIP(), nextLoopbackIP()
	s, _, client := udpPolicyNode(t, nil,
		testPod("echo", eip, "tab", map[string]string{"app": "echo"}),
		testPod("client", cip, "tab", map[string]string{"role": "client"}),
		testPod("keep", kip, "tab", map[string]string{"app": "keep"}),
	)
	ep := udpListen(t, podCtx(t, eip), ":7000")
	keep := udpListen(t, podCtx(t, kip), ":7000")
	c := udpDial(t, podCtx(t, cip), net.JoinHostPort(eip, "7000"))
	k := udpDial(t, podCtx(t, cip), net.JoinHostPort(kip, "7000"))
	udpAsk(t, c, "q", ep)
	udpAsk(t, k, "q", keep)
	if n := s.flowCount(); n != 2 {
		t.Fatalf("%d flows, want 2", n)
	}
	start := time.Now()
	if _, err := client.NetworkingV1().NetworkPolicies("default").Create(context.Background(), ingressPolicy("echo", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the denied flow is still open", func() bool { return s.flowCount() == 1 })
	// At most recheckDelay after the informer event, earlier when a
	// recheck of an older event already waits.
	t.Logf("flow removed %v after the policy change", time.Since(start))
	c.Write([]byte("denied"))
	udpNothing(t, ep)
	udpAsk(t, k, "q", keep)
}

// FL12: a denied datagram makes no flow. After a policy allows it, the next
// datagram arrives at once.
func TestUDPPolicyDeniedNoFlow(t *testing.T) {
	eip, cip := nextLoopbackIP(), nextLoopbackIP()
	s, p, client := udpPolicyNode(t, nil,
		testPod("echo", eip, "tab", map[string]string{"app": "echo"}),
		testPod("client", cip, "tab", map[string]string{"role": "client"}),
		ingressPolicy("echo", nil),
	)
	ep := udpListen(t, podCtx(t, eip), ":7000")
	c := udpDial(t, podCtx(t, cip), net.JoinHostPort(eip, "7000"))
	c.Write([]byte("denied"))
	udpNothing(t, ep)
	if n := s.flowCount(); n != 0 {
		t.Fatalf("%d flows after a denied datagram", n)
	}
	allow := ingressPolicy("echo", fromRole("client"), npPort(v1.ProtocolUDP, 7000))
	if _, err := client.NetworkingV1().NetworkPolicies("default").Update(context.Background(), allow, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The informer of the engine gets the change.
	src, dst := c.local, ep.local
	waitFor(t, "the engine does not see the new policy", func() bool { return p.allowedUDP(context.Background(), src, dst) })
	udpAsk(t, c, "allowed", ep)
	if n := s.flowCount(); n != 1 {
		t.Errorf("%d flows, want 1", n)
	}
}

// The host of the node (127.0.0.0/8, the node IP) and a pod to its own pod
// IP are not examined, as for TCP (admitIngress). Without a tab node, the
// hook allows.
func TestUDPPolicyHost(t *testing.T) {
	eip := nextLoopbackIP()
	_, p, _ := udpPolicyNode(t, nil,
		testPod("echo", eip, "tab", map[string]string{"app": "echo"}),
		ingressPolicy("echo", nil),
	)
	ctx := context.Background()
	dst := netip.MustParseAddrPort(eip + ":7000")
	for _, src := range []string{"127.0.0.1:40000", "127.0.0.53:40000", tabNodeIP + ":40000", eip + ":40000"} {
		if !p.allowedUDP(ctx, netip.MustParseAddrPort(src), dst) {
			t.Errorf("%s -> %s: denied, want not examined", src, dst)
		}
	}
	if p.allowedUDP(ctx, netip.MustParseAddrPort("10.244.88.250:40000"), dst) {
		t.Error("another source: allowed, want denied")
	}
	// A pod to itself through the UDP table.
	ep := udpListen(t, podCtx(t, eip), ":7000")
	udpAsk(t, udpDial(t, podCtx(t, eip), "127.0.0.1:7000"), "self", ep)

	withTabNode(t, nil)
	if !udpPolicy(ctx, netip.MustParseAddrPort("10.244.88.250:40000"), dst) {
		t.Error("without a tab node, udpPolicy denies")
	}
	withTabNode(t, &tabNode{})
	if !udpPolicy(ctx, netip.MustParseAddrPort("10.244.88.250:40000"), dst) {
		t.Error("without a policy engine, udpPolicy denies")
	}
}
