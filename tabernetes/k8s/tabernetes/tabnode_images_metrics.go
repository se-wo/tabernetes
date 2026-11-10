package main

// registry.k8s.io/metrics-server/metrics-server of the built-in images
// (tabnode_images.go): the program parses the flags of the real one and runs
// an extension apiserver for metrics.k8s.io (metricsserver.go) and the
// scraper of the kubelets (metricsserver_scrape.go). The addon
// (addons/metrics-server.yaml) deploys it, so kubectl top and the
// HorizontalPodAutoscaler work.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"mvdan.cc/sh/v3/interp"
)

func init() {
	imageDefs = append(imageDefs, &imageDef{
		img:   builtinImage{name: "metrics-server", entrypoint: []string{"/metrics-server"}},
		repos: []string{"metrics-server/metrics-server"}, short: true,
		registry: "registry.k8s.io", tag: "v0.8.0", size: 19_000_000,
	})
	imagePrograms["metrics-server"] = metricsServerProg
}

// msOptions are the flags of metrics-server that this program uses.
type msOptions struct {
	certDir           string
	securePort        int
	resolution        time.Duration
	kubeletPort       int
	insecureTLS       bool
	useNodeStatusPort bool
	preferredTypes    []string
	kubeletCA         string
	nodeSelector      string
}

// minMetricResolution is what upstream accepts at least.
const minMetricResolution = 10 * time.Second

// Flags of the real metrics-server (its own, the ones of the generic
// apiserver options and of klog) that a manifest may pass. They are accepted
// and ignored: the program has no use for them.
var (
	msIgnoredStrings = []string{
		"kubeconfig", "authentication-kubeconfig", "authorization-kubeconfig", "bind-address",
		"client-ca-file", "requestheader-client-ca-file", "requestheader-extra-headers-prefix",
		"tls-cert-file", "tls-private-key-file", "tls-min-version", "kubelet-client-certificate",
		"kubelet-client-key", "vmodule", "log-flush-frequency", "log_dir", "log_file",
		"log_file_max_size", "log_backtrace_at", "stderrthreshold", "profiling-address",
	}
	msIgnoredBools = []string{
		"authentication-skip-lookup", "authentication-tolerate-lookup-failure", "authentication-token-webhook",
		"deprecated-kubelet-completely-insecure", "contention-profiling", "profiling", "disable-http2-serving",
		"logtostderr", "alsologtostderr", "skip_headers", "skip_log_headers", "add_dir_header", "one_output",
		"permit-port-sharing", "permit-address-sharing", "enable-priority-and-fairness",
	}
	msIgnoredSlices = []string{
		"tls-cipher-suites", "tls-sni-cert-key", "authorization-always-allow-paths", "requestheader-allowed-names",
		"requestheader-group-headers", "requestheader-username-headers", "requestheader-uid-headers",
	}
	msIgnoredDurations = []string{"shutdown-delay-duration", "authentication-token-webhook-cache-ttl"}
)

