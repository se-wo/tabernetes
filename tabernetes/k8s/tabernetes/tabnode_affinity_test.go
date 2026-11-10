package main

import (
	"testing"
	"time"
)

// TestPickAffinity: without affinity the endpoints rotate; with ClientIP
// affinity a client sticks to its endpoint until the timeout, other clients
// are independent, and an endpoint that is gone is replaced.
func TestPickAffinity(t *testing.T) {
	s := newServiceProxy(nil)
	eps := []endpoint{{ip: "10.0.0.1", port: 80}, {ip: "10.0.0.2", port: 80}, {ip: "10.0.0.3", port: 80}}

	plain := svcPort{namespace: "d", name: "web"}
	seen := map[int]bool{}
	for range 3 {
		seen[s.pick(plain, "10.0.0.9", eps)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("without affinity the endpoints rotate, got %v", seen)
	}

	sticky := svcPort{namespace: "d", name: "sticky", affinity: time.Hour}
	first := s.pick(sticky, "10.0.0.9", eps)
	for range 5 {
		if got := s.pick(sticky, "10.0.0.9", eps); got != first {
			t.Fatalf("client moved from %d to %d", first, got)
		}
	}
	if other := s.pick(sticky, "10.0.0.8", eps); other == first {
		t.Errorf("a second client should get the next endpoint, got %d again", other)
	}

	// The endpoint is gone: the client gets another one and sticks to it.
	rest := append(append([]endpoint{}, eps[:first]...), eps[first+1:]...)
	moved := s.pick(sticky, "10.0.0.9", rest)
	for range 3 {
		if got := s.pick(sticky, "10.0.0.9", rest); got != moved {
			t.Fatalf("client did not stick after the move: %d, %d", moved, got)
		}
	}

	// The timeout runs from the last use.
	short := svcPort{namespace: "d", name: "short", affinity: 50 * time.Millisecond}
	a := s.pick(short, "10.0.0.9", eps)
	time.Sleep(80 * time.Millisecond)
	s.pick(short, "10.0.0.7", eps) // advances the rotation
	if b := s.pick(short, "10.0.0.9", eps); b == a {
		t.Errorf("after the timeout the client should be rotated on, got %d again", b)
	}
}
