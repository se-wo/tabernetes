package main

// The API server of the built-in metrics-server (tabnode_images_metrics.go):
// a real k8s.io/apiserver GenericAPIServer that serves metrics.k8s.io/v1beta1
// (NodeMetrics and PodMetrics) from the storage of the scraper
// (metricsserver_scrape.go), like pkg/server and pkg/api of
// sigs.k8s.io/metrics-server.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/server/healthz"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/util/compatibility"
	corev1listers "k8s.io/client-go/listers/core/v1"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	certutil "k8s.io/client-go/util/cert"
	generatedopenapi "k8s.io/kubernetes/pkg/generated/openapi"
	"k8s.io/metrics/pkg/apis/metrics"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

// msUsageProvider is the storage as the API sees it.
type msUsageProvider interface {
	NodeUsage(name string) (nodeUsage, bool)
	PodUsage(key types.NamespacedName) (podUsage, bool)
}

func nodeMetricsOf(node *corev1.Node, u nodeUsage) metrics.NodeMetrics {
	return metrics.NodeMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name:              node.Name,
			Labels:            maps.Clone(node.Labels),
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Timestamp: metav1.NewTime(u.ts),
		Window:    metav1.Duration{Duration: u.window},
		Usage:     usageList(u.cpuNano, u.memBytes),
	}
}

func podMetricsOf(pod *corev1.Pod, u podUsage) metrics.PodMetrics {
	pm := metrics.PodMetrics{
		ObjectMeta: metav1.ObjectMeta{
			Name:              pod.Name,
			Namespace:         pod.Namespace,
			Labels:            maps.Clone(pod.Labels),
			CreationTimestamp: metav1.NewTime(time.Now()),
		},
		Timestamp: metav1.NewTime(u.ts),
		Window:    metav1.Duration{Duration: u.window},
	}
	for _, c := range u.containers {
		pm.Containers = append(pm.Containers, metrics.ContainerMetrics{Name: c.name, Usage: usageList(c.cpuNano, c.memBytes)})
	}
	return pm
}

// usageList holds the CPU in nano-cores and the memory in bytes, as upstream.
func usageList(cpuNano, memBytes int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewScaledQuantity(cpuNano, resource.Nano),
		corev1.ResourceMemory: *resource.NewQuantity(memBytes, resource.BinarySI),
	}
}

// selectors of a list request: labels and the field metadata.name (and
// metadata.namespace for pods).
func listSelectors(o *metainternalversion.ListOptions) (labels.Selector, fields.Selector) {
	ls, fs := labels.Everything(), fields.Everything()
	if o != nil {
		if o.LabelSelector != nil {
			ls = o.LabelSelector
		}
		if o.FieldSelector != nil {
			fs = o.FieldSelector
		}
	}
	return ls, fs
}

// --- nodes ---

type nodeMetricsREST struct {
	gr    schema.GroupResource
	usage msUsageProvider
	nodes corev1listers.NodeLister
}

var (
	_ rest.Storage              = &nodeMetricsREST{}
	_ rest.KindProvider         = &nodeMetricsREST{}
	_ rest.SingularNameProvider = &nodeMetricsREST{}
	_ rest.Getter               = &nodeMetricsREST{}
	_ rest.Lister               = &nodeMetricsREST{}
	_ rest.Scoper               = &nodeMetricsREST{}
	_ rest.TableConvertor       = &nodeMetricsREST{}
	_ rest.Storage              = &podMetricsREST{}
	_ rest.KindProvider         = &podMetricsREST{}
	_ rest.SingularNameProvider = &podMetricsREST{}
	_ rest.Getter               = &podMetricsREST{}
	_ rest.Lister               = &podMetricsREST{}
	_ rest.Scoper               = &podMetricsREST{}
	_ rest.TableConvertor       = &podMetricsREST{}
)

func (m *nodeMetricsREST) New() runtime.Object     { return &metrics.NodeMetrics{} }
func (m *nodeMetricsREST) NewList() runtime.Object { return &metrics.NodeMetricsList{} }
func (m *nodeMetricsREST) Destroy()                {}
func (m *nodeMetricsREST) Kind() string            { return "NodeMetrics" }
func (m *nodeMetricsREST) GetSingularName() string { return "nodemetrics" }
func (m *nodeMetricsREST) NamespaceScoped() bool   { return false }

