package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/metrics/pkg/apis/metrics"
)

// kubeletText is /metrics/resource of a kubelet: millisecond stamps on every
// sample.
type kubeletCtr struct {
	ns, pod, name string
	cpu           float64
	mem           int64
	start         float64
}

func kubeletText(ts time.Time, nodeCPU float64, nodeMem int64, ctrs ...kubeletCtr) string {
	ms := ts.UnixMilli()
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP node_cpu_usage_seconds_total [STABLE] Cumulative cpu time consumed by the node in core-seconds\n")
	fmt.Fprintf(&b, "# TYPE node_cpu_usage_seconds_total counter\nnode_cpu_usage_seconds_total %g %d\n", nodeCPU, ms)
	fmt.Fprintf(&b, "# HELP node_memory_working_set_bytes [STABLE] Current working set of the node in bytes\n")
	fmt.Fprintf(&b, "# TYPE node_memory_working_set_bytes gauge\nnode_memory_working_set_bytes %d %d\n", nodeMem, ms)
	fmt.Fprintf(&b, "# TYPE container_cpu_usage_seconds_total counter\n")
	for _, c := range ctrs {
		fmt.Fprintf(&b, "container_cpu_usage_seconds_total{container=%q,namespace=%q,pod=%q} %g %d\n", c.name, c.ns, c.pod, c.cpu, ms)
	}
	fmt.Fprintf(&b, "# TYPE container_memory_working_set_bytes gauge\n")
	for _, c := range ctrs {
		fmt.Fprintf(&b, "container_memory_working_set_bytes{container=%q,namespace=%q,pod=%q} %d %d\n", c.name, c.ns, c.pod, c.mem, ms)
	}
	fmt.Fprintf(&b, "# TYPE container_start_time_seconds gauge\n")
	for _, c := range ctrs {
		fmt.Fprintf(&b, "container_start_time_seconds{container=%q,namespace=%q,pod=%q} %g %d\n", c.name, c.ns, c.pod, c.start, ms)
	}
	fmt.Fprintf(&b, "# TYPE pod_cpu_usage_seconds_total counter\n")
	for _, c := range ctrs {
		fmt.Fprintf(&b, "pod_cpu_usage_seconds_total{namespace=%q,pod=%q} %g %d\n", c.ns, c.pod, c.cpu, ms)
	}
	fmt.Fprintf(&b, "# TYPE resource_scrape_error gauge\nresource_scrape_error 0\n")
	return b.String()
}

var t0 = time.Date(2025, 11, 10, 8, 0, 0, 0, time.UTC)

