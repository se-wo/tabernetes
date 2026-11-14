package main

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
)

// The tests of package W4.3: CoreDNS over UDP (UD18), the TC bit (UD22),
// one chargeDNS for each query over UDP (AC3), nslookup and nc -u on the
// UDP table (UD19).

// dnsTestProxy is testProxy with an index of the EndpointSlices by their
// service, as the informer of the service proxy has.
func dnsTestProxy(svcs ...*v1.Service) *serviceProxy {
	s := testProxy(svcs...)
	s.slices = cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{sliceIndex: func(obj any) ([]string, error) {
		es := obj.(*discoveryv1.EndpointSlice)
		return []string{es.Namespace + "/" + es.Labels[discoveryv1.LabelServiceName]}, nil
	}})
	return s
}

// dnsSlice is an EndpointSlice of the service ns/name with one port for
// each entry of ports ("name/PROTO") and the endpoints ips on node.
func dnsSlice(ns, name, node string, ready bool, ports []string, ips ...string) *discoveryv1.EndpointSlice {
	es := &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: ns, Name: name + "-" + node, Labels: map[string]string{discoveryv1.LabelServiceName: name}},
		AddressType: discoveryv1.AddressTypeIPv4,
	}
	for _, p := range ports {
		pn, proto, _ := strings.Cut(p, "/")
		es.Ports = append(es.Ports, discoveryv1.EndpointPort{Name: ptr.To(pn), Port: ptr.To[int32](53), Protocol: ptr.To(v1.Protocol(proto))})
	}
	for _, ip := range ips {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{ip}, NodeName: ptr.To(node), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ready)}})
	}
	return es
}

// kubeDNSService is kube-system/kube-dns with 53/UDP and 53/TCP.
func kubeDNSService() *v1.Service {
	svc := testService(metav1.NamespaceSystem, "kube-dns", "10.96.0.10")
	svc.Spec.Ports = []v1.ServicePort{
		{Name: "dns", Port: 53, Protocol: v1.ProtocolUDP},
		{Name: "dns-tcp", Port: 53, Protocol: v1.ProtocolTCP},
	}
	return svc
}

// dnsWorld is a tab node with a CoreDNS pod: its UDP socket and its TCP
// listener on port 53 answer from s.
type dnsWorld struct {
	s     *serviceProxy
	ip    string // the pod IP of CoreDNS
	stats *corednsStats
}

func newDNSWorld(t *testing.T, svcs ...*v1.Service) *dnsWorld {
	t.Helper()
	ext := testService("default", "ext", "")
	ext.Spec.Type = v1.ServiceTypeExternalName
	ext.Spec.ExternalName = "web.default.svc.cluster.local"
	many := testService("default", "many", v1.ClusterIPNone)
	many.Spec.Ports = []v1.ServicePort{{Port: 80}}
	s := dnsTestProxy(append([]*v1.Service{testService("default", "web", "10.96.1.1"), ext, many}, svcs...)...)
	var ips []string
	for i := range 40 {
		ips = append(ips, "10.250.0."+strconv.Itoa(i+1))
	}
	es := dnsSlice("default", "many", "node-1", true, nil, ips...)
	es.Ports = []discoveryv1.EndpointPort{{Name: ptr.To(""), Port: ptr.To[int32](80)}}
	s.slices.Add(es)
	s.setCorefile(".:53 {\n hosts {\n 10.1.2.3 foo.example.com\n }\n}\n")

	_, cidr, _ := net.ParseCIDR(clusterCIDR)
	withTabNode(t, &tabNode{podCIDR: cidr, net: s})

	w := &dnsWorld{s: s, ip: nextLoopbackIP(), stats: &corednsStats{}}
	ctx := podCtx(t, w.ip)
	u := udpListen(t, ctx, ":53")
	go s.serveDNSUDP(ctx, u, w.stats)
	ln, err := net.Listen("tcp", w.ip+":53")
	if err != nil {
		t.Fatal(err)
	}
	go s.serveDNS(ctx, ln, w.stats)
	return w
}

