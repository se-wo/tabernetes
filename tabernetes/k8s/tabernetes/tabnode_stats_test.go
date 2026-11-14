package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/transport"
	"k8s.io/streaming/pkg/httpstream/spdy"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"sigs.k8s.io/yaml"
)

func testAcct(limit string) *ctrAcct {
	spec := v1.Container{Name: "app"}
	if limit != "" {
		spec.Resources.Limits = v1.ResourceList{v1.ResourceCPU: resource.MustParse(limit)}
	}
	return newCtrAcct("id1", spec, imageProfile{memory: 4 << 20}, time.Now())
}

func TestAcctWorkRespectsLimit(t *testing.T) {
	a := testAcct("100m")
	start := time.Now()
	if err := a.Work(context.Background(), 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 300*time.Millisecond {
		t.Errorf("Work(50ms) at 100m took %v, want >= ~300ms (4 throttled periods)", el)
	}
	a.mu.Lock()
	cpu := a.cpu
	a.mu.Unlock()
	if cpu < 49*time.Millisecond || cpu > 51*time.Millisecond {
		t.Errorf("Work charged %v, want ~50ms", cpu)
	}
	free := testAcct("")
	start = time.Now()
	free.Work(context.Background(), 50*time.Millisecond)
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Errorf("Work(50ms) without limit took %v", el)
	}
}

func TestAcctChargeClamps(t *testing.T) {
	a := testAcct("100m") // 10ms per period
	a.ChargeCPU(time.Second)
	a.mu.Lock()
	cpu := a.cpu
	a.mu.Unlock()
	if cpu > 10*time.Millisecond {
		t.Errorf("ChargeCPU(1s) counted %v, want at most the quota of 10ms", cpu)
	}
	var nilAcct *ctrAcct
	nilAcct.ChargeCPU(time.Second) // no panic
}

func TestAcctUsageRate(t *testing.T) {
	a := testAcct("")
	t0 := a.start
	a.Usage(t0, false)
	a.cpu += 500 * time.Millisecond
	a.Usage(t0.Add(time.Second), false)
	a.cpu += 500 * time.Millisecond
	u := a.Usage(t0.Add(2*time.Second), false)
	if u.UsageNanoCores < 4e8 || u.UsageNanoCores > 6e8 {
		t.Errorf("rate %d nanocores, want ~500m", u.UsageNanoCores)
	}
	if u.UsageCoreNanoSeconds != uint64(time.Second) {
		t.Errorf("total %d", u.UsageCoreNanoSeconds)
	}
}

func TestAcctMemory(t *testing.T) {
	a := testAcct("")
	if got := a.WorkingSet(); got != 4<<20 {
		t.Errorf("base %d", got)
	}
	if got := a.AddMemory(1 << 20); got != 5<<20 {
		t.Errorf("after add %d", got)
	}
	a.AddMemory(-10 << 20)
	if got := a.WorkingSet(); got != 4<<20 {
		t.Errorf("free below zero: %d", got)
	}
	a.SetMemory("wasm", 2<<20)
	a.SetMemory("tmpfs", 1<<20)
	if got := a.WorkingSet(); got != 7<<20 {
		t.Errorf("sources %d", got)
	}
	a.SetMemory("tmpfs", 0)
	if got := a.WorkingSet(); got != 6<<20 {
		t.Errorf("source dropped %d", got)
	}
}

func TestAcctRestartResets(t *testing.T) {
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app", Image: "nginx"}}})
	c := w.ctrs[0]
	t.Cleanup(func() { forgetAcct(c) })
	w.mu.Lock()
	c.id = "run1"
	w.mu.Unlock()
	c.memSet("stress", 8<<20)
	acctOf(c).ChargeCPU(time.Millisecond)
	if c.memUsage() < 8<<20 {
		t.Fatalf("memUsage %d", c.memUsage())
	}
	w.mu.Lock()
	c.id = "run2"
	w.mu.Unlock()
	if c.memUsage() >= 8<<20 {
		t.Errorf("restart kept memory sources: %d", c.memUsage())
	}
	if acctOf(c).cpu != 0 {
		t.Errorf("restart kept CPU")
	}
}