func scrapeText(t *testing.T, s *msStorage, node, text string) {
	t.Helper()
	r, err := parseResourceMetrics(strings.NewReader(text), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.Store(map[string]*scrapeResult{node: r})
}

func TestParseAndRate(t *testing.T) {
	web := types.NamespacedName{Namespace: "default", Name: "web"}
	start := float64(t0.Add(-time.Hour).Unix())
	s := newMetricsStorage()
	scrapeText(t, s, "tab", kubeletText(t0, 100, 1<<30,
		kubeletCtr{"default", "web", "app", 10, 64 << 20, start},
		kubeletCtr{"default", "web", "sidecar", 1, 8 << 20, start}))
	if _, ok := s.NodeUsage("tab"); ok {
		t.Error("node reported with one point")
	}
	if _, ok := s.PodUsage(web); ok {
		t.Error("pod reported with one point")
	}
	scrapeText(t, s, "tab", kubeletText(t0.Add(15*time.Second), 107.5, 2<<30,
		kubeletCtr{"default", "web", "app", 13, 70 << 20, start},
		kubeletCtr{"default", "web", "sidecar", 1.15, 9 << 20, start}))

	n, ok := s.NodeUsage("tab")
	if !ok {
		t.Fatal("no node usage")
	}
	if n.cpuNano != 500_000_000 || n.window != 15*time.Second || n.memBytes != 2<<30 || !n.ts.Equal(t0.Add(15*time.Second)) {
		t.Errorf("node usage = %+v", n)
	}
	p, ok := s.PodUsage(web)
	if !ok {
		t.Fatal("no pod usage")
	}
	if len(p.containers) != 2 || p.containers[0].name != "app" || p.containers[1].name != "sidecar" {
		t.Fatalf("containers = %+v", p.containers)
	}
	if p.containers[0].cpuNano != 200_000_000 || p.containers[0].memBytes != 70<<20 {
		t.Errorf("app = %+v", p.containers[0])
	}
	if got := p.containers[1].cpuNano; got < 9_999_000 || got > 10_000_001 {
		t.Errorf("sidecar cpu = %d, want 10m", got)
	}
	if p.window != 15*time.Second {
		t.Errorf("window = %v", p.window)
	}
}

func TestParseErrors(t *testing.T) {
	for name, text := range map[string]string{
		"garbage":    "this is not { prometheus",
		"no node":    "# TYPE container_cpu_usage_seconds_total counter\ncontainer_cpu_usage_seconds_total{container=\"a\",namespace=\"d\",pod=\"p\"} 1\n",
		"scrape err": strings.Replace(kubeletText(t0, 1, 1), "resource_scrape_error 0", "resource_scrape_error 1", 1),
	} {
		if _, err := parseResourceMetrics(strings.NewReader(text), t0); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	// A sample without a stamp gets the time of the scrape.
	r, err := parseResourceMetrics(strings.NewReader("node_cpu_usage_seconds_total 3\nnode_memory_working_set_bytes 4\n"), t0)
	if err != nil || !r.node.ts.Equal(t0) || r.node.cpuSec != 3 || r.node.mem != 4 {
		t.Errorf("untyped node = %+v, %v", r, err)
	}
}

func TestContainerRestart(t *testing.T) {
	web := types.NamespacedName{Namespace: "default", Name: "web"}
	start := float64(t0.Add(-time.Hour).Unix())
	s := newMetricsStorage()
	scrapeText(t, s, "tab", kubeletText(t0, 1, 1, kubeletCtr{"default", "web", "app", 50, 1 << 20, start}))
	scrapeText(t, s, "tab", kubeletText(t0.Add(15*time.Second), 2, 1, kubeletCtr{"default", "web", "app", 51.5, 1 << 20, start}))
	if p, ok := s.PodUsage(web); !ok || p.containers[0].cpuNano != 100_000_000 {
		t.Fatalf("before restart: %+v, %v", p, ok)
	}
	// The container restarted: its counter began again, with a new start time.
	restart := float64(t0.Add(20 * time.Second).Unix())
	scrapeText(t, s, "tab", kubeletText(t0.Add(30*time.Second), 3, 1, kubeletCtr{"default", "web", "app", 0.5, 1 << 20, restart}))
	if _, ok := s.PodUsage(web); ok {
		t.Error("pod reported right after a restart, from points of two incarnations")
	}
	scrapeText(t, s, "tab", kubeletText(t0.Add(45*time.Second), 4, 1, kubeletCtr{"default", "web", "app", 2, 1 << 20, restart}))
	p, ok := s.PodUsage(web)
	if !ok || p.containers[0].cpuNano != 100_000_000 {
		t.Errorf("after restart: %+v, %v", p, ok)
	}
	// A counter that went down without a new start time is a restart as well.
	scrapeText(t, s, "tab", kubeletText(t0.Add(60*time.Second), 5, 1, kubeletCtr{"default", "web", "app", 1, 1 << 20, restart}))
	if _, ok := s.PodUsage(web); ok {
		t.Error("pod reported after its counter decreased")
	}
	// The same stamp again (the kubelet answered from its cache) changes nothing.
	scrapeText(t, s, "tab", kubeletText(t0.Add(75*time.Second), 6, 1, kubeletCtr{"default", "web", "app", 2, 1 << 20, restart}))
	scrapeText(t, s, "tab", kubeletText(t0.Add(75*time.Second), 6, 1, kubeletCtr{"default", "web", "app", 2, 1 << 20, restart}))
	if p, ok := s.PodUsage(web); !ok || p.window != 15*time.Second {
		t.Errorf("cached answer: %+v, %v", p, ok)
	}
}

func TestPodWithMissingContainer(t *testing.T) {
	web := types.NamespacedName{Namespace: "default", Name: "web"}
	start := float64(t0.Add(-time.Hour).Unix())
	s := newMetricsStorage()
	scrapeText(t, s, "tab", kubeletText(t0, 1, 1, kubeletCtr{"default", "web", "app", 1, 1, start}))
	// The second container shows up with its first point only.
	scrapeText(t, s, "tab", kubeletText(t0.Add(15*time.Second), 2, 1,
		kubeletCtr{"default", "web", "app", 2, 1, start},
		kubeletCtr{"default", "web", "late", 0.1, 1, start}))
	if _, ok := s.PodUsage(web); ok {
		t.Error("pod reported although a container has one point")
	}
	scrapeText(t, s, "tab", kubeletText(t0.Add(30*time.Second), 3, 1,
		kubeletCtr{"default", "web", "app", 3, 1, start},
		kubeletCtr{"default", "web", "late", 0.2, 1, start}))
	if p, ok := s.PodUsage(web); !ok || len(p.containers) != 2 {
		t.Errorf("pod = %+v, %v", p, ok)
	}
	// A node that fails loses its points and its pods.
	s.Store(nil)
	if _, ok := s.PodUsage(web); ok {
		t.Error("pod of a failed node still reported")
	}
	if !s.Ready() {
		t.Error("storage not ready after a scrape")
	}
}

func testListers(nodes []*corev1.Node, pods []*corev1.Pod) (corev1listers.NodeLister, corev1listers.PodLister) {
	ni := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, n := range nodes {
		ni.Add(n)
	}
	pi := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, p := range pods {
		pi.Add(p)
	}
	return corev1listers.NewNodeLister(ni), corev1listers.NewPodLister(pi)
}

func testObjects() ([]*corev1.Node, []*corev1.Pod, *msStorage) {
	node := func(name string, l map[string]string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: l}}
	}
	pod := func(ns, name string, l map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: l}}
	}
	nodes := []*corev1.Node{node("tab", map[string]string{"role": "tab"}), node("worker", map[string]string{"role": "worker"})}
	pods := []*corev1.Pod{
		pod("default", "web-1", map[string]string{"app": "web"}),
		pod("default", "web-2", map[string]string{"app": "web"}),
		pod("default", "db", map[string]string{"app": "db"}),
		pod("other", "web-1", map[string]string{"app": "web"}),
		pod("default", "nometrics", nil),
	}
	start := float64(t0.Unix())
	s := newMetricsStorage()
	for _, ts := range []time.Duration{0, 15 * time.Second} {
		f := float64(ts / time.Second)
		r1, _ := parseResourceMetrics(strings.NewReader(kubeletText(t0.Add(ts), 10+f, 1<<30,
			kubeletCtr{"default", "web-1", "app", f, 1 << 20, start},
			kubeletCtr{"default", "web-2", "app", f, 1 << 20, start},
			kubeletCtr{"other", "web-1", "app", f, 1 << 20, start},
			kubeletCtr{"default", "gone", "app", f, 1 << 20, start})), t0)
		r2, _ := parseResourceMetrics(strings.NewReader(kubeletText(t0.Add(ts), 20+f, 2<<30,
			kubeletCtr{"default", "db", "pg", f, 5 << 20, start})), t0)
		s.Store(map[string]*scrapeResult{"tab": r1, "worker": r2})
	}
	return nodes, pods, s
}

