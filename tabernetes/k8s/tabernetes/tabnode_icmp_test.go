package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
	npafake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
)

// The ICMP echo of ping (docs/wasi-sockets.md, section 7.9, IC1 to IC8):
// the real engine of kube-network-policies decides each echo request.

// The pods of the tests. The pod CIDR of this tab node is 10.244.0.0/24.
const (
	icmpClient = "10.244.0.2" // role=client
	icmpTarget = "10.244.0.3" // app=target
	icmpDenied = "10.244.0.4" // role=denied, default-deny egress
	icmpClosed = "10.244.0.5" // app=closed, default-deny ingress
	icmpTCP80  = "10.244.0.6" // app=tcp80, ingress only TCP port 80
	icmpOpen   = "10.244.0.7" // app=open, ingress without ports
	icmpRemote = "10.244.1.2" // a pod on the node worker tab-worker-1
	icmpDocker = "10.244.5.7" // a pod IP of the Docker node docker-1
	workerIP   = "172.18.0.3" // the node IP of tab-worker-1
)

// icmpNode sets a tab node with the pods above, the policies and the nodes
// tab-worker-1 (a node worker) and docker-1 (a Docker node with a flannel
// pod CIDR and no tab node).
func icmpNode(t *testing.T) (*netPolicy, *fake.Clientset) {
	t.Helper()
	all := metav1.LabelSelector{}
	egressDeny := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "denied", Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "denied"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		},
	}
	open := ingressPolicy("open", []networkingv1.NetworkPolicyPeer{{PodSelector: &all}})
	objs := []runtime.Object{
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}},
		testPod("client", icmpClient, "tab", map[string]string{"role": "client"}),
		testPod("target", icmpTarget, "tab", map[string]string{"app": "target"}),
		testPod("denied", icmpDenied, "tab", map[string]string{"role": "denied"}),
		testPod("closed", icmpClosed, "tab", map[string]string{"app": "closed"}),
		testPod("tcp80", icmpTCP80, "tab", map[string]string{"app": "tcp80"}),
		testPod("open", icmpOpen, "tab", map[string]string{"app": "open"}),
		testPod("remote", icmpRemote, "tab-worker-1", map[string]string{"app": "remote"}),
		egressDeny,
		ingressPolicy("closed", nil),
		ingressPolicy("tcp80", []networkingv1.NetworkPolicyPeer{{PodSelector: &all}}, npPort(v1.ProtocolTCP, 80)),
		open,
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "tab-worker-1"},
			Spec:   v1.NodeSpec{PodCIDR: "10.244.1.0/24", PodCIDRs: []string{"10.244.1.0/24"}},
			Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: workerIP}}}},
		&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "docker-1"},
			Spec: v1.NodeSpec{PodCIDR: "10.244.5.0/24"}},
	}
	client := fake.NewClientset(objs...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p := readyNetPolicy(ctx, t, client, npafake.NewSimpleClientset())
	waitFor(t, "the nodes are in the informer", func() bool {
		l, _ := p.nodes.List(labels.Everything())
		return len(l) == 2
	})
	_, cidr, _ := net.ParseCIDR("10.244.0.0/24")
	used := map[string]bool{}
	for _, ip := range []string{icmpClient, icmpTarget, icmpDenied, icmpClosed, icmpTCP80, icmpOpen} {
		used[ip] = true
	}
	withTabNode(t, &tabNode{podCIDR: cidr, usedIPs: used, policy: p})
	return p, client
}

// pingIn runs a script in the pod with the IP ip ("" is the terminal).
func pingIn(t *testing.T, ip, script string) (string, int) {
	t.Helper()
	ctx := context.Background()
	if ip != "" {
		ctx = podCtx(t, ip)
	}
	var out bytes.Buffer
	handler := func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			if u := coreutils[args[0]]; u != nil {
				return u(ctx, interp.HandlerCtx(ctx), args[1:], next)
			}
			return next(ctx, args)
		}
	}
	r, err := interp.New(interp.StdIO(strings.NewReader(""), &out, &out), interp.Dir(t.TempDir()), interp.ExecHandlers(handler))
	if err != nil {
		t.Fatal(err)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatal(err)
	}
	err = r.Run(ctx, f)
	code := 0
	if st, ok := interp.IsExitStatus(err); ok {
		code = int(st)
	} else if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return out.String(), code
}

// countICMP counts the decisions of echo requests.
func countICMP(t *testing.T) *atomic.Int32 {
	var calls atomic.Int32
	saved := icmpPolicy
	icmpPolicy = func(ctx context.Context, src, dst net.IP) bool {
		calls.Add(1)
		return saved(ctx, src, dst)
	}
	t.Cleanup(func() { icmpPolicy = saved })
	return &calls
}

// IC4, IC5: the engine examines the egress of the source and the ingress of
// the destination, with no protocol and port 0. The host of the node is
// not examined.
func TestICMPPolicyEgressIngress(t *testing.T) {
	p, _ := icmpNode(t)
	ip := net.ParseIP
	ctx := context.Background()
	for _, tc := range []struct {
		src, dst string
		want     bool
	}{
		{icmpClient, icmpTarget, true},
		{icmpDenied, icmpTarget, false}, // egress of the source
		{icmpClient, icmpClosed, false}, // ingress of the destination
		{"127.0.0.1", icmpClosed, true},
		{tabNodeIP, icmpClosed, true},
		{icmpClosed, icmpClosed, true},
	} {
		if got := p.allowedICMP(ctx, ip(tc.src), ip(tc.dst)); got != tc.want {
			t.Errorf("allowedICMP(%s, %s) = %v, want %v", tc.src, tc.dst, got, tc.want)
		}
	}
	// The same call that allowedICMP makes: no protocol and port 0.
	if p.decide(ctx, "", ip(icmpClient), 0, ip(icmpClosed), 0) {
		t.Error("decide without protocol allows the closed pod")
	}
	// Until the informers sync, everything passes.
	var nilPolicy *netPolicy
	if !nilPolicy.allowedICMP(ctx, ip(icmpDenied), ip(icmpClosed)) {
		t.Error("no engine denies")
	}
}