// statsNode is a node with one pod of two running containers and one finished
// init container.
func statsNode(t *testing.T) *tabNode {
	t.Helper()
	w := testWorker(t, v1.PodSpec{
		InitContainers: []v1.Container{{Name: "init", Image: "busybox"}},
		Containers:     []v1.Container{{Name: "web", Image: "nginx"}, {Name: "side", Image: "busybox"}},
	})
	w.mu.Lock()
	for i, c := range w.ctrs {
		c.id = "id" + strconv.Itoa(i)
		if c.init {
			c.state = v1.ContainerState{Terminated: &v1.ContainerStateTerminated{}}
		} else {
			c.state = v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: metav1.Now()}}
		}
	}
	w.mu.Unlock()
	t.Cleanup(func() {
		for _, c := range w.ctrs {
			forgetAcct(c)
		}
	})
	n := &tabNode{workers: map[types.UID]*podWorker{w.uid: w}}
	return n
}

func TestSummaryHandler(t *testing.T) {
	n := statsNode(t)
	for _, c := range n.workerList()[0].ctrs {
		if !c.init {
			acctOf(c).Work(context.Background(), 5*time.Millisecond)
		}
	}
	rec := httptest.NewRecorder()
	n.getSummary(rec, httptest.NewRequest("GET", "/stats/summary?only_cpu_and_memory=true", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var s stats.Summary
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Node.NodeName != tabNodeName || s.Node.CPU == nil || s.Node.Memory == nil || s.Node.StartTime.IsZero() {
		t.Fatalf("node: %+v", s.Node)
	}
	m := s.Node.Memory
	if *m.WorkingSetBytes == 0 || *m.UsageBytes < *m.WorkingSetBytes || m.AvailableBytes == nil {
		t.Errorf("node memory %+v", m)
	}
	if len(s.Node.SystemContainers) == 0 || s.Node.SystemContainers[0].Name != "pods" {
		t.Errorf("systemContainers %+v", s.Node.SystemContainers)
	}
	if len(s.Pods) != 1 {
		t.Fatalf("pods: %d", len(s.Pods))
	}
	p := s.Pods[0]
	if p.PodRef.Name != "p" || p.PodRef.Namespace != "ns" || p.PodRef.UID != "uid" {
		t.Errorf("podRef %+v", p.PodRef)
	}
	if len(p.Containers) != 2 {
		t.Fatalf("containers: %+v", p.Containers)
	}
	var sum uint64
	for _, c := range p.Containers {
		if c.Name == "init" {
			t.Errorf("finished init container listed")
		}
		if c.CPU == nil || c.Memory == nil || c.StartTime.IsZero() {
			t.Errorf("container %s: %+v", c.Name, c)
		}
		sum += *c.Memory.WorkingSetBytes
	}
	if *p.Memory.WorkingSetBytes != sum {
		t.Errorf("pod memory %d != sum %d", *p.Memory.WorkingSetBytes, sum)
	}
	if *p.CPU.UsageCoreNanoSeconds < uint64(10*time.Millisecond)-1 {
		t.Errorf("pod cpu %d", *p.CPU.UsageCoreNanoSeconds)
	}
	rec = httptest.NewRecorder()
	n.getSummary(rec, httptest.NewRequest("GET", "/stats/summary?only_cpu_and_memory=maybe", nil))
	if rec.Code != 400 {
		t.Errorf("bad only_cpu_and_memory: %d", rec.Code)
	}
}

var sampleRe = regexp.MustCompile(`^([a-z_]+)(\{[^}]*\})? ([0-9.e+-]+)( [0-9]+)?$`)

func TestResourceMetrics(t *testing.T) {
	n := statsNode(t)
	rec := httptest.NewRecorder()
	n.getResourceMetrics(rec, httptest.NewRequest("GET", "/metrics/resource", nil))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	types := map[string]string{}
	got := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(rec.Body.String()), "\n") {
		if f, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(f, " ")
			types[name] = typ
			continue
		}
		if strings.HasPrefix(line, "# HELP ") {
			continue
		}
		m := sampleRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("bad line %q", line)
		}
		name, labels, ts := m[1], m[2], m[4]
		got[name]++
		if types[name] == "" {
			t.Errorf("%s without TYPE before", name)
		}
		if name != "container_start_time_seconds" && name != "resource_scrape_error" && ts == "" {
			t.Errorf("sample without timestamp: %q", line)
		}
		switch {
		case strings.HasPrefix(name, "container_"):
			for _, l := range []string{`container="`, `namespace="ns"`, `pod="p"`} {
				if !strings.Contains(labels, l) {
					t.Errorf("%q misses %s", line, l)
				}
			}
		case strings.HasPrefix(name, "pod_"):
			if labels != `{namespace="ns",pod="p"}` {
				t.Errorf("labels %q", line)
			}
		default:
			if labels != "" {
				t.Errorf("labels on %q", line)
			}
		}
	}
	for name, want := range map[string]int{
		"node_cpu_usage_seconds_total": 1, "node_memory_working_set_bytes": 1,
		"container_cpu_usage_seconds_total": 2, "container_memory_working_set_bytes": 2, "container_start_time_seconds": 2,
		"pod_cpu_usage_seconds_total": 1, "pod_memory_working_set_bytes": 1, "resource_scrape_error": 1,
	} {
		if got[name] != want {
			t.Errorf("%s: %d samples, want %d", name, got[name], want)
		}
	}
	if types["container_cpu_usage_seconds_total"] != "counter" || types["container_memory_working_set_bytes"] != "gauge" {
		t.Errorf("types %v", types)
	}
}

