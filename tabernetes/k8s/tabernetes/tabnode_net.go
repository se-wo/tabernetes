package main

// The service proxy: kube-proxy and the cluster DNS of the tab. There is no
// real network in the wasm, only Go's in-process one, where a socket is an
// exact ip:port. So instead of iptables rules the proxy listens on every
// cluster IP and port of every Service (and on the node ports) and splices
// each connection to an endpoint. Names of services resolve from the same
// informer cache (resolve), which is what CoreDNS would answer.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

const (
	clusterDomain = "cluster.local"
	sliceIndex    = "service"
)

// serviceProxy is kube-proxy for the tab: it serves cluster IPs and node
// ports in the in-process network and resolves service names.
type serviceProxy struct {
	client   kubernetes.Interface
	services corelisters.ServiceLister
	slices   cache.Indexer
	synced   atomic.Bool
	kick     chan struct{}

	mu        sync.Mutex
	listeners map[string]*svcListener // by ip:port
	failed    map[string]string       // addresses that could not listen, by error
	next      map[string]uint32       // round robin of each service port

	restOnce sync.Once
	rest     *rest.Config
	restErr  error
}

// svcPort is what a listener forwards to.
type svcPort struct {
	namespace, name string
	portName        string
}

type svcListener struct {
	ln     net.Listener
	target atomic.Pointer[svcPort]
}

func newServiceProxy(client kubernetes.Interface) *serviceProxy {
	s := &serviceProxy{
		client:    client,
		kick:      make(chan struct{}, 1),
		listeners: map[string]*svcListener{},
		failed:    map[string]string{},
		next:      map[string]uint32{},
	}
	return s
}

// run watches services and endpoint slices until ctx ends.
func (s *serviceProxy) run(ctx context.Context) {
	factory := informers.NewSharedInformerFactory(s.client, 0)
	svcInformer := factory.Core().V1().Services()
	sliceInformer := factory.Discovery().V1().EndpointSlices().Informer()
	if err := sliceInformer.AddIndexers(cache.Indexers{sliceIndex: func(obj any) ([]string, error) {
		es := obj.(*discoveryv1.EndpointSlice)
		if svc := es.Labels[discoveryv1.LabelServiceName]; svc != "" {
			return []string{es.Namespace + "/" + svc}, nil
		}
		return nil, nil
	}}); err != nil {
		fmt.Fprintln(os.Stderr, "service proxy:", err)
		return
	}
	s.slices = sliceInformer.GetIndexer()
	s.services = svcInformer.Lister()
	kick := func() {
		select {
		case s.kick <- struct{}{}:
		default:
		}
	}
	svcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { kick() },
		UpdateFunc: func(any, any) { kick() },
		DeleteFunc: func(any) { kick() },
	})
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), svcInformer.Informer().HasSynced, sliceInformer.HasSynced) {
		return
	}
	s.synced.Store(true)
	kick()
	for {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for addr, l := range s.listeners {
				l.ln.Close()
				delete(s.listeners, addr)
			}
			s.mu.Unlock()
			return
		case <-s.kick:
			s.sync()
		}
	}
}

