package main

// The resource endpoints of the kubelet of the tab node: /stats/summary (the
// Summary API, what `kubectl get --raw /api/v1/nodes/tab/proxy/stats/summary`
// shows) and /metrics/resource (Prometheus text, what a metrics-server
// scrapes). The containers come from the accounting of tabnode_acct.go. The
// node is the memory of the Go runtime plus the containers, and the CPU time
// the instance ran (or a model of it, see nodeSysModel). On the control plane
// node the four control plane pods are cut out of the node: their CPU by the
// bytes each one moved, their memory by the store of etcd and by goroutines.
// The pods never add up to more than the node.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"

	"k8s.io/kubernetes/cmd/tabernetes/etcdshim"
)

const (
	// requestCost is the CPU a request to a built-in server costs.
	requestCost = 200 * time.Microsecond
	// statsAuthTTL is how long a successful TokenReview and
	// SubjectAccessReview count, like the caches of the kubelet.
	statsAuthTTL = time.Minute
)

// nodeStart is the start of the node (the wasm instance).
var nodeStart = time.Now()

// chargeRequests makes every request to a built-in server cost a little CPU
// of the container the server runs in.
func chargeRequests(ctx context.Context, h http.Handler) http.Handler {
	a := acctFrom(ctx)
	if a == nil {
		return h
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.ChargeCPU(requestCost)
		h.ServeHTTP(w, r)
	})
}

// ctrSnap is a running container, picked under the lock of its pod.
type ctrSnap struct {
	c       *ctr
	name    string
	started time.Time
	memory  *int64 // limits.memory in bytes, nil without
}

// runningCtrs lists the containers that run now; finished init containers are
// not in it.
func (w *podWorker) runningCtrs() []ctrSnap {
	w.mu.Lock()
	defer w.mu.Unlock()
	var list []ctrSnap
	for _, group := range [][]*ctr{w.ctrs, w.ephemeral} {
		for _, c := range group {
			if r := c.state.Running; r != nil {
				s := ctrSnap{c: c, name: c.spec.Name, started: r.StartedAt.Time}
				if q, ok := c.spec.Resources.Limits[v1.ResourceMemory]; ok && q.Value() > 0 {
					lim := q.Value()
					s.memory = &lim
				}
				list = append(list, s)
			}
		}
	}
	return list
}

// forgetAccts drops the accounts of the containers of a pod that is gone.
func (w *podWorker) forgetAccts() {
	w.mu.Lock()
	all := append(append([]*ctr{}, w.ctrs...), w.ephemeral...)
	w.mu.Unlock()
	for _, c := range all {
		forgetAcct(c)
	}
}

// sampleStats samples the containers once a second, so that the rate of the
// CPU (usageNanoCores) has a window.
func (n *tabNode) sampleStats(ctx context.Context) {
	if n.static != nil {
		go countGoroutinesLoop(ctx)
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n.sampleOnce(now)
		}
	}
}

func (n *tabNode) sampleOnce(now time.Time) {
	ws := n.workerList()
	for _, w := range ws {
		for _, s := range w.runningCtrs() {
			acctOf(s.c).Usage(now, true)
		}
	}
	nodeModel.sample(now, len(ws))
}

// procBusy is the time the wasm instance ran Go code since it started:
// clock.js adds up every call into the instance (countBusy). false when
// nothing counts it.
var procBusy = func() (time.Duration, bool) {
	f := js.Global().Get("tabernetesBusy")
	if f.Type() != js.TypeFunction {
		return 0, false
	}
	return time.Duration(f.Invoke().Float() * float64(time.Millisecond)), true
}

// cpBytesRead reads the bytes the control plane moved (controlplane.go).
var cpBytesRead = cpBytesNow