// fakeBusy replaces procBusy for a test; ok false is a page that does not
// count.
// It also stops the garbage collector of the test from counting.
func fakeBusy(t *testing.T, busy *time.Duration, ok *bool) {
	t.Helper()
	old, oldGC := procBusy, gcCPU
	procBusy = func() (time.Duration, bool) { return *busy, *ok }
	gcCPU = func() time.Duration { return 0 }
	t.Cleanup(func() { procBusy, gcCPU = old, oldGC })
}

// fakeBytes replaces cpBytesRead for a test.
func fakeBytes(t *testing.T, etcd *int64, rest map[string]int64) {
	t.Helper()
	old := cpBytesRead
	cpBytesRead = func() (int64, map[string]int64) {
		cp := map[string]int64{}
		for k, v := range rest {
			cp[k] = v
		}
		return *etcd, cp
	}
	t.Cleanup(func() { cpBytesRead = old })
}

func TestNodeCPUMonotonic(t *testing.T) {
	for _, measure := range []bool{false, true} {
		var busy time.Duration
		var etcd int64
		rest := map[string]int64{}
		fakeBusy(t, &busy, &measure)
		fakeBytes(t, &etcd, rest)
		var m nodeSysModel
		now := time.Now()
		var last uint64
		lastComps := map[string]uint64{}
		for i := range 30 {
			// the measured time and the bytes jump around, even back
			busy += time.Duration((i%5)-1) * 30 * time.Millisecond
			etcd += int64(i % 3 * 1000)
			rest[compScheduler] += int64(i % 2 * 500)
			u := m.sample(now.Add(time.Duration(i%7)*time.Second), 3, true)
			if u.node.UsageCoreNanoSeconds < last {
				t.Fatalf("measured=%v: node cpu went back", measure)
			}
			last = u.node.UsageCoreNanoSeconds
			for name, c := range u.comps {
				if c.UsageCoreNanoSeconds < lastComps[name] {
					t.Fatalf("measured=%v: %s went back", measure, name)
				}
				lastComps[name] = c.UsageCoreNanoSeconds
			}
		}
	}
}

// TestCPUPodsWithinNode: in every interval the node gets at least what the
// containers and the control plane pods got, and the control plane pods
// never more than was measured; also when a container charges more than
// the instance ran.
func TestCPUPodsWithinNode(t *testing.T) {
	for _, measure := range []bool{false, true} {
		var busy time.Duration
		var etcd int64
		rest := map[string]int64{}
		fakeBusy(t, &busy, &measure)
		fakeBytes(t, &etcd, rest)
		var m nodeSysModel
		now := time.Now()
		var prev cpuSampleOf
		prevCtrs, prevIdle := time.Duration(0), time.Duration(0)
		for i := range 40 {
			now = now.Add(time.Second)
			dBusy := 100 * time.Millisecond
			busy += dBusy
			etcd += 4000
			rest[compController] += 1000
			rest[compScheduler] += 300
			rest[""] += 2000
			if i%4 == 0 {
				// a model that over-charges: 3 s of CPU in 1 s
				nodeCPU.add(3 * time.Second)
			}
			if i%4 == 2 {
				// idle rates: they take nothing from the control plane
				nodeCPU.add(200 * time.Millisecond)
				idleCPU.add(200 * time.Millisecond)
			}
			u := m.sample(now, 2, true)
			if i > 0 {
				var dComps time.Duration
				for _, name := range compNames {
					dComps += time.Duration(u.comps[name].UsageCoreNanoSeconds - prev.comps[name].UsageCoreNanoSeconds)
				}
				dNode := time.Duration(u.node.UsageCoreNanoSeconds - prev.node.UsageCoreNanoSeconds)
				dCtrs := m.ctrs - prevCtrs
				if dNode < dCtrs+dComps {
					t.Fatalf("measured=%v interval %d: node %v < containers %v + control plane %v", measure, i, dNode, dCtrs, dComps)
				}
				if dWork := dCtrs - (m.idle - prevIdle); measure && dComps > max(dBusy-dWork, 0) {
					t.Fatalf("interval %d: control plane %v > measured %v less the work of the containers %v", i, dComps, dBusy, dWork)
				}
				if measure && i%4 != 0 && dNode != max(dBusy, dCtrs+dComps) {
					t.Errorf("interval %d: node %v, want the measured %v", i, dNode, dBusy)
				}
				if measure && i%4 == 2 && dComps < dBusy*8/10 {
					t.Errorf("interval %d: idle rates took from the control plane: %v of %v", i, dComps, dBusy)
				}
				if dComps == 0 && !(measure && i%4 == 0) {
					t.Errorf("measured=%v interval %d: the control plane got nothing", measure, i)
				}
			}
			prev, prevCtrs, prevIdle = u, m.ctrs, m.idle
		}
	}
}

