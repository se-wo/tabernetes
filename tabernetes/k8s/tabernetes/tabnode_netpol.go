package main

// NetworkPolicy for the tab nodes (docs/networking-policy.md). The decision
// engine of sigs.k8s.io/kube-network-policies, the NetworkPolicy
// implementation of SIG Network, without its nftables dataplane: it is asked
// when a connection opens, which is what the dataplane sends it on a real
// node, the first packet of a new connection.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"sigs.k8s.io/kube-network-policies/pkg/api"
	"sigs.k8s.io/kube-network-policies/pkg/network"
	"sigs.k8s.io/kube-network-policies/pkg/networkpolicy"
	"sigs.k8s.io/kube-network-policies/pkg/podinfo"
	npav1alpha2 "sigs.k8s.io/kube-network-policies/plugins/npa-v1alpha2"
	npapi "sigs.k8s.io/network-policy-api/apis/v1alpha2"
	npaclient "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned"
	npainformers "sigs.k8s.io/network-policy-api/pkg/client/informers/externalversions"
)

// netPolicy decides whether a connection may open.
type netPolicy struct {
	engine   *networkpolicy.PolicyEngine
	provider api.PodInfoProvider
	nodes    corelisters.NodeLister

	mu       sync.Mutex
	admitted map[admitKey]*admission

	// Strict mode (tabnode_netpol_strict.go).
	liveMu sync.Mutex
	live   map[*liveConn]struct{}
	kick   chan struct{}
}

// admitKey is a connection that a dial has decided already: the source IP
// and the destination ip:port. The source port is not known before the dial.
type admitKey struct{ src, dst string }

type admission struct {
	n     int // dials that have not reached the listener yet
	until time.Time
}

// admitTTL is how long a decision waits for its connection to be accepted.
const admitTTL = 30 * time.Second

// synTimeout is how long a dropped connection takes to fail when the dial
// has no deadline: the SYN retries of Linux.
const synTimeout = 127 * time.Second

// newNetPolicy starts the informers of the engine: all pods, namespaces,
// nodes, NetworkPolicies and ClusterNetworkPolicies of the cluster. Pods of
// other nodes are needed to know the source of a connection.
//
// The evaluators run in the order of upstream's npa-v1alpha2 build:
// ClusterNetworkPolicies of the Admin tier, NetworkPolicies, then the
// Baseline tier. Rules on domain names never match: the tab has no DNS
// traffic to learn the addresses from (egress to the internet goes through
// fetch(), without an IP).
func newNetPolicy(ctx context.Context, client kubernetes.Interface, npa npaclient.Interface, nodeName string) (*netPolicy, error) {
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
	npaFactory := npainformers.NewSharedInformerFactory(npa, 0)
	cnps := npaFactory.Policy().V1alpha2().ClusterNetworkPolicies()
	evaluators := []api.PolicyEvaluator{
		npav1alpha2.NewClusterNetworkPolicy(npapi.AdminTier, cnps, nil, nodes.Lister()),
		standard,
		npav1alpha2.NewClusterNetworkPolicy(npapi.BaselineTier, cnps, nil, nodes.Lister()),
	}
	p := &netPolicy{
		engine:   networkpolicy.NewPolicyEngine(provider, evaluators),
		provider: provider,
		nodes:    nodes.Lister(),
		admitted: map[admitKey]*admission{},
		live:     map[*liveConn]struct{}{},
		kick:     make(chan struct{}, 1),
	}
	p.watchChanges(ctx, pods.Informer(), namespaces.Informer(), policies.Informer(), cnps.Informer())
	factory.Start(ctx.Done())
	npaFactory.Start(ctx.Done())
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

// podByIP is the pod with the IP ip, from the informers of the engine.
func (p *netPolicy) podByIP(ip string) *api.PodInfo {
	if p == nil {
		return nil
	}
	pod, _ := p.provider.GetPodInfoByIP(ip)
	return pod
}

// admit notes that a dial from src to dst was allowed, so the listener of
// dst does not decide the same connection again.
func (p *netPolicy) admit(src net.IP, dst *net.TCPAddr) {
	if p == nil {
		return
	}
	now := time.Now()
	k := admitKey{src.String(), dst.String()}
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, a := range p.admitted {
		if now.After(a.until) {
			delete(p.admitted, key)
		}
	}
	a := p.admitted[k]
	if a == nil {
		a = &admission{}
		p.admitted[k] = a
	}
	a.n++
	a.until = now.Add(admitTTL)
}

// consume takes one admission of src to dst, if there is one.
func (p *netPolicy) consume(src net.IP, dst *net.TCPAddr) bool {
	if p == nil {
		return false
	}
	k := admitKey{src.String(), dst.String()}
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.admitted[k]
	if a == nil || time.Now().After(a.until) {
		delete(p.admitted, k)
		return false
	}
	if a.n--; a.n <= 0 {
		delete(p.admitted, k)
	}
	return true
}

// dialFromPod dials addr (an ip:port) for the pod with IP srcIP. A cluster IP
// or node port goes to one endpoint, like the DNAT of kube-proxy, so the
// policy sees the pod behind it. A denied connection is dropped: the dial
// fails with a timeout, as NetworkPolicy drops the SYN on a real node.
func (n *tabNode) dialFromPod(ctx context.Context, srcIP, addr string) (net.Conn, error) {
	src := &net.TCPAddr{IP: net.ParseIP(srcIP)}
	dst, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil || src.IP == nil || dst.IP == nil {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr) // fails like before
	}
	if dst.IP.Equal(src.IP) {
		// The pod itself, also its localhost: loopback on a real node,
		// which NetworkPolicy never sees.
		return (&net.Dialer{LocalAddr: src}).DialContext(ctx, "tcp", addr)
	}
	ep := endpoint{ip: dst.IP.String(), port: int32(dst.Port)}
	if n.net != nil {
		e, ok, err := n.net.endpointFor(addr)
		switch {
		case ok && err != nil:
			return nil, refusedError(dst)
		case ok:
			ep = e
			dst = &net.TCPAddr{IP: net.ParseIP(e.ip), Port: int(e.port)}
		case !n.isTabPodIP(ep.ip):
			// A pod IP of another node: through the apiserver.
			if pod := n.policy.podByIP(ep.ip); pod != nil {
				ep.remote = true
				ep.node = pod.GetNode().GetName()
				ep.pod = &v1.ObjectReference{Kind: "Pod", Namespace: pod.GetNamespace().GetName(), Name: pod.GetName()}
			}
		}
	}
	if !n.policy.allowed(ctx, src, dst) {
		return nil, dropped(ctx, dst)
	}
	n.policy.admit(src.IP, dst)
	c, err := n.net.dialEndpoint(ctx, ep, src)
	if err != nil {
		return nil, err
	}
	conn, ok := c.(net.Conn)
	if !ok {
		// A pod on another node, through the apiserver's port-forward.
		conn = &streamConn{ReadWriteCloser: c, local: src, remote: dst}
	}
	return n.policy.track(conn, src, dst), nil
}