func newMetricsServerFlags(out io.Writer) (*pflag.FlagSet, *msOptions) {
	o := &msOptions{}
	fs := pflag.NewFlagSet("metrics-server", pflag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {}
	fs.SortFlags = false
	fs.StringVar(&o.certDir, "cert-dir", "apiserver.local.config/certificates", "The directory where the TLS certs are located.")
	fs.IntVar(&o.securePort, "secure-port", 443, "The port on which to serve HTTPS with authentication and authorization.")
	fs.DurationVar(&o.resolution, "metric-resolution", 60*time.Second, "The resolution at which metrics-server will retain metrics, must set value at least 10s.")
	fs.IntVar(&o.kubeletPort, "kubelet-port", 10250, "The port to use to connect to Kubelets.")
	fs.BoolVar(&o.insecureTLS, "kubelet-insecure-tls", false, "Do not verify CA of serving certificates presented by Kubelets.  For testing purposes only.")
	fs.BoolVar(&o.useNodeStatusPort, "kubelet-use-node-status-port", false, "Use the port in the node status. Takes precedence over --kubelet-port flag.")
	fs.StringSliceVar(&o.preferredTypes, "kubelet-preferred-address-types", []string{"Hostname", "InternalDNS", "InternalIP", "ExternalDNS", "ExternalIP"}, "The priority of node address types used when determining an address for connecting to a node.")
	fs.StringVar(&o.kubeletCA, "kubelet-certificate-authority", "", "Path to the CA to use to validate the Kubelet's serving certificates.")
	fs.StringVar(&o.nodeSelector, "node-selector", "", "Selector (label query) to filter nodes on, supports '=', '==', and '!='.")
	fs.IntP("v", "v", 0, "number for the log level verbosity")
	for _, n := range msIgnoredStrings {
		fs.String(n, "", "")
	}
	for _, n := range msIgnoredBools {
		fs.Bool(n, false, "")
	}
	for _, n := range msIgnoredSlices {
		fs.StringSlice(n, nil, "")
	}
	for _, n := range msIgnoredDurations {
		fs.Duration(n, 0, "")
	}
	return fs, o
}

// klogOut writes lines in the format of klog, which the real binary logs in:
// "I1110 08:00:00.000000       1 serving.go:380] message".
type klogOut struct {
	mu sync.Mutex
	w  io.Writer
}

func (k *klogOut) log(level byte, src, format string, a ...any) {
	k.mu.Lock()
	defer k.mu.Unlock()
	fmt.Fprintf(k.w, "%c%s %7d %s] %s\n", level, time.Now().UTC().Format("0102 15:04:05.000000"), 1, src, fmt.Sprintf(format, a...))
}

func (k *klogOut) info(src, format string, a ...any)  { k.log('I', src, format, a...) }
func (k *klogOut) warn(src, format string, a ...any)  { k.log('W', src, format, a...) }
func (k *klogOut) error(src, format string, a ...any) { k.log('E', src, format, a...) }

func metricsServerProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	fs, o := newMetricsServerFlags(hc.Stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, pflag.ErrHelp) {
			fmt.Fprintf(hc.Stdout, "Launches metrics-server\n\nUsage:\n  metrics-server [flags]\n\nFlags:\n%s", fs.FlagUsages())
			return nil
		}
		fmt.Fprintf(hc.Stderr, "Error: %v\n", err)
		return interp.ExitStatus(1)
	}
	log := &klogOut{w: hc.Stderr}
	pe := procOf(ctx)
	if pe == nil {
		fmt.Fprintln(hc.Stderr, "metrics-server: needs a pod with a service account")
		return interp.ExitStatus(1)
	}
	if o.resolution < minMetricResolution {
		log.warn("options.go:104", "\"metric-resolution\" is below %v, using that", minMetricResolution)
		o.resolution = minMetricResolution
	}
	var selector labels.Selector
	if o.nodeSelector != "" {
		sel, err := labels.Parse(o.nodeSelector)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "Error: invalid --node-selector: %v\n", err)
			return interp.ExitStatus(1)
		}
		selector = sel
	}
	version := "v0.8.0"
	if t := imageTag(ctx); strings.HasPrefix(t, "v") && fullVersion.MatchString(strings.TrimPrefix(t, "v")) {
		version = t
	}

	// The service account of the pod is the identity of the whole program:
	// the kubeconfig below is for the informers and for the delegated
	// authentication and authorization.
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenFile := pe.root.real(saDir + "/token")
	if _, err := os.Stat(tokenFile); err != nil {
		log.error("server.go:75", "\"Failed to create the client\" err=\"unable to load in-cluster configuration: no service account token in %s\"", saDir)
		return interp.ExitStatus(1)
	}
	kubeconfig := filepath.Join(pe.w.dir, "containers", pe.c.spec.Name, "kubeconfig-metrics-server")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: in-cluster
  cluster:
    server: https://10.96.0.1:443
    certificate-authority: %s