// sync opens a listener for every cluster IP and node port and closes the
// ones of services and ports that are gone.
func (s *serviceProxy) sync() {
	services, err := s.services.List(labels.Everything())
	if err != nil {
		return
	}
	want := map[string]svcPort{}
	for _, svc := range services {
		if svc.Spec.Type == v1.ServiceTypeExternalName {
			continue
		}
		for _, p := range svc.Spec.Ports {
			if p.Protocol != "" && p.Protocol != v1.ProtocolTCP {
				continue // UDP and SCTP have no place in the in-process network
			}
			t := svcPort{svc.Namespace, svc.Name, p.Name}
			for _, ip := range clusterIPs(svc) {
				want[net.JoinHostPort(ip, strconv.Itoa(int(p.Port)))] = t
			}
			if p.NodePort > 0 && (svc.Spec.Type == v1.ServiceTypeNodePort || svc.Spec.Type == v1.ServiceTypeLoadBalancer) {
				// The node IP, and localhost: on a real node, a node port
				// answers on every address, and "proxy <port>" of the page
				// opens localhost ports.
				for _, ip := range []string{tabNodeIP, "127.0.0.1"} {
					want[net.JoinHostPort(ip, strconv.Itoa(int(p.NodePort)))] = t
				}
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for addr, l := range s.listeners {
		if _, ok := want[addr]; !ok {
			l.ln.Close()
			delete(s.listeners, addr)
		}
	}
	for addr := range s.failed {
		if _, ok := want[addr]; !ok {
			delete(s.failed, addr)
		}
	}
	for addr, t := range want {
		if l := s.listeners[addr]; l != nil {
			l.target.Store(&t)
			continue
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			// Log once per address, sync runs on every service change.
			if s.failed[addr] != err.Error() {
				fmt.Fprintf(os.Stderr, "service proxy: %s/%s: %v\n", t.namespace, t.name, err)
				s.failed[addr] = err.Error()
			}
			continue
		}
		delete(s.failed, addr)
		l := &svcListener{ln: ln}
		l.target.Store(&t)
		s.listeners[addr] = l
		go s.serve(l)
	}
}

func clusterIPs(svc *v1.Service) []string {
	ips := svc.Spec.ClusterIPs
	if len(ips) == 0 && svc.Spec.ClusterIP != "" {
		ips = []string{svc.Spec.ClusterIP}
	}
	var out []string
	for _, ip := range ips {
		if ip != "" && ip != v1.ClusterIPNone {
			out = append(out, ip)
		}
	}
	return out
}

func (s *serviceProxy) serve(l *svcListener) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		go s.forward(c, *l.target.Load())
	}
}

// endpoint is one address of a service port.
type endpoint struct {
	ip     string
	port   int32
	node   string
	pod    *v1.ObjectReference
	remote bool // on a joined node, out of reach of the wasm
}

// forward connects c to an endpoint of t: round robin over the ready
// endpoints on the tab, through the apiserver for endpoints on joined nodes.
func (s *serviceProxy) forward(c net.Conn, t svcPort) {
	eps := s.endpoints(t)
	if len(eps) == 0 {
		c.Close() // like a REJECT rule of kube-proxy: no endpoints
		return
	}
	local := slices.DeleteFunc(slices.Clone(eps), func(e endpoint) bool { return e.remote })
	if len(local) > 0 {
		eps = local
	}
	key := t.namespace + "/" + t.name + ":" + t.portName
	s.mu.Lock()
	start := s.next[key]
	s.next[key]++
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var lastErr error
	for i := range eps {
		e := eps[(int(start)+i)%len(eps)]
		up, err := s.dialEndpoint(ctx, e)
		if err != nil {
			lastErr = err
			continue
		}
		go pipeConns(c, up)
		return
	}
	fmt.Fprintf(os.Stderr, "service proxy: %s/%s:%s: %v\n", t.namespace, t.name, t.portName, lastErr)
	c.Close()
}

func (s *serviceProxy) dialEndpoint(ctx context.Context, e endpoint) (io.ReadWriteCloser, error) {
	if e.remote {
		if e.pod == nil {
			return nil, fmt.Errorf("endpoint %s on node %s is not a pod, it cannot be reached from the tab", e.ip, e.node)
		}
		return s.portForward(ctx, e.pod.Namespace, e.pod.Name, e.port)
	}
	ip := e.ip
	if ip == tabNodeIP && e.port == 6443 {
		// The kubernetes service: the apiserver listens on 127.0.0.1 only.
		ip = "127.0.0.1"
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(int(e.port))))
}