// TestCPUGCStaysInNode: the time of the garbage collector comes off what
// the control plane pods get, also when a cycle shows up larger than an
// interval; the node keeps it.
func TestCPUGCStaysInNode(t *testing.T) {
	busy, ok := time.Duration(0), true
	var etcd int64
	rest := map[string]int64{}
	fakeBusy(t, &busy, &ok)
	fakeBytes(t, &etcd, rest)
	var gc time.Duration
	gcCPU = func() time.Duration { return gc }
	var m nodeSysModel
	now := time.Now()
	m.sample(now, 0, true)
	var comps, node time.Duration
	prev := m.sample(now, 0, true) // the first measured interval
	for i := range 10 {
		now = now.Add(time.Second)
		busy += 100 * time.Millisecond
		etcd += 1000 // only etcd and the apiserver moved bytes: the shares add up to 1
		if i == 2 {
			gc += 250 * time.Millisecond // a cycle ends, longer than an interval
		}
		u := m.sample(now, 0, true)
		var d time.Duration
		for _, name := range compNames {
			d += time.Duration(u.comps[name].UsageCoreNanoSeconds - prev.comps[name].UsageCoreNanoSeconds)
		}
		dn := time.Duration(u.node.UsageCoreNanoSeconds - prev.node.UsageCoreNanoSeconds)
		if d < 0 || dn < 100*time.Millisecond-time.Microsecond || dn > 100*time.Millisecond+time.Microsecond {
			t.Errorf("interval %d: control plane %v, node %v", i, d, dn)
		}
		if (i == 2 || i == 3) && d != 0 {
			t.Errorf("interval %d: the control plane got %v while the GC time was paid off", i, d)
		}
		comps += d
		node += dn
		prev = u
	}
	if want := time.Second - 250*time.Millisecond; comps < want-time.Millisecond || comps > want+time.Millisecond {
		t.Errorf("control plane got %v of 1s with 250ms GC, want %v", comps, want)
	}
}

// TestCPUNoControlPlane: a node without the control plane (a Web Worker)
// splits nothing, even when its own client moves bytes; with idle
// containers the node is max(measured, containers).
func TestCPUNoControlPlane(t *testing.T) {
	busy, ok := time.Duration(0), true
	var etcd int64
	rest := map[string]int64{}
	fakeBusy(t, &busy, &ok)
	fakeBytes(t, &etcd, rest)
	var m nodeSysModel
	now := time.Now()
	m.sample(now, 0, false)
	prev := m.sample(now, 0, false)
	prevCtrs := m.ctrs
	for i := range 10 {
		now = now.Add(time.Second)
		dBusy := 40 * time.Millisecond
		busy += dBusy
		rest[""] += 5000
		// idle containers: charged, but nothing ran for them
		idle := time.Duration(i*20) * time.Millisecond
		nodeCPU.add(idle)
		idleCPU.add(idle)
		u := m.sample(now, 3, false)
		for _, name := range compNames {
			if c := u.comps[name].UsageCoreNanoSeconds; c != 0 {
				t.Fatalf("interval %d: %s got %v on a node without the control plane", i, name, time.Duration(c))
			}
		}
		dNode := time.Duration(u.node.UsageCoreNanoSeconds - prev.node.UsageCoreNanoSeconds)
		if want := max(dBusy, m.ctrs-prevCtrs); dNode != want {
			t.Errorf("interval %d: node %v, want %v", i, dNode, want)
		}
		prev, prevCtrs = u, m.ctrs
	}
}

