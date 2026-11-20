package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

// The in-process network of Go for js/wasm gives ephemeral ports from 1
// upward, for all tests of the package. A connection that is still open can
// then have a port that a later test listens on, for example 80 or 443 of
// TestIngressSNIReload. This moves the first ephemeral port above 1023.
func init() {
	for {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return
		}
		port := ln.Addr().(*net.TCPAddr).Port
		ln.Close()
		if port >= 1024 || port == 0 {
			return
		}
	}
}

// flowNode sets a tab node with a service proxy for one test. The pod CIDR
// of the tab node is 10.244.88.0/24, the IPs of nextLoopbackIP: an endpoint
// with another IP and another node is remote.
func flowNode(t *testing.T, svcs []*v1.Service, eps ...*discoveryv1.EndpointSlice) *serviceProxy {
	t.Helper()
	s := newServiceProxy(nil)
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, svc := range svcs {
		idx.Add(svc)
	}
	s.services = corelisters.NewServiceLister(idx)
	s.slices = cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{sliceIndex: func(obj any) ([]string, error) {
		e := obj.(*discoveryv1.EndpointSlice)
		return []string{e.Namespace + "/" + e.Labels[discoveryv1.LabelServiceName]}, nil
	}})
	for _, es := range eps {
		s.slices.Add(es)
	}
	s.synced.Store(true)
	s.sync()
	t.Cleanup(func() {
		s.mu.Lock()
		for _, l := range s.listeners {
			l.ln.Close()
		}
		s.mu.Unlock()
		s.flowMu.Lock()
		if s.flowTimer != nil {
			s.flowTimer.Stop()
		}
		s.flowMu.Unlock()
	})
	_, cidr, _ := net.ParseCIDR("10.244.88.0/24")
	withTabNode(t, &tabNode{net: s, podCIDR: cidr})
	return s
}

// udpService is a Service with the ClusterIP ip and the ports, as
// "name/port/protocol".
func udpService(name, ip string, ports ...v1.ServicePort) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: v1.ServiceSpec{ClusterIP: ip, ClusterIPs: []string{ip}, Ports: ports}}
}

func udpSvcPort(name string, port int32) v1.ServicePort {
	return v1.ServicePort{Name: name, Port: port, Protocol: v1.ProtocolUDP}
}

// udpSlice is an EndpointSlice of the Service svc with one port and ready
// endpoints on this node.
func udpSlice(svc string, port discoveryv1.EndpointPort, ips ...string) *discoveryv1.EndpointSlice {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "default", Name: svc + "-abcde", Labels: map[string]string{discoveryv1.LabelServiceName: svc}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Ports:       []discoveryv1.EndpointPort{port},
	}
	for _, ip := range ips {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{ip}, NodeName: ptr.To(tabNodeName), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	}
	return es
}

func udpEPPort(name string, port int32) discoveryv1.EndpointPort {
	return discoveryv1.EndpointPort{Name: ptr.To(name), Port: ptr.To(port), Protocol: ptr.To(v1.ProtocolUDP)}
}

// flowClock sets the clock of the flow table for one test. The function
// moves it.
func flowClock(t *testing.T) func(time.Duration) {
	var mu sync.Mutex
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	saved := flowNow
	flowNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	t.Cleanup(func() { flowNow = saved })
	return func(d time.Duration) {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
	}
}

// setUDPPolicy sets udpPolicy for one test.
func setUDPPolicy(t *testing.T, f func(ctx context.Context, src, dst netip.AddrPort) bool) {
	saved := udpPolicy
	udpPolicy = f
	t.Cleanup(func() { udpPolicy = saved })
}

