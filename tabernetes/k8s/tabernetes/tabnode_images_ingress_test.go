package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"mvdan.cc/sh/v3/interp"
	"sigs.k8s.io/yaml"
)

// ingTestEnv is a cluster without a cluster: Services whose endpoints are
// HTTP servers in the in-process network, and the rendered configuration
// loaded into an nginx instance that is never started.
type ingTestEnv struct {
	t    *testing.T
	in   *ingInput
	dir  string
	n    *nginxInst
	cfg  *ngxConf
	out  *ingOutput
	logs *bytes.Buffer
}

func newIngTestEnv(t *testing.T) *ingTestEnv {
	e := &ingTestEnv{t: t, dir: t.TempDir(), logs: &bytes.Buffer{}, in: &ingInput{
		services: map[string]*v1.Service{}, slices: map[string][]*discoveryv1.EndpointSlice{},
		secrets: map[string]*v1.Secret{}, fakeCert: ingressFakeCert,
	}}
	c := &ingController{real: e.real}
	if err := c.fakeCertificate(); err != nil {
		t.Fatal(err)
	}
	e.write("/etc/nginx/mime.types", nginxMimeTypes)
	return e
}

func (e *ingTestEnv) real(p string) string { return filepath.Join(e.dir, p) }

func (e *ingTestEnv) write(p, content string) {
	os.MkdirAll(path.Dir(e.real(p)), 0o755)
	if err := os.WriteFile(e.real(p), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// backend adds a Service whose endpoint answers with its name, the URI and
// the Host it got.
func (e *ingTestEnv) backend(ns, name string, port int32) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s host=%s xfh=%s rid=%t", name, r.URL.RequestURI(), r.Host, r.Header.Get("X-Forwarded-Host"), r.Header.Get("X-Request-ID") != "")
	}))
	e.t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	target, _ := strconv.Atoi(p)
	e.service(ns, name, port, int32(target), "127.0.0.1")
}

// service adds a Service with the given ready endpoints (none: no endpoints).
func (e *ingTestEnv) service(ns, name string, port, target int32, ips ...string) {
	e.in.services[ns+"/"+name] = &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: v1.ServiceSpec{ClusterIP: "10.96.0.50", Ports: []v1.ServicePort{{Name: "http", Port: port}}}}
	es := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name + "-abcde",
		Labels: map[string]string{discoveryv1.LabelServiceName: name}},
		Ports: []discoveryv1.EndpointPort{{Name: ptr.To("http"), Port: ptr.To(target)}}}
	for _, ip := range ips {
		es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{ip}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}})
	}
	// A pod that is not ready is no upstream server.
	es.Endpoints = append(es.Endpoints, discoveryv1.Endpoint{Addresses: []string{"10.244.9.9"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(false)}})
	e.in.slices[ns+"/"+name] = []*discoveryv1.EndpointSlice{es}
}

func (e *ingTestEnv) load() {
	e.t.Helper()
	e.out = renderIngressConf(e.in)
	for p, content := range e.out.files {
		e.write(p, content)
	}
	e.write(ingressConfFile, e.out.conf)
	cfg, err := ngxLoad(e.real, ingressConfFile, "")
	if err != nil {
		e.t.Fatalf("rendered configuration: %v\n%s", err, e.out.conf)
	}
	e.cfg = cfg
	e.n = &nginxInst{ctx: context.Background(), real: e.real, version: ingressNginxVersion, ports: map[string]bool{},
		hc:   interp.HandlerContext{Stdout: io.Discard, Stderr: io.Discard},
		errw: &syncWriter{w: io.Discard}, outw: &syncWriter{w: e.logs}}
	e.n.cfg.Store(cfg)
}

// get sends a request through nginx, as if it came in on port.
func (e *ingTestEnv) get(host, uri string, ssl bool) (int, string, http.Header) {
	port := "80"
	if ssl {
		port = "443"
	}
	r := httptest.NewRequest(http.MethodGet, "http://"+host+uri, nil)
	if host == "" {
		r.Host = ""
	}
	w := httptest.NewRecorder()
	e.n.serve(w, r, port, ssl)
	return w.Code, w.Body.String(), w.Header()
}