func TestCPShares(t *testing.T) {
	for _, tc := range []struct {
		etcd int64
		rest map[string]int64
	}{
		{0, nil},
		{1000, nil},
		{0, map[string]int64{"": 500}},
		{1000, map[string]int64{compController: 10, compScheduler: 20, compAPIServer: 30, "": 40}},
		{0, map[string]int64{compController: 10}},
		{5, map[string]int64{compScheduler: -7}},
	} {
		sh := cpShares(tc.etcd, tc.rest)
		var sum float64
		for name, v := range sh {
			if v < 0 {
				t.Errorf("%v %v: %s share %v", tc.etcd, tc.rest, name, v)
			}
			sum += v
		}
		if sum > 1+1e-9 {
			t.Errorf("%v %v: shares add up to %v", tc.etcd, tc.rest, sum)
		}
	}
	if sh := cpShares(0, map[string]int64{}); len(sh) != 0 {
		t.Errorf("no bytes: %v", sh)
	}
	// The bytes of other clients stay in the node.
	if sh := cpShares(0, map[string]int64{"": 100}); sh[compAPIServer] != 0.5 {
		t.Errorf("only other clients: %v", sh)
	}
}

func TestCPMemory(t *testing.T) {
	m := goMem{heapInuse: 300 << 20, live: 150 << 20, stack: 40 << 20}
	counts := map[string]int{compAPIServer: 2000, compController: 1500, compScheduler: 300, compEtcd: 100, "": 1100}
	for _, tc := range []struct {
		stored uint64
		counts map[string]int
		total  int
	}{
		{5 << 20, counts, 5000},
		{5 << 20, nil, 0},         // before the first count
		{900 << 20, counts, 5000}, // more than the heap
		{0, map[string]int{}, 1},  // no component goroutines
	} {
		got := cpMemory(m, tc.stored, tc.counts, tc.total)
		var sum uint64
		for _, v := range got {
			sum += v
		}
		if sum > m.live+m.stack || sum > m.heapInuse {
			t.Errorf("%+v: pods %d > live+stack %d", tc, sum, m.live+m.stack)
		}
	}
	got := cpMemory(m, 5<<20, counts, 5000)
	if got[compEtcd] != 5<<20+(40<<20)*100/5000 {
		t.Errorf("etcd %d", got[compEtcd])
	}
	if !(got[compAPIServer] > got[compController] && got[compController] > got[compScheduler] && got[compScheduler] > 0) {
		t.Errorf("split %v", got)
	}
	if fb := cpMemory(m, 5<<20, nil, 0); fb[compAPIServer] == 0 || fb[compAPIServer] != fb[compScheduler] {
		t.Errorf("fallback %v", fb)
	}
}

func TestParseGoroutineCounts(t *testing.T) {
	text := `goroutine profile: total 9
4 @ 0x1 0x2
# labels: {"component":"kube-apiserver"}
#	0x1	foo+0x1	/x.go:1

3 @ 0x1 0x3
#	0x1	foo+0x1	/x.go:1

2 @ 0x1 0x4
# labels: {"a":"b", "component":"etcd"}
`
	counts, total := parseGoroutineCounts(text)
	if total != 9 || counts[compAPIServer] != 4 || counts[compEtcd] != 2 || counts[""] != 3 {
		t.Errorf("counts %v total %d", counts, total)
	}
	// A real profile finds a labelled goroutine.
	stop := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		labelGoroutine("kube-scheduler")
		go func() { <-stop }() // inherits the label
		close(ready)
		<-stop
	}()
	<-ready
	defer close(stop)
	counts, total = countGoroutines()
	if counts[compScheduler] < 2 || total < counts[compScheduler]+counts[""] {
		t.Errorf("profile counts %v total %d", counts, total)
	}
}

type fakeRT struct{ status int }

func (f fakeRT) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader("hello world")), Request: r}, nil
}

// TestRESTBytes: every client-go transport (k8s/patch.go) counts the bodies
// for the component of the goroutine that sends the request, also when
// another goroutine reads the response, like a watch.
func TestRESTBytes(t *testing.T) {
	rt, err := transport.HTTPWrappersForConfig(&transport.Config{}, fakeRT{200})
	if err != nil {
		t.Fatal(err)
	}
	_, before := cpBytesNow()
	var resp *http.Response
	done := make(chan struct{})
	go func() {
		labelGoroutine("kube-controller-manager")
		req, _ := http.NewRequest("PUT", "http://x/api", strings.NewReader(strings.Repeat("b", 66)))
		resp, err = rt.RoundTrip(req)
		close(done)
	}()
	<-done
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body) // read here, in a goroutine of no component
	req, _ := http.NewRequest("GET", "http://x/api", nil)
	resp2, _ := rt.RoundTrip(req)
	io.ReadAll(resp2.Body)
	_, after := cpBytesNow()
	if d := after[compController] - before[compController]; d != 66+11 {
		t.Errorf("controller manager %d bytes", d)
	}
	if d := after[""] - before[""]; d != 11 {
		t.Errorf("no component %d bytes", d)
	}
	// An upgraded connection keeps its own body.
	up, _ := transport.WrapRoundTripper(fakeRT{http.StatusSwitchingProtocols}).RoundTrip(req)
	if _, ok := up.Body.(countingBody); ok {
		t.Error("the body of an upgrade is wrapped")
	}
}

