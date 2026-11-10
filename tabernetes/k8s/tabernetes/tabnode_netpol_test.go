package main

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	npapi "sigs.k8s.io/network-policy-api/apis/v1alpha2"
	npafake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
)

func testPod(name, ip, node string, labels map[string]string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels, UID: types.UID("uid-" + name)},
		Spec:       v1.PodSpec{NodeName: node, Containers: []v1.Container{{Name: "c"}}},
		Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: ip, PodIPs: []v1.PodIP{{IP: ip}}},
	}
}

func tcpAddr(s string) *net.TCPAddr {
	a, err := net.ResolveTCPAddr("tcp", s)
	if err != nil {
		panic(err)
	}
	return a
}

// TestNetPolicy runs the engine of kube-network-policies in js/wasm: web
// accepts only role=client on port 80.
func TestNetPolicy(t *testing.T) {
	port80 := intstr.FromInt32(80)
	tcp := v1.ProtocolTCP // the apiserver defaults it, the fake client does not
	client := fake.NewClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}},
		testPod("web", "10.244.0.2", "tab", map[string]string{"app": "web"}),
		testPod("client", "10.244.0.3", "tab", map[string]string{"role": "client"}),
		testPod("other", "10.244.1.2", "tab-worker-1", map[string]string{"role": "other"}),
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port80}},
				}},
			},
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := readyNetPolicy(ctx, t, client, npafake.NewSimpleClientset())
	for _, c := range []struct {
		src, dst string
		want     bool
	}{
		{"10.244.0.3:0", "10.244.0.2:80", true},    // client -> web:80
		{"10.244.0.3:0", "10.244.0.2:8080", false}, // wrong port
		{"10.244.1.2:0", "10.244.0.2:80", false},   // wrong labels, other node
		{"10.0.0.1:0", "10.244.0.2:80", false},     // not a pod, no ipBlock
		{"10.244.0.2:0", "10.244.0.3:80", true},    // no policy selects client
	} {
		if got := p.allowed(ctx, tcpAddr(c.src), tcpAddr(c.dst)); got != c.want {
			t.Errorf("%s -> %s: allowed %v, want %v", c.src, c.dst, got, c.want)
		}
	}
	if pod := p.podByIP("10.244.1.2"); pod == nil || pod.GetName() != "other" || pod.GetNamespace().GetName() != "default" {
		t.Errorf("podByIP(10.244.1.2) = %v, want default/other", pod)
	}
}

func readyNetPolicy(ctx context.Context, t *testing.T, client *fake.Clientset, npa *npafake.Clientset) *netPolicy {
	t.Helper()
	p, err := newNetPolicy(ctx, client, npa, "tab")
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !p.engine.Ready(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("engine not ready")
		}
	}
	return p
}

// TestClusterNetworkPolicy: the Admin tier wins over NetworkPolicies, the
// Baseline tier applies where no NetworkPolicy does.
func TestClusterNetworkPolicy(t *testing.T) {
	port80 := intstr.FromInt32(80)
	tcp := v1.ProtocolTCP
	client := fake.NewClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}},
		testPod("web", "10.244.0.2", "tab", map[string]string{"app": "web"}),
		testPod("client", "10.244.0.3", "tab", map[string]string{"role": "client"}),
		testPod("other", "10.244.0.4", "tab", map[string]string{"role": "other"}),
		&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
				Ingress: []networkingv1.NetworkPolicyIngressRule{{
					From:  []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port80}},
				}},
			},
		},
	)
	all := metav1.LabelSelector{}
	npa := npafake.NewSimpleClientset(
		// Admin: role=other never reaches web, whatever NetworkPolicies say.
		&npapi.ClusterNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "admin"},
			Spec: npapi.ClusterNetworkPolicySpec{
				Tier: npapi.AdminTier, Priority: 10,
				Subject: npapi.ClusterNetworkPolicySubject{Pods: &npapi.NamespacedPod{NamespaceSelector: all, PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}},
				Ingress: []npapi.ClusterNetworkPolicyIngressRule{{
					Action: npapi.ClusterNetworkPolicyRuleActionDeny,
					From:   []npapi.ClusterNetworkPolicyIngressPeer{{Pods: &npapi.NamespacedPod{NamespaceSelector: all, PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "other"}}}}},
				}},
			},
		},
		// Baseline: nothing reaches role=client, unless a NetworkPolicy allows it.
		&npapi.ClusterNetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "baseline"},
			Spec: npapi.ClusterNetworkPolicySpec{
				Tier: npapi.BaselineTier, Priority: 10,
				Subject: npapi.ClusterNetworkPolicySubject{Pods: &npapi.NamespacedPod{NamespaceSelector: all, PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"role": "client"}}}},
				Ingress: []npapi.ClusterNetworkPolicyIngressRule{{
					Action: npapi.ClusterNetworkPolicyRuleActionDeny,
					From:   []npapi.ClusterNetworkPolicyIngressPeer{{Namespaces: &all}},
				}},
			},
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := readyNetPolicy(ctx, t, client, npa)
	for _, c := range []struct {
		src, dst string
		want     bool
	}{
		{"10.244.0.3:0", "10.244.0.2:80", true},  // client -> web: the NetworkPolicy allows it
		{"10.244.0.4:0", "10.244.0.2:80", false}, // other -> web: Admin denies it first
		{"10.244.0.2:0", "10.244.0.3:80", false}, // web -> client: Baseline denies it
		{"10.244.0.2:0", "10.244.0.4:80", true},  // web -> other: no policy at all
	} {
		if got := p.allowed(ctx, tcpAddr(c.src), tcpAddr(c.dst)); got != c.want {
			t.Errorf("%s -> %s: allowed %v, want %v", c.src, c.dst, got, c.want)
		}
	}
}

