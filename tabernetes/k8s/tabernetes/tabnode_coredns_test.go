package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

const shippedCorefile = `.:53 {
    errors
    health {
        lameduck 5s
    }
    ready
    prometheus :9153
    kubernetes cluster.local in-addr.arpa ip6.arpa {
        pods insecure
        fallthrough in-addr.arpa ip6.arpa
        ttl 30
    }
    forward . /etc/resolv.conf {
        max_concurrent 1000
    }
    cache 30
    loop
    reload
    loadbalance
}
`

func TestParseCorefile(t *testing.T) {
	cf, err := parseCorefile(shippedCorefile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cf.keys, []string{".:53"}) {
		t.Errorf("keys %v", cf.keys)
	}
	for _, p := range []string{"health", "ready", "prometheus", "kubernetes", "forward"} {
		if _, ok := cf.plugins[p]; !ok {
			t.Errorf("plugin %s missing", p)
		}
	}
	if got := cf.plugins["prometheus"]; len(got) != 1 || got[0] != ":9153" {
		t.Errorf("prometheus args %v", got)
	}

	cf, err = parseCorefile(`# comment
.:53 {
    hosts {
        10.1.2.3 Foo.Example.com bar.example.com. # trailing
        ::1 v6.example.com
        fallthrough
    }
    rewrite name exact a.default.svc.cluster.local b.default.svc.cluster.local
    rewrite continue name suffix .old.local .default.svc.cluster.local
    rewrite name regex (.*)\.legacy\.com {1}.default.svc.cluster.local
    rewrite ttl . 5
}
example.org:53 {
    whoami
}
`)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cf.keys, []string{".:53", "example.org:53"}) {
		t.Errorf("keys %v", cf.keys)
	}
	if got := cf.hosts["foo.example.com"]; !reflect.DeepEqual(got, []string{"10.1.2.3"}) {
		t.Errorf("hosts foo: %v", got)
	}
	if _, ok := cf.hosts["bar.example.com"]; !ok {
		t.Errorf("hosts bar: %v", cf.hosts)
	}
	if _, ok := cf.hosts["v6.example.com"]; ok {
		t.Errorf("IPv6 entry kept")
	}
	if len(cf.rewrite) != 3 {
		t.Fatalf("rewrites %d, want 3", len(cf.rewrite))
	}
	for in, want := range map[string]string{
		"a.default.svc.cluster.local": "b.default.svc.cluster.local",
		"web.old.local":               "web.default.svc.cluster.local",
		"x.legacy.com":                "x.default.svc.cluster.local",
		"other.example":               "other.example",
	} {
		if got, _ := cf.name(in); got != want {
			t.Errorf("rewrite %s = %s, want %s", in, got, want)
		}
	}

	for _, bad := range []string{".:53 {\n errors\n", ".:53 {\n}\n}\n", "errors\n", ".:53 {\n rewrite name regex ( a b\n}\n", ".:53 {\n rewrite name bogus a b\n}\n"} {
		if _, err := parseCorefile(bad); err == nil {
			t.Errorf("no error for %q", bad)
		}
	}
}

func testProxy(svcs ...*v1.Service) *serviceProxy {
	s := newServiceProxy(nil)
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, svc := range svcs {
		idx.Add(svc)
	}
	s.services = corelisters.NewServiceLister(idx)
	s.slices = cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{sliceIndex: func(any) ([]string, error) { return nil, nil }})
	s.synced.Store(true)
	return s
}

func testService(ns, name, ip string) *v1.Service {
	return &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: v1.ServiceSpec{ClusterIP: ip}}
}

func TestLookupCorefile(t *testing.T) {
	s := testProxy(testService("default", "web", "10.96.1.1"), testService("kube-system", "kube-dns", "10.96.0.10"))
	if _, ok := s.lookup("foo.example.com", "default"); ok {
		t.Fatal("foo.example.com resolves without a Corefile")
	}
	s.setCorefile(`.:53 {
    hosts {
        10.1.2.3 foo.example.com short
        fallthrough
    }
    rewrite name foo.default.svc.cluster.local web.default.svc.cluster.local
    kubernetes cluster.local
}
`)
	for host, want := range map[string]string{
		"foo.example.com":               "10.1.2.3",
		"foo.example.com.":              "10.1.2.3",
		"short":                         "10.1.2.3",
		"web":                           "10.96.1.1",
		"foo":                           "10.96.1.1", // rewritten, through the search path
		"foo.default.svc.cluster.local": "10.96.1.1",
	} {
		if got, ok := s.resolve(host, "default"); !ok || got != want {
			t.Errorf("resolve %s = %s %v, want %s", host, got, ok, want)
		}
	}
	// A broken Corefile keeps the old rules.
	s.setCorefile(".:53 {\n hosts {\n")
	if st := s.corefile.Load(); st.err == nil {
		t.Error("no error state")
	}
	if got, ok := s.resolve("short", "default"); !ok || got != "10.1.2.3" {
		t.Errorf("after a broken Corefile: %s %v", got, ok)
	}
	// Without the ConfigMap, back to plain names.
	s.corefile.Store(nil)
	if _, ok := s.resolve("short", "default"); ok {
		t.Error("short resolves without a Corefile")
	}
}

func dnsQuery(t *testing.T, c net.Conn, id uint16, name string, typ dnsmessage.Type) dnsmessage.Message {
	t.Helper()
	n := dnsmessage.MustNewName(name)
	b := dnsmessage.NewBuilder(make([]byte, 2, 512), dnsmessage.Header{ID: id, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: n, Type: typ, Class: dnsmessage.ClassINET})
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint16(msg, uint16(len(msg)-2))
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf); err != nil {
		t.Fatal(err)
	}
	if m.ID != id || !m.Response {
		t.Errorf("header %+v", m.Header)
	}
	return m
}

