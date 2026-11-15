package main

// The UDP flows of the tab node (docs/wasi-sockets.md, section 8.8): the
// Service translation and the conntrack of kube-proxy for the UDP table
// (tabnode_udp.go). The table calls udpRoute for each datagram. The first
// datagram of a flow gets the Service translation and the examination of
// udpPolicy (FL6). The next datagrams in the two directions use the flow
// (FL7): the answers of the endpoint get the ClusterIP and port as their
// source (UD15).

import (
	"context"
	"net/netip"
	"slices"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
)

// The timeouts of a flow (fact 16, the defaults of nf_conntrack on Linux).
const (
	flowTimeout       = 30 * time.Second  // FL2, FL4
	flowStreamTimeout = 120 * time.Second // FL3
	flowStreamAfter   = 2 * time.Second   // FL3: an answer, and a datagram 2 s after the first one
	flowSweepEvery    = 10 * time.Second  // the timer that removes the flows that no datagram looks up
)

// flowNow is the clock of the flow table. Tests change it (plan rule 5).
var flowNow = time.Now

// udpPolicy decides the first datagram of a new flow (FL6). src and dst are
// the tuple after the Service translation. false drops the datagram, and the
// table makes no flow (FL12). W4.2b sets it in an init function of
// tabnode_netpol.go. The default allows each datagram.
var udpPolicy = func(ctx context.Context, src, dst netip.AddrPort) bool { return true }

func init() {
	udpRoute = routeUDP
}

// routeUDP is udpRoute of the UDP table: the flow table of the service proxy
// of this tab node. Without a service proxy (tests of the table only), each
// datagram goes to its destination.
func routeUDP(ctx context.Context, src, dst netip.AddrPort) (from, to netip.AddrPort, v udpVerdict) {
	n := theTabNode
	if n == nil || n.net == nil {
		return src, dst, udpDeliver
	}
	return n.net.routeUDP(ctx, src, dst)
}

// flowTuple is the source and the destination of a datagram.
type flowTuple struct{ src, dst netip.AddrPort }

// udpFlow is one flow of section 8.8 (FL1). The tuple before the Service
// translation is orig. The tuple after it is via and ep.
type udpFlow struct {
	orig    flowTuple      // the client and the destination that the client gave
	ep      netip.AddrPort // the endpoint: the destination after the translation
	via     netip.AddrPort // the source that the endpoint sees: orig.src, or another port at a clash
	service bool           // orig.dst is a ClusterIP or a node port of a UDP Service

	first     time.Time // the first datagram
	lastOrig  time.Time // the last datagram from the client
	lastReply time.Time // the last datagram from the endpoint, zero for none
	stream    bool      // FL3: answers, and active 2 s after the first datagram
}

// flowEntry is a flow in the map of the tuples: reply is true for the tuple
// of the answers, from the endpoint to the client.
type flowEntry struct {
	f     *udpFlow
	reply bool
}

// replyTuple is the tuple of the answers of the endpoint.
func (f *udpFlow) replyTuple() flowTuple { return flowTuple{src: f.ep, dst: f.via} }

func (f *udpFlow) last() time.Time {
	if f.lastReply.After(f.lastOrig) {
		return f.lastReply
	}
	return f.lastOrig
}

// expired reports whether the flow is closed at now (FL2 to FL4).
func (f *udpFlow) expired(now time.Time) bool {
	timeout := flowTimeout
	if f.stream {
		timeout = flowStreamTimeout
	}
	return now.Sub(f.last()) >= timeout
}

// touch notes a datagram of the flow at now. As nf_conntrack does, a flow
// with an answer becomes a stream at a datagram 2 seconds or more after the
// first datagram (FL3).
func (f *udpFlow) touch(now time.Time, reply bool) {
	if reply {
		f.lastReply = now
	} else {
		f.lastOrig = now
	}
	if !f.lastReply.IsZero() && now.Sub(f.first) >= flowStreamAfter {
		f.stream = true
	}
}

// flowTable is the part of serviceProxy for the flows.
type flowTable struct {
	flowMu    sync.Mutex
	flows     map[flowTuple]flowEntry // two tuples for each flow: the client's and the answers'
	flowTimer *time.Timer
}