// udpDial connects a new socket of the pod ctx to addr.
func udpDial(t *testing.T, ctx context.Context, addr string) *udpConn {
	t.Helper()
	c, err := dialClusterUDP(ctx, addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// udpAsk sends q from c and gives what the endpoint ep got, and its source.
func udpAsk(t *testing.T, c *udpConn, q string, ep *udpConn) net.Addr {
	t.Helper()
	if _, err := c.Write([]byte(q)); err != nil {
		t.Fatalf("write: %v", err)
	}
	b, from := udpRecv(t, ep)
	if string(b) != q {
		t.Fatalf("endpoint %v got %q, want %q", ep.LocalAddr(), b, q)
	}
	return from
}

// UD16, UD17: a Service with 53/TCP and 53/UDP on one ClusterIP. A TCP
// dial and a UDP datagram reach their endpoints. UD7, UD15: the answer of
// the endpoint has the ClusterIP and port as its source.
func TestUDPServiceTCPAndUDP(t *testing.T) {
	eip, cip := nextLoopbackIP(), nextLoopbackIP()
	svc := udpService("kube-dns", "10.96.88.10", udpSvcPort("dns", 53), v1.ServicePort{Name: "dns-tcp", Port: 53, Protocol: v1.ProtocolTCP})
	es := udpSlice("kube-dns", udpEPPort("dns", 5353), eip)
	es.Ports = append(es.Ports, discoveryv1.EndpointPort{Name: ptr.To("dns-tcp"), Port: ptr.To(int32(5354)), Protocol: ptr.To(v1.ProtocolTCP)})
	flowNode(t, []*v1.Service{svc}, es)

	ln, err := net.Listen("tcp", net.JoinHostPort(eip, "5354"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Write([]byte("tcp"))
			c.Close()
		}
	}()
	tc, err := net.Dial("tcp", "10.96.88.10:53")
	if err != nil {
		t.Fatalf("TCP dial: %v", err)
	}
	tc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if b, err := io.ReadAll(tc); err != nil || string(b) != "tcp" {
		t.Errorf("TCP: %q %v", b, err)
	}
	tc.Close()

	ep := udpListen(t, podCtx(t, eip), ":5353")
	c := udpDial(t, podCtx(t, cip), "10.96.88.10:53")
	from := udpAsk(t, c, "query", ep)
	if from.String() != c.LocalAddr().String() {
		t.Errorf("the endpoint got the datagram from %v, want the client %v", from, c.LocalAddr())
	}
	ep.WriteTo([]byte("answer"), from)
	if b, from := udpRecv(t, c); string(b) != "answer" || from.String() != "10.96.88.10:53" {
		t.Errorf("client got %q from %v, want the ClusterIP", b, from)
	}
}

// UD10, UD11: a connected client to a Service without endpoints gets
// ECONNREFUSED from the table. No flow.
func TestUDPServiceNoEndpoints(t *testing.T) {
	s := flowNode(t, []*v1.Service{udpService("empty", "10.96.88.11", udpSvcPort("dns", 53))})
	c := udpDial(t, podCtx(t, nextLoopbackIP()), "10.96.88.11:53")
	if _, err := c.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("q")); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("second write: %v, want ECONNREFUSED", err)
	}
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows", n)
	}
}

// UD12: a ClusterIP and a port without a Service port: no error, no answer,
// no flow.
func TestUDPServiceNoPort(t *testing.T) {
	eip := nextLoopbackIP()
	s := flowNode(t, []*v1.Service{udpService("dns", "10.96.88.12", udpSvcPort("dns", 53))}, udpSlice("dns", udpEPPort("dns", 54), eip))
	ep := udpListen(t, podCtx(t, eip), ":54")
	c := udpDial(t, podCtx(t, nextLoopbackIP()), "10.96.88.12:54")
	for range 2 {
		if _, err := c.Write([]byte("q")); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	udpNothing(t, c)
	udpNothing(t, ep)
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows", n)
	}
}

// UD13: a pod sends to its own Service and gets the datagram. The answer
// comes from the ClusterIP.
func TestUDPServiceOwnPod(t *testing.T) {
	ip := nextLoopbackIP()
	flowNode(t, []*v1.Service{udpService("self", "10.96.88.14", udpSvcPort("u", 53))}, udpSlice("self", udpEPPort("u", 8053), ip))
	ctx := podCtx(t, ip)
	ep := udpListen(t, ctx, ":8053")
	c := udpDial(t, ctx, "10.96.88.14:53")
	from := udpAsk(t, c, "self", ep)
	ep.WriteTo([]byte("back"), from)
	if b, from := udpRecv(t, c); string(b) != "back" || from.String() != "10.96.88.14:53" {
		t.Errorf("client got %q from %v", b, from)
	}
}