// The apiserver translates the WebSocket of kubectl exec and attach into SPDY
// to the node: the SPDY round tripper takes the TLS config of the transport of
// the kubelet client, through restBytes.
func TestRESTBytesUpgradeTLS(t *testing.T) {
	rt, err := transport.New(&transport.Config{TLS: transport.TLSConfig{ServerName: "node"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.(restBytes); !ok {
		t.Fatalf("transport %T is not restBytes", rt)
	}
	spdyRT, err := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{UpgradeTransport: rt})
	if err != nil {
		t.Fatal(err)
	}
	if tc := spdyRT.TLSClientConfig(); tc == nil || tc.ServerName != "node" {
		t.Errorf("TLS config %+v", tc)
	}
}

func TestCountingListener(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int64
	cl := countingListener{l, &n}
	defer cl.Close()
	go func() {
		c, err := cl.Accept()
		if err != nil {
			return
		}
		b := make([]byte, 5)
		io.ReadFull(c, b)
		c.Write([]byte("hello world"))
		c.Close()
	}()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Write([]byte("12345"))
	io.ReadAll(c)
	c.Close()
	if n.Load() != 16 {
		t.Errorf("counted %d bytes", n.Load())
	}
}

// cpNode is statsNode with the static pods of the control plane: etcd and
// kube-apiserver with a mirror pod, kube-scheduler without, and an exited
// kube-controller-manager.
func cpNode(t *testing.T) (*tabNode, *staticPods) {
	t.Helper()
	n := statsNode(t)
	var comps []*component
	for _, name := range compNames {
		c := testComponent(name, nil)
		if name == compController {
			c.setExited(errors.New("boom"))
		}
		comps = append(comps, c)
	}
	m := newStaticPods(n, comps)
	for _, sp := range m.pods {
		if sp.c.name != compScheduler {
			sp.mirror = &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: sp.pod.Name, Namespace: sp.pod.Namespace, UID: types.UID("uid-" + sp.c.name)}}
		}
	}
	n.static = m
	return n, m
}

func TestStatsPods(t *testing.T) {
	_, m := cpNode(t)
	var names []string
	for _, p := range m.statsPods() {
		names = append(names, p.ref.Namespace+"/"+p.ref.Name+"/"+p.ref.UID+"/"+p.c.name)
	}
	want := []string{"kube-system/kube-apiserver-tab/uid-kube-apiserver/kube-apiserver", "kube-system/etcd-tab/uid-etcd/etcd"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Errorf("stats pods %v, want %v", names, want)
	}
	var none *staticPods
	if none.statsPods() != nil {
		t.Error("a worker node has control plane pods")
	}
}

func TestSummaryControlPlane(t *testing.T) {
	busy, ok := time.Duration(0), true
	fakeBusy(t, &busy, &ok)
	n, _ := cpNode(t)
	// a container that over-charges
	for _, c := range n.workerList()[0].ctrs {
		if !c.init {
			acctOf(c).ChargeCPU(2 * time.Second)
		}
	}
	for i := range 3 {
		busy += 50 * time.Millisecond
		cpBytes.etcd.Add(1000)
		s := n.summary(time.Now())
		var etcd *stats.PodStats
		var cpu, mem uint64
		for i, p := range s.Pods {
			if p.PodRef.Namespace == "kube-system" && p.PodRef.Name == "etcd-tab" {
				etcd = &s.Pods[i]
			}
			if p.PodRef.Name == "kube-scheduler-tab" || p.PodRef.Name == "kube-controller-manager-tab" {
				t.Errorf("listed %s", p.PodRef.Name)
			}
			cpu += *p.CPU.UsageCoreNanoSeconds
			mem += *p.Memory.WorkingSetBytes
		}
		if etcd == nil || len(etcd.Containers) != 1 || etcd.Containers[0].Name != "etcd" || etcd.PodRef.UID != "uid-etcd" {
			t.Fatalf("etcd-tab: %+v", etcd)
		}
		if !etcd.StartTime.Time.Equal(testComponent("etcd", nil).start) {
			t.Errorf("etcd start %v", etcd.StartTime)
		}
		if len(s.Pods) != 3 {
			t.Errorf("%d pods", len(s.Pods))
		}
		if cpu > *s.Node.CPU.UsageCoreNanoSeconds {
			t.Errorf("round %d: pods cpu %d > node %d", i, cpu, *s.Node.CPU.UsageCoreNanoSeconds)
		}
		if mem > *s.Node.Memory.WorkingSetBytes {
			t.Errorf("round %d: pods memory %d > node %d", i, mem, *s.Node.Memory.WorkingSetBytes)
		}
		pc := s.Node.SystemContainers[0]
		if *pc.CPU.UsageCoreNanoSeconds != cpu || *pc.Memory.WorkingSetBytes != mem {
			t.Errorf("system container pods %d/%d, pods %d/%d", *pc.CPU.UsageCoreNanoSeconds, *pc.Memory.WorkingSetBytes, cpu, mem)
		}
		b, _ := json.Marshal(s)
		if l := strings.ToLower(string(b)); strings.Contains(l, "browser") || strings.Contains(l, "tabernetes") {
			t.Errorf("the summary names the platform: %s", b)
		}
	}
}