// gcCPU is the CPU time the garbage collector and the scavenger of the
// runtime used since the start. The runtime adds a GC cycle when the cycle
// ends, so the counter moves in steps.
var gcCPU = func() time.Duration {
	s := []metrics.Sample{{Name: "/cpu/classes/gc/total:cpu-seconds"}, {Name: "/cpu/classes/scavenge/total:cpu-seconds"}}
	metrics.Read(s)
	var sum float64
	for _, x := range s {
		if x.Value.Kind() == metrics.KindFloat64 {
			sum += x.Value.Float64()
		}
	}
	return time.Duration(sum * float64(time.Second))
}

// The control plane pods, by the name of their component and container.
const (
	compAPIServer  = "kube-apiserver"
	compController = "kube-controller-manager"
	compScheduler  = "kube-scheduler"
	compEtcd       = "etcd"
)

var compNames = []string{compAPIServer, compController, compScheduler, compEtcd}

// cpShares splits the work of an interval between the control plane pods by
// the bytes each one moved in it. etcd: in and out of its listener.
// kube-apiserver: the same bytes (it is the only client of etcd), the REST
// bodies of all clients it served, and those of its own client. The
// controller manager and the scheduler: the REST bodies of their own
// clients. The bodies of other clients (the node, the add-ons, kubectl)
// weigh as much again but go to no pod: that work stays in the node. So the
// shares never add up to more than 1. Without any bytes all shares are 0:
// the components did no requests, and the time stays in the node.
func cpShares(etcd int64, rest map[string]int64) map[string]float64 {
	var all int64
	for _, n := range rest {
		all += max(n, 0)
	}
	etcd = max(etcd, 0)
	w := map[string]float64{
		compEtcd:       float64(etcd),
		compAPIServer:  float64(etcd + all + max(rest[compAPIServer], 0)),
		compController: float64(max(rest[compController], 0)),
		compScheduler:  float64(max(rest[compScheduler], 0)),
	}
	others := float64(all - max(rest[compAPIServer], 0) - max(rest[compController], 0) - max(rest[compScheduler], 0))
	total := others
	for _, v := range w {
		total += v
	}
	if total <= 0 {
		return map[string]float64{}
	}
	for k, v := range w {
		w[k] = v / total
	}
	return w
}

// rateMeter is the rate of a CPU counter over the last rateWindow.
type rateMeter struct{ samples []cpuSample }

func (r *rateMeter) at(now time.Time, total time.Duration) uint64 {
	if n := len(r.samples); n == 0 || now.Sub(r.samples[n-1].t) >= time.Second {
		r.samples = append(r.samples, cpuSample{now, total})
	}
	for len(r.samples) > 2 && now.Sub(r.samples[1].t) >= rateWindow {
		r.samples = r.samples[1:]
	}
	if old := r.samples[0]; now.Sub(old.t) > 0 && total >= old.cpu {
		return uint64(float64(total-old.cpu) / now.Sub(old.t).Seconds())
	}
	return 0
}

// nodeSysModel keeps the CPU counters of the node and of the control plane
// pods. With procBusy the node is measured: the time the instance ran, or
// what the containers and the control plane pods were charged when that is
// more (a model of a container can charge more than really ran). Without it
// the node is the containers plus a modelled system share: the control plane
// on the tab node, a little on a worker node. Either way the control plane
// pods get the measured time less the work the containers were charged and
// less the time of the garbage collector (or the modelled system share),
// split by cpShares, never more.
type nodeSysModel struct {
	mu       sync.Mutex
	last     time.Time
	measured bool          // busy is a value of procBusy
	busy     time.Duration // procBusy at last
	ctrs     time.Duration // nodeCPU at last
	idle     time.Duration // idleCPU at last
	gc       time.Duration // gcCPU at last
	gcDebt   time.Duration // GC time not yet taken out of the measured time
	etcd     int64         // cpBytesRead at last
	rest     map[string]int64

	total time.Duration            // the node counter
	comps map[string]time.Duration // the counters of the control plane pods
	rate  rateMeter
	rates map[string]*rateMeter
}

var nodeModel nodeSysModel

