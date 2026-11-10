package main

// The scraper and the storage of the built-in metrics-server
// (tabnode_images_metrics.go, metricsserver.go): a port of pkg/scraper and
// pkg/storage of sigs.k8s.io/metrics-server. Every resolution it reads
// /metrics/resource from the kubelet of each node (the Prometheus text format
// of the real kubelet) and keeps the last two points of the node and of each
// container. CPU usage is the rate of the cumulative counter between them.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corev1listers "k8s.io/client-go/listers/core/v1"
)

// nodePoint is one reading of the node: cumulative CPU seconds and the
// working set of memory.
type nodePoint struct {
	ts     time.Time
	cpuSec float64
	mem    int64
}

// ctrPoint is one reading of a container. A new start time means the
// container restarted and its counter began again.
type ctrPoint struct {
	ts     time.Time
	start  time.Time
	cpuSec float64
	mem    int64
}

// scrapeResult is what one kubelet reports.
type scrapeResult struct {
	node *nodePoint
	pods map[types.NamespacedName]map[string]*ctrPoint
}

// parseResourceMetrics reads the output of /metrics/resource. The kubelet
// stamps each sample in milliseconds; now stands in for a missing stamp.
func parseResourceMetrics(r io.Reader, now time.Time) (*scrapeResult, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return nil, fmt.Errorf("unable to parse metrics: %w", err)
	}
	if f := fams["resource_scrape_error"]; f != nil && len(f.Metric) > 0 && sampleValue(f.Metric[0]) != 0 {
		return nil, fmt.Errorf("the kubelet reported resource_scrape_error")
	}
	res := &scrapeResult{pods: map[types.NamespacedName]map[string]*ctrPoint{}}

	var cpu, mem *dto.Metric
	if f := fams["node_cpu_usage_seconds_total"]; f != nil && len(f.Metric) > 0 {
		cpu = f.Metric[0]
	}
	if f := fams["node_memory_working_set_bytes"]; f != nil && len(f.Metric) > 0 {
		mem = f.Metric[0]
	}
	if cpu == nil || mem == nil {
		return nil, fmt.Errorf("node_cpu_usage_seconds_total or node_memory_working_set_bytes missing in the metrics")
	}
	res.node = &nodePoint{ts: sampleTime(cpu, now), cpuSec: sampleValue(cpu), mem: int64(sampleValue(mem))}

	ctr := func(m *dto.Metric) *ctrPoint {
		l := metricLabels(m)
		if l["container"] == "" || l["pod"] == "" {
			return nil
		}
		key := types.NamespacedName{Namespace: l["namespace"], Name: l["pod"]}
		cs := res.pods[key]
		if cs == nil {
			cs = map[string]*ctrPoint{}
			res.pods[key] = cs
		}
		p := cs[l["container"]]
		if p == nil {
			p = &ctrPoint{mem: -1}
			cs[l["container"]] = p
		}
		return p
	}
	// Containers need CPU and memory; the start time is optional.
	hasCPU := map[*ctrPoint]bool{}
	if f := fams["container_cpu_usage_seconds_total"]; f != nil {
		for _, m := range f.Metric {
			if p := ctr(m); p != nil {
				p.ts, p.cpuSec = sampleTime(m, now), sampleValue(m)
				hasCPU[p] = true
			}
		}
	}
	if f := fams["container_memory_working_set_bytes"]; f != nil {
		for _, m := range f.Metric {
			if p := ctr(m); p != nil {
				p.mem = int64(sampleValue(m))
			}
		}
	}
	if f := fams["container_start_time_seconds"]; f != nil {
		for _, m := range f.Metric {
			if p := ctr(m); p != nil {
				sec := sampleValue(m)
				p.start = time.Unix(0, int64(sec*1e9))
			}
		}
	}
	for key, cs := range res.pods {
		for name, p := range cs {
			if !hasCPU[p] || p.mem < 0 {
				delete(cs, name)
			}
		}
		if len(cs) == 0 {
			delete(res.pods, key)
		}
	}
	return res, nil
}

func sampleValue(m *dto.Metric) float64 {
	switch {
	case m.Counter != nil:
		return m.Counter.GetValue()
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Untyped != nil:
		return m.Untyped.GetValue()
	}
	return 0
}

func sampleTime(m *dto.Metric, now time.Time) time.Time {
	if m.TimestampMs != nil {
		return time.UnixMilli(m.GetTimestampMs())
	}
	return now
}

