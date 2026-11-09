package main

// NetworkPolicy for the tab nodes (docs/networking-policy.md). The decision
// engine of sigs.k8s.io/kube-network-policies, the NetworkPolicy
// implementation of SIG Network, without its nftables dataplane: it is asked
// when a connection opens, which is what the dataplane sends it on a real
// node, the first packet of a new connection.

import (
	"context"
	"fmt"
	"net"
	"os"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/kube-network-policies/pkg/api"
	"sigs.k8s.io/kube-network-policies/pkg/network"
	"sigs.k8s.io/kube-network-policies/pkg/networkpolicy"
	"sigs.k8s.io/kube-network-policies/pkg/podinfo"
)

// netPolicy decides whether a connection may open.
type netPolicy struct {
	engine *networkpolicy.PolicyEngine
}

// newNetPolicy starts the informers of the engine: all pods, namespaces,
// nodes and NetworkPolicies of the cluster. Pods of other nodes are needed
// to know the source of a connection.
func newNetPolicy(ctx context.Context, client kubernetes.Interface, nodeName string) (*netPolicy, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 0,
		// The cache keeps every pod of the cluster: drop what the engine
		// never reads.
		informers.WithTransform(func(obj any) (any, error) {
			if m, err := meta.Accessor(obj); err == nil {
				m.SetManagedFields(nil)
			}
			return obj, nil
		}))
	pods := factory.Core().V1().Pods()
	namespaces := factory.Core().V1().Namespaces()
	nodes := factory.Core().V1().Nodes()
	policies := factory.Networking().V1().NetworkPolicies()

	resolver, err := podinfo.NewInformerResolver(pods.Informer())
	if err != nil {
		return nil, err
	}
	provider, err := podinfo.NewInformerProvider(pods, namespaces, nodes, nil, []podinfo.IPResolver{resolver})
	if err != nil {
		return nil, err
	}
	standard := networkpolicy.NewStandardNetworkPolicy(nodeName, namespaces, pods, policies)
	p := &netPolicy{engine: networkpolicy.NewPolicyEngine(provider, []api.PolicyEvaluator{standard})}
	factory.Start(ctx.Done())
	return p, nil
}

// allowed reports whether a TCP connection from src to dst may open. Until
// the informers have synced it allows everything, like the fail-open default
// of kube-network-policies.
func (p *netPolicy) allowed(ctx context.Context, src, dst *net.TCPAddr) bool {
	if p == nil || !p.engine.Ready() {
		return true
	}
	family := v1.IPv4Protocol
	if dst.IP.To4() == nil {
		family = v1.IPv6Protocol
	}
	ok, err := p.engine.EvaluatePacket(ctx, &network.Packet{
		Family:  family,
		SrcIP:   src.IP,
		DstIP:   dst.IP,
		Proto:   v1.ProtocolTCP,
		SrcPort: src.Port,
		DstPort: dst.Port,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "network policy: %s -> %s: %v\n", src, dst, err)
	}
	return ok
}