func ingress(ns, name string, age int, ann map[string]string, rules ...netv1.IngressRule) *netv1.Ingress {
	return &netv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Annotations: ann,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 1, 1, 0, age, 0, 0, time.UTC))},
		Spec: netv1.IngressSpec{IngressClassName: ptr.To("nginx"), Rules: rules},
	}
}

func rule(host string, paths ...netv1.HTTPIngressPath) netv1.IngressRule {
	return netv1.IngressRule{Host: host, IngressRuleValue: netv1.IngressRuleValue{HTTP: &netv1.HTTPIngressRuleValue{Paths: paths}}}
}

func ingPath(p string, pt netv1.PathType, svc string, port int32) netv1.HTTPIngressPath {
	return netv1.HTTPIngressPath{Path: p, PathType: ptr.To(pt), Backend: netv1.IngressBackend{
		Service: &netv1.IngressServiceBackend{Name: svc, Port: netv1.ServiceBackendPort{Number: port}}}}
}

const (
	prefix = netv1.PathTypePrefix
	exact  = netv1.PathTypeExact
	impl   = netv1.PathTypeImplementationSpecific
)

// The path types of the Kubernetes Ingress documentation, hosts, wildcard
// hosts, rules without host, missing backends and the default backend.
func TestIngressRouting(t *testing.T) {
	e := newIngTestEnv(t)
	for _, s := range []string{"web", "foo", "exact", "baz", "impl", "wild", "nohost", "old", "new"} {
		e.backend("default", s, 80)
	}
	e.service("default", "empty", 80, 80)
	e.in.ingresses = []*netv1.Ingress{
		ingress("default", "demo", 1, nil, rule("demo.local",
			ingPath("/", prefix, "web", 80),
			ingPath("/foo", prefix, "foo", 80),
			ingPath("/foo/bar", exact, "exact", 80),
			ingPath("/foo/baz/", prefix, "baz", 80),
			ingPath("/impl", impl, "impl", 80),
			ingPath("/missing", prefix, "gone", 80),
			ingPath("/empty", prefix, "empty", 80),
		)),
		ingress("default", "wild", 2, nil, rule("*.wild.local", ingPath("/", prefix, "wild", 80))),
		ingress("default", "nohost", 3, nil, rule("", ingPath("/nohost", prefix, "nohost", 80))),
		// The same host and path in two Ingresses: the older wins.
		ingress("default", "b-new", 5, nil, rule("dup.local", ingPath("/", prefix, "new", 80))),
		ingress("default", "a-old", 4, nil, rule("dup.local", ingPath("/", prefix, "old", 80))),
	}
	e.load()
	for _, tc := range []struct {
		host, uri string
		code      int
		body      string // prefix of the answer
	}{
		{"demo.local", "/", 200, "web /"},
		{"demo.local:30080", "/", 200, "web / host=demo.local:30080 xfh=demo.local:30080 rid=true"},
		{"DEMO.local", "/x?a=1", 200, "web /x?a=1"},
		{"demo.local", "/foo", 200, "foo /foo"},
		{"demo.local", "/foo/", 200, "foo /foo/"},
		{"demo.local", "/foo/x", 200, "foo /foo/x"},
		{"demo.local", "/foobar", 200, "web /foobar"}, // element-wise prefix
		{"demo.local", "/foo/bar", 200, "exact /foo/bar"},
		{"demo.local", "/foo/bar/", 200, "foo /foo/bar/"}, // Exact is exact
		{"demo.local", "/foo/baz", 200, "baz /foo/baz"},
		{"demo.local", "/foo/baz/q", 200, "baz /foo/baz/q"}, // longest prefix
		{"demo.local", "/impl", 200, "impl /impl"},
		{"demo.local", "/implx", 200, "impl /implx"}, // string prefix of nginx
		{"demo.local", "/missing", 503, "<html>\r\n<head><title>503 Service Temporarily Unavailable</title>"},
		{"demo.local", "/empty/x", 503, "<html>"},
		{"a.wild.local", "/", 200, "wild /"},
		{"b.a.wild.local", "/z", 200, "wild /z"},
		{"wild.local", "/", 404, "<html>"},
		{"unknown.local", "/nohost/1", 200, "nohost /nohost/1"},
		{"", "/nohost", 200, "nohost /nohost"},
		{"unknown.local", "/", 404, "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found</h1></center>\r\n<hr><center>nginx</center>"},
		{"dup.local", "/", 200, "old /"},
	} {
		code, body, _ := e.get(tc.host, tc.uri, false)
		if code != tc.code || !strings.HasPrefix(body, tc.body) {
			t.Errorf("%s%s: %d %q, want %d %q", tc.host, tc.uri, code, body, tc.code, tc.body)
		}
	}
	// The access log of ingress-nginx, with the upstream.
	if logs := e.logs.String(); !strings.Contains(logs, `"GET /foo/x HTTP/1.1" 200 `) || !strings.Contains(logs, "[default-foo-80] [] 127.0.0.1:") {
		t.Errorf("access log:\n%s", logs)
	}
	if !strings.Contains(strings.Join(e.out.warnings, "\n"), `service="default/gone"`) {
		t.Errorf("warnings: %q", e.out.warnings)
	}
	if strings.Contains(e.out.conf, "10.244.9.9") {
		t.Error("an endpoint that is not ready is an upstream server")
	}
}