// TestResourceMetricsControlPlane: what metrics-server makes of two scrapes.
func TestResourceMetricsControlPlane(t *testing.T) {
	busy, ok := time.Duration(0), true
	fakeBusy(t, &busy, &ok)
	n, _ := cpNode(t)
	results := map[string]*scrapeResult{}
	store := newMetricsStorage()
	for range 2 {
		busy += 80 * time.Millisecond
		cpBytes.etcd.Add(5000)
		rec := httptest.NewRecorder()
		n.getResourceMetrics(rec, httptest.NewRequest("GET", "/metrics/resource", nil))
		text := rec.Body.String()
		if !strings.Contains(text, `container_memory_working_set_bytes{container="etcd",namespace="kube-system",pod="etcd-tab"}`) {
			t.Fatalf("no etcd-tab in\n%s", text)
		}
		if l := strings.ToLower(text); strings.Contains(l, "browser") || strings.Contains(l, "tabernetes") {
			t.Errorf("the metrics name the platform")
		}
		r, err := parseResourceMetrics(strings.NewReader(text), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		results["tab"] = r
		store.Store(results)
		time.Sleep(20 * time.Millisecond)
	}
	u, ok := store.PodUsage(types.NamespacedName{Namespace: "kube-system", Name: "etcd-tab"})
	if !ok || len(u.containers) != 1 || u.containers[0].name != "etcd" || u.containers[0].cpuNano <= 0 {
		t.Fatalf("etcd-tab usage %+v %v", u, ok)
	}
	// No etcd runs in the test, so etcd holds nothing; the apiserver has its
	// share of the heap.
	if u, ok := store.PodUsage(types.NamespacedName{Namespace: "kube-system", Name: "kube-apiserver-tab"}); !ok || u.containers[0].memBytes <= 0 {
		t.Errorf("kube-apiserver-tab usage %+v %v", u, ok)
	}
}

func TestStatsAuth(t *testing.T) {
	cs := fake.NewSimpleClientset()
	var reviews int
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		reviews++
		tr := a.(k8stesting.CreateAction).GetObject().(*authenticationv1.TokenReview)
		tr.Status = authenticationv1.TokenReviewStatus{}
		switch tr.Spec.Token {
		case "good":
			tr.Status.Authenticated = true
			tr.Status.User = authenticationv1.UserInfo{Username: "system:serviceaccount:kube-system:metrics-server"}
		case "nope":
			tr.Status.Authenticated = true
			tr.Status.User = authenticationv1.UserInfo{Username: "nobody"}
		}
		return true, tr, nil
	})
	var sub string
	cs.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		sar := a.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		ra := sar.Spec.ResourceAttributes
		sub = ra.Resource + "/" + ra.Subresource + "/" + ra.Name + "/" + ra.Verb
		sar.Status.Allowed = sar.Spec.User != "nobody"
		return true, sar, nil
	})
	n := &tabNode{admin: cs}
	h := n.statsAuthorized("metrics", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	do := func(auth string, tlsOK bool) int {
		r := httptest.NewRequest("GET", "/metrics/resource", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if tlsOK {
			r.TLS = &tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{{}}}}
		}
		rec := httptest.NewRecorder()
		h(rec, r)
		return rec.Code
	}
	if c := do("", false); c != 401 {
		t.Errorf("no token: %d", c)
	}
	if c := do("Bearer bad", false); c != 401 {
		t.Errorf("bad token: %d", c)
	}
	if c := do("", true); c != 200 {
		t.Errorf("client certificate: %d", c)
	}
	if c := do("Bearer good", false); c != 200 {
		t.Errorf("good token: %d", c)
	}
	if want := "nodes/metrics/" + tabNodeName + "/get"; sub != want {
		t.Errorf("SubjectAccessReview for %q, want %q", sub, want)
	}
	before := reviews
	if c := do("Bearer good", false); c != 200 || reviews != before {
		t.Errorf("cached token: %d, %d new reviews", c, reviews-before)
	}
	statsAuth.Lock()
	clear(statsAuth.ok)
	statsAuth.Unlock()
	if c := do("Bearer nope", false); c != 403 {
		t.Errorf("forbidden token: %d", c)
	}
}

