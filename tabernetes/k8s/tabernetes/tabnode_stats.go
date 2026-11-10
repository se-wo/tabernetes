package main

// The resource endpoints of the kubelet of the tab node: /stats/summary (the
// Summary API, what `kubectl get --raw /api/v1/nodes/tab/proxy/stats/summary`
// shows) and /metrics/resource (Prometheus text, what a metrics-server
// scrapes). The numbers come from the accounting of tabnode_acct.go; the node
// adds a model of the system share and the memory of the Go runtime.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
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

// nodeSysModel models the CPU of the node itself: the control plane on the tab
// node, a little on a worker node.
type nodeSysModel struct {
	mu      sync.Mutex
	last    time.Time
	sys     time.Duration
	samples []cpuSample
}

var nodeModel nodeSysModel

// sample returns the CPU of the node at now: the containers plus the system
// share. The counter never goes back.
func (m *nodeSysModel) sample(now time.Time, pods int) ctrUsage {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last.IsZero() {
		m.last = nodeStart
	}
	if dt := now.Sub(m.last); dt > 0 {
		rate := 0.015
		if tabNodeName == "tab" {
			rate = 0.060 + 0.002*float64(pods)
		}
		jitter := 0.9 + 0.2*math.Abs(math.Sin(float64(now.UnixNano())/3e9))
		m.sys += time.Duration(float64(dt) * rate * jitter)
		m.last = now
	}
	total := m.sys + nodeCPU.get()
	if n := len(m.samples); n == 0 || now.Sub(m.samples[n-1].t) >= time.Second {
		m.samples = append(m.samples, cpuSample{now, total})
	}
	for len(m.samples) > 2 && now.Sub(m.samples[1].t) >= rateWindow {
		m.samples = m.samples[1:]
	}
	var rate uint64
	if old := m.samples[0]; now.Sub(old.t) > 0 && total >= old.cpu {
		rate = uint64(float64(total-old.cpu) / now.Sub(old.t).Seconds())
	}
	return ctrUsage{Time: now, StartTime: nodeStart, UsageCoreNanoSeconds: uint64(total), UsageNanoCores: rate}
}

// nodeMemory is the memory of the node from the Go runtime: what the wasm
// instance holds. capacity is the memory of the node object.
func nodeMemory() (usage, workingSet, capacity uint64) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	workingSet = m.HeapInuse + m.StackInuse + m.MSpanInuse + m.MCacheInuse + m.GCSys + m.OtherSys
	usage = m.Sys - min(m.HeapReleased, m.Sys)
	_, _, gib := browserInfo()
	return max(usage, workingSet), workingSet, uint64(gib) << 30
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

// collect samples every pod with a running container at now, and the node.
func (n *tabNode) collect(now time.Time) (pods []podStatsOf, node ctrUsage, podsSum ctrUsage) {
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
		podsSum = podsSum.sum(p.sum)
		pods = append(pods, p)
	}
	node = nodeModel.sample(now, len(ws))
	return pods, node, podsSum
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
	pods, node, podsSum := n.collect(now)
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
	usage, ws, capacity := nodeMemory()
	// the model of the containers counts on top of what the runtime holds
	ws += podsSum.WorkingSetBytes
	usage = max(usage+podsSum.WorkingSetBytes, ws)
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
	pods, node, podsSum := n.collect(now)
	_, ws, _ := nodeMemory()
	ws += podsSum.WorkingSetBytes
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