// cpuSampleOf is the CPU of the node and of the control plane pods at a time.
type cpuSampleOf struct {
	node  ctrUsage
	comps map[string]ctrUsage // by component, no StartTime
}

// sample advances all counters to now under one lock and returns them. The
// counters never go back. In each interval the node gets at least what the
// containers and the control plane pods got.
func (m *nodeSysModel) sample(now time.Time, pods int) cpuSampleOf {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last.IsZero() {
		m.last = nodeStart
		m.comps = map[string]time.Duration{}
		m.rates = map[string]*rateMeter{}
	}
	ctrs, idle := nodeCPU.get(), idleCPU.get()
	etcd, rest := cpBytesRead()
	busy, measured := procBusy()
	gc := gcCPU()

	dt := max(now.Sub(m.last), 0)
	dCtrs := max(ctrs-m.ctrs, 0)
	// the work the containers were charged, without their idle rates
	dWork := max(dCtrs-max(idle-m.idle, 0), 0)
	dRest := map[string]int64{}
	for k, v := range rest {
		dRest[k] = v - m.rest[k]
	}
	shares := cpShares(etcd-m.etcd, dRest)
	var work time.Duration // the time to split between the control plane pods
	var dBusy time.Duration
	if measured && m.measured {
		// The work the containers were charged for ran in the same
		// instance but moved no bytes: it is not the control plane's. Their
		// idle rates ran nowhere and take nothing from it.
		dBusy = max(busy-m.busy, 0)
		work = max(dBusy-dWork, 0)
		// The garbage collector works for all of the heap: its time stays
		// in the node. A cycle shows up at its end, all at once, so what
		// does not fit into this interval comes off the next ones.
		m.gcDebt += max(gc-m.gc, 0)
		take := min(m.gcDebt, work)
		work -= take
		m.gcDebt -= take
	} else {
		// No measurement (or the first one): the model.
		rate := 0.015
		if tabNodeName == "tab" {
			rate = 0.060 + 0.002*float64(pods)
		}
		jitter := 0.9 + 0.2*math.Abs(math.Sin(float64(now.UnixNano())/3e9))
		work = time.Duration(float64(dt) * rate * jitter)
	}
	var dComps time.Duration
	for _, name := range compNames {
		d := time.Duration(float64(work) * shares[name])
		m.comps[name] += d
		dComps += d
	}
	if measured && m.measured {
		m.total += max(dBusy, dCtrs+dComps)
	} else {
		m.total += work + dCtrs
	}
	if now.After(m.last) {
		m.last = now
	}
	m.ctrs, m.idle, m.etcd, m.rest = ctrs, idle, etcd, rest
	m.busy, m.measured = busy, measured
	m.gc = max(m.gc, gc)

	out := cpuSampleOf{
		node:  ctrUsage{Time: now, StartTime: nodeStart, UsageCoreNanoSeconds: uint64(m.total), UsageNanoCores: m.rate.at(now, m.total)},
		comps: map[string]ctrUsage{},
	}
	for _, name := range compNames {
		r := m.rates[name]
		if r == nil {
			r = &rateMeter{}
			m.rates[name] = r
		}
		out.comps[name] = ctrUsage{Time: now, UsageCoreNanoSeconds: uint64(m.comps[name]), UsageNanoCores: r.at(now, m.comps[name])}
	}
	return out
}

// goMem is the memory of the Go runtime.
type goMem struct {
	workingSet, usage uint64
	heapInuse, stack  uint64
	live              uint64 // the heap the last GC marked live
}

var liveHeapMetric = []metrics.Sample{{Name: "/gc/heap/live:bytes"}}

func readGoMem() goMem {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	ws := m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse + m.GCSys + m.OtherSys
	usage := m.Sys - min(m.HeapReleased, m.Sys)
	s := make([]metrics.Sample, len(liveHeapMetric))
	copy(s, liveHeapMetric)
	metrics.Read(s)
	var live uint64
	if s[0].Value.Kind() == metrics.KindUint64 {
		live = s[0].Value.Uint64()
	}
	return goMem{workingSet: ws, usage: max(usage, ws), heapInuse: m.HeapInuse, stack: m.StackInuse, live: live}
}

