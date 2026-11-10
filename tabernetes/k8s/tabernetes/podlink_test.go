package main

import (
	"net"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestPodLinkNode(t *testing.T) {
	for _, c := range []struct {
		label, node string
		route, ok   bool
	}{
		{"pod:tab-worker-1", "tab-worker-1", true, true},
		{"pod;from=tab", "tab", false, true},
		{"pod:", "", false, false},
		{"pod;from=", "", false, false},
		{"api", "", false, false},
		{"kubelet:10250", "", false, false},
	} {
		node, route, ok := podLinkNode(c.label)
		if node != c.node || route != c.route || ok != c.ok {
			t.Errorf("podLinkNode(%q) = %q, %v, %v; want %q, %v, %v", c.label, node, route, ok, c.node, c.route, c.ok)
		}
	}
}

func TestParsePodLinkHeader(t *testing.T) {
	src, dst, err := parsePodLinkHeader("TABP/1 tcp 10.244.1.3:0 10.244.0.2:80")
	if err != nil || src.String() != "10.244.1.3:0" || dst.String() != "10.244.0.2:80" {
		t.Errorf("got %v %v %v", src, dst, err)
	}
	for _, bad := range []string{"", "TABP/2 tcp 1.2.3.4:0 1.2.3.5:80", "TABP/1 udp 1.2.3.4:0 1.2.3.5:80", "TABP/1 tcp x 1.2.3.5:80", "TABP/1 tcp 1.2.3.4:0",
		"TABP/1 tcp localhost:0 1.2.3.5:80", "TABP/1 tcp 1.2.3.4:0 example.com:80", "TABP/1 tcp 1.2.3.4:0 1.2.3.5:0"} {
		if _, _, err := parsePodLinkHeader(bad); err == nil {
			t.Errorf("parsePodLinkHeader(%q) did not fail", bad)
		}
	}
	line, err := readPodLinkLine(strings.NewReader("ok\nrest"), 64)
	if line != "ok" || err != nil {
		t.Errorf("readLine = %q, %v", line, err)
	}
	if _, err := readPodLinkLine(strings.NewReader(strings.Repeat("x", 100)), 64); err == nil {
		t.Error("readLine accepted a line that is too long")
	}
}

func TestNodeOwns(t *testing.T) {
	idx := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	idx.Add(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "tab-worker-1"},
		Spec:       v1.NodeSpec{PodCIDR: "10.244.1.0/24", PodCIDRs: []string{"10.244.1.0/24"}},
		Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.2"}}},
	})
	p := &netPolicy{nodes: corelisters.NewNodeLister(idx)}
	for _, c := range []struct {
		node, ip string
		want     bool
	}{
		{"tab-worker-1", "10.244.1.7", true},
		{"tab-worker-1", "10.0.0.2", true},
		{"tab-worker-1", "10.244.0.7", false}, // a pod of another node
		{"tab-worker-1", "10.0.0.1", false},   // another node IP
		{"tab-worker-2", "10.244.1.7", false}, // unknown node
	} {
		if got := p.nodeOwns(c.node, net.ParseIP(c.ip)); got != c.want {
			t.Errorf("nodeOwns(%s, %s) = %v, want %v", c.node, c.ip, got, c.want)
		}
	}
	// A pod channel serves the pod IPs of a node that takes them, not its
	// node IP (hostNetwork pods) and not a node without the label.
	idx.Add(&v1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "docker-1", Labels: map[string]string{podLinkNodeLabel: "v1"}},
		Spec:       v1.NodeSpec{PodCIDRs: []string{"10.244.2.0/24"}},
		Status:     v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.3"}}},
	})
	n := &tabNode{policy: p}
	for _, c := range []struct {
		node, ip string
		want     bool
	}{
		{"docker-1", "10.244.2.4", true},
		{"docker-1", "10.0.0.3", false},
		{"docker-1", "10.244.1.7", false},
		{"tab-worker-1", "10.244.1.7", false}, // no label
		{"gone", "10.244.2.4", false},
	} {
		if got := n.podLinkTarget(c.node, c.ip); got != c.want {
			t.Errorf("podLinkTarget(%s, %s) = %v, want %v", c.node, c.ip, got, c.want)
		}
	}
	if got := p.nodeOf(net.ParseIP("10.244.1.9")); got != "tab-worker-1" {
		t.Errorf("nodeOf(10.244.1.9) = %q, want tab-worker-1", got)
	}
	if got := p.nodeOf(net.ParseIP("10.244.9.9")); got != "" {
		t.Errorf("nodeOf(10.244.9.9) = %q, want none", got)
	}
}