// routeUDP does the steps of section 8.8 for one datagram.
func (s *serviceProxy) routeUDP(ctx context.Context, src, dst netip.AddrPort) (from, to netip.AddrPort, v udpVerdict) {
	now := flowNow()
	key := flowTuple{src, dst}
	s.flowMu.Lock()
	if e, ok := s.flows[key]; ok {
		if !e.f.expired(now) {
			// An open flow: no new decision (FL7).
			e.f.touch(now, e.reply)
			if e.reply {
				from, to = e.f.orig.dst, e.f.orig.src // UD15
			} else {
				from, to = e.f.via, e.f.ep
			}
			s.flowMu.Unlock()
			return from, to, udpDeliver
		}
		s.removeFlowLocked(e.f)
	}
	s.flowMu.Unlock()

	// A new flow: the Service translation, then the policy.
	ep, service, v := s.translateUDP(src, dst)
	if v != udpDeliver {
		return src, dst, v
	}
	if !udpPolicy(ctx, src, ep) {
		return src, dst, udpDrop // FL12: no flow
	}
	f := &udpFlow{orig: key, ep: ep, via: src, service: service, first: now, lastOrig: now}
	s.flowMu.Lock()
	s.addFlowLocked(f)
	s.flowMu.Unlock()
	return f.via, ep, udpDeliver
}

// translateUDP is the Service translation of a new datagram from src to dst
// (UD7, UD10 to UD12, UD16): the endpoint, and whether dst is a UDP Service.
// The work of endpoints and pick runs here, inside udpRoute, so the UDP
// table charges it to the sender (AC1).
func (s *serviceProxy) translateUDP(src, dst netip.AddrPort) (netip.AddrPort, bool, udpVerdict) {
	s.mu.Lock()
	t := s.udpTargets[dst]
	cluster := s.clusterIPSet[dst.Addr()]
	s.mu.Unlock()
	if t == nil {
		if cluster {
			return dst, false, udpDrop // UD12: no rule of kube-proxy matches
		}
		return dst, false, udpDeliver
	}
	eps := s.endpoints(*t)
	if len(eps) == 0 {
		return dst, true, udpRefuse // UD10, UD11, fact 17
	}
	// Until phase 5, only the endpoints on this tab node (section 8.8). A
	// Service with only endpoints on other nodes drops the datagram without
	// an error. W5.1 sends these datagrams on the udp channel at this point.
	local := slices.DeleteFunc(slices.Clone(eps), func(e endpoint) bool { return e.remote })
	if len(local) == 0 {
		return dst, true, udpDrop
	}
	e := local[s.pick(*t, src.Addr().String(), local)] // FL10, FL11
	ip, err := netip.ParseAddr(e.ip)
	if err != nil {
		return dst, true, udpDrop
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(e.port)), true, udpDeliver
}

// addFlowLocked adds f. If another flow has the tuple of the answers of f
// (the same endpoint and client, another original destination), f gets
// another source port, as the source NAT of Linux does at a clash: the two
// flows stay apart, and each answer comes from its own original
// destination (section 8.8). A flow that still has one of the tuples of f
// ends: the newest flow wins.
func (s *serviceProxy) addFlowLocked(f *udpFlow) {
	if s.flows == nil {
		s.flows = map[flowTuple]flowEntry{}
	}
	if e, ok := s.flows[f.replyTuple()]; ok && e.f.orig != f.orig {
		if p, ok := s.freeViaLocked(f); ok {
			f.via = p
		}
	}
	reply := f.replyTuple()
	for _, k := range []flowTuple{f.orig, reply} {
		if old, ok := s.flows[k]; ok && old.f != f {
			s.removeFlowLocked(old.f)
		}
	}
	s.flows[f.orig] = flowEntry{f: f}
	if reply != f.orig { // a socket that sends to itself has one tuple
		s.flows[reply] = flowEntry{f: f, reply: true}
	}
	if s.flowTimer == nil {
		s.flowTimer = time.AfterFunc(flowSweepEvery, s.sweepFlows)
	}
}

// freeViaLocked gives a source port for f on the IP of the client that no
// flow to the endpoint of f and no socket of the UDP table uses.
func (s *serviceProxy) freeViaLocked(f *udpFlow) (netip.AddrPort, bool) {
	ip := f.orig.src.Addr()
	for port := proxyEphemeralFirst; port <= proxyEphemeralLast; port++ {
		via := netip.AddrPortFrom(ip, uint16(port))
		if _, used := s.flows[flowTuple{src: f.ep, dst: via}]; used {
			continue
		}
		if _, used := s.flows[flowTuple{src: via, dst: f.ep}]; used {
			continue
		}
		udpMu.Lock()
		bound := udpSockets[via] != nil
		udpMu.Unlock()
		if !bound {
			return via, true
		}
	}
	return netip.AddrPort{}, false
}

func (s *serviceProxy) removeFlowLocked(f *udpFlow) {
	for _, k := range []flowTuple{f.orig, f.replyTuple()} {
		if e, ok := s.flows[k]; ok && e.f == f {
			delete(s.flows, k)
		}
	}
}