// dropped waits like a client whose SYN is dropped: until its context ends,
// at most synTimeout, then the dial fails with a timeout.
func dropped(ctx context.Context, dst net.Addr) error {
	t := time.NewTimer(synTimeout)
	defer t.Stop()
	select {
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.Canceled) {
			return &net.OpError{Op: "dial", Net: "tcp", Addr: dst, Err: ctx.Err()}
		}
	case <-t.C:
	}
	return &net.OpError{Op: "dial", Net: "tcp", Addr: dst, Err: os.ErrDeadlineExceeded}
}

// refusedError is the answer to a connection nobody accepts, a service without
// endpoints included (kube-proxy rejects those).
func refusedError(dst net.Addr) error {
	return &net.OpError{Op: "dial", Net: "tcp", Addr: dst, Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
}

// policyListener decides the ingress of connections that no dial of this
// node has decided: dials that do not go through dialCluster. A denied one
// is closed right away; the dialer already has its connection.
type policyListener struct {
	net.Listener
	n *tabNode
}

func (l *policyListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		switch ok, decided := l.n.admitIngress(c); {
		case ok && decided:
			src, dst := c.RemoteAddr().(*net.TCPAddr), c.LocalAddr().(*net.TCPAddr)
			return l.n.policy.track(c, src, dst), nil
		case ok:
			return c, nil
		}
		c.Close()
	}
}

// admitIngress allows the host of the node (loopback, the node IP), the pod
// itself, connections a dial has decided, and what the policy allows.
// decided is true when the policy decided here; such a connection is
// tracked for strict mode.
func (n *tabNode) admitIngress(c net.Conn) (ok, decided bool) {
	src, ok1 := c.RemoteAddr().(*net.TCPAddr)
	dst, ok2 := c.LocalAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return true, false
	}
	switch {
	case src.IP.IsLoopback(), src.IP.Equal(net.ParseIP(tabNodeIP)), src.IP.Equal(dst.IP):
		return true, false
	case n.policy.consume(src.IP, dst):
		return true, false
	}
	return n.policy.allowed(context.Background(), src, dst), true
}

// streamConn is a byte stream to a pod on another node as a net.Conn. It has
// no deadlines; the callers on the tab node use contexts.
type streamConn struct {
	io.ReadWriteCloser
	local, remote net.Addr
}

func (c *streamConn) LocalAddr() net.Addr              { return c.local }
func (c *streamConn) RemoteAddr() net.Addr             { return c.remote }
func (c *streamConn) SetDeadline(time.Time) error      { return nil }
func (c *streamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *streamConn) SetWriteDeadline(time.Time) error { return nil }

func (c *streamConn) CloseWrite() error {
	if cw, ok := c.ReadWriteCloser.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}
