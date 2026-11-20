package main

import (
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
)

// seedProxy makes the random choices of s repeatable.
func seedProxy(s *serviceProxy, seed uint64) {
	s.rand = rand.New(rand.NewPCG(seed, seed))
}

// TestPickRandom: without affinity each new connection gets one of the
// endpoints at random with the same probability, as kube-proxy in iptables
// mode picks them; every endpoint is picked, and nothing alternates.
func TestPickRandom(t *testing.T) {
	s := newServiceProxy(nil)
	seedProxy(s, 1)
	eps := []endpoint{{ip: "10.0.0.1", port: 80}, {ip: "10.0.0.2", port: 80}, {ip: "10.0.0.3", port: 80}}
	plain := svcPort{namespace: "d", name: "web"}

	const n = 3000
	counts := make([]int, len(eps))
	repeats := 0 // the same endpoint twice in a row: round robin never does that
	last := -1
	for range n {
		i := s.pick(plain, "10.0.0.9", eps)
		if i < 0 || i >= len(eps) {
			t.Fatalf("pick gave %d for %d endpoints", i, len(eps))
		}
		counts[i]++
		if i == last {
			repeats++
		}
		last = i
	}
	for i, c := range counts {
		if c == 0 {
			t.Errorf("endpoint %d never picked: %v", i, counts)
		}
		// 1000 expected; a uniform pick stays well within 15 %.
		if c < n/len(eps)*85/100 || c > n/len(eps)*115/100 {
			t.Errorf("not uniform: %v", counts)
		}
	}
	if repeats == 0 {
		t.Errorf("the picks alternate like round robin: %v", counts)
	}

	// Every endpoint wins within a few picks, whatever the seed.
	for seed := range uint64(20) {
		seedProxy(s, seed)
		seen := map[int]bool{}
		for range 100 {
			seen[s.pick(plain, "", eps)] = true
		}
		if len(seen) != len(eps) {
			t.Errorf("seed %d: only %v picked in 100 tries", seed, seen)
		}
	}
}

// TestPickOne: a single endpoint always wins, with and without affinity.
func TestPickOne(t *testing.T) {
	s := newServiceProxy(nil)
	one := []endpoint{{ip: "10.0.0.1", port: 80}}
	for _, p := range []svcPort{{namespace: "d", name: "web"}, {namespace: "d", name: "sticky", affinity: time.Hour}} {
		for c := range 50 {
			if got := s.pick(p, "10.0.1."+strconv.Itoa(c), one); got != 0 {
				t.Fatalf("%s: pick gave %d", p.name, got)
			}
		}
	}
}

// TestNoEndpoints: a Service without endpoints is rejected, as the REJECT
// rule of kube-proxy does; there is nothing to pick from.
func TestNoEndpoints(t *testing.T) {
	s := newServiceProxy(nil)
	t1 := &svcListener{}
	t1.target.Store(&svcPort{namespace: "d", name: "empty"})
	s.listeners["10.96.0.50:80"] = t1
	if _, ok, err := s.endpointFor("10.96.0.50:80", "10.0.0.9"); !ok || err != errNoEndpoints {
		t.Errorf("endpointFor: ok %v, err %v; want a service without endpoints", ok, err)
	}
}

// TestPickAffinity: with ClientIP affinity a client sticks to its endpoint
// until the timeout, other clients are independent, and an endpoint that is
// gone is replaced. Only the first choice of a client is random.
func TestPickAffinity(t *testing.T) {
	s := newServiceProxy(nil)
	seedProxy(s, 2)
	eps := []endpoint{{ip: "10.0.0.1", port: 80}, {ip: "10.0.0.2", port: 80}, {ip: "10.0.0.3", port: 80}}

	sticky := svcPort{namespace: "d", name: "sticky", affinity: time.Hour}
	first := s.pick(sticky, "10.0.0.9", eps)
	for range 50 {
		if got := s.pick(sticky, "10.0.0.9", eps); got != first {
			t.Fatalf("client moved from %d to %d", first, got)
		}
	}
	// Other clients are bound at random, so all endpoints are in use.
	seen := map[int]bool{}
	for c := range 60 {
		client := "10.0.2." + strconv.Itoa(c)
		i := s.pick(sticky, client, eps)
		seen[i] = true
		if again := s.pick(sticky, client, eps); again != i {
			t.Fatalf("client %s moved from %d to %d", client, i, again)
		}
	}
	if len(seen) != len(eps) {
		t.Errorf("new clients should get every endpoint, got %v", seen)
	}

	// The endpoint is gone: the client gets another one and sticks to it.
	rest := append(append([]endpoint{}, eps[:first]...), eps[first+1:]...)
	moved := s.pick(sticky, "10.0.0.9", rest)
	for range 10 {
		if got := s.pick(sticky, "10.0.0.9", rest); got != moved {
			t.Fatalf("client did not stick after the move: %d, %d", moved, got)
		}
	}

	// The timeout runs from the last use: after it the client is picked
	// anew, so in some of many tries it leaves its endpoint.
	short := svcPort{namespace: "d", name: "short", affinity: time.Millisecond}
	a := s.pick(short, "10.0.0.9", eps)
	left := false
	for range 50 {
		time.Sleep(2 * time.Millisecond)
		if s.pick(short, "10.0.0.9", eps) != a {
			left = true
			break
		}
	}
	if !left {
		t.Errorf("after the timeout the client should be picked anew, got %d each time", a)
	}
}