// goroutineCounts is the number of goroutines of each component ("" for
// none), from a goroutine profile. The profile stops the world for up to a
// tenth of a second with thousands of goroutines, so a timer takes it now
// and then (countGoroutinesLoop) and the stats use the last one.
var goroutineCounts struct {
	sync.Mutex
	at     time.Time
	byComp map[string]int
	total  int
}

const (
	// goroutineCountFirst is when the first count is taken after the node
	// started, once the cluster has booted.
	goroutineCountFirst = 45 * time.Second
	// goroutineCountEvery is how often it is taken again.
	goroutineCountEvery = 5 * time.Minute
)

// countGoroutines counts the goroutines by the label "component" that
// labelGoroutine (complogs.go) gives them.
func countGoroutines() (map[string]int, int) {
	var b bytes.Buffer
	pprof.Lookup("goroutine").WriteTo(&b, 1)
	return parseGoroutineCounts(b.String())
}

// parseGoroutineCounts reads the text form of a goroutine profile: "<count>
// @ <pcs>" per stack, then "# labels: {...}" when the stack has labels.
func parseGoroutineCounts(text string) (map[string]int, int) {
	counts := map[string]int{}
	total, last := 0, 0 // last: the count of the stack before
	for _, line := range strings.Split(text, "\n") {
		if l, ok := strings.CutPrefix(line, "# labels: "); ok {
			if _, v, ok := strings.Cut(l, `"component":"`); ok && last > 0 {
				comp, _, _ := strings.Cut(v, `"`)
				counts[""] -= last
				counts[comp] += last
			}
			last = 0
			continue
		}
		f, _, ok := strings.Cut(line, " @")
		n, err := strconv.Atoi(f)
		if !ok || err != nil {
			last = 0
			continue
		}
		counts[""] += n
		total += n
		last = n
	}
	return counts, total
}

func refreshGoroutineCounts(now time.Time) {
	byComp, total := countGoroutines()
	goroutineCounts.Lock()
	goroutineCounts.at, goroutineCounts.byComp, goroutineCounts.total = now, byComp, total
	goroutineCounts.Unlock()
}

// countGoroutinesLoop counts the goroutines once the cluster has booted and
// then every goroutineCountEvery, until ctx ends.
func countGoroutinesLoop(ctx context.Context) {
	t := time.NewTimer(max(time.Until(nodeStart.Add(goroutineCountFirst)), 0))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			refreshGoroutineCounts(now)
			t.Reset(goroutineCountEvery)
		}
	}
}

// cpMemory splits the memory of the Go runtime between the control plane
// pods. etcd holds its store (counted by the store) and its share of the
// stacks by goroutines. The rest of the live heap and the stacks goes by the
// share of goroutines of each component; the share of the goroutines of no
// component (and of etcd, already counted) stays in the node. All of it fits
// in the live heap plus the stacks and in the heap in use, so the pods never
// hold more than the node. Before the first count (total 0) the rest goes in
// four even parts: one for each of the three components, one for the node.
func cpMemory(m goMem, stored uint64, counts map[string]int, total int) map[string]uint64 {
	budget := min(m.live+m.stack, m.heapInuse)
	out := map[string]uint64{}
	share := func(name string) float64 {
		if total <= 0 {
			if name == compEtcd {
				return 0
			}
			return 0.25
		}
		return float64(counts[name]) / float64(total)
	}
	etcd := min(stored+uint64(float64(m.stack)*share(compEtcd)), budget)
	out[compEtcd] = etcd
	rest := budget - etcd
	for _, name := range []string{compAPIServer, compController, compScheduler} {
		out[name] = uint64(float64(rest) * share(name))
	}
	return out
}

