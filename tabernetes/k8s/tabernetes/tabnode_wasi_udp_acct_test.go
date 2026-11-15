package main

import (
	"bytes"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
)

// AC1 for UDP, the combined test (docs/wasi-sockets.md, section 10.3): a
// component (a fake pod worker) sends 10 MB through a UDP Service and
// receives the answers of the endpoint. The CPU of the container
// increases, and the control plane pods get none of the charged time. The
// TCP part is TestWasiTCPChargesCPU; TestWasiUDPChargesCPU and
// TestUDPChargesCPU charge one direction without a Service.
func TestWasiUDPServiceChargesCPU(t *testing.T) {
	c := &ctr{id: "udp-acct-test", wasm: true}
	t.Cleanup(func() { forgetAcct(c) })
	w := udpPeer(t, nextLoopbackIP(), c)
	epIP := nextLoopbackIP()
	const clusterIP = "10.96.88.47"
	flowNode(t, []*v1.Service{udpService("echo", clusterIP, udpSvcPort("u", 7))}, udpSlice("echo", udpEPPort("u", 7007), epIP))
	// The endpoint answers each datagram with the same bytes.
	ep := udpListen(t, podCtx(t, epIP), ":7007")
	go func() {
		buf := make([]byte, udpMaxDatagram)
		for {
			n, from, err := ep.ReadFrom(buf)
			if err != nil {
				return
			}
			ep.WriteTo(buf[:n], from)
		}
	}()
	w.udpBound(1, "0.0.0.0", 0)

	var busy time.Duration
	measured := true
	var etcd int64
	rest := map[string]int64{compAPIServer: 0}
	fakeBusy(t, &busy, &measured)
	fakeBytes(t, &etcd, rest)
	var m nodeSysModel
	now := time.Now()
	m.sample(now, 1, true)
	m.sample(now.Add(time.Second), 1, true)
	before := acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds
	ctrsBefore, idleBefore := nodeCPU.get(), idleCPU.get()

	// One datagram after the other, within the credits of rule 6: the Go
	// side gives the credit of a sent datagram back, and the pod worker
	// gives the credit of a received datagram back.
	const size, chunk = 10 << 20, 60000
	credit, sent, answered := udpCredit, 0, 0
	for sent < size {
		n := min(chunk, size-sent)
		for credit < n {
			credit += w.wait("credit", 1).Get("n").Int()
		}
		data := bytes.Repeat([]byte{byte(sent / chunk)}, n)
		w.datagram(1, clusterIP, 7, data)
		credit -= n
		sent += n
		got, from := w.udpGot(1)
		// UD15: the boundaries and the source of the answer.
		if !bytes.Equal(got, data) || from != clusterIP+":7" {
			t.Fatalf("answer of %d bytes from %s, want %d bytes from %s:7", len(got), from, n, clusterIP)
		}
		answered += len(got)
		w.send(map[string]any{"type": "credit", "socket": 1, "kind": "datagram-bytes", "n": len(got)})
	}
	if answered != size {
		t.Fatalf("%d bytes answered, want %d", answered, size)
	}
	var after uint64
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if after = acctOf(c).Usage(time.Now(), false).UsageCoreNanoSeconds; after > before {
			break
		}
	}
	if after <= before {
		t.Fatalf("CPU of the container %d, before %d: nothing charged", after, before)
	}

	// The instance was busy for the charged time only, while the control
	// plane moved datagrams: the control plane pods get none of it.
	charged := max(nodeCPU.get()-ctrsBefore-(idleCPU.get()-idleBefore), 0)
	if charged == 0 {
		t.Fatal("the node counts no charged time")
	}
	busy += charged
	rest[compAPIServer] += 1000
	cp := m.sample(now.Add(2*time.Second), 1, true)
	for _, name := range compNames {
		if u := cp.comps[name].UsageCoreNanoSeconds; u != 0 {
			t.Errorf("control plane pod %s got %d ns of the charged time", name, u)
		}
	}
	// Without the charge, the same busy time goes to the control plane.
	var m2 nodeSysModel
	busy = 0
	m2.sample(now, 1, true)
	m2.sample(now.Add(time.Second), 1, true)
	busy += charged
	rest[compAPIServer] += 1000
	if u := m2.sample(now.Add(2*time.Second), 1, true).comps[compAPIServer].UsageCoreNanoSeconds; u == 0 {
		t.Error("the model gives no time to the control plane without a charge")
	}
	if w.p.Drops() != 0 {
		t.Errorf("%d drops", w.p.Drops())
	}
}