func TestDNSOverTCP(t *testing.T) {
	ext := testService("default", "ext", "")
	ext.Spec.Type = v1.ServiceTypeExternalName
	ext.Spec.ExternalName = "web.default.svc.cluster.local"
	s := testProxy(testService("default", "web", "10.96.1.1"), ext)
	s.setCorefile(".:53 {\n hosts {\n 10.1.2.3 foo.example.com\n }\n}\n")

	ln, err := net.Listen("tcp", "10.244.250.10:53")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stats := &corednsStats{}
	go s.serveDNS(ctx, ln, stats)
	c, err := net.Dial("tcp", "10.244.250.10:53")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	m := dnsQuery(t, c, 1, "web.default.svc.cluster.local.", dnsmessage.TypeA)
	if m.RCode != dnsmessage.RCodeSuccess || !m.Authoritative || len(m.Answers) != 1 {
		t.Fatalf("web: %+v", m)
	}
	if a := m.Answers[0].Body.(*dnsmessage.AResource).A; a != [4]byte{10, 96, 1, 1} {
		t.Errorf("web A %v", a)
	}
	m = dnsQuery(t, c, 2, "foo.example.com.", dnsmessage.TypeA)
	if len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.AResource).A != [4]byte{10, 1, 2, 3} {
		t.Errorf("hosts: %+v", m)
	}
	m = dnsQuery(t, c, 3, "ext.default.svc.cluster.local.", dnsmessage.TypeA)
	if len(m.Answers) != 2 || m.Answers[0].Header.Type != dnsmessage.TypeCNAME {
		t.Errorf("externalname: %+v", m.Answers)
	}
	m = dnsQuery(t, c, 4, "1.1.96.10.in-addr.arpa.", dnsmessage.TypePTR)
	if len(m.Answers) != 1 || m.Answers[0].Body.(*dnsmessage.PTRResource).PTR.String() != "web.default.svc.cluster.local." {
		t.Errorf("ptr: %+v", m)
	}
	m = dnsQuery(t, c, 5, "nope.default.svc.cluster.local.", dnsmessage.TypeA)
	if m.RCode != dnsmessage.RCodeNameError || len(m.Answers) != 0 {
		t.Errorf("nxdomain: %+v", m)
	}
	m = dnsQuery(t, c, 6, "web.default.svc.cluster.local.", dnsmessage.TypeAAAA)
	if m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Errorf("aaaa: %+v", m)
	}
	if out := stats.metrics(); !strings.Contains(out, `type="A",zone="."} 4`) || !strings.Contains(out, "NXDOMAIN") {
		t.Errorf("metrics:\n%s", out)
	}
}

func TestCorednsEvictions(t *testing.T) {
	now := time.Now()
	node := func(name string, tab, ready, unsched bool) *v1.Node {
		n := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		if tab {
			n.Labels[tabTaintKey] = "true"
		}
		n.Spec.Unschedulable = unsched
		st := v1.ConditionFalse
		if ready {
			st = v1.ConditionTrue
		}
		n.Status.Conditions = []v1.NodeCondition{{Type: v1.NodeReady, Status: st}}
		return n
	}
	pod := func(name, ns, node string, labels map[string]string, deleting bool) *v1.Pod {
		p := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}, Spec: v1.PodSpec{NodeName: node}}
		if deleting {
			p.DeletionTimestamp = &metav1.Time{Time: now}
		}
		return p
	}
	dns := map[string]string{"k8s-app": "kube-dns"}
	pods := []*v1.Pod{
		pod("a", "kube-system", "tab", dns, false),
		pod("b", "kube-system", "tab", dns, true),
		pod("c", "kube-system", "docker", dns, false),
		pod("d", "default", "tab", dns, false),
		pod("e", "kube-system", "tab", map[string]string{"k8s-app": "other"}, false),
	}
	tab := node("tab", true, true, false)
	for _, tc := range []struct {
		name  string
		nodes []*v1.Node
		last  time.Time
		want  []string
	}{
		{"only the tab", []*v1.Node{tab}, time.Time{}, nil},
		{"docker ready", []*v1.Node{tab, node("d1", false, true, false)}, time.Time{}, []string{"a"}},
		{"docker not ready", []*v1.Node{tab, node("d1", false, false, false)}, time.Time{}, nil},
		{"docker cordoned", []*v1.Node{tab, node("d1", false, true, true)}, time.Time{}, nil},
		{"another tab node", []*v1.Node{tab, node("w1", true, true, false)}, time.Time{}, nil},
		{"cooldown", []*v1.Node{tab, node("d1", false, true, false)}, now.Add(-time.Minute), nil},
		{"cooldown over", []*v1.Node{tab, node("d1", false, true, false)}, now.Add(-3 * time.Minute), []string{"a"}},
	} {
		var got []string
		for _, p := range corednsEvictions(tc.nodes, pods, "tab", now, tc.last) {
			got = append(got, p.Name)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCorednsImage(t *testing.T) {
	img := lookupImage("registry.k8s.io/coredns/coredns:v1.15.0")
	if img == nil || img.name != "coredns" || img.entrypoint[0] != "/coredns" {
		t.Fatalf("image %+v", img)
	}
	if imagePrograms["coredns"] == nil {
		t.Error("no program")
	}
}