// podStatsOf is the stats of one pod with the running containers.
type podStatsOf struct {
	ref   stats.PodReference
	start time.Time
	ctrs  []ctrStatsOf
	sum   ctrUsage
}

type ctrStatsOf struct {
	name    string
	started time.Time
	u       ctrUsage
	limit   *int64
}

// nodeStats is a sample of the node and its pods.
type nodeStats struct {
	pods    []podStatsOf
	node    ctrUsage // CPU of the node
	podsSum ctrUsage // all pods: the system container "pods"
	// ctrMem is the memory model of the containers. It comes on top of what
	// the runtime holds; the control plane pods are part of that.
	ctrMem uint64
	mem    goMem
}

// workingSet is the working set of the node.
func (s nodeStats) workingSet() uint64 { return s.mem.workingSet + s.ctrMem }

// collect samples every pod with a running container at now, the control
// plane pods and the node.
func (n *tabNode) collect(now time.Time) nodeStats {
	var st nodeStats
	ws := n.workerList()
	for _, w := range ws {
		snaps := w.runningCtrs()
		if len(snaps) == 0 {
			continue
		}
		p := podStatsOf{ref: stats.PodReference{Name: w.name, Namespace: w.namespace, UID: string(w.uid)}, start: w.started}
		for _, s := range snaps {
			u := acctOf(s.c).Usage(now, true)
			p.ctrs = append(p.ctrs, ctrStatsOf{name: s.name, started: s.started, u: u, limit: s.memory})
			p.sum = p.sum.sum(u)
		}
		st.podsSum = st.podsSum.sum(p.sum)
		st.pods = append(st.pods, p)
	}
	st.ctrMem = st.podsSum.WorkingSetBytes
	cpu := nodeModel.sample(now, len(ws))
	st.node = cpu.node
	st.mem = readGoMem()
	if cps := n.static.statsPods(); len(cps) > 0 {
		goroutineCounts.Lock()
		counts, total := goroutineCounts.byComp, goroutineCounts.total
		goroutineCounts.Unlock()
		mem := cpMemory(st.mem, uint64(max(etcdshim.StoredBytes(), 0)), counts, total)
		for _, cp := range cps {
			u := cpu.comps[cp.c.name]
			u.StartTime = cp.c.start
			u.WorkingSetBytes = mem[cp.c.name]
			p := podStatsOf{ref: cp.ref, start: cp.c.start, sum: u,
				ctrs: []ctrStatsOf{{name: cp.c.name, started: cp.c.start, u: u}}}
			st.podsSum = st.podsSum.sum(u)
			st.pods = append(st.pods, p)
		}
	}
	return st
}

func cpuStats(u ctrUsage) *stats.CPUStats {
	t := metav1.NewTime(u.Time)
	return &stats.CPUStats{Time: t, UsageNanoCores: &u.UsageNanoCores, UsageCoreNanoSeconds: &u.UsageCoreNanoSeconds}
}

func memStats(now time.Time, workingSet, usage uint64, available *uint64) *stats.MemoryStats {
	zero := uint64(0)
	return &stats.MemoryStats{
		Time: metav1.NewTime(now), AvailableBytes: available, UsageBytes: &usage, WorkingSetBytes: &workingSet,
		RSSBytes: &workingSet, PageFaults: &zero, MajorPageFaults: &zero,
	}
}

