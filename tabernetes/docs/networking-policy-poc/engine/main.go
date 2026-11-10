// Throwaway proof of concept for docs/networking-policy.md: the
// kube-network-policies engine in js/wasm, fed by informers, asked about
// connections instead of packets. Run it with ../run.sh engine.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"sigs.k8s.io/kube-network-policies/pkg/api"
	"sigs.k8s.io/kube-network-policies/pkg/network"
	"sigs.k8s.io/kube-network-policies/pkg/networkpolicy"
	"sigs.k8s.io/kube-network-policies/pkg/podinfo"
)

func pod(name, ip, node string, labels map[string]string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels, UID: types.UID("uid-" + name)},
		Spec:       v1.PodSpec{NodeName: node, Containers: []v1.Container{{Name: "c", Ports: []v1.ContainerPort{{Name: "http", ContainerPort: 80}}}}},
		Status:     v1.PodStatus{Phase: v1.PodRunning, PodIP: ip, PodIPs: []v1.PodIP{{IP: ip}}},
	}
}

func main() {
	fmt.Println("GOOS/GOARCH:", runtime.GOOS, runtime.GOARCH)
	port80 := intstr.FromInt32(80)
	tcp := v1.ProtocolTCP // the apiserver defaults it, the fake client does not
	client := fake.NewClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}},
		pod("web", "10.244.0.2", "tab", map[string]string{"app": "web"}),
		pod("client", "10.244.0.3", "tab", map[string]string{"role": "client"}),
		pod("other", "10.244.1.2", "tab-worker-1", map[string]string{"role": "other"}),
		// web only from role=client on port 80.
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
	f := informers.NewSharedInformerFactory(client, 0)
	pods, nss, nodes, nps := f.Core().V1().Pods(), f.Core().V1().Namespaces(), f.Core().V1().Nodes(), f.Networking().V1().NetworkPolicies()
	np := networkpolicy.NewStandardNetworkPolicy("tab", nss, pods, nps)
	var syncs atomic.Int32 // informer handlers call it from their goroutines
	np.SetDataplaneSyncCallback(func() { syncs.Add(1) })
	resolver, err := podinfo.NewInformerResolver(pods.Informer())
	if err != nil {
		panic(err)
	}
	provider, err := podinfo.NewInformerProvider(pods, nss, nodes, nil, []podinfo.IPResolver{resolver})
	if err != nil {
		panic(err)
	}
	engine := networkpolicy.NewPolicyEngine(provider, []api.PolicyEvaluator{np})
	ctx := context.Background()
	f.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), pods.Informer().HasSynced, nss.Informer().HasSynced, nps.Informer().HasSynced, nodes.Informer().HasSynced) {
		panic("no sync")
	}
	fmt.Println("engine ready:", engine.Ready(), "sync callbacks so far:", syncs.Load())

	conn := func(src, dst string, port int) {
		p := &network.Packet{Family: v1.IPv4Protocol, SrcIP: net.ParseIP(src), DstIP: net.ParseIP(dst), Proto: v1.ProtocolTCP, SrcPort: 40000, DstPort: port}
		start := time.Now()
		ok, err := engine.EvaluatePacket(ctx, p)
		fmt.Printf("%-11s -> %s:%-4d allowed=%-5v err=%v (%v)\n", src, dst, port, ok, err, time.Since(start))
	}
	conn("10.244.0.3", "10.244.0.2", 80)   // client -> web:80     allow
	conn("10.244.0.3", "10.244.0.2", 8080) // client -> web:8080   deny (port)
	conn("10.244.1.2", "10.244.0.2", 80)   // other (worker) -> web deny (labels)
	conn("10.0.0.1", "10.244.0.2", 80)     // node IP -> web: deny; node traffic skips the engine on its own node
	conn("10.244.0.2", "10.244.0.2", 80)   // web -> itself: deny; the tab skips the engine here (loopback)
	conn("10.244.0.2", "10.244.0.3", 80)   // web -> client         allow (no egress policy)
	os.Exit(0)
}
