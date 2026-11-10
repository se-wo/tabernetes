package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	stats "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
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

func TestNodeCPUMonotonic(t *testing.T) {
	now := time.Now()
	var last uint64
	for i := range 20 {
		u := nodeModel.sample(now.Add(time.Duration(i)*time.Second), 3)
		if u.UsageCoreNanoSeconds < last {
			t.Fatalf("node cpu went back")
		}
		last = u.UsageCoreNanoSeconds
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