func TestIngressDefaultBackend(t *testing.T) {
	e := newIngTestEnv(t)
	e.backend("default", "web", 80)
	e.backend("default", "fallback", 80)
	e.backend("default", "catchall", 80)
	withRules := ingress("default", "rules", 1, nil, rule("demo.local", ingPath("/app", prefix, "web", 80)))
	withRules.Spec.DefaultBackend = &netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "fallback", Port: netv1.ServiceBackendPort{Number: 80}}}
	e.in.ingresses = []*netv1.Ingress{withRules}
	e.load()
	if code, body, _ := e.get("demo.local", "/other", false); code != 200 || !strings.HasPrefix(body, "fallback /other") {
		t.Errorf("host without /: %d %q, want the default backend of its Ingress", code, body)
	}
	if code, _, _ := e.get("unknown.local", "/", false); code != 404 {
		t.Errorf("unknown host: %d, want 404 (the backend of an Ingress with rules is not the catch-all)", code)
	}
	only := ingress("default", "only", 2, nil)
	only.Spec.DefaultBackend = &netv1.IngressBackend{Service: &netv1.IngressServiceBackend{Name: "catchall", Port: netv1.ServiceBackendPort{Number: 80}}}
	e.in.ingresses = append(e.in.ingresses, only)
	e.load()
	if code, body, _ := e.get("unknown.local", "/x", false); code != 200 || !strings.HasPrefix(body, "catchall /x") {
		t.Errorf("unknown host: %d %q, want the backend of the Ingress without rules", code, body)
	}
}

func TestIngressRewrite(t *testing.T) {
	e := newIngTestEnv(t)
	e.backend("default", "app", 80)
	e.backend("default", "other", 80)
	e.in.ingresses = []*netv1.Ingress{
		ingress("default", "rw", 1, map[string]string{annRewriteTarget: "/$2"}, rule("rw.local", ingPath("/app(/|$)(.*)", impl, "app", 80))),
		ingress("default", "plain", 2, nil, rule("rw.local", ingPath("/other", prefix, "other", 80))),
	}
	e.load()
	if !strings.Contains(e.out.conf, `location ~* "^/app(/|$)(.*)" {`) || !strings.Contains(e.out.conf, `rewrite "(?i)/app(/|$)(.*)" /$2 break;`) {
		t.Errorf("configuration:\n%s", e.out.conf)
	}
	for _, tc := range []struct{ uri, body string }{
		{"/app", "app / "},
		{"/app/", "app / "},
		{"/app/x/y?q=1", "app /x/y?q=1 "},
		{"/APP/z", "app /z "},
		{"/other/z", "other /other/z "}, // regex locations for the whole host
		{"/otherz", "other /otherz "},   // ^/other matches: upstream does the same
	} {
		if code, body, _ := e.get("rw.local", tc.uri, false); code != 200 || !strings.HasPrefix(body, tc.body) {
			t.Errorf("%s: %d %q, want %q", tc.uri, code, body, tc.body)
		}
	}
	if code, _, _ := e.get("rw.local", "/nope", false); code != 404 {
		t.Errorf("/nope: %d, want 404", code)
	}
}