func metricLabels(m *dto.Metric) map[string]string {
	l := make(map[string]string, len(m.Label))
	for _, p := range m.Label {
		l[p.GetName()] = p.GetValue()
	}
	return l
}

// nodeSeries and ctrSeries hold the last two points. prev is nil until a
// second point arrived or after a restart.
type nodeSeries struct{ last, prev *nodePoint }
type ctrSeries struct{ last, prev *ctrPoint }

func (o *nodeSeries) advance(p *nodePoint) *nodeSeries {
	switch {
	case o == nil || o.last == nil:
		return &nodeSeries{last: p}
	case !p.ts.After(o.last.ts):
		return o // the kubelet answered from its cache
	case p.cpuSec < o.last.cpuSec:
		return &nodeSeries{last: p} // the counter began again
	}
	return &nodeSeries{last: p, prev: o.last}
}

func (o *ctrSeries) advance(p *ctrPoint) *ctrSeries {
	switch {
	case o == nil || o.last == nil:
		return &ctrSeries{last: p}
	case !p.ts.After(o.last.ts):
		return o
	case !p.start.Equal(o.last.start) || p.cpuSec < o.last.cpuSec:
		return &ctrSeries{last: p} // restarted: the old point does not count
	}
	return &ctrSeries{last: p, prev: o.last}
}

// rate is the CPU use in nano-cores between two readings.
func cpuRate(prevTS, lastTS time.Time, prevSec, lastSec float64) (nano int64, window time.Duration, ok bool) {
	window = lastTS.Sub(prevTS)
	if window <= 0 || lastSec < prevSec {
		return 0, 0, false
	}
	return int64((lastSec - prevSec) / window.Seconds() * 1e9), window, true
}

// nodeUsage and podUsage are what the API serves.
type nodeUsage struct {
	ts       time.Time
	window   time.Duration
	cpuNano  int64
	memBytes int64
}

type msCtrUsage struct {
	name     string
	cpuNano  int64
	memBytes int64
}

type podUsage struct {
	ts         time.Time
	window     time.Duration
	containers []msCtrUsage
}

// msStorage is pkg/storage: metrics by node and by pod.
type msStorage struct {
	mu    sync.RWMutex
	nodes map[string]*nodeSeries
	pods  map[types.NamespacedName]map[string]*ctrSeries
	ready bool
}

func newMetricsStorage() *msStorage {
	return &msStorage{nodes: map[string]*nodeSeries{}, pods: map[types.NamespacedName]map[string]*ctrSeries{}}
}

// Store takes the results of one scrape. Nodes that failed are not in it, so
// they and their pods lose their points, like upstream.
func (s *msStorage) Store(results map[string]*scrapeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes := map[string]*nodeSeries{}
	pods := map[types.NamespacedName]map[string]*ctrSeries{}
	for name, r := range results {
		nodes[name] = s.nodes[name].advance(r.node)
		for key, cs := range r.pods {
			series := map[string]*ctrSeries{}
			for cname, p := range cs {
				series[cname] = s.pods[key][cname].advance(p)
			}
			pods[key] = series
		}
	}
	s.nodes, s.pods, s.ready = nodes, pods, true
}

// Ready is true after the first scrape finished.
func (s *msStorage) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

// NodeUsage is the usage of a node, if it has two points.
func (s *msStorage) NodeUsage(name string) (nodeUsage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ser := s.nodes[name]
	if ser == nil || ser.prev == nil {
		return nodeUsage{}, false
	}
	nano, window, ok := cpuRate(ser.prev.ts, ser.last.ts, ser.prev.cpuSec, ser.last.cpuSec)
	if !ok {
		return nodeUsage{}, false
	}
	return nodeUsage{ts: ser.last.ts, window: window, cpuNano: nano, memBytes: ser.last.mem}, true
}

// PodUsage is the usage of a pod. All its containers need two points: one that
// just started or restarted keeps the pod out for a scrape.
func (s *msStorage) PodUsage(key types.NamespacedName) (podUsage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cs := s.pods[key]
	if len(cs) == 0 {
		return podUsage{}, false
	}
	var u podUsage
	for name, ser := range cs {
		if ser.prev == nil {
			return podUsage{}, false
		}
		nano, window, ok := cpuRate(ser.prev.ts, ser.last.ts, ser.prev.cpuSec, ser.last.cpuSec)
		if !ok {
			return podUsage{}, false
		}
		u.containers = append(u.containers, msCtrUsage{name: name, cpuNano: nano, memBytes: ser.last.mem})
		if ser.last.ts.After(u.ts) {
			u.ts = ser.last.ts
		}
		if window > u.window {
			u.window = window
		}
	}
	sort.Slice(u.containers, func(i, j int) bool { return u.containers[i].name < u.containers[j].name })
	return u, true
}