func (n *tabNode) summary(now time.Time) stats.Summary {
	st := n.collect(now)
	pods, node, podsSum := st.pods, st.node, st.podsSum
	s := stats.Summary{Pods: []stats.PodStats{}}
	for _, p := range pods {
		ps := stats.PodStats{PodRef: p.ref, StartTime: metav1.NewTime(p.start), Containers: []stats.ContainerStats{}}
		for _, c := range p.ctrs {
			var avail *uint64
			if c.limit != nil {
				a := uint64(max(*c.limit-int64(c.u.WorkingSetBytes), 0))
				avail = &a
			}
			ps.Containers = append(ps.Containers, stats.ContainerStats{
				Name: c.name, StartTime: metav1.NewTime(c.started),
				CPU: cpuStats(c.u), Memory: memStats(now, c.u.WorkingSetBytes, c.u.WorkingSetBytes, avail),
			})
		}
		ps.CPU = cpuStats(p.sum)
		ps.Memory = memStats(now, p.sum.WorkingSetBytes, p.sum.WorkingSetBytes, nil)
		s.Pods = append(s.Pods, ps)
	}
	_, _, gib := browserInfo()
	capacity := uint64(gib) << 30
	// the model of the containers counts on top of what the runtime holds
	ws := st.workingSet()
	usage := max(st.mem.usage+st.ctrMem, ws)
	avail := capacity - min(ws, capacity)
	s.Node = stats.NodeStats{
		NodeName:  tabNodeName,
		StartTime: metav1.NewTime(nodeStart),
		CPU:       cpuStats(node),
		Memory:    memStats(now, ws, usage, &avail),
		SystemContainers: []stats.ContainerStats{{
			Name: stats.SystemContainerPods, StartTime: metav1.NewTime(nodeStart),
			CPU: cpuStats(podsSum), Memory: memStats(now, podsSum.WorkingSetBytes, podsSum.WorkingSetBytes, nil),
		}},
	}
	return s
}