// testCert is a self-signed certificate for names, in PEM.
func testCert(t *testing.T, names ...string) (crt, key []byte) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func TestIngressTLS(t *testing.T) {
	e := newIngTestEnv(t)
	e.backend("default", "web", 80)
	crt, key := testCert(t, "tls.local")
	e.in.secrets["default/tls"] = &v1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "tls"}, Type: v1.SecretTypeTLS,
		Data: map[string][]byte{v1.TLSCertKey: crt, v1.TLSPrivateKeyKey: key}}
	secure := ingress("default", "secure", 1, nil, rule("tls.local", ingPath("/", prefix, "web", 80)))
	secure.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"tls.local"}, SecretName: "tls"}}
	noRedirect := ingress("default", "noredirect", 2, map[string]string{annSSLRedirect: "false"}, rule("noredir.local", ingPath("/", prefix, "web", 80)))
	noRedirect.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"noredir.local"}}}
	missing := ingress("default", "missing", 3, nil, rule("missing.local", ingPath("/", prefix, "web", 80)))
	missing.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"missing.local"}, SecretName: "nope"}}
	forced := ingress("default", "forced", 4, map[string]string{annForceSSLRedirect: "true"}, rule("forced.local", ingPath("/", prefix, "web", 80)))
	e.in.ingresses = []*netv1.Ingress{secure, noRedirect, missing, forced}
	e.load()
	if _, ok := e.out.files["/etc/ingress-controller/ssl/default-tls.pem"]; !ok {
		t.Errorf("files: %v", e.out.files)
	}
	if w := strings.Join(e.out.warnings, "\n"); !strings.Contains(w, `Error getting SSL certificate "default/nope": local SSL certificate default/nope was not found. Using default certificate`) {
		t.Errorf("warnings: %s", w)
	}
	for _, tc := range []struct {
		host, uri string
		ssl       bool
		code      int
		location  string
	}{
		{"tls.local", "/a?b=1", false, 308, "https://tls.local/a?b=1"},
		{"tls.local:30080", "/", false, 308, "https://tls.local/"},
		{"tls.local", "/a", true, 200, ""},
		{"noredir.local", "/", false, 200, ""},
		{"missing.local", "/", false, 200, ""}, // no usable certificate: no redirect
		{"forced.local", "/", false, 308, "https://forced.local/"},
		{"other.local", "/", false, 404, ""},
	} {
		code, _, h := e.get(tc.host, tc.uri, tc.ssl)
		if code != tc.code || h.Get("Location") != tc.location {
			t.Errorf("%s%s (ssl %v): %d %q, want %d %q", tc.host, tc.uri, tc.ssl, code, h.Get("Location"), tc.code, tc.location)
		}
	}
	// The certificate by SNI: the host's own, else the fake certificate.
	for sni, want := range map[string]string{
		"tls.local": "tls.local", "TLS.local": "tls.local", "missing.local": "Kubernetes Ingress Controller Fake Certificate",
		"": "Kubernetes Ingress Controller Fake Certificate", "other.local": "Kubernetes Ingress Controller Fake Certificate",
	} {
		c, err := e.n.certFor(e.cfg, "443", sni)
		if err != nil || c.Leaf == nil || c.Leaf.Subject.CommonName != want {
			t.Errorf("SNI %q: %v %v, want %s", sni, c, err, want)
		}
	}
}