func (m *nodeMetricsREST) List(ctx context.Context, o *metainternalversion.ListOptions) (runtime.Object, error) {
	ls, fs := listSelectors(o)
	nodes, err := m.nodes.List(ls)
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("failed to list nodes: %w", err))
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	list := &metrics.NodeMetricsList{}
	for _, n := range nodes {
		if !fs.Matches(fields.Set{"metadata.name": n.Name}) {
			continue
		}
		if u, ok := m.usage.NodeUsage(n.Name); ok {
			list.Items = append(list.Items, nodeMetricsOf(n, u))
		}
	}
	return list, nil
}

func (m *nodeMetricsREST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	node, err := m.nodes.Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, apierrors.NewInternalError(fmt.Errorf("failed to find node %q: %w", name, err))
	}
	u, ok := m.usage.NodeUsage(name)
	if !ok {
		return nil, apierrors.NewNotFound(m.gr, name)
	}
	nm := nodeMetricsOf(node, u)
	return &nm, nil
}

var nodeColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: "Name of the node."},
	{Name: "CPU", Type: "string", Description: "CPU usage of the node."},
	{Name: "Memory", Type: "string", Description: "Memory usage (working set) of the node."},
	{Name: "Window", Type: "string", Description: "The time window the CPU usage is averaged over."},
}

var podColumns = []metav1.TableColumnDefinition{
	{Name: "Name", Type: "string", Format: "name", Description: "Name of the pod."},
	{Name: "CPU", Type: "string", Description: "CPU usage of all containers of the pod."},
	{Name: "Memory", Type: "string", Description: "Memory usage (working set) of all containers of the pod."},
	{Name: "Window", Type: "string", Description: "The time window the CPU usage is averaged over."},
}

func cpuCell(q resource.Quantity) string { return fmt.Sprintf("%dm", q.MilliValue()) }
func memCell(q resource.Quantity) string { return fmt.Sprintf("%dMi", q.Value()/(1<<20)) }

func windowCell(d metav1.Duration) string { return d.Duration.Round(time.Millisecond).String() }

func (m *nodeMetricsREST) ConvertToTable(ctx context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	var items []metrics.NodeMetrics
	t := &metav1.Table{ColumnDefinitions: nodeColumns}
	switch o := obj.(type) {
	case *metrics.NodeMetrics:
		items = []metrics.NodeMetrics{*o}
	case *metrics.NodeMetricsList:
		items, t.ListMeta = o.Items, o.ListMeta
	default:
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
	for i := range items {
		nm := &items[i]
		t.Rows = append(t.Rows, metav1.TableRow{
			Cells:  []any{nm.Name, cpuCell(nm.Usage[corev1.ResourceCPU]), memCell(nm.Usage[corev1.ResourceMemory]), windowCell(nm.Window)},
			Object: runtime.RawExtension{Object: nm},
		})
	}
	return t, nil
}

// --- pods ---

type podMetricsREST struct {
	gr    schema.GroupResource
	usage msUsageProvider
	pods  corev1listers.PodLister
}

func (m *podMetricsREST) New() runtime.Object     { return &metrics.PodMetrics{} }
func (m *podMetricsREST) NewList() runtime.Object { return &metrics.PodMetricsList{} }
func (m *podMetricsREST) Destroy()                {}
func (m *podMetricsREST) Kind() string            { return "PodMetrics" }
func (m *podMetricsREST) GetSingularName() string { return "podmetrics" }
func (m *podMetricsREST) NamespaceScoped() bool   { return true }

func (m *podMetricsREST) List(ctx context.Context, o *metainternalversion.ListOptions) (runtime.Object, error) {
	ls, fs := listSelectors(o)
	ns := genericapirequest.NamespaceValue(ctx)
	var (
		pods []*corev1.Pod
		err  error
	)
	if ns == metav1.NamespaceAll {
		pods, err = m.pods.List(ls)
	} else {
		pods, err = m.pods.Pods(ns).List(ls)
	}
	if err != nil {
		return nil, apierrors.NewInternalError(fmt.Errorf("failed to list pods: %w", err))
	}
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
	list := &metrics.PodMetricsList{}
	for _, p := range pods {
		if !fs.Matches(fields.Set{"metadata.name": p.Name, "metadata.namespace": p.Namespace}) {
			continue
		}
		if u, ok := m.usage.PodUsage(types.NamespacedName{Namespace: p.Namespace, Name: p.Name}); ok {
			list.Items = append(list.Items, podMetricsOf(p, u))
		}
	}
	return list, nil
}

func (m *podMetricsREST) Get(ctx context.Context, name string, _ *metav1.GetOptions) (runtime.Object, error) {
	ns := genericapirequest.NamespaceValue(ctx)
	pod, err := m.pods.Pods(ns).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, err
		}
		return nil, apierrors.NewInternalError(fmt.Errorf("failed to find pod %s/%s: %w", ns, name, err))
	}
	u, ok := m.usage.PodUsage(types.NamespacedName{Namespace: ns, Name: name})
	if !ok {
		return nil, apierrors.NewNotFound(m.gr, name)
	}
	pm := podMetricsOf(pod, u)
	return &pm, nil
}