// getSummary serves /stats/summary; only_cpu_and_memory is accepted, the
// summary has nothing else.
func (n *tabNode) getSummary(w http.ResponseWriter, r *http.Request) {
	if s := r.URL.Query().Get("only_cpu_and_memory"); s != "" {
		if _, err := strconv.ParseBool(s); err != nil {
			http.Error(w, "invalid only_cpu_and_memory: "+s, http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(n.summary(time.Now()))
}

// getResourceMetrics serves /metrics/resource in the text format, with the
// names, help texts and types of the resource metrics of a kubelet.
func (n *tabNode) getResourceMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	st := n.collect(now)
	pods, node, ws := st.pods, st.node, st.workingSet()
	type sample struct {
		labels string
		value  float64
		ts     time.Time // zero for none
	}
	type family struct {
		name, help, typ string
		samples         []sample
	}
	lbl := func(kv ...string) string {
		var b strings.Builder
		b.WriteByte('{')
		for i := 0; i < len(kv); i += 2 {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, "%s=%q", kv[i], kv[i+1])
		}
		b.WriteByte('}')
		return b.String()
	}
	secs := func(ns uint64) float64 { return float64(ns) / float64(time.Second) }
	fams := []*family{
		{name: "node_cpu_usage_seconds_total", help: "Cumulative cpu time consumed by the node in core-seconds", typ: "counter",
			samples: []sample{{"", secs(node.UsageCoreNanoSeconds), now}}},
		{name: "node_memory_working_set_bytes", help: "Current working set of the node in bytes", typ: "gauge",
			samples: []sample{{"", float64(ws), now}}},
	}
	cStart := &family{name: "container_start_time_seconds", help: "Start time of the container since unix epoch in seconds", typ: "gauge"}
	cCPU := &family{name: "container_cpu_usage_seconds_total", help: "Cumulative cpu time consumed by the container in core-seconds", typ: "counter"}
	cMem := &family{name: "container_memory_working_set_bytes", help: "Current working set of the container in bytes", typ: "gauge"}
	pCPU := &family{name: "pod_cpu_usage_seconds_total", help: "Cumulative cpu time consumed by the pod in core-seconds", typ: "counter"}
	pMem := &family{name: "pod_memory_working_set_bytes", help: "Current working set of the pod in bytes", typ: "gauge"}
	for _, p := range pods {
		for _, c := range p.ctrs {
			l := lbl("container", c.name, "namespace", p.ref.Namespace, "pod", p.ref.Name)
			cStart.samples = append(cStart.samples, sample{l, float64(c.started.UnixNano()) / 1e9, time.Time{}})
			cCPU.samples = append(cCPU.samples, sample{l, secs(c.u.UsageCoreNanoSeconds), c.u.Time})
			cMem.samples = append(cMem.samples, sample{l, float64(c.u.WorkingSetBytes), c.u.Time})
		}
		l := lbl("namespace", p.ref.Namespace, "pod", p.ref.Name)
		pCPU.samples = append(pCPU.samples, sample{l, secs(p.sum.UsageCoreNanoSeconds), p.sum.Time})
		pMem.samples = append(pMem.samples, sample{l, float64(p.sum.WorkingSetBytes), p.sum.Time})
	}
	fams = append(fams, cStart, cCPU, cMem, pCPU, pMem, &family{
		name: "resource_scrape_error", help: "1 if there was an error while getting container metrics, 0 otherwise", typ: "gauge",
		samples: []sample{{"", 0, time.Time{}}},
	})

	var b strings.Builder
	for _, f := range fams {
		if len(f.samples) == 0 {
			continue
		}
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		for _, s := range f.samples {
			fmt.Fprintf(&b, "%s%s %s", f.name, s.labels, strconv.FormatFloat(s.value, 'g', -1, 64))
			if !s.ts.IsZero() {
				fmt.Fprintf(&b, " %d", s.ts.UnixMilli())
			}
			b.WriteByte('\n')
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, b.String())
}

// statsAuth caches the successful reviews of bearer tokens.
var statsAuth = struct {
	sync.Mutex
	ok map[string]time.Time // token hash + subresource -> valid until
}{ok: map[string]time.Time{}}

// statsAuthorized lets in what authorized lets in (a client certificate of
// the cluster CA, like the apiserver has), and a bearer token that a
// TokenReview authenticates and a SubjectAccessReview allows to get
// nodes/<subresource> of this node: the webhook authentication and
// authorization of a kubelet, for a metrics-server or Prometheus.
func (n *tabNode) statsAuthorized(subresource string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
			h(w, r)
			return
		}
		token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !found || strings.TrimSpace(token) == "" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		token = strings.TrimSpace(token)
		sum := sha256.Sum256([]byte(token))
		key := hex.EncodeToString(sum[:]) + "/" + subresource
		now := time.Now()
		statsAuth.Lock()
		until, cached := statsAuth.ok[key]
		statsAuth.Unlock()
		if !cached || now.After(until) {
			code, msg := n.reviewToken(r.Context(), token, subresource)
			if code != 0 {
				http.Error(w, msg, code)
				return
			}
			statsAuth.Lock()
			if len(statsAuth.ok) > 1000 {
				for k, t := range statsAuth.ok {
					if now.After(t) {
						delete(statsAuth.ok, k)
					}
				}
			}
			statsAuth.ok[key] = now.Add(statsAuthTTL)
			statsAuth.Unlock()
		}
		h(w, r)
	}
}

// reviewToken asks the apiserver; it returns 0 for allowed, else the status
// and the message.
func (n *tabNode) reviewToken(ctx context.Context, token, subresource string) (int, string) {
	tr, err := n.admin.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return http.StatusInternalServerError, "Authentication failed: " + err.Error()
	}
	if !tr.Status.Authenticated {
		return http.StatusUnauthorized, "Unauthorized"
	}
	u := tr.Status.User
	extra := map[string]authorizationv1.ExtraValue{}
	for k, v := range u.Extra {
		extra[k] = authorizationv1.ExtraValue(v)
	}
	sar, err := n.admin.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: u.Username, Groups: u.Groups, UID: u.UID, Extra: extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb: "get", Resource: "nodes", Subresource: subresource, Name: tabNodeName,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return http.StatusInternalServerError, "Authorization error: " + err.Error()
	}
	if !sar.Status.Allowed {
		return http.StatusForbidden, fmt.Sprintf("Forbidden (user=%s, verb=get, resource=nodes, subresource=%s)", u.Username, subresource)
	}
	return 0, ""
}