// IC1, IC7: a rule with ports does not match ICMP. A rule without ports
// matches it.
func TestICMPPolicyPorts(t *testing.T) {
	icmpNode(t)
	out, code := pingIn(t, icmpClient, "ping -c 1 -W 0.05 "+icmpTCP80)
	if code != 1 || !strings.Contains(out, "1 packets transmitted, 0 packets received, 100% packet loss") {
		t.Errorf("ping to the TCP 80 pod: %q %d", out, code)
	}
	out, code = pingIn(t, icmpClient, "ping -c 2 -i 0.01 "+icmpOpen)
	if code != 0 || !strings.Contains(out, "64 bytes from "+icmpOpen+": seq=1 ttl=64 time=") ||
		!strings.Contains(out, "2 packets transmitted, 2 packets received, 0% packet loss") {
		t.Errorf("ping to the open pod: %q %d", out, code)
	}
}

// IC8: each echo request gets a decision. A new policy denies the next
// request.
func TestICMPEachRequest(t *testing.T) {
	p, client := icmpNode(t)
	calls := countICMP(t)
	out, code := pingIn(t, icmpClient, "ping -c 3 -i 0.01 "+icmpTarget)
	if code != 0 || !strings.Contains(out, "3 packets transmitted, 3 packets received") {
		t.Fatalf("ping: %q %d", out, code)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("%d decisions for 3 echo requests, want 3", n)
	}
	if _, err := client.NetworkingV1().NetworkPolicies("default").Create(context.Background(), ingressPolicy("target", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the engine sees the new policy", func() bool {
		return !p.allowedICMP(context.Background(), net.ParseIP(icmpClient), net.ParseIP(icmpTarget))
	})
	out, code = pingIn(t, icmpClient, "ping -c 1 -W 0.05 "+icmpTarget)
	if code != 1 || !strings.Contains(out, "1 packets transmitted, 0 packets received") {
		t.Errorf("ping after the policy: %q %d", out, code)
	}
}

// IC2: the node IP gives a reply, to the terminal and to a pod. A pod with
// a default-deny egress policy gets no reply.
func TestICMPNodeIP(t *testing.T) {
	icmpNode(t)
	for _, src := range []string{"", icmpClient} {
		out, code := pingIn(t, src, "ping -c 1 "+tabNodeIP)
		if code != 0 || !strings.Contains(out, "64 bytes from "+tabNodeIP+": seq=0 ttl=64") {
			t.Errorf("ping from %q to the node IP: %q %d", src, out, code)
		}
	}
	out, code := pingIn(t, icmpDenied, "ping -c 1 -W 0.05 "+tabNodeIP)
	if code != 1 || !strings.Contains(out, "0 packets received") {
		t.Errorf("ping from the denied pod to the node IP: %q %d", out, code)
	}
	// 127.0.0.1 and the own pod IP always reply.
	for _, dst := range []string{"127.0.0.1", icmpDenied} {
		if out, code := pingIn(t, icmpDenied, "ping -c 1 "+dst); code != 0 {
			t.Errorf("ping from the denied pod to %s: %q %d", dst, out, code)
		}
	}
}

// IC3: an IP of another node gives the text at once: a pod IP of a node
// worker, its node IP, and a pod IP of a Docker node.
func TestICMPOtherNode(t *testing.T) {
	icmpNode(t)
	calls := countICMP(t)
	for ip, node := range map[string]string{icmpRemote: "tab-worker-1", workerIP: "tab-worker-1", icmpDocker: "docker-1"} {
		start := time.Now()
		out, code := pingIn(t, icmpClient, "ping -c 3 -W 1 "+ip)
		want := "ping: " + ip + " is on node " + node + ": ICMP between nodes is not available\n"
		if code != 1 || out != want {
			t.Errorf("ping %s: %q %d, want %q", ip, out, code, want)
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Errorf("ping %s took %v", ip, d)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d decisions, want 0", n)
	}
}

// IC6: a ClusterIP gives no reply. A pod IP of another node is not a pod of
// this tab node, though the informers know it. An address that nobody has
// gives no reply.
func TestICMPNoReply(t *testing.T) {
	p, _ := icmpNode(t)
	for _, ip := range []string{"10.96.0.50", "10.244.0.99", "192.0.2.1"} {
		out, code := pingIn(t, icmpClient, "ping -c 1 -W 0.05 "+ip)
		if code != 1 || !strings.Contains(out, "1 packets transmitted, 0 packets received, 100% packet loss") {
			t.Errorf("ping %s: %q %d", ip, out, code)
		}
	}
	if p.podByIP(icmpRemote) == nil {
		t.Fatal("the informers do not know the remote pod")
	}
	if theTabNode.isTabPodIP(icmpRemote) || hostExists(net.ParseIP(icmpRemote)) {
		t.Error("the pod of the node worker is a pod of this tab node")
	}
}