// FL1: one client to two ClusterIPs with the same endpoint gives two flows,
// also with one client socket for both (fact 11 of W4.5a): the tuple of the
// answers then gets another source port, as the source NAT of Linux does.
// Each answer comes from its own ClusterIP.
func TestUDPFlowTuples(t *testing.T) {
	eip := nextLoopbackIP()
	s := flowNode(t, []*v1.Service{
		udpService("a", "10.96.88.15", udpSvcPort("u", 53)),
		udpService("b", "10.96.88.16", udpSvcPort("u", 53)),
	}, udpSlice("a", udpEPPort("u", 53), eip), udpSlice("b", udpEPPort("u", 53), eip))
	ep := udpListen(t, podCtx(t, eip), ":53")
	ctx := podCtx(t, nextLoopbackIP())
	a := udpDial(t, ctx, "10.96.88.15:53")
	b := udpDial(t, ctx, "10.96.88.16:53")
	udpAsk(t, a, "a", ep)
	udpAsk(t, b, "b", ep)
	if n := s.flowCount(); n != 2 {
		t.Fatalf("%d flows, want 2", n)
	}
	for _, c := range []*udpConn{a, b} {
		ep.WriteTo([]byte("x"), c.LocalAddr())
		if _, from := udpRecv(t, c); from.String() != c.RemoteAddr().String() {
			t.Errorf("%v got the answer from %v", c.LocalAddr(), from)
		}
	}

	// One unconnected socket to the two ClusterIPs, two rounds.
	u := udpListen(t, ctx, ":0")
	for round := range 2 {
		var froms []net.Addr
		for i, svc := range []string{"10.96.88.15:53", "10.96.88.16:53"} {
			u.WriteTo([]byte(strconv.Itoa(i)), udpTo(svc))
			b, from := udpRecv(t, ep)
			if string(b) != strconv.Itoa(i) {
				t.Fatalf("round %d: endpoint got %q", round, b)
			}
			froms = append(froms, from)
		}
		if froms[0].String() != u.LocalAddr().String() || froms[1].String() == froms[0].String() {
			t.Errorf("round %d: the endpoint sees the sources %v", round, froms)
		}
		for i, svc := range []string{"10.96.88.15:53", "10.96.88.16:53"} {
			ep.WriteTo([]byte("x"), froms[i])
			if _, from := udpRecv(t, u); from.String() != svc {
				t.Errorf("round %d: the answer to %s comes from %v", round, svc, from)
			}
		}
	}
	if n := s.flowCount(); n != 4 {
		t.Errorf("%d flows, want 4", n)
	}
	// FL13 also removes the flow with the other source port.
	theTabNode.releaseIP(u.local.Addr().String())
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows after releaseIP", n)
	}
}

// flowPair is a client and an endpoint socket, without a Service.
func flowPair(t *testing.T) (s *serviceProxy, c, ep *udpConn) {
	eip := nextLoopbackIP()
	s = flowNode(t, nil)
	ep = udpListen(t, podCtx(t, eip), ":7000")
	c = udpDial(t, podCtx(t, nextLoopbackIP()), net.JoinHostPort(eip, "7000"))
	return s, c, ep
}

// FL2: a flow without answers closes after 30 seconds without a datagram.
func TestUDPFlowTimeoutNoAnswer(t *testing.T) {
	move := flowClock(t)
	s, c, ep := flowPair(t)
	udpAsk(t, c, "q", ep)
	move(29 * time.Second)
	if n := s.flowCount(); n != 1 {
		t.Fatalf("%d flows after 29 s", n)
	}
	move(2 * time.Second)
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows after 31 s", n)
	}
}

// FL3: a flow with answers that is active 2 seconds after its first
// datagram stays 120 seconds.
func TestUDPFlowTimeoutStream(t *testing.T) {
	move := flowClock(t)
	s, c, ep := flowPair(t)
	from := udpAsk(t, c, "q", ep)
	ep.WriteTo([]byte("a"), from)
	udpRecv(t, c)
	move(3 * time.Second)
	udpAsk(t, c, "q", ep)
	move(100 * time.Second)
	if n := s.flowCount(); n != 1 {
		t.Fatalf("%d flows after 100 s", n)
	}
	move(21 * time.Second)
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows after 121 s", n)
	}
}

// FL4: a query and its answer: the flow closes after 30 seconds.
func TestUDPFlowTimeoutAnswer(t *testing.T) {
	move := flowClock(t)
	s, c, ep := flowPair(t)
	from := udpAsk(t, c, "q", ep)
	move(time.Second)
	ep.WriteTo([]byte("a"), from)
	udpRecv(t, c)
	move(31 * time.Second)
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows after 31 s", n)
	}
	// A datagram after the end makes a new flow.
	udpAsk(t, c, "q", ep)
	if n := s.flowCount(); n != 1 {
		t.Errorf("%d flows after a new datagram", n)
	}
}