// expireFlowsLocked removes the closed flows.
func (s *serviceProxy) expireFlowsLocked(now time.Time) {
	for _, e := range s.flows {
		if e.f.expired(now) {
			s.removeFlowLocked(e.f)
		}
	}
}

// sweepFlows is the timer of the flows that no datagram looks up.
func (s *serviceProxy) sweepFlows() {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	s.expireFlowsLocked(flowNow())
	switch {
	case len(s.flows) == 0:
		if s.flowTimer != nil {
			s.flowTimer.Stop()
		}
		s.flowTimer = nil
	case s.flowTimer == nil:
		s.flowTimer = time.AfterFunc(flowSweepEvery, s.sweepFlows)
	default:
		s.flowTimer.Reset(flowSweepEvery)
	}
}

// openFlows gives the open flows, each one time.
func (s *serviceProxy) openFlows() []*udpFlow {
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	s.expireFlowsLocked(flowNow())
	var out []*udpFlow
	for _, e := range s.flows {
		if !e.reply {
			out = append(out, e.f)
		}
	}
	return out
}

// removeFlows removes each flow of list that drop selects. It gives the
// number of removed flows.
func (s *serviceProxy) removeFlows(list []*udpFlow, drop func(f *udpFlow) bool) int {
	var gone []*udpFlow
	for _, f := range list {
		if drop(f) {
			gone = append(gone, f)
		}
	}
	s.flowMu.Lock()
	defer s.flowMu.Unlock()
	n := 0
	for _, f := range gone {
		if e, ok := s.flows[f.orig]; ok && e.f == f {
			s.removeFlowLocked(f)
			n++
		}
	}
	return n
}

// recheckFlows examines all open flows again with allow, and removes each
// flow that allow denies now (FL8, FL9). allow gets the tuple of the flow
// after the Service translation. It gives the number of removed flows.
func (s *serviceProxy) recheckFlows(allow func(src, dst netip.AddrPort) bool) int {
	return s.removeFlows(s.openFlows(), func(f *udpFlow) bool { return !allow(f.orig.src, f.ep) })
}

// flowCount gives the number of open flows, for tests.
func (s *serviceProxy) flowCount() int { return len(s.openFlows()) }

// removeFlowsOf removes the flows with ip in a tuple (FL13): releaseIP
// calls it before a new pod can get the IP.
func (s *serviceProxy) removeFlowsOf(ip string) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return
	}
	a = a.Unmap()
	s.removeFlows(s.openFlows(), func(f *udpFlow) bool {
		return f.orig.src.Addr() == a || f.orig.dst.Addr() == a || f.ep.Addr() == a || f.via.Addr() == a
	})
}

// pruneFlows removes the flows of a Service to an endpoint that is no
// longer in the endpoints of the Service, and the flows of a Service port
// that is gone (FL5). The events of the Services and of the EndpointSlices
// start it. The next datagram of the client makes a new flow.
func (s *serviceProxy) pruneFlows() {
	list := s.openFlows()
	current := map[netip.AddrPort]map[netip.AddrPort]bool{} // by orig.dst: the endpoints now
	s.removeFlows(list, func(f *udpFlow) bool {
		if !f.service {
			return false
		}
		eps, ok := current[f.orig.dst]
		if !ok {
			eps = map[netip.AddrPort]bool{}
			s.mu.Lock()
			t := s.udpTargets[f.orig.dst]
			s.mu.Unlock()
			if t != nil {
				for _, e := range s.endpoints(*t) {
					if ip, err := netip.ParseAddr(e.ip); err == nil {
						eps[netip.AddrPortFrom(ip.Unmap(), uint16(e.port))] = true
					}
				}
			}
			current[f.orig.dst] = eps
		}
		return !eps[f.ep]
	})
}

// udpTargetsOf gives the UDP Service ports of svc by address: the ClusterIPs
// and, for NodePort and LoadBalancer, the node IP with the node port (inside
// the tab only, section 11). The address 127.0.0.1 is not necessary: the UDP
// table changes 127.0.0.0/8 to the IP of the socket before udpRoute.
func udpTargetsOf(svc *v1.Service, p v1.ServicePort, cluster, node svcPort, want map[netip.AddrPort]*svcPort) {
	for _, ip := range clusterIPs(svc) {
		if a, err := netip.ParseAddr(ip); err == nil {
			t := cluster
			want[netip.AddrPortFrom(a.Unmap(), uint16(p.Port))] = &t
		}
	}
	if p.NodePort > 0 && (svc.Spec.Type == v1.ServiceTypeNodePort || svc.Spec.Type == v1.ServiceTypeLoadBalancer) {
		if a, err := netip.ParseAddr(tabNodeIP); err == nil {
			t := node
			want[netip.AddrPortFrom(a.Unmap(), uint16(p.NodePort))] = &t
		}
	}
}