// dnsMsg is a query; udpSize > 0 adds an EDNS0 OPT record.
func dnsMsg(t *testing.T, id uint16, name string, typ dnsmessage.Type, udpSize int) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	if udpSize > 0 {
		b.StartAdditionals()
		var rh dnsmessage.ResourceHeader
		rh.SetEDNS0(udpSize, dnsmessage.RCodeSuccess, false)
		b.OPTResource(rh, dnsmessage.OPTResource{})
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// udpDNS sends one query from a client pod to the CoreDNS pod over UDP and
// gives the answer and its size.
func (w *dnsWorld) udpDNS(t *testing.T, id uint16, name string, typ dnsmessage.Type, udpSize int) (dnsmessage.Message, int) {
	t.Helper()
	c := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	if _, err := c.WriteTo(dnsMsg(t, id, name, typ, udpSize), udpTo(w.ip+":53")); err != nil {
		t.Fatal(err)
	}
	b, from := udpRecv(t, c)
	if from.String() != w.ip+":53" {
		t.Errorf("answer from %v", from)
	}
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Fatal(err)
	}
	if m.ID != id || !m.Response {
		t.Errorf("header %+v", m.Header)
	}
	return m, len(b)
}

func (w *dnsWorld) tcpDNS(t *testing.T, id uint16, name string, typ dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	c, err := net.Dial("tcp", w.ip+":53")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return dnsQuery(t, c, id, name, typ)
}