// The timer removes the closed flows that no datagram looks up.
func TestUDPFlowSweep(t *testing.T) {
	move := flowClock(t)
	s, c, ep := flowPair(t)
	udpAsk(t, c, "q", ep)
	move(31 * time.Second)
	s.sweepFlows()
	s.flowMu.Lock()
	n, timer := len(s.flows), s.flowTimer
	s.flowMu.Unlock()
	if n != 0 || timer != nil {
		t.Errorf("after the sweep: %d tuples, timer %v", n, timer)
	}
}

// FL5: an EndpointSlice without the endpoint removes the flow. The next
// datagram goes to the other endpoint.
func TestUDPFlowEndpointGone(t *testing.T) {
	e1, e2 := nextLoopbackIP(), nextLoopbackIP()
	es := udpSlice("two", udpEPPort("u", 53), e1, e2)
	s := flowNode(t, []*v1.Service{udpService("two", "10.96.88.17", udpSvcPort("u", 53))}, es)
	eps := map[string]*udpConn{e1: udpListen(t, podCtx(t, e1), ":53"), e2: udpListen(t, podCtx(t, e2), ":53")}
	c := udpDial(t, podCtx(t, nextLoopbackIP()), "10.96.88.17:53")
	c.Write([]byte("1"))
	var first, other string
	for ip, ep := range eps {
		ep.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, _, err := ep.ReadFrom(make([]byte, 8)); err == nil {
			first = ip
		} else {
			other = ip
		}
		ep.SetReadDeadline(time.Time{})
	}
	if first == "" || other == "" {
		t.Fatalf("the datagram went to %q", first)
	}
	// The flow keeps the endpoint.
	udpAsk(t, c, "2", eps[first])

	gone := udpSlice("two", udpEPPort("u", 53), other)
	s.slices.Update(gone)
	s.pruneFlows() // the event handler of the EndpointSlices in run
	if n := s.flowCount(); n != 0 {
		t.Fatalf("%d flows after the endpoint left", n)
	}
	udpAsk(t, c, "3", eps[other])
	udpNothing(t, eps[first])
}

// FL10: with sessionAffinity ClientIP, 10 sockets of one client get one
// endpoint. Without it, each new flow gets an endpoint at random, so both
// get some (a fixed seed keeps the test repeatable).
func TestUDPFlowAffinity(t *testing.T) {
	e1, e2 := nextLoopbackIP(), nextLoopbackIP()
	sticky := udpService("sticky", "10.96.88.18", udpSvcPort("u", 53))
	sticky.Spec.SessionAffinity = v1.ServiceAffinityClientIP
	plain := udpService("plain", "10.96.88.19", udpSvcPort("u", 53))
	s := flowNode(t, []*v1.Service{sticky, plain}, udpSlice("sticky", udpEPPort("u", 53), e1, e2), udpSlice("plain", udpEPPort("u", 53), e1, e2))
	seedProxy(s, 1)
	ep1, ep2 := udpListen(t, podCtx(t, e1), ":53"), udpListen(t, podCtx(t, e2), ":53")
	ctx := podCtx(t, nextLoopbackIP())
	count := func(addr string) (n1, n2 int) {
		for i := range 10 {
			udpDial(t, ctx, addr).Write([]byte(strconv.Itoa(i)))
		}
		for _, x := range []struct {
			c *udpConn
			n *int
		}{{ep1, &n1}, {ep2, &n2}} {
			for {
				x.c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
				if _, _, err := x.c.ReadFrom(make([]byte, 8)); err != nil {
					break
				}
				*x.n++
			}
		}
		return n1, n2
	}
	if n1, n2 := count("10.96.88.18:53"); n1+n2 != 10 || (n1 != 0 && n2 != 0) {
		t.Errorf("sessionAffinity: %d and %d datagrams, want 10 at one endpoint", n1, n2)
	}
	if n1, n2 := count("10.96.88.19:53"); n1+n2 != 10 || n1 == 0 || n2 == 0 {
		t.Errorf("random: %d and %d datagrams, want 10 at both endpoints", n1, n2)
	}
	if n := s.flowCount(); n != 20 {
		t.Errorf("%d flows, want 20", n)
	}
}