// A handshake on a listening nginx: the certificate comes from the
// configuration at the time of the handshake, also after a reload.
func TestIngressSNIReload(t *testing.T) {
	e := newIngTestEnv(t)
	e.backend("default", "web", 80)
	e.in.ingresses = []*netv1.Ingress{ingress("default", "web", 1, nil, rule("a.local", ingPath("/", prefix, "web", 80)))}
	e.load()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.n.ctx = ctx
	if err := e.n.listen(e.cfg); err != nil {
		t.Fatal(err)
	}
	peer := func(sni string) string {
		c, err := tls.Dial("tcp", "127.0.0.1:443", &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		if err != nil {
			t.Fatalf("handshake %s: %v", sni, err)
		}
		defer c.Close()
		return c.ConnectionState().PeerCertificates[0].Subject.CommonName
	}
	if got := peer("b.local"); got != "Kubernetes Ingress Controller Fake Certificate" {
		t.Errorf("before: %s", got)
	}
	crt, key := testCert(t, "b.local")
	e.in.secrets["default/b"] = &v1.Secret{Data: map[string][]byte{v1.TLSCertKey: crt, v1.TLSPrivateKeyKey: key}}
	ing := ingress("default", "b", 2, nil, rule("b.local", ingPath("/", prefix, "web", 80)))
	ing.Spec.TLS = []netv1.IngressTLS{{Hosts: []string{"b.local"}, SecretName: "b"}}
	e.in.ingresses = append(e.in.ingresses, ing)
	n := e.n
	e.load()
	// what the controller does on a reload
	if err := n.listen(e.cfg); err != nil {
		t.Fatal(err)
	}
	n.cfg.Store(e.cfg)
	if got := peer("b.local"); got != "b.local" {
		t.Errorf("after the reload: %s", got)
	}
}

func TestIngressOwned(t *testing.T) {
	classes := map[string]*netv1.IngressClass{
		"nginx": {Spec: netv1.IngressClassSpec{Controller: "k8s.io/ingress-nginx"}},
		"other": {Spec: netv1.IngressClassSpec{Controller: "example.com/other"}},
	}
	class := func(n string) *netv1.IngressClass { return classes[n] }
	f := &ingressFlags{class: "nginx", controllerClass: "k8s.io/ingress-nginx"}
	withClass := func(name string) *netv1.Ingress {
		i := ingress("default", "x", 1, nil)
		i.Spec.IngressClassName = ptr.To(name)
		return i
	}
	without := ingress("default", "x", 1, nil)
	without.Spec.IngressClassName = nil
	annotated := func(v string) *netv1.Ingress {
		i := ingress("default", "x", 1, map[string]string{annIngressClass: v})
		i.Spec.IngressClassName = nil
		return i
	}
	for _, tc := range []struct {
		name string
		ing  *netv1.Ingress
		f    *ingressFlags
		ok   bool
	}{
		{"class nginx", withClass("nginx"), f, true},
		{"class of another controller", withClass("other"), f, false},
		{"unknown class", withClass("gone"), f, false},
		{"legacy annotation", annotated("nginx"), f, true},
		{"other legacy annotation", annotated("traefik"), f, false},
		{"no class", without, f, false},
		{"no class, --watch-ingress-without-class", without, &ingressFlags{class: "nginx", controllerClass: "k8s.io/ingress-nginx", watchWithoutClass: true}, true},
	} {
		_, err := ingressOwned(tc.ing, class, tc.f)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v, want owned %v", tc.name, err, tc.ok)
		}
	}
}

func TestIngressStatusAddresses(t *testing.T) {
	str := func(l []netv1.IngressLoadBalancerIngress) string {
		var s []string
		for _, a := range l {
			s = append(s, a.IP+a.Hostname)
		}
		return strings.Join(s, ",")
	}
	if got := str(statusFromAddresses("10.0.0.9, lb.example.com,10.0.0.9")); got != "10.0.0.9,lb.example.com" {
		t.Errorf("--publish-status-address: %s", got)
	}
	for _, tc := range []struct {
		svc  v1.Service
		want string
	}{
		{v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeClusterIP, ClusterIP: "10.96.0.7"}}, "10.96.0.7"},
		{v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeNodePort, ClusterIP: "10.96.0.7"}}, "10.96.0.7"},
		{v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeNodePort, ClusterIP: "10.96.0.7", ExternalIPs: []string{"192.0.2.1"}}}, "192.0.2.1"},
		{v1.Service{Spec: v1.ServiceSpec{Type: v1.ServiceTypeLoadBalancer, ClusterIP: "10.96.0.7"}, Status: v1.ServiceStatus{
			LoadBalancer: v1.LoadBalancerStatus{Ingress: []v1.LoadBalancerIngress{{IP: "203.0.113.5"}, {Hostname: "lb.example.com"}}}}}, "203.0.113.5,lb.example.com"},
	} {
		if got := str(statusFromService(&tc.svc)); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.svc.Spec.Type, got, tc.want)
		}
	}
	node := &v1.Node{Status: v1.NodeStatus{Addresses: []v1.NodeAddress{{Type: v1.NodeInternalIP, Address: "10.0.0.1"}, {Type: v1.NodeHostName, Address: "tab"}}}}
	if got := nodeAddress(node, false); got != "10.0.0.1" {
		t.Errorf("node without ExternalIP: %s", got)
	}
	node.Status.Addresses = append(node.Status.Addresses, v1.NodeAddress{Type: v1.NodeExternalIP, Address: "198.51.100.1"})
	if got := nodeAddress(node, false); got != "198.51.100.1" {
		t.Errorf("node with ExternalIP: %s", got)
	}
	if got := nodeAddress(node, true); got != "10.0.0.1" {
		t.Errorf("--report-node-internal-ip-address: %s", got)
	}
}