// UD18: a query over UDP gets the same answer as over TCP (TestDNSOverTCP),
// and the metrics count each protocol.
func TestDNSOverUDP(t *testing.T) {
	w := newDNSWorld(t)
	for i, q := range []struct {
		name string
		typ  dnsmessage.Type
	}{
		{"web.default.svc.cluster.local.", dnsmessage.TypeA},
		{"foo.example.com.", dnsmessage.TypeA},
		{"ext.default.svc.cluster.local.", dnsmessage.TypeA},
		{"1.1.96.10.in-addr.arpa.", dnsmessage.TypePTR},
		{"nope.default.svc.cluster.local.", dnsmessage.TypeA},
		{"web.default.svc.cluster.local.", dnsmessage.TypeAAAA},
	} {
		id := uint16(i + 1)
		u, _ := w.udpDNS(t, id, q.name, q.typ, 0)
		tc := w.tcpDNS(t, id, q.name, q.typ)
		if !reflect.DeepEqual(u, tc) {
			t.Errorf("%s: udp %+v\ntcp %+v", q.name, u, tc)
		}
	}
	m, _ := w.udpDNS(t, 9, "web.default.svc.cluster.local.", dnsmessage.TypeA, 0)
	if m.RCode != dnsmessage.RCodeSuccess || !m.Authoritative || len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{10, 96, 1, 1} {
		t.Errorf("web: %+v", m)
	}
	out := w.stats.metrics()
	for _, want := range []string{
		`proto="tcp",server="dns://:53",type="A",zone="."} 4`,
		`proto="udp",server="dns://:53",type="A",zone="."} 5`,
		`proto="udp",server="dns://:53",type="PTR",zone="."} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics without %s:\n%s", want, out)
		}
	}
	// A query that does not parse gets no answer, and CoreDNS reads on.
	c := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	c.WriteTo([]byte{1, 2, 3}, udpTo(w.ip+":53"))
	udpNothing(t, c)
	w.udpDNS(t, 10, "web.default.svc.cluster.local.", dnsmessage.TypeA, 0)
}

// UD22: 40 addresses do not fit into 512 bytes. The answer over UDP has the
// TC bit, the question and no records; over TCP it is complete. An EDNS0
// OPT record with a larger UDP size gets the complete answer over UDP.
func TestDNSTruncated(t *testing.T) {
	w := newDNSWorld(t)
	const name = "many.default.svc.cluster.local."
	m, size := w.udpDNS(t, 1, name, dnsmessage.TypeA, 0)
	if !m.Truncated || len(m.Answers) != 0 || len(m.Questions) != 1 || m.RCode != dnsmessage.RCodeSuccess || size > dnsUDPSize {
		t.Errorf("udp: TC %v, %d answers, %d bytes: %+v", m.Truncated, len(m.Answers), size, m.Header)
	}
	if tc := w.tcpDNS(t, 2, name, dnsmessage.TypeA); tc.Truncated || len(tc.Answers) != 40 {
		t.Errorf("tcp: TC %v, %d answers", tc.Truncated, len(tc.Answers))
	}
	m, size = w.udpDNS(t, 3, name, dnsmessage.TypeA, 1232)
	if m.Truncated || len(m.Answers) != 40 || size <= dnsUDPSize {
		t.Errorf("udp with EDNS0: TC %v, %d answers, %d bytes", m.Truncated, len(m.Answers), size)
	}
	if len(m.Additionals) != 1 || m.Additionals[0].Header.Type != dnsmessage.TypeOPT {
		t.Errorf("no OPT record in %+v", m.Additionals)
	}
	// An EDNS0 size below 512 is 512.
	if m, _ = w.udpDNS(t, 4, name, dnsmessage.TypeA, 100); !m.Truncated {
		t.Error("EDNS0 100: no TC bit")
	}
	if m, _ = w.udpDNS(t, 5, "web.default.svc.cluster.local.", dnsmessage.TypeA, 100); m.Truncated || len(m.Answers) != 1 {
		t.Errorf("a small answer: %+v", m)
	}
}

// AC3: one query over UDP is charged one time (TestCoreDNSCharge).
func TestCoreDNSChargeUDP(t *testing.T) {
	w := newDNSWorld(t)
	c := testAcct("")
	defer addCoreDNS(c)()
	if m, _ := w.udpDNS(t, 1, "web.default.svc.cluster.local.", dnsmessage.TypeA, 0); len(m.Answers) != 1 {
		t.Fatalf("web: %+v", m)
	}
	if c.cpu != dnsQueryCost {
		t.Errorf("one query charged %v, want %v", c.cpu, dnsQueryCost)
	}
}

// shortNslookup makes the tries of nslookup short for one test.
func shortNslookup(t *testing.T) {
	tries, timeout, tcp := nslookupTries, nslookupTimeout, nslookupTCPTimeout
	nslookupTries, nslookupTimeout, nslookupTCPTimeout = 2, 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { nslookupTries, nslookupTimeout, nslookupTCPTimeout = tries, timeout, tcp })
}

// requests is the number of queries over proto in the metrics.
func (w *dnsWorld) requests(proto string) int {
	w.stats.mu.Lock()
	defer w.stats.mu.Unlock()
	n := 0
	for k, v := range w.stats.req {
		if k[0] == proto {
			n += int(v)
		}
	}
	return n
}

// UD19: nslookup with a server argument uses UDP; the search path sends one
// query for each name until one has an answer, and each one is one
// chargeDNS (AC3). The output is the one of busybox.
func TestNslookupServerUDP(t *testing.T) {
	w := newDNSWorld(t)
	acct := testAcct("")
	defer addCoreDNS(acct)()
	out, code := sh(t, "nslookup web "+w.ip)
	want := "Server:\t\t" + w.ip + "\nAddress:\t" + w.ip + ":53\n\nName:\tweb.default.svc.cluster.local\nAddress: 10.96.1.1\n\n"
	if code != 0 || out != want {
		t.Errorf("got %q (exit %d), want %q", out, code, want)
	}
	if w.requests("udp") != 1 || w.requests("tcp") != 0 || acct.cpu != dnsQueryCost {
		t.Errorf("udp %d, tcp %d, charged %v", w.requests("udp"), w.requests("tcp"), acct.cpu)
	}
	// foo.example.com: three names of the search path, then the name.
	out, code = sh(t, "nslookup foo.example.com "+w.ip)
	if code != 0 || !strings.HasSuffix(out, "Name:\tfoo.example.com\nAddress: 10.1.2.3\n\n") {
		t.Errorf("hosts: %q (exit %d)", out, code)
	}
	if w.requests("udp") != 5 || acct.cpu != 5*dnsQueryCost {
		t.Errorf("search path: udp %d, charged %v", w.requests("udp"), acct.cpu)
	}
	// A dot at the end: no search path.
	sh(t, "nslookup foo.example.com. "+w.ip)
	if w.requests("udp") != 6 {
		t.Errorf("a name with a dot at the end: udp %d", w.requests("udp"))
	}
	out, code = sh(t, "nslookup ext "+w.ip)
	if code != 0 || !strings.Contains(out, "ext.default.svc.cluster.local\tcanonical name = web.default.svc.cluster.local\nName:\tweb.default.svc.cluster.local\nAddress: 10.96.1.1\n") {
		t.Errorf("cname: %q", out)
	}
	out, code = sh(t, "nslookup nope "+w.ip)
	if code != 1 || !strings.HasSuffix(out, "\n\n** server can't find nope.default.svc.cluster.local: NXDOMAIN\n\n") {
		t.Errorf("nxdomain: %q (exit %d)", out, code)
	}
	out, code = sh(t, "nslookup 10.96.1.1 "+w.ip)
	if code != 0 || !strings.HasSuffix(out, "\n\n1.1.96.10.in-addr.arpa\tname = web.default.svc.cluster.local\n\n") {
		t.Errorf("reverse: %q (exit %d)", out, code)
	}
	out, code = sh(t, "nslookup 10.96.9.9 "+w.ip)
	if code != 1 || !strings.HasSuffix(out, "** server can't find 9.9.96.10.in-addr.arpa: NXDOMAIN\n\n") {
		t.Errorf("reverse nxdomain: %q (exit %d)", out, code)
	}
}

// UD19, UD22: -vc uses TCP at once; an answer with the TC bit makes
// nslookup ask again over TCP.
func TestNslookupTCP(t *testing.T) {
	w := newDNSWorld(t)
	out, code := sh(t, "nslookup -vc web.default.svc.cluster.local. "+w.ip)
	if code != 0 || !strings.HasSuffix(out, "Name:\tweb.default.svc.cluster.local\nAddress: 10.96.1.1\n\n") {
		t.Errorf("-vc: %q (exit %d)", out, code)
	}
	if w.requests("tcp") != 1 || w.requests("udp") != 0 {
		t.Errorf("-vc: udp %d, tcp %d", w.requests("udp"), w.requests("tcp"))
	}
	out, code = sh(t, "nslookup many.default.svc.cluster.local. "+w.ip)
	if code != 0 || strings.Count(out, "Name:\tmany.default.svc.cluster.local\nAddress: 10.250.0.") != 40 {
		t.Errorf("TC: %q (exit %d)", out, code)
	}
	if w.requests("tcp") != 2 || w.requests("udp") != 1 {
		t.Errorf("TC: udp %d, tcp %d", w.requests("udp"), w.requests("tcp"))
	}
}

// UD19: a server without a socket on port 53 refuses the datagram, and a
// server that is not reached gives no answer. nslookup tries again and then
// writes the text of busybox.
func TestNslookupNoServer(t *testing.T) {
	newDNSWorld(t)
	shortNslookup(t)
	for _, server := range []string{nextLoopbackIP(), "10.99.0.1"} {
		start := time.Now()
		out, code := sh(t, "nslookup web "+server)
		if code != 1 || out != nslookupNoServer+"\n\n" {
			t.Errorf("%s: %q (exit %d)", server, out, code)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s: %v", server, d)
		}
	}
}

// UD19, section 8.7: the default server (the ClusterIP of kube-dns) uses UDP
// with a ready endpoint of kube-dns on this tab node, else TCP to the same
// ClusterIP, and without any endpoint the answer of the service proxy.
func TestNslookupDefaultServer(t *testing.T) {
	w := newDNSWorld(t, kubeDNSService())
	clusterIP := netip.MustParseAddrPort("10.96.0.10:53")
	pod := netip.MustParseAddrPort(w.ip + ":53")
	// The Service translation of W4.2a for 10.96.0.10:53/UDP.
	udpHook(t, func(ctx context.Context, src, dst netip.AddrPort) (netip.AddrPort, netip.AddrPort, udpVerdict) {
		switch {
		case dst == clusterIP:
			return src, pod, udpDeliver
		case src == pod:
			return clusterIP, dst, udpDeliver
		}
		return src, dst, udpDeliver
	})
	// The service proxy for 10.96.0.10:53/TCP.
	ln, err := net.Listen("tcp", "10.96.0.10:53")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.s.serveDNS(ctx, ln, w.stats)

	want := "Server:\t\t10.96.0.10\nAddress:\t10.96.0.10:53\n\nName:\tweb.default.svc.cluster.local\nAddress: 10.96.1.1\n\n"
	for _, tc := range []struct {
		name     string
		slice    *discoveryv1.EndpointSlice
		proto    string
		udp, tcp int // the queries that CoreDNS gets
	}{
		{"no endpoint", nil, "", 0, 0},
		{"a local endpoint", dnsSlice("kube-system", "kube-dns", tabNodeName, true, []string{"dns/UDP", "dns-tcp/TCP"}, w.ip), "udp", 1, 0},
		{"a local endpoint that is not ready", dnsSlice("kube-system", "kube-dns", tabNodeName, false, []string{"dns/UDP", "dns-tcp/TCP"}, w.ip), "", 0, 0},
		{"an endpoint on a Docker node", dnsSlice("kube-system", "kube-dns", "node-1", true, []string{"dns/UDP", "dns-tcp/TCP"}, "10.245.0.7"), "tcp", 0, 1},
	} {
		w.s.slices.Replace(nil, "")
		if tc.slice != nil {
			w.s.slices.Add(tc.slice)
		}
		if got := w.s.kubeDNSProto(); got != tc.proto {
			t.Errorf("%s: kubeDNSProto %q, want %q", tc.name, got, tc.proto)
		}
		udp, tcp := w.requests("udp"), w.requests("tcp")
		out, code := sh(t, "nslookup web")
		if code != 0 || out != want {
			t.Errorf("%s: %q (exit %d)", tc.name, out, code)
		}
		if w.requests("udp")-udp != tc.udp || w.requests("tcp")-tcp != tc.tcp {
			t.Errorf("%s: udp %d, tcp %d", tc.name, w.requests("udp")-udp, w.requests("tcp")-tcp)
		}
	}
}

// udpEcho answers each datagram on c with the same bytes until c closes.
func udpEcho(c *udpConn) {
	go func() {
		b := make([]byte, udpMaxDatagram)
		for {
			n, from, err := c.ReadFrom(b)
			if err != nil {
				return
			}
			c.WriteTo(b[:n], from)
		}
	}()
}

// UD19: nc -u sends a line to a UDP echo socket and writes the answer; -w
// ends it; a port without a socket ends it with the result of busybox.
func TestNcUDP(t *testing.T) {
	udpTabNode(t)
	ip := nextLoopbackIP()
	echo := udpListen(t, podCtx(t, ip), ":7")
	udpEcho(echo)
	start := time.Now()
	out, code := sh(t, "echo hello | nc -u -w 0.3 "+ip+" 7")
	if code != 0 || out != "hello\n" {
		t.Errorf("echo: %q (exit %d)", out, code)
	}
	// Without stdin, -w ends it.
	out, code = sh(t, "nc -u -w 0.2 "+ip+" 7 </dev/null")
	if code != 0 || out != "" {
		t.Errorf("no stdin: %q (exit %d)", out, code)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("-w took %v", d)
	}
	// UD9: the datagram is refused.
	out, code = sh(t, "echo hello | nc -u -w 1 "+ip+" 9")
	if code != ncUDPRefusedCode || out != ncUDPRefusedText+strings.Repeat("\n", min(len(ncUDPRefusedText), 1)) {
		t.Errorf("refused: %q (exit %d)", out, code)
	}
	if _, code = sh(t, "nc -u -w 1 nope.invalid 7"); code != 1 {
		t.Errorf("bad address: exit %d", code)
	}
}

// UD19: nc -u -l receives a datagram, writes it, and answers to its source.
func TestNcUDPListen(t *testing.T) {
	udpTabNode(t)
	var wg sync.WaitGroup
	var out string
	var code int
	wg.Go(func() { out, code = sh(t, "echo pong | nc -u -l -w 0.3 -p 5353") })
	addr := netip.AddrPortFrom(netip.MustParseAddr(tabNodeIP), 5353)
	for i := 0; ; i++ {
		udpMu.Lock()
		_, ok := udpSockets[addr]
		udpMu.Unlock()
		if ok {
			break
		}
		if i == 500 {
			t.Fatal("nc -u -l does not bind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := udpListen(t, podCtx(t, nextLoopbackIP()), ":0")
	c.WriteTo([]byte("ping\n"), net.UDPAddrFromAddrPort(addr))
	// Each read of stdin is one datagram: "pong\n" can come in two.
	var got string
	for len(got) < len("pong\n") {
		b, from := udpRecv(t, c)
		if from.String() != addr.String() {
			t.Errorf("answer from %v", from)
		}
		got += string(b)
	}
	if got != "pong\n" {
		t.Errorf("answer %q", got)
	}
	wg.Wait()
	if code != 0 || out != "ping\n" {
		t.Errorf("nc -u -l: %q (exit %d)", out, code)
	}
	// Without a datagram, -w ends it.
	if _, code := sh(t, "nc -u -l -w 0.1 -p 5354"); code != 1 {
		t.Errorf("no datagram: exit %d", code)
	}
}