func TestListSelectors(t *testing.T) {
	nodes, pods, store := testObjects()
	nl, pl := testListers(nodes, pods)
	nrest := &nodeMetricsREST{gr: metrics.Resource("nodemetrics"), usage: store, nodes: nl}
	prest := &podMetricsREST{gr: metrics.Resource("podmetrics"), usage: store, pods: pl}

	sel := func(l, f string) *metainternalversion.ListOptions {
		o := &metainternalversion.ListOptions{LabelSelector: labels.Everything(), FieldSelector: fields.Everything()}
		if l != "" {
			o.LabelSelector, _ = labels.Parse(l)
		}
		if f != "" {
			o.FieldSelector, _ = fields.ParseSelector(f)
		}
		return o
	}
	nodeNames := func(o *metainternalversion.ListOptions) string {
		l, err := nrest.List(context.Background(), o)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range l.(*metrics.NodeMetricsList).Items {
			names = append(names, m.Name)
		}
		return strings.Join(names, ",")
	}
	podNames := func(ns string, o *metainternalversion.ListOptions) string {
		ctx := genericapirequest.WithNamespace(context.Background(), ns)
		l, err := prest.List(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, m := range l.(*metrics.PodMetricsList).Items {
			names = append(names, m.Namespace+"/"+m.Name)
		}
		return strings.Join(names, ",")
	}
	for _, c := range []struct{ got, want string }{
		{nodeNames(nil), "tab,worker"},
		{nodeNames(sel("role=worker", "")), "worker"},
		{nodeNames(sel("", "metadata.name=tab")), "tab"},
		{nodeNames(sel("role=worker", "metadata.name=tab")), ""},
		// the pod "nometrics" has no metrics, "gone" no longer exists
		{podNames("", nil), "default/db,default/web-1,default/web-2,other/web-1"},
		{podNames("default", nil), "default/db,default/web-1,default/web-2"},
		{podNames("default", sel("app=web", "")), "default/web-1,default/web-2"},
		{podNames("", sel("app=web", "")), "default/web-1,default/web-2,other/web-1"},
		{podNames("default", sel("", "metadata.name=db")), "default/db"},
		{podNames("other", sel("app=db", "")), ""},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}

	// Labels of the Node and the Pod are copied, the usage is in the units of upstream.
	l, _ := nrest.List(context.Background(), sel("role=tab", ""))
	nm := l.(*metrics.NodeMetricsList).Items[0]
	if nm.Labels["role"] != "tab" || nm.CreationTimestamp.IsZero() || nm.Window.Duration != 15*time.Second {
		t.Errorf("node metrics = %+v", nm)
	}
	cpu, mem := nm.Usage[corev1.ResourceCPU], nm.Usage[corev1.ResourceMemory]
	if cpu.MilliValue() != 1000 || mem.Value() != 1<<30 {
		t.Errorf("usage = %v, %v", cpu.String(), mem.String())
	}

	ctx := genericapirequest.WithNamespace(context.Background(), "default")
	if _, err := prest.Get(ctx, "web-1", &metav1.GetOptions{}); err != nil {
		t.Error(err)
	}
	if _, err := prest.Get(ctx, "nometrics", &metav1.GetOptions{}); err == nil || !strings.Contains(err.Error(), `podmetrics.metrics.k8s.io "nometrics" not found`) {
		t.Errorf("Get without metrics: %v", err)
	}
	if _, err := prest.Get(ctx, "gone", &metav1.GetOptions{}); err == nil {
		t.Error("Get of a missing pod worked")
	}
	if _, err := nrest.Get(context.Background(), "nope", &metav1.GetOptions{}); err == nil {
		t.Error("Get of a missing node worked")
	}
}

func TestTableConversion(t *testing.T) {
	nodes, pods, store := testObjects()
	nl, pl := testListers(nodes, pods)
	nrest := &nodeMetricsREST{gr: metrics.Resource("nodemetrics"), usage: store, nodes: nl}
	prest := &podMetricsREST{gr: metrics.Resource("podmetrics"), usage: store, pods: pl}

	l, _ := nrest.List(context.Background(), nil)
	tab, err := nrest.ConvertToTable(context.Background(), l, nil)
	if err != nil {
		t.Fatal(err)
	}
	var cols []string
	for _, c := range tab.ColumnDefinitions {
		cols = append(cols, c.Name)
	}
	if strings.Join(cols, ",") != "Name,CPU,Memory,Window" || len(tab.Rows) != 2 {
		t.Fatalf("columns %v, rows %d", cols, len(tab.Rows))
	}
	if got := fmt.Sprint(tab.Rows[0].Cells); got != "[tab 1000m 1024Mi 15s]" {
		t.Errorf("row = %q", got)
	}
	if tab.Rows[0].Object.Object == nil {
		t.Error("row without object")
	}

	ctx := genericapirequest.WithNamespace(context.Background(), "default")
	one, err := prest.Get(ctx, "web-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tab, err = prest.ConvertToTable(ctx, one, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tab.Rows) != 1 || fmt.Sprint(tab.Rows[0].Cells) != "[web-1 1000m 1Mi 15s]" {
		t.Errorf("pod rows = %+v", tab.Rows)
	}
	if _, err := prest.ConvertToTable(ctx, &corev1.Pod{}, nil); err == nil {
		t.Error("table of a foreign object worked")
	}
}

func TestMetricsServerFlags(t *testing.T) {
	args := strings.Fields("--cert-dir=/tmp --secure-port=10250 --kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname " +
		"--kubelet-use-node-status-port --metric-resolution=15s --kubelet-insecure-tls --kubelet-port=10255 --v=2 " +
		"--tls-cipher-suites=TLS_AES_128_GCM_SHA256,TLS_AES_256_GCM_SHA384 --authorization-always-allow-paths=/livez,/readyz --logtostderr")
	fs, o := newMetricsServerFlags(io.Discard)
	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if o.certDir != "/tmp" || o.securePort != 10250 || o.resolution != 15*time.Second || !o.insecureTLS || !o.useNodeStatusPort ||
		o.kubeletPort != 10255 || strings.Join(o.preferredTypes, ",") != "InternalIP,ExternalIP,Hostname" {
		t.Errorf("options = %+v", o)
	}
	fs, o = newMetricsServerFlags(io.Discard)
	if err := fs.Parse(nil); err != nil || o.resolution != 60*time.Second || o.securePort != 443 || o.kubeletPort != 10250 {
		t.Errorf("defaults = %+v, %v", o, err)
	}
	fs, _ = newMetricsServerFlags(io.Discard)
	if err := fs.Parse([]string{"--no-such-flag=1"}); err == nil || !strings.Contains(err.Error(), "unknown flag: --no-such-flag") {
		t.Errorf("unknown flag: %v", err)
	}
}

func TestKlogLine(t *testing.T) {
	var b strings.Builder
	l := &klogOut{w: &b}
	l.error("scraper.go:149", "\"Failed to scrape node\" err=%q node=%q", "boom", "tab")
	line := b.String()
	if !strings.HasPrefix(line, "E") || !strings.Contains(line, "       1 scraper.go:149] \"Failed to scrape node\" err=\"boom\" node=\"tab\"\n") {
		t.Errorf("line = %q", line)
	}
}

// fakeKubelet is /metrics/resource on a TLS server that checks the bearer token.
type fakeKubelet struct {
	*httptest.Server
	mu    sync.Mutex
	calls int
	token string
	start float64
}

func newFakeKubelet(token string) *fakeKubelet {
	k := &fakeKubelet{token: token, start: float64(t0.Unix())}
	k.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics/resource" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+k.token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		k.mu.Lock()
		k.calls++
		n := k.calls
		k.mu.Unlock()
		// every call is 10 seconds and 2 CPU-seconds later
		ts := t0.Add(time.Duration(n) * 10 * time.Second)
		io.WriteString(w, kubeletText(ts, float64(2*n), int64(n)<<20,
			kubeletCtr{"default", "web", "app", float64(n), 7 << 20, k.start}))
	}))
	return k
}