// The configuration of the image: no Ingress, the default server only.
func TestIngressEmptyConf(t *testing.T) {
	e := newIngTestEnv(t)
	e.load()
	if code, body, _ := e.get("x", "/", false); code != 404 || !strings.Contains(body, "<center>nginx</center>") {
		t.Errorf("%d %q", code, body)
	}
	if code, _, _ := e.get("x", "/healthz", false); code != 200 {
		t.Errorf("/healthz of the default server: %d", code)
	}
	if lookupImage("registry.k8s.io/ingress-nginx/controller:v1.13.3@sha256:abc") == nil {
		t.Error("the controller image is not built in")
	}
}

// The addon runs the built-in image, and its IngressClass is the default.
func TestIngressAddon(t *testing.T) {
	b, err := addonFS.ReadFile("addons/ingress-nginx.yaml")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, doc := range strings.Split(string(b), "\n---\n") {
		var meta metav1.TypeMeta
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			t.Fatal(err)
		}
		kinds[meta.Kind] = true
		switch meta.Kind {
		case "Deployment":
			d := &appsv1.Deployment{}
			if err := yaml.UnmarshalStrict([]byte(doc), d); err != nil {
				t.Fatal(err)
			}
			c := d.Spec.Template.Spec.Containers[0]
			img := lookupImage(c.Image)
			if img == nil || img.name != "ingress-nginx/controller" || imagePrograms[path.Base(c.Args[0])] == nil || imagePrograms[path.Base(img.entrypoint[0])] == nil {
				t.Errorf("image %s: %+v, args %v", c.Image, img, c.Args)
			}
			if sel := d.Spec.Template.Spec.NodeSelector["kubernetes.io/os"]; sel != (&tabNode{}).nodeLabels()["kubernetes.io/os"] {
				t.Errorf("nodeSelector kubernetes.io/os=%s does not fit the tab node", sel)
			}
		case "IngressClass":
			ic := &netv1.IngressClass{}
			if err := yaml.UnmarshalStrict([]byte(doc), ic); err != nil {
				t.Fatal(err)
			}
			if ic.Annotations[netv1.AnnotationIsDefaultIngressClass] != "true" || ic.Spec.Controller != "k8s.io/ingress-nginx" {
				t.Errorf("IngressClass %+v", ic)
			}
		}
	}
	for _, k := range []string{"Namespace", "ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding", "ConfigMap", "Service", "Deployment", "IngressClass"} {
		if !kinds[k] {
			t.Errorf("no %s", k)
		}
	}
	if kinds["ValidatingWebhookConfiguration"] || kinds["Job"] {
		t.Error("the admission webhook is not part of the addon")
	}
}

// curl --resolve keeps the name for Host and SNI and dials the address.
func TestCurlResolve(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got <- r.Host }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	tr := cmdTransport{resolve: map[string]string{"demo.local:" + port: "127.0.0.1"}}
	resp, err := (&http.Client{Transport: tr}).Get("http://demo.local:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := <-got; h != "demo.local:"+port {
		t.Errorf("Host %s", h)
	}
	c, err := parseCurl(context.Background(), interp.HandlerContext{}, []string{"--resolve", "demo.local:443:10.0.0.1", "-k", "https://demo.local"})
	if err != nil || len(c.resolve) != 1 || c.resolve[0] != "demo.local:443:10.0.0.1" {
		t.Errorf("parseCurl: %+v %v", c, err)
	}
	if flags, urls := splitWgetArgs([]string{"-qO-", "--header", "Host: demo.local", "10.0.0.1:30080"}); len(flags) != 3 || flags[2] != "Host: demo.local" || len(urls) != 1 {
		t.Errorf("wget --header: %q %q", flags, urls)
	}
}