// TestAddonProfiles: every add-on image has its profile, and its base stays
// well under limits.memory, so an idle add-on is never OOM-killed.
func TestAddonProfiles(t *testing.T) {
	want := map[string]imageProfile{
		"registry.k8s.io/coredns/coredns":               {13 << 20, 0.0015},
		"registry.k8s.io/metrics-server/metrics-server": {18 << 20, 0.002},
		"rancher/local-path-provisioner":                {8 << 20, 0.0005},
		"registry.k8s.io/ingress-nginx/controller":      {90 << 20, 0.002},
	}
	seen := map[string]bool{}
	entries, err := addonFS.ReadDir("addons")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := addonFS.ReadFile("addons/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range strings.Split(string(b), "\n---") {
			var obj struct {
				Spec struct {
					Template struct {
						Spec v1.PodSpec `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
				continue
			}
			for _, c := range obj.Spec.Template.Spec.Containers {
				repo, _ := splitImage(c.Image)
				w, ok := want[repo]
				if !ok {
					continue
				}
				seen[repo] = true
				img := lookupImage(c.Image)
				if img == nil {
					t.Errorf("%s is no built-in image", c.Image)
					continue
				}
				if got := profileOf(img, false); got != w {
					t.Errorf("%s: profile %+v, want %+v", c.Image, got, w)
				}
				// The work of an add-on charges CPU, not memory: the base is
				// all an idle one holds. Keep it under half of the limit.
				if limit := memLimit(c); limit > 0 && int64(w.memory)*2 > limit {
					t.Errorf("%s: base %d too close to limits.memory %d", c.Image, w.memory, limit)
				}
				a := newCtrAcct("id", c, profileOf(img, false), time.Now())
				if limit := memLimit(c); limit > 0 && int64(a.WorkingSet()) > limit {
					t.Errorf("%s: OOM-killed when idle", c.Image)
				}
			}
		}
	}
	for repo := range want {
		if !seen[repo] {
			t.Errorf("no add-on runs %s", repo)
		}
	}
	// the last path element still finds a profile, the full name first
	if p := profileOf(&builtinImage{name: "someone/nginx"}, false); p != imageProfiles["nginx"] {
		t.Errorf("someone/nginx: %+v", p)
	}
}

// TestCoreDNSCharge: lookups go round robin to the CoreDNS containers of the
// node, and stop when they exit.
func TestCoreDNSCharge(t *testing.T) {
	a, b := testAcct(""), testAcct("")
	ra := addCoreDNS(a)
	rb := addCoreDNS(b)
	for range 10 {
		chargeDNS()
	}
	if a.cpu != 5*dnsQueryCost || b.cpu != 5*dnsQueryCost {
		t.Errorf("charged %v and %v", a.cpu, b.cpu)
	}
	ra()
	rb()
	chargeDNS() // none left: nothing happens
	if a.cpu != 5*dnsQueryCost || b.cpu != 5*dnsQueryCost {
		t.Errorf("charged after the exit: %v and %v", a.cpu, b.cpu)
	}

	// One query over TCP is charged once, not by serveDNS and lookupName.
	c := testAcct("")
	defer addCoreDNS(c)()
	s := testProxy(testService("default", "web", "10.96.1.1"))
	ln, err := net.Listen("tcp", "10.244.250.11:53")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	go s.serveDNS(ctx, ln, &corednsStats{})
	conn, err := net.Dial("tcp", "10.244.250.11:53")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if m := dnsQuery(t, conn, 1, "web.default.svc.cluster.local.", dnsmessage.TypeA); len(m.Answers) != 1 {
		t.Fatalf("web: %+v", m)
	}
	if c.cpu != dnsQueryCost {
		t.Errorf("one query charged %v, want %v", c.cpu, dnsQueryCost)
	}
}