func TestScraper(t *testing.T) {
	kl := newFakeKubelet("secret")
	defer kl.Close()
	host, portStr, _ := net.SplitHostPort(strings.TrimPrefix(kl.URL, "https://"))
	var port int32
	fmt.Sscan(portStr, &port)
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte("wrong\n"), 0o600)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "tab"},
		Status: corev1.NodeStatus{
			Addresses:       []corev1.NodeAddress{{Type: corev1.NodeHostName, Address: "tab"}, {Type: corev1.NodeInternalIP, Address: host}},
			DaemonEndpoints: corev1.NodeDaemonEndpoints{KubeletEndpoint: corev1.DaemonEndpoint{Port: port}},
		},
	}
	nl, _ := testListers([]*corev1.Node{node}, nil)
	hc, err := newKubeletClient(kubeletScraperConfig{insecureTLS: true})
	if err != nil {
		t.Fatal(err)
	}
	store := newMetricsStorage()
	var errs []string
	s := &kubeletScraper{
		nodes: nl, storage: store, client: hc, tokenFile: tokenFile, timeout: 5 * time.Second,
		preferred:         []corev1.NodeAddressType{"InternalIP", "ExternalIP", "Hostname"},
		useNodeStatusPort: true, port: 1,
		logErr: func(node string, err error) { errs = append(errs, node+": "+err.Error()) },
	}
	ctx := context.Background()

	// A wrong token is an error of that node, and nothing is stored for it.
	s.scrapeOnce(ctx)
	if len(errs) != 1 || !strings.Contains(errs[0], "401 Unauthorized") || !strings.HasPrefix(errs[0], "tab: ") {
		t.Fatalf("errors = %q", errs)
	}
	if _, ok := store.NodeUsage("tab"); ok || !store.Ready() {
		t.Error("storage after a failed scrape")
	}

	// The token is read for every scrape.
	os.WriteFile(tokenFile, []byte("secret\n"), 0o600)
	s.scrapeOnce(ctx)
	s.scrapeOnce(ctx)
	if len(errs) != 1 {
		t.Fatalf("errors = %q", errs)
	}
	n, ok := store.NodeUsage("tab")
	if !ok || n.cpuNano != 200_000_000 || n.window != 10*time.Second {
		t.Errorf("node usage = %+v, %v", n, ok)
	}
	p, ok := store.PodUsage(types.NamespacedName{Namespace: "default", Name: "web"})
	if !ok || p.containers[0].cpuNano != 100_000_000 || p.containers[0].memBytes != 7<<20 {
		t.Errorf("pod usage = %+v, %v", p, ok)
	}
	if err := s.timely(time.Minute); err != nil {
		t.Error(err)
	}

	// Without --kubelet-use-node-status-port the port comes from --kubelet-port.
	s.useNodeStatusPort = false
	if u, _ := s.nodeURL(node); !strings.HasSuffix(u, ":1/metrics/resource") {
		t.Errorf("url = %q", u)
	}
	// By preference: the Hostname comes before the IP.
	s.preferred = []corev1.NodeAddressType{"Hostname", "InternalIP"}
	if u, _ := s.nodeURL(node); !strings.HasPrefix(u, "https://tab:") {
		t.Errorf("url = %q", u)
	}
	s.preferred = []corev1.NodeAddressType{"ExternalDNS"}
	if _, err := s.nodeURL(node); err == nil {
		t.Error("node without a preferred address type")
	}

	// Verification is on without --kubelet-insecure-tls.
	hc, _ = newKubeletClient(kubeletScraperConfig{})
	s.client, s.useNodeStatusPort = hc, true
	s.preferred = []corev1.NodeAddressType{"InternalIP"}
	errs = nil
	s.scrapeOnce(ctx)
	if len(errs) != 1 || !strings.Contains(errs[0], "certificate") {
		t.Errorf("errors = %q", errs)
	}
}