// kubeletScraper reads /metrics/resource of every node.
type kubeletScraper struct {
	nodes     corev1listers.NodeLister
	selector  labels.Selector
	storage   *msStorage
	client    *http.Client
	tokenFile string
	// address types in the order of preference, as the flag gives them
	preferred         []corev1.NodeAddressType
	useNodeStatusPort bool
	port              int
	timeout           time.Duration
	// logErr reports a failed node; may be nil.
	logErr func(node string, err error)

	mu       sync.Mutex
	lastTick time.Time
}

type kubeletScraperConfig struct {
	insecureTLS bool
	caFile      string
	// dial leaves from the pod (dialCluster); nil uses the default dialer.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

func newKubeletClient(c kubeletScraperConfig) (*http.Client, error) {
	tc := &tls.Config{InsecureSkipVerify: c.insecureTLS} //nolint:gosec // --kubelet-insecure-tls
	if c.caFile != "" {
		pem, err := os.ReadFile(c.caFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read --kubelet-certificate-authority: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates in %s", c.caFile)
		}
		tc.RootCAs = pool
	}
	dial := c.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	// DialContext also keeps the transport off fetch() of the browser.
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:     tc,
		DialContext:         dial,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}}, nil
}

// nodeURL is the address the scraper uses: the first address of the first
// preferred type that the node has.
func (s *kubeletScraper) nodeURL(node *corev1.Node) (string, error) {
	var addr string
	for _, t := range s.preferred {
		for _, a := range node.Status.Addresses {
			if a.Type == t && a.Address != "" {
				addr = a.Address
				break
			}
		}
		if addr != "" {
			break
		}
	}
	if addr == "" {
		return "", fmt.Errorf("no address of type %v found on node %q", s.preferred, node.Name)
	}
	port := s.port
	if p := node.Status.DaemonEndpoints.KubeletEndpoint.Port; s.useNodeStatusPort && p != 0 {
		port = int(p)
	}
	return "https://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/metrics/resource", nil
}

func (s *kubeletScraper) scrapeNode(ctx context.Context, node *corev1.Node) (*scrapeResult, error) {
	url, err := s.nodeURL(node)
	if err != nil {
		return nil, err
	}
	// The token rotates, so it is read each time.
	token, err := os.ReadFile(s.tokenFile)
	if err != nil {
		return nil, fmt.Errorf("unable to read the service account token: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "text/plain")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 16<<20)
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(body, 256))
		return nil, fmt.Errorf("request failed - %q, response: %q", resp.Status, strings.TrimSpace(string(msg)))
	}
	return parseResourceMetrics(body, time.Now())
}

// scrapeOnce reads all nodes at the same time and stores what came back.
func (s *kubeletScraper) scrapeOnce(ctx context.Context) {
	sel := s.selector
	if sel == nil {
		sel = labels.Everything()
	}
	nodes, err := s.nodes.List(sel)
	if err != nil {
		if s.logErr != nil {
			s.logErr("", err)
		}
		return
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		results = map[string]*scrapeResult{}
	)
	for _, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.scrapeNode(ctx, n)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if s.logErr != nil && ctx.Err() == nil {
					s.logErr(n.Name, err)
				}
				return
			}
			results[n.Name] = r
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	s.storage.Store(results)
	s.mu.Lock()
	s.lastTick = time.Now()
	s.mu.Unlock()
}

// run scrapes at once and then every resolution until ctx ends.
func (s *kubeletScraper) run(ctx context.Context, resolution time.Duration) {
	t := time.NewTicker(resolution)
	defer t.Stop()
	for {
		s.scrapeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// timely tells whether the last scrape is recent: the liveness check
// metric-collection-timely.
func (s *kubeletScraper) timely(within time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastTick.IsZero() && time.Since(s.lastTick) > within {
		return fmt.Errorf("no metrics collected for %v", time.Since(s.lastTick).Round(time.Second))
	}
	return nil
}