func (m *podMetricsREST) ConvertToTable(ctx context.Context, obj runtime.Object, _ runtime.Object) (*metav1.Table, error) {
	var items []metrics.PodMetrics
	t := &metav1.Table{ColumnDefinitions: podColumns}
	switch o := obj.(type) {
	case *metrics.PodMetrics:
		items = []metrics.PodMetrics{*o}
	case *metrics.PodMetricsList:
		items, t.ListMeta = o.Items, o.ListMeta
	default:
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
	for i := range items {
		pm := &items[i]
		cpu, mem := resource.Quantity{}, resource.Quantity{}
		for _, c := range pm.Containers {
			cpu.Add(c.Usage[corev1.ResourceCPU])
			mem.Add(c.Usage[corev1.ResourceMemory])
		}
		t.Rows = append(t.Rows, metav1.TableRow{
			Cells:  []any{pm.Name, cpuCell(cpu), memCell(mem), windowCell(pm.Window)},
			Object: runtime.RawExtension{Object: pm},
		})
	}
	return t, nil
}

// --- the server ---

// msServerConfig is what newMetricsAPIServer needs.
type msServerConfig struct {
	listener net.Listener
	// serving certificate and key (PEM)
	cert, key []byte
	nodes     corev1listers.NodeLister
	pods      corev1listers.PodLister
	usage     msUsageProvider
	version   string
	// auth sets the authenticator and the authorizer.
	auth func(c *genericapiserver.Config) error
	// readyz and livez checks besides the generic ones
	ready, live []healthz.HealthChecker
}

// newMetricsAPIServer builds the apiserver. Everything is per instance (the
// scheme, the codecs, the checks), so the program can run again in the same
// process.
func newMetricsAPIServer(cfg *msServerConfig) (*genericapiserver.GenericAPIServer, error) {
	scheme := runtime.NewScheme()
	if err := metrics.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1beta1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := scheme.SetVersionPriority(v1beta1.SchemeGroupVersion); err != nil {
		return nil, err
	}
	// the options of requests (ListOptions and friends) for the empty group
	metav1.AddToGroupVersion(scheme, schema.GroupVersion{Version: "v1"})
	scheme.AddUnversionedTypes(schema.GroupVersion{Version: "v1"},
		&metav1.Status{}, &metav1.APIVersions{}, &metav1.APIGroupList{}, &metav1.APIGroup{}, &metav1.APIResourceList{})
	codecs := serializer.NewCodecFactory(scheme)

	c := genericapiserver.NewConfig(codecs)
	c.EffectiveVersion = compatibility.DefaultBuildEffectiveVersion()
	c.EnableMetrics = false
	c.EnableProfiling = false
	// The genericapiserver wants a loopback client config. Nothing here uses
	// it, and 127.0.0.1 is not this pod in the in-process network.
	c.LoopbackClientConfig = podLoopbackConfig(cfg.listener)

	so := genericoptions.NewSecureServingOptions()
	so.Listener = cfg.listener
	cert, err := dynamiccertificates.NewStaticCertKeyContent("Generated self signed cert", cfg.cert, cfg.key)
	if err != nil {
		return nil, err
	}
	so.ServerCert.GeneratedCert = cert
	if err := so.ApplyTo(&c.SecureServing); err != nil {
		return nil, err
	}

	c.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(generatedopenapi.GetOpenAPIDefinitions, openapinamer.NewDefinitionNamer(scheme))
	c.OpenAPIConfig.Info.Title = "Kubernetes metrics-server"
	c.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(generatedopenapi.GetOpenAPIDefinitions, openapinamer.NewDefinitionNamer(scheme))
	c.OpenAPIV3Config.Info.Title = "Kubernetes metrics-server"
	if cfg.version != "" {
		c.OpenAPIConfig.Info.Version = cfg.version
		c.OpenAPIV3Config.Info.Version = cfg.version
	}

	if err := cfg.auth(c); err != nil {
		return nil, err
	}

	srv, err := c.Complete(nil).New("metrics-server", genericapiserver.NewEmptyDelegate())
	if err != nil {
		return nil, err
	}
	info := genericapiserver.NewDefaultAPIGroupInfo(metrics.GroupName, scheme, metav1.ParameterCodec, codecs)
	info.VersionedResourcesStorageMap[v1beta1.SchemeGroupVersion.Version] = map[string]rest.Storage{
		"nodes": &nodeMetricsREST{gr: metrics.Resource("nodemetrics"), usage: cfg.usage, nodes: cfg.nodes},
		"pods":  &podMetricsREST{gr: metrics.Resource("podmetrics"), usage: cfg.usage, pods: cfg.pods},
	}
	if err := srv.InstallAPIGroup(&info); err != nil {
		return nil, err
	}
	if len(cfg.ready) > 0 {
		if err := srv.AddReadyzChecks(cfg.ready...); err != nil {
			return nil, err
		}
	}
	if len(cfg.live) > 0 {
		if err := srv.AddLivezChecks(0, cfg.live...); err != nil {
			return nil, err
		}
	}
	return srv, nil
}

// delegatedAuth makes the apiserver delegate authentication and
// authorization to the kube-apiserver of the kubeconfig (TokenReview,
// SubjectAccessReview, and the front proxy CA of the ConfigMap
// extension-apiserver-authentication), like every extension apiserver.
func delegatedAuth(kubeconfig string, wrap transport.WrapperFunc) func(c *genericapiserver.Config) error {
	return func(c *genericapiserver.Config) error {
		authn := genericoptions.NewDelegatingAuthenticationOptions()
		authn.RemoteKubeConfigFile = kubeconfig
		authn.CustomRoundTripperFn = wrap
		if err := authn.ApplyTo(&c.Authentication, c.SecureServing, c.OpenAPIConfig); err != nil {
			return err
		}
		authz := genericoptions.NewDelegatingAuthorizationOptions()
		authz.RemoteKubeConfigFile = kubeconfig
		authz.CustomRoundTripperFn = wrap
		return authz.ApplyTo(&c.Authorization)
	}
}

// dialWrapper makes the clients of the delegated authentication leave from
// the pod. The kubeconfig is all the options take, so there is no place for a
// dialer; but they let us wrap the transport. It is replaced by one that
// trusts the CA file (the rest of the client config, the token, is added
// around it). With a dialer it also stays off fetch() of the browser.
func dialWrapper(dial func(ctx context.Context, network, addr string) (net.Conn, error), caFile string) transport.WrapperFunc {
	return func(rt http.RoundTripper) http.RoundTripper {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if pem, err := os.ReadFile(caFile); err == nil {
			tc.RootCAs = x509.NewCertPool()
			tc.RootCAs.AppendCertsFromPEM(pem)
		}
		return &http.Transport{
			DialContext:         dial,
			TLSClientConfig:     tc,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		}
	}
}

// podLoopbackConfig is the loopback client config of the apiserver: the
// listener of the pod, not 127.0.0.1. Without a token it opens nothing.
func podLoopbackConfig(ln net.Listener) *restclient.Config {
	return &restclient.Config{Host: "https://" + ln.Addr().String(), TLSClientConfig: restclient.TLSClientConfig{Insecure: true}}
}

// msServingCert loads the serving certificate of the container from dir, or
// makes a self-signed one and writes it there (/tmp is an emptyDir, so a
// restart of the container keeps it). It is ECDSA: RSA keys take ages in Wasm.
func msServingCert(dir, host string, dns []string, ips []net.IP) (cert, key []byte, generated bool, err error) {
	crt, keyFile := filepath.Join(dir, "apiserver.crt"), filepath.Join(dir, "apiserver.key")
	if c, err := os.ReadFile(crt); err == nil {
		if k, err := os.ReadFile(keyFile); err == nil {
			if _, err := tls.X509KeyPair(c, k); err == nil {
				return c, k, false, nil
			}
		}
	}
	cert, key, err = certutil.GenerateSelfSignedCertKey(host, ips, dns)
	if err != nil {
		return nil, nil, false, fmt.Errorf("unable to generate self signed cert: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err == nil {
		// Best effort: the certificate is in memory anyway.
		os.WriteFile(crt, cert, 0o644)
		os.WriteFile(keyFile, key, 0o600)
	}
	return cert, key, true, nil
}