func TestNetPolicyAdmit(t *testing.T) {
	p := &netPolicy{admitted: map[admitKey]*admission{}}
	src, dst := net.ParseIP("10.244.0.3"), tcpAddr("10.244.0.2:80")
	if p.consume(src, dst) {
		t.Fatal("consumed an admission that was never made")
	}
	p.admit(src, dst)
	p.admit(src, dst)
	for i := range 2 {
		if !p.consume(src, dst) {
			t.Fatalf("admission %d missing", i)
		}
	}
	if p.consume(src, dst) {
		t.Fatal("consumed more admissions than were made")
	}
	var nilPolicy *netPolicy
	if !nilPolicy.allowed(context.Background(), tcpAddr("10.244.0.3:0"), dst) {
		t.Error("a node without policy engine must allow")
	}
}

func TestDropped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := dropped(ctx, tcpAddr("10.244.0.2:80"))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("dropped: %v, want a timeout", err)
	}
	if time.Since(start) < 20*time.Millisecond {
		t.Error("dropped returned before the deadline")
	}
	if err := refusedError(tcpAddr("10.244.0.2:80")); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("refusedError: %v, want ECONNREFUSED", err)
	}
}

// TestFakeNetSourceAddress checks what the policy hooks rely on: Go's fake
// network of js/wasm keeps the local address of a dial, and refuses a dial
// to an address without listener.
func TestFakeNetSourceAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "10.244.250.2:80")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Addr, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c.RemoteAddr()
			c.Close()
		}
	}()
	c, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("10.244.250.3")}}).Dial("tcp", "10.244.250.2:80")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := (<-accepted).(*net.TCPAddr).IP.String(); got != "10.244.250.3" {
		t.Errorf("listener sees %s, want 10.244.250.3", got)
	}
	if _, err := net.Dial("tcp", "10.244.250.9:80"); err == nil || !refused(err) {
		t.Errorf("dial without listener: %v, want ECONNREFUSED", err)
	}
}

// TestStrictMode: a policy that denies an open connection closes it.
func TestStrictMode(t *testing.T) {
	client := fake.NewClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}},
		testPod("web", "10.244.0.2", "tab", map[string]string{"app": "web"}),
		testPod("client", "10.244.0.3", "tab", map[string]string{"role": "client"}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := readyNetPolicy(ctx, t, client, npafake.NewSimpleClientset())
	src, dst := tcpAddr("10.244.0.3:40000"), tcpAddr("10.244.0.2:80")
	if !p.allowed(ctx, src, dst) {
		t.Fatal("no policy yet: must be allowed")
	}
	a, b := net.Pipe()
	defer b.Close()
	tracked := p.track(a, src, dst)
	// Host traffic and connections the policy still allows stay open.
	keepA, keepB := net.Pipe()
	defer keepB.Close()
	kept := p.track(keepA, tcpAddr("10.244.0.2:40001"), tcpAddr("10.244.0.3:80"))
	defer kept.Close()

	deny := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "deny-web", Namespace: "default"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	if _, err := client.NetworkingV1().NetworkPolicies("default").Create(ctx, deny, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 1))
		closed <- err
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the denied connection is still open")
	}
	p.liveMu.Lock()
	n := len(p.live)
	p.liveMu.Unlock()
	if n != 1 {
		t.Errorf("%d tracked connections, want 1 (the allowed one)", n)
	}
	tracked.Close()
}