contexts:
- name: in-cluster
  context: {cluster: in-cluster, user: sa}
current-context: in-cluster
users:
- name: sa
  user:
    tokenFile: %s
`, pe.root.real(saDir+"/ca.crt"), tokenFile)
	os.MkdirAll(filepath.Dir(kubeconfig), 0o755)
	if err := os.WriteFile(kubeconfig, []byte(cfg), 0o600); err != nil {
		log.error("server.go:75", "\"Failed to create the client\" err=%q", err.Error())
		return interp.ExitStatus(1)
	}

	// Only the shutdown of the apiserver may close the listener: RunServer
	// panics (and takes the tab with it) when the listener fails while the
	// server is not stopping. listenPod would close it when ctx ends.
	ln, err := listenPod(context.WithoutCancel(ctx), fmt.Sprintf(":%d", o.securePort))
	if err != nil {
		log.error("server.go:92", "\"Failed to run\" err=\"failed to create listener: failed to listen on 0.0.0.0:%d: %v\"", o.securePort, unwrap(err))
		return interp.ExitStatus(1)
	}
	// Once the apiserver runs, its shutdown closes the listener.
	var run *msRun
	defer func() {
		if run == nil || !run.started {
			ln.Close()
		}
	}()

	certDir := pe.root.real(o.certDir)
	if !filepath.IsAbs(o.certDir) {
		certDir = pe.root.real(filepath.Join("/", o.certDir))
	}
	cert, key, generated, err := msServingCert(certDir, "localhost",
		[]string{"metrics-server", "metrics-server.kube-system", "metrics-server.kube-system.svc"},
		[]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP(pe.ip)})
	if err != nil {
		log.error("server.go:92", "\"Failed to run\" err=%q", err.Error())
		return interp.ExitStatus(1)
	}
	if generated {
		log.info("serving.go:380", "Generated self-signed cert (%s, %s)", filepath.Join(o.certDir, "apiserver.crt"), filepath.Join(o.certDir, "apiserver.key"))
	}

	// The clients dial from the pod, not with the default dialer: the pod
	// IP is the source of the connection (NetworkPolicy), and a transport
	// without DialContext would use fetch() of the browser.
	dial := func(c context.Context, network, addr string) (net.Conn, error) {
		return dialCluster(withProc(c, pe), network, addr)
	}
	run = &msRun{
		log: log, ln: ln, version: version, cert: cert, key: key, kubeconfig: kubeconfig, tokenFile: tokenFile, caFile: pe.root.real(saDir + "/ca.crt"),
		dial: dial, opts: o, selector: selector,
	}
	if err := run.run(ctx); err != nil {
		log.error("server.go:92", "\"Failed to run\" err=%q", err.Error())
		return interp.ExitStatus(1)
	}
	return nil
}

// msRun is one run of the program: the informers, the scraper and the
// apiserver. It holds no state outside, so a restarted container starts clean.
type msRun struct {
	log        *klogOut
	ln         net.Listener
	version    string
	cert, key  []byte
	kubeconfig string
	tokenFile  string
	caFile     string
	dial       func(ctx context.Context, network, addr string) (net.Conn, error)
	opts       *msOptions
	selector   labels.Selector
	started    bool // the apiserver serves on ln
}

func (r *msRun) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	rc, err := clientcmd.BuildConfigFromFlags("", r.kubeconfig)
	if err != nil {
		return err
	}
	rc.Dial = r.dial
	rc.QPS, rc.Burst = 50, 100
	client, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return err
	}
	factory := informers.NewSharedInformerFactory(client, 0)
	nodeInf := factory.Core().V1().Nodes()
	podInf := factory.Core().V1().Pods()
	nodeSynced := nodeInf.Informer().HasSynced
	podSynced := podInf.Informer().HasSynced
	factory.Start(ctx.Done())

	storage := newMetricsStorage()
	timeout := time.Duration(float64(r.opts.resolution) * 0.8)
	hc, err := newKubeletClient(kubeletScraperConfig{insecureTLS: r.opts.insecureTLS, caFile: r.opts.kubeletCA, dial: r.dial})
	if err != nil {
		return err
	}
	var preferred []corev1.NodeAddressType
	for _, t := range r.opts.preferredTypes {
		preferred = append(preferred, corev1.NodeAddressType(t))
	}
	scraper := &kubeletScraper{
		nodes: nodeInf.Lister(), selector: r.selector, storage: storage, client: hc, tokenFile: r.tokenFile,
		preferred: preferred, useNodeStatusPort: r.opts.useNodeStatusPort, port: r.opts.kubeletPort, timeout: timeout,
		logErr: func(node string, err error) {
			if node == "" {
				r.log.error("scraper.go:121", "\"Failed to list nodes\" err=%q", err.Error())
				return
			}
			r.log.error("scraper.go:149", "\"Failed to scrape node\" err=%q node=%q", err.Error(), node)
		},
	}

	cfg := &msServerConfig{
		listener: r.ln, cert: r.cert, key: r.key, version: r.version,
		nodes: nodeInf.Lister(), pods: podInf.Lister(), usage: storage,
		auth: delegatedAuth(r.kubeconfig, dialWrapper(r.dial, r.caFile)),
		ready: []healthz.HealthChecker{
			healthz.NamedCheck("metric-storage-ready", func(*http.Request) error {
				if !storage.Ready() {
					return errors.New("no metrics to serve")
				}
				return nil
			}),
			healthz.NamedCheck("metric-informer-sync", func(*http.Request) error {
				if !nodeSynced() || !podSynced() {
					return errors.New("metric informer not yet synced")
				}
				return nil
			}),
		},
		live: []healthz.HealthChecker{
			healthz.NamedCheck("metric-collection-timely", func(*http.Request) error {
				return scraper.timely(max(3*r.opts.resolution, time.Minute))
			}),
		},
	}

	// The cluster may still be creating the ConfigMap with the front proxy
	// CA, or not answer yet: try again for a while before giving up.
	var srv *genericapiserver.GenericAPIServer
	for attempt := 1; ; attempt++ {
		srv, err = newMetricsAPIServer(cfg)
		if err == nil {
			break
		}
		if attempt == 10 || ctx.Err() != nil {
			return err
		}
		r.log.error("authentication.go:332", "\"Unable to set up the apiserver, retrying\" err=%q", err.Error())
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
	r.log.info("handler.go:288", "Adding GroupVersion metrics.k8s.io v1beta1 to ResourceManager")
	r.log.info("requestheader_controller.go:180", "Starting RequestHeaderAuthRequestController")
	r.log.info("shared_informer.go:313", "Waiting for caches to sync for RequestHeaderAuthRequestController")
	r.log.info("configmap_cafile_content.go:205", "\"Starting controller\" name=\"client-ca::kube-system::extension-apiserver-authentication::client-ca-file\"")
	r.log.info("configmap_cafile_content.go:205", "\"Starting controller\" name=\"client-ca::kube-system::extension-apiserver-authentication::requestheader-client-ca-file\"")
	r.log.info("tlsconfig.go:243", "\"Starting DynamicServingCertificateController\"")
	r.log.info("secure_serving.go:213", "Serving securely on [::]:%d", r.opts.securePort)

	go scraper.run(ctx, r.opts.resolution)
	done := make(chan error, 1)
	r.started = true
	go func() { done <- srv.PrepareRun().RunWithContext(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	// Give the graceful shutdown a moment; the container is stopping.
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	r.log.info("secure_serving.go:258", "Stopped listening on [::]:%d", r.opts.securePort)
	return nil
}