// FL11: internalTrafficPolicy Local. The local endpoint gets each
// datagram, never the endpoint on node-1 (TestUDPNodeLocalPolicy checks
// that the remote endpoint gets datagrams without Local).
func TestUDPFlowLocalPolicy(t *testing.T) {
	eip := nextLoopbackIP()
	svc := udpService("local", "10.96.88.20", udpSvcPort("u", 53))
	svc.Spec.InternalTrafficPolicy = ptr.To(v1.ServiceInternalTrafficPolicyLocal)
	es := udpSlice("local", udpEPPort("u", 53), eip)
	es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.244.1.6"}, NodeName: ptr.To("node-1"), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	s := flowNode(t, []*v1.Service{svc}, es)
	s.mu.Lock()
	target := s.udpTargets[netip.MustParseAddrPort("10.96.88.20:53")]
	s.mu.Unlock()
	if target == nil || !target.local || target.proto != v1.ProtocolUDP {
		t.Fatalf("target %+v", target)
	}
	ep := udpListen(t, podCtx(t, eip), ":53")
	ctx := podCtx(t, nextLoopbackIP())
	for i := range 4 {
		udpAsk(t, udpDial(t, ctx, "10.96.88.20:53"), strconv.Itoa(i), ep)
	}
}

// NodePort: the node IP and the node port reach the endpoint inside the tab.
func TestUDPFlowNodePort(t *testing.T) {
	eip := nextLoopbackIP()
	svc := udpService("np", "10.96.88.21", v1.ServicePort{Name: "u", Port: 53, NodePort: 30053, Protocol: v1.ProtocolUDP})
	svc.Spec.Type = v1.ServiceTypeNodePort
	flowNode(t, []*v1.Service{svc}, udpSlice("np", udpEPPort("u", 5300), eip))
	ep := udpListen(t, podCtx(t, eip), ":5300")
	c := udpDial(t, podCtx(t, nextLoopbackIP()), net.JoinHostPort(tabNodeIP, "30053"))
	from := udpAsk(t, c, "np", ep)
	ep.WriteTo([]byte("a"), from)
	if _, from := udpRecv(t, c); from.String() != net.JoinHostPort(tabNodeIP, "30053") {
		t.Errorf("answer from %v", from)
	}
}

// FL12: a udpPolicy that denies makes no flow. A later udpPolicy that
// allows gives the next datagram at once.
func TestUDPFlowPolicyDeny(t *testing.T) {
	s, c, ep := flowPair(t)
	var deny bool
	var calls int
	setUDPPolicy(t, func(ctx context.Context, src, dst netip.AddrPort) bool {
		calls++
		if src.String() != c.LocalAddr().String() || dst.String() != ep.LocalAddr().String() {
			t.Errorf("udpPolicy(%v, %v)", src, dst)
		}
		return !deny
	})
	deny = true
	if _, err := c.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	udpNothing(t, ep)
	if n := s.flowCount(); n != 0 {
		t.Fatalf("%d flows after a denied datagram", n)
	}
	deny = false
	udpAsk(t, c, "q", ep)
	udpAsk(t, c, "q", ep) // FL7: no new examination
	if calls != 2 {
		t.Errorf("udpPolicy ran %d times, want 2", calls)
	}
}

// FL13: releaseIP removes the flows of the IP.
func TestUDPFlowReleaseIP(t *testing.T) {
	s, c, ep := flowPair(t)
	udpAsk(t, c, "q", ep)
	theTabNode.releaseIP(c.local.Addr().String())
	if n := s.flowCount(); n != 0 {
		t.Errorf("%d flows after releaseIP", n)
	}
}

// recheckFlows removes the flows that allow denies now, and gives their
// number (the API of W4.2b, FL8, FL9).
func TestUDPFlowRecheck(t *testing.T) {
	s, c, ep := flowPair(t)
	udpAsk(t, c, "q", ep)
	other := udpDial(t, podCtx(t, nextLoopbackIP()), ep.LocalAddr().String())
	udpAsk(t, other, "q", ep)
	n := s.recheckFlows(func(src, dst netip.AddrPort) bool {
		if dst != ep.local {
			t.Errorf("recheck gets %v -> %v", src, dst)
		}
		return src != c.local
	})
	if n != 1 || s.flowCount() != 1 {
		t.Errorf("recheckFlows gives %d, %d flows stay", n, s.flowCount())
	}
}