// testServer runs the apiserver on a local listener without delegated
// authentication, and returns a client of it.
func testServer(t *testing.T, store *msStorage, nl corev1listers.NodeLister, pl corev1listers.PodLister) (*http.Client, string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, generated, err := msServingCert(t.TempDir(), "localhost", nil, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil || !generated {
		t.Fatal(generated, err)
	}
	srv, err := newMetricsAPIServer(&msServerConfig{
		listener: ln, cert: cert, key: key, nodes: nl, pods: pl, usage: store, version: "v0.8.0",
		auth: func(c *genericapiserver.Config) error {
			c.Authentication.Authenticator = authenticator.RequestFunc(func(*http.Request) (*authenticator.Response, bool, error) {
				return &authenticator.Response{User: &user.DefaultInfo{Name: "test", Groups: []string{"system:authenticated"}}}, true, nil
			})
			c.Authorization.Authorizer = authorizerfactory.NewAlwaysAllowAuthorizer()
			return nil
		},
		ready: []healthz.HealthChecker{healthz.NamedCheck("metric-storage-ready", func(*http.Request) error {
			if !store.Ready() {
				return fmt.Errorf("no metrics to serve")
			}
			return nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.PrepareRun().RunWithContext(ctx) }()
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		DialContext:     (&net.Dialer{}).DialContext,
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	return client, "https://" + ln.Addr().String(), func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the apiserver did not stop")
		}
		client.CloseIdleConnections()
	}
}

func get(t *testing.T, c *http.Client, url, accept string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestAPIServer(t *testing.T) {
	nodes, pods, store := testObjects()
	nl, pl := testListers(nodes, pods)
	// The program runs again when its container restarts: start the server
	// twice in this process.
	for round := 1; round <= 2; round++ {
		client, base, stop := testServer(t, store, nl, pl)

		code, body := get(t, client, base+"/apis/metrics.k8s.io/v1beta1", "")
		if code != 200 || !strings.Contains(string(body), `"nodes"`) || !strings.Contains(string(body), `"pods"`) {
			t.Fatalf("round %d: discovery: %d %s", round, code, body)
		}
		code, body = get(t, client, base+"/apis/metrics.k8s.io/v1beta1/nodes?labelSelector=role%3Dtab", "")
		var list struct {
			Kind  string
			Items []struct {
				Metadata struct{ Name string }
				Window   string
				Usage    map[string]string
			}
		}
		if err := json.Unmarshal(body, &list); err != nil || code != 200 {
			t.Fatalf("round %d: nodes: %d %s", round, code, body)
		}
		if list.Kind != "NodeMetricsList" || len(list.Items) != 1 || list.Items[0].Metadata.Name != "tab" ||
			list.Items[0].Window != "15s" || list.Items[0].Usage["cpu"] != "1" || list.Items[0].Usage["memory"] != "1Gi" {
			t.Errorf("round %d: nodes = %s", round, body)
		}
		code, body = get(t, client, base+"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods/web-1", "")
		if code != 200 || !strings.Contains(string(body), `"kind":"PodMetrics"`) || !strings.Contains(string(body), `"name":"app"`) {
			t.Errorf("round %d: pod: %d %s", round, code, body)
		}
		code, _ = get(t, client, base+"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods/nometrics", "")
		if code != 404 {
			t.Errorf("round %d: pod without metrics: %d", round, code)
		}
		code, body = get(t, client, base+"/apis/metrics.k8s.io/v1beta1/namespaces/default/pods", "application/json;as=Table;g=meta.k8s.io;v=v1")
		if code != 200 || !strings.Contains(string(body), `"kind":"Table"`) || !strings.Contains(string(body), `"Window"`) || !strings.Contains(string(body), `"web-1"`) {
			t.Errorf("round %d: table: %d %s", round, code, body)
		}
		code, body = get(t, client, base+"/openapi/v3/apis/metrics.k8s.io/v1beta1", "")
		if code != 200 || !strings.Contains(string(body), "NodeMetrics") {
			t.Errorf("round %d: openapi: %d %.200s", round, code, body)
		}
		for _, path := range []string{"/livez", "/healthz", "/readyz/metric-storage-ready"} {
			if code, body := get(t, client, base+path, ""); code != 200 {
				t.Errorf("round %d: %s: %d %s", round, path, code, body)
			}
		}
		stop()
	}

	// readyz fails until the first scrape completed.
	nl2, pl2 := testListers(nodes, pods)
	client, base, stop := testServer(t, newMetricsStorage(), nl2, pl2)
	defer stop()
	if code, body := get(t, client, base+"/readyz/metric-storage-ready", ""); code != 500 || !strings.Contains(string(body), "no metrics to serve") {
		t.Errorf("readyz before the first scrape: %d %s", code, body)
	}
}

// fakeKubeAPIServer answers what the delegated authentication and
// authorization of the metrics API ask the kube-apiserver: the ConfigMap
// extension-apiserver-authentication, TokenReview and SubjectAccessReview.
func fakeKubeAPIServer(caPEM []byte) *httptest.Server {
	cm := map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": "extension-apiserver-authentication", "namespace": "kube-system", "resourceVersion": "1"},
		"data": map[string]string{
			"client-ca-file":                     string(caPEM),
			"requestheader-client-ca-file":       string(caPEM),
			"requestheader-allowed-names":        `["front-proxy-client"]`,
			"requestheader-extra-headers-prefix": `["X-Remote-Extra-"]`,
			"requestheader-group-headers":        `["X-Remote-Group"]`,
			"requestheader-username-headers":     `["X-Remote-User"]`,
		},
	}
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/api/v1/namespaces/kube-system/configmaps"):
			if r.URL.Query().Get("watch") == "true" {
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			if strings.HasSuffix(p, "/extension-apiserver-authentication") {
				json.NewEncoder(w).Encode(cm)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"apiVersion": "v1", "kind": "ConfigMapList", "metadata": map[string]any{"resourceVersion": "1"}, "items": []any{cm}})
		case p == "/apis/authentication.k8s.io/v1/tokenreviews":
			var tr map[string]any
			json.NewDecoder(r.Body).Decode(&tr)
			if tr["spec"].(map[string]any)["token"] == "good" {
				tr["status"] = map[string]any{"authenticated": true, "user": map[string]any{"username": "alice", "groups": []string{"system:authenticated"}}}
			} else {
				tr["status"] = map[string]any{"authenticated": false}
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(tr)
		case p == "/apis/authorization.k8s.io/v1/subjectaccessreviews":
			var sar map[string]any
			json.NewDecoder(r.Body).Decode(&sar)
			sar["status"] = map[string]any{"allowed": sar["spec"].(map[string]any)["user"] == "alice"}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(sar)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestDelegatedAuth(t *testing.T) {
	nodes, pods, store := testObjects()
	nl, pl := testListers(nodes, pods)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, _, err := msServingCert(t.TempDir(), "localhost", nil, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	kube := fakeKubeAPIServer(cert)
	defer kube.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	os.WriteFile(ca, pemOf(kube.Certificate().Raw), 0o644)
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("sa-token"), 0o600)
	kubeconfig := filepath.Join(dir, "kubeconfig")
	os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: %s, certificate-authority: %s}
contexts:
- name: c
  context: {cluster: c, user: sa}
current-context: c
users:
- name: sa
  user: {tokenFile: %s}
`, kube.URL, ca, token)), 0o600)

	var mu sync.Mutex
	dials := 0
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	srv, err := newMetricsAPIServer(&msServerConfig{
		listener: ln, cert: cert, key: key, nodes: nl, pods: pl, usage: store,
		auth: delegatedAuth(kubeconfig, dialWrapper(dial, ca)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.PrepareRun().RunWithContext(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("the apiserver did not stop")
		}
	}()
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		DialContext: (&net.Dialer{}).DialContext, TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	defer client.CloseIdleConnections()
	do := func(path, auth string) int {
		req, _ := http.NewRequest(http.MethodGet, "https://"+ln.Addr().String()+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		path, token string
		want        int
	}{
		{"/apis/metrics.k8s.io/v1beta1/nodes", "good", 200},
		{"/apis/metrics.k8s.io/v1beta1/nodes", "", 403},    // anonymous, denied by the SubjectAccessReview
		{"/apis/metrics.k8s.io/v1beta1/nodes", "bad", 401}, // TokenReview says no
		{"/livez", "", 200}, // always allowed
	} {
		if got := do(c.path, c.token); got != c.want {
			t.Errorf("GET %s with token %q: %d, want %d", c.path, c.token, got, c.want)
		}
	}
	mu.Lock()
	if dials == 0 {
		t.Error("the delegated clients did not use our dialer")
	}
	mu.Unlock()
}

func pemOf(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
