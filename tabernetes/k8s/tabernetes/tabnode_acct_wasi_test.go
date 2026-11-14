package main

import (
	"testing"
	"time"
)

// AC2: the cpu message of a pod worker goes to the account of its container,
// and not to the time of the Go thread.
func TestWasiCPUCharges(t *testing.T) {
	h := wasiHandlers["cpu"]
	if h == nil {
		t.Fatal("no handler for cpu")
	}
	p, _ := testPeer(t, map[string]wasiHandler{"cpu": h}, nil)
	c := &ctr{id: "cpu-test", wasm: true}
	t.Cleanup(func() { forgetAcct(c) })

	// Without a container in the state of the peer, nothing is charged.
	other, _ := testPeer(t, map[string]wasiHandler{"cpu": h}, nil)
	before := idleCPU.get()
	other.dispatch(msg(map[string]any{"type": "cpu", "ms": 100}))
	// startComponent puts the container into the state before "start".
	p.State(wasiCPUState, func() any { return c })
	p.dispatch(msg(map[string]any{"type": "cpu", "ms": 1500}))
	p.dispatch(msg(map[string]any{"type": "cpu", "ms": 250.5}))
	want := 1750500 * time.Microsecond
	deadline := time.Now().Add(5 * time.Second)
	var got time.Duration
	for time.Now().Before(deadline) {
		got = time.Duration(acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds)
		if got >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got != want {
		t.Fatalf("charged %v, want %v", got, want)
	}
	if d := idleCPU.get() - before; d != want {
		t.Fatalf("the time off the Go thread grew by %v, want %v", d, want)
	}
	if wasmProfile.idle != 0 {
		t.Fatalf("wasmProfile has an idle rate %v", wasmProfile.idle)
	}
}