// endpoints lists the ready endpoints of a service port; if there are none,
// the serving ones that are terminating, as kube-proxy does.
func (s *serviceProxy) endpoints(t svcPort) []endpoint {
	if s.slices == nil {
		return nil
	}
	objs, _ := s.slices.ByIndex(sliceIndex, t.namespace+"/"+t.name)
	var ready, serving []endpoint
	seen := map[string]bool{}
	for _, obj := range objs {
		es := obj.(*discoveryv1.EndpointSlice)
		if es.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		var port int32
		found := false
		for _, p := range es.Ports {
			if p.Port != nil && deref(p.Name) == t.portName && (p.Protocol == nil || *p.Protocol == v1.ProtocolTCP) {
				port, found = *p.Port, true
				break
			}
		}
		if !found {
			continue
		}
		for _, ep := range es.Endpoints {
			if len(ep.Addresses) == 0 {
				continue
			}
			e := endpoint{ip: ep.Addresses[0], port: port, node: deref(ep.NodeName), pod: ep.TargetRef}
			e.remote = e.node != "" && e.node != tabNodeName
			if theTabNode != nil && theTabNode.isTabPodIP(e.ip) {
				e.remote = false
			}
			if e.pod != nil && e.pod.Kind != "Pod" {
				e.pod = nil
			}
			if k := net.JoinHostPort(e.ip, strconv.Itoa(int(port))); seen[k] {
				continue
			} else {
				seen[k] = true
			}
			c := ep.Conditions
			switch {
			case c.Ready == nil || *c.Ready:
				ready = append(ready, e)
			case (c.Serving == nil || *c.Serving) && c.Terminating != nil && *c.Terminating:
				serving = append(serving, e)
			}
		}
	}
	if len(ready) > 0 {
		return ready
	}
	return serving
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// pipeConns copies both ways and keeps half-closes: a client that is done
// sending still gets the answer.
func pipeConns(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	var once sync.Once
	closeBoth := func() { a.Close(); b.Close() }
	cp := func(dst, src io.ReadWriteCloser) {
		defer wg.Done()
		if _, err := io.Copy(dst, src); err != nil {
			once.Do(closeBoth)
			return
		}
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			once.Do(closeBoth)
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	once.Do(closeBoth)
}

// portForward tunnels to a pod on a joined node through the port-forward of
// the apiserver, which reaches the kubelet there over the WebRTC tunnel. The
// wasm cannot dial pod IPs of other nodes.
func (s *serviceProxy) portForward(ctx context.Context, namespace, pod string, port int32) (io.ReadWriteCloser, error) {
	s.restOnce.Do(func() {
		s.rest, s.restErr = clientcmd.BuildConfigFromFlags("", p("admin.kubeconfig"))
	})
	if s.restErr != nil {
		return nil, s.restErr
	}
	transport, upgrader, err := spdy.RoundTripperFor(s.rest)
	if err != nil {
		return nil, err
	}
	u := s.client.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
	type result struct {
		conn httpstream.Connection
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		conn, _, err := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, u).Dial(portforward.PortForwardProtocolV1Name)
		ch <- result{conn, err}
	}()
	var conn httpstream.Connection
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("port-forward to pod %s/%s: %w", namespace, pod, r.err)
		}
		conn = r.conn
	case <-ctx.Done():
		go func() {
			if r := <-ch; r.conn != nil {
				r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
	h := http.Header{}
	h.Set(v1.StreamType, v1.StreamTypeError)
	h.Set(v1.PortHeader, strconv.Itoa(int(port)))
	h.Set(v1.PortForwardRequestIDHeader, "0")
	errStream, err := conn.CreateStream(h)
	if err != nil {
		conn.Close()
		return nil, err
	}
	errStream.Close() // nothing to send on it
	h.Set(v1.StreamType, v1.StreamTypeData)
	data, err := conn.CreateStream(h)
	if err != nil {
		conn.Close()
		return nil, err
	}
	go func() {
		// The kubelet reports a failed dial in the pod on the error stream.
		if msg, _ := io.ReadAll(errStream); len(msg) > 0 {
			fmt.Fprintf(os.Stderr, "service proxy: port-forward to pod %s/%s:%d: %s\n", namespace, pod, port, msg)
			conn.Close()
		}
	}()
	return &forwardConn{Stream: data, conn: conn}, nil
}

// forwardConn is the data stream of a port-forward as a connection.
type forwardConn struct {
	httpstream.Stream
	conn httpstream.Connection
}

func (f *forwardConn) CloseWrite() error { return f.Stream.Close() }
func (f *forwardConn) Close() error {
	f.Stream.Reset()
	return f.conn.Close()
}

// dnsAnswer is what the cluster DNS would answer for a name.
type dnsAnswer struct {
	name  string   // the fully qualified name that matched
	cname string   // ExternalName services
	ips   []string // A records
}

// lookup answers a name the way CoreDNS does for a pod of namespace: with
// the search path <ns>.svc.cluster.local, svc.cluster.local, cluster.local.
func (s *serviceProxy) lookup(host, namespace string) (dnsAnswer, bool) {
	if !s.synced.Load() {
		return dnsAnswer{}, false
	}
	host = strings.ToLower(host)
	var candidates []string
	if abs, ok := strings.CutSuffix(host, "."); ok {
		candidates = []string{abs}
	} else if host == clusterDomain || strings.HasSuffix(host, "."+clusterDomain) {
		candidates = []string{host}
	} else {
		if namespace != "" {
			candidates = append(candidates, host+"."+namespace+".svc."+clusterDomain)
		}
		candidates = append(candidates, host+".svc."+clusterDomain, host+"."+clusterDomain)
	}
	for _, name := range candidates {
		if a, ok := s.lookupFQDN(name, 0); ok {
			return a, true
		}
	}
	return dnsAnswer{}, false
}

func (s *serviceProxy) lookupFQDN(name string, depth int) (dnsAnswer, bool) {
	rest, ok := strings.CutSuffix(name, "."+clusterDomain)
	if !ok || depth > 4 {
		return dnsAnswer{}, false
	}
	a := dnsAnswer{name: name}
	labels := strings.Split(rest, ".")
	switch {
	case len(labels) == 3 && labels[2] == "pod":
		// 10-244-0-5.default.pod.cluster.local
		ip := net.ParseIP(strings.ReplaceAll(labels[0], "-", "."))
		if ip == nil || ip.To4() == nil {
			return a, false
		}
		a.ips = []string{ip.String()}
		return a, true
	case len(labels) == 3 && labels[2] == "svc":
		svc, err := s.services.Services(labels[1]).Get(labels[0])
		if err != nil {
			return a, false
		}
		switch {
		case svc.Spec.Type == v1.ServiceTypeExternalName:
			a.cname = strings.TrimSuffix(strings.ToLower(svc.Spec.ExternalName), ".")
			if t, ok := s.lookupFQDN(a.cname, depth+1); ok {
				a.ips = t.ips
			}
			return a, true
		case svc.Spec.ClusterIP == v1.ClusterIPNone:
			for _, e := range s.endpoints(svcPort{namespace: svc.Namespace, name: svc.Name, portName: anyPort(svc)}) {
				a.ips = append(a.ips, e.ip)
			}
			a.ips = uniqStrings(a.ips)
		default:
			a.ips = clusterIPs(svc)
		}
		return a, len(a.ips) > 0
	case len(labels) == 4 && labels[3] == "svc":
		// hostname.subdomain.ns.svc.cluster.local: pods with spec.hostname
		// and spec.subdomain behind a headless service (StatefulSets).
		objs, _ := s.slices.ByIndex(sliceIndex, labels[2]+"/"+labels[1])
		for _, obj := range objs {
			for _, ep := range obj.(*discoveryv1.EndpointSlice).Endpoints {
				if deref(ep.Hostname) == labels[0] && len(ep.Addresses) > 0 {
					a.ips = append(a.ips, ep.Addresses[0])
				}
			}
		}
		a.ips = uniqStrings(a.ips)
		return a, len(a.ips) > 0
	}
	return a, false
}

// anyPort is the name of the first port of a service; the endpoints of a
// headless service are the same for all of its ports.
func anyPort(svc *v1.Service) string {
	if len(svc.Spec.Ports) > 0 {
		return svc.Spec.Ports[0].Name
	}
	return ""
}

func uniqStrings(list []string) []string {
	slices.Sort(list)
	return slices.Compact(list)
}

// resolve resolves a DNS name of the cluster seen from namespace: web,
// web.ns, web.ns.svc, web.ns.svc.cluster.local, 10-244-0-5.ns.pod...
func (s *serviceProxy) resolve(host, namespace string) (string, bool) {
	a, ok := s.lookup(host, namespace)
	if !ok || len(a.ips) == 0 {
		return "", false
	}
	if len(a.ips) == 1 {
		return a.ips[0], true
	}
	// Several A records (headless services): round robin, like DNS.
	s.mu.Lock()
	i := s.next["dns:"+a.name]
	s.next["dns:"+a.name]++
	s.mu.Unlock()
	return a.ips[int(i)%len(a.ips)], true
}

// reverse is the PTR record of a cluster IP: the name of its service.
func (s *serviceProxy) reverse(ip string) (string, bool) {
	if !s.synced.Load() {
		return "", false
	}
	services, _ := s.services.List(labels.Everything())
	for _, svc := range services {
		if slices.Contains(clusterIPs(svc), ip) {
			return svc.Name + "." + svc.Namespace + ".svc." + clusterDomain, true
		}
	}
	return "", false
}

// dnsServer is the address nslookup reports: the kube-dns service.
func (s *serviceProxy) dnsServer() string {
	if s.synced.Load() {
		if svc, err := s.services.Services(metav1.NamespaceSystem).Get("kube-dns"); err == nil && len(clusterIPs(svc)) > 0 {
			return clusterIPs(svc)[0]
		}
	}
	return "10.96.0.10"
}

// serviceEnv is the docker-link style environment of services of a namespace
// (KUBERNETES_SERVICE_HOST etc. always, the others with links), as the
// kubelet sets it.
func (s *serviceProxy) serviceEnv(namespace string, links bool) []string {
	list := s.envServices(namespace, links)
	byName := map[string]*v1.Service{}
	for _, svc := range list {
		if len(clusterIPs(svc)) == 0 {
			continue
		}
		switch {
		case svc.Namespace == metav1.NamespaceDefault && svc.Name == "kubernetes":
			if _, ok := byName[svc.Name]; !ok {
				byName[svc.Name] = svc
			}
		case svc.Namespace == namespace && links:
			byName[svc.Name] = svc
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	var env []string
	for _, name := range names {
		svc := byName[name]
		ip := clusterIPs(svc)[0]
		prefix := strings.ToUpper(strings.ReplaceAll(svc.Name, "-", "_"))
		env = append(env, prefix+"_SERVICE_HOST="+ip)
		for i, p := range svc.Spec.Ports {
			port := strconv.Itoa(int(p.Port))
			proto := strings.ToLower(string(p.Protocol))
			if proto == "" {
				proto = "tcp"
			}
			if i == 0 {
				env = append(env, prefix+"_SERVICE_PORT="+port, prefix+"_PORT="+proto+"://"+net.JoinHostPort(ip, port))
			}
			if p.Name != "" {
				env = append(env, prefix+"_SERVICE_PORT_"+strings.ToUpper(strings.ReplaceAll(p.Name, "-", "_"))+"="+port)
			}
			pp := prefix + "_PORT_" + port + "_" + strings.ToUpper(proto)
			env = append(env, pp+"="+proto+"://"+net.JoinHostPort(ip, port), pp+"_PROTO="+proto, pp+"_PORT="+port, pp+"_ADDR="+ip)
		}
	}
	return env
}

// envServices are the services of namespace and default. Before the
// informer has synced (a pod that starts with the node), they come straight
// from the apiserver.
func (s *serviceProxy) envServices(namespace string, links bool) []*v1.Service {
	if s.synced.Load() {
		var list []*v1.Service
		if k, err := s.services.Services(metav1.NamespaceDefault).Get("kubernetes"); err == nil {
			list = append(list, k)
		}
		if links {
			ns, _ := s.services.Services(namespace).List(labels.Everything())
			list = append(list, ns...)
		}
		return list
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var list []*v1.Service
	if k, err := s.client.CoreV1().Services(metav1.NamespaceDefault).Get(ctx, "kubernetes", metav1.GetOptions{}); err == nil {
		list = append(list, k)
	}
	if links {
		if ns, err := s.client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range ns.Items {
				list = append(list, &ns.Items[i])
			}
		}
	}
	return list
}
