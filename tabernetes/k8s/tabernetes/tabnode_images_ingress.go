package main

// registry.k8s.io/ingress-nginx/controller: the controller of ingress-nginx
// on the built-in nginx (tabnode_images_web.go). It watches Ingresses,
// IngressClasses, Services, EndpointSlices and Secrets with the service
// account of its pod, renders /etc/nginx/nginx.conf the way ingress-nginx
// does (one server per host, locations per pathType, upstreams of the ready
// endpoints) and reloads nginx when the file changes. The addon
// addons/ingress-nginx.yaml runs it; on a Docker node the same manifest runs
// the real image.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	netv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"mvdan.cc/sh/v3/interp"
)

const (
	ingressRelease      = "v1.13.3"
	ingressNginxVersion = "1.27.1" // the nginx of that release
	ingressSSLDir       = "/etc/ingress-controller/ssl"
	ingressFakeCert     = ingressSSLDir + "/default-fake-certificate.pem"
	ingressConfFile     = "/etc/nginx/nginx.conf"

	annRewriteTarget    = "nginx.ingress.kubernetes.io/rewrite-target"
	annUseRegex         = "nginx.ingress.kubernetes.io/use-regex"
	annSSLRedirect      = "nginx.ingress.kubernetes.io/ssl-redirect"
	annForceSSLRedirect = "nginx.ingress.kubernetes.io/force-ssl-redirect"
	annBackendProtocol  = "nginx.ingress.kubernetes.io/backend-protocol"
	annServiceUpstream  = "nginx.ingress.kubernetes.io/service-upstream"
	annIngressClass     = "kubernetes.io/ingress.class"
)

func init() {
	imagePrograms["nginx-ingress-controller"] = ingressControllerProg
	imagePrograms["dumb-init"] = dumbInitProg
	// The preStop hook of the upstream manifest. Containers of the tab get
	// no signals, so there is nothing to wait for.
	imagePrograms["wait-shutdown"] = func(context.Context, interp.HandlerContext, []string, interp.ExecHandlerFunc) error { return nil }
	imageDefs = append(imageDefs, &imageDef{
		img: builtinImage{
			name:       "ingress-nginx/controller",
			entrypoint: []string{"/usr/bin/dumb-init", "--"},
			cmd:        []string{"/nginx-ingress-controller"},
			workdir:    "/etc/nginx",
			files: mergeFiles(alpineFiles, map[string]string{
				ingressConfFile:                 renderIngressConf(&ingInput{fakeCert: ingressFakeCert}).conf,
				"/etc/nginx/mime.types":         nginxMimeTypes,
				ingressSSLDir + "/.keep":        "",
				"/etc/nginx/template/README.md": "nginx.tmpl of ingress-nginx is Go code here: tabnode_images_ingress.go of Tabernetes renders nginx.conf.\n",
			}),
		},
		repos: []string{"ingress-nginx/controller"}, registry: "registry.k8s.io", tag: ingressRelease, size: 113_406_382,
	})
}

// dumbInitProg is dumb-init: the entrypoint of the controller image. Its
// flags are accepted, then the command runs.
func dumbInitProg(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		a := args[0]
		args = args[1:]
		switch a {
		case "--":
			goto run
		case "-V", "--version":
			fmt.Fprintln(hc.Stderr, "dumb-init v1.2.5")
			return nil
		case "-r", "--rewrite":
			if len(args) > 0 {
				args = args[1:]
			}
		}
	}
run:
	if len(args) == 0 {
		fmt.Fprintln(hc.Stderr, "Usage: dumb-init [option] command [[arg] ...]")
		return interp.ExitStatus(1)
	}
	return exec(ctx, args)
}

// --- rendering: Ingresses and friends -> nginx.conf ---

// ingInput is what the configuration is made of: the Ingresses of the
// controller and the objects they refer to, by "namespace/name".
type ingInput struct {
	ingresses  []*netv1.Ingress
	services   map[string]*v1.Service
	slices     map[string][]*discoveryv1.EndpointSlice // by namespace/service
	secrets    map[string]*v1.Secret
	defaultSSL string // namespace/name of --default-ssl-certificate
	fakeCert   string // the self-signed certificate of the controller
}

// ingOutput is the rendered configuration: nginx.conf, the certificate
// files it names, and what the controller logs about it.
type ingOutput struct {
	conf     string
	files    map[string]string
	warnings []string
}

type ingBackend struct {
	name            string // the upstream, <namespace>-<service>-<port>
	ns, svc, port   string
	servers         []string // ready endpoints, ip:port
	https           bool
	serviceUpstream bool
}

type ingLoc struct {
	path     string // the path of the Ingress
	mod      string // "", "=", "~*"
	ing      *netv1.Ingress
	be       *ingBackend // nil: the default backend, 404
	rewrite  string
	redirect bool // ssl-redirect for plain http
}

func (l *ingLoc) key() string { return l.mod + " " + l.path }

type ingServer struct {
	host     string
	regex    bool
	cert     string // ssl_certificate
	tls      bool   // a usable certificate from a tls entry: ssl-redirect applies
	locs     []*ingLoc
	seen     map[string]bool
	defBE    *ingBackend
	defBESet bool
}

// sortIngresses orders by age: on the same host and path the oldest wins.
func sortIngresses(list []*netv1.Ingress) []*netv1.Ingress {
	out := append([]*netv1.Ingress{}, list...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})
	return out
}

func renderIngressConf(in *ingInput) *ingOutput {
	out := &ingOutput{files: map[string]string{}}
	warn := func(format string, a ...any) { out.warnings = append(out.warnings, fmt.Sprintf(format, a...)) }
	ings := sortIngresses(in.ingresses)
	defaultCert := in.fakeCert
	if in.defaultSSL != "" {
		if p, err := ingSecretPEM(in, in.defaultSSL, "", out); err == nil {
			defaultCert = p
		} else {
			warn(`"Error getting SSL certificate" err=%q secret=%q`, err.Error(), in.defaultSSL)
		}
	}

	servers := map[string]*ingServer{}
	server := func(host string) *ingServer {
		if s := servers[host]; s != nil {
			return s
		}
		s := &ingServer{host: host, cert: defaultCert, seen: map[string]bool{}}
		servers[host] = s
		return s
	}
	server("_")
	ruleHost := func(r netv1.IngressRule) string {
		if r.Host == "" {
			return "_"
		}
		return strings.ToLower(r.Host)
	}
	// A host with use-regex or rewrite-target on any of its Ingresses gets
	// regex locations only.
	for _, ing := range ings {
		regex := ing.Annotations[annUseRegex] == "true" || ing.Annotations[annRewriteTarget] != ""
		for _, r := range ing.Spec.Rules {
			s := server(ruleHost(r))
			s.regex = s.regex || regex
		}
	}
	// TLS: the oldest Ingress that names a host decides its certificate.
	tlsDone := map[string]bool{}
	for _, ing := range ings {
		for _, t := range ing.Spec.TLS {
			hosts := t.Hosts
			if len(hosts) == 0 {
				for _, r := range ing.Spec.Rules {
					if r.Host != "" {
						hosts = append(hosts, r.Host)
					}
				}
			}
			for _, h := range hosts {
				h = strings.ToLower(h)
				if tlsDone[h] {
					continue
				}
				tlsDone[h] = true
				s := server(h)
				if t.SecretName == "" {
					s.tls = true
					continue
				}
				key := ing.Namespace + "/" + t.SecretName
				p, err := ingSecretPEM(in, key, h, out)
				if err != nil {
					warn("Error getting SSL certificate %q: %v. Using default certificate", key, err)
					continue
				}
				s.cert, s.tls = p, true
			}
		}
	}

	backends := map[string]*ingBackend{}
	backend := func(ing *netv1.Ingress, b *netv1.IngressBackend) *ingBackend {
		if b == nil || b.Service == nil {
			return nil
		}
		port := b.Service.Port.Name
		if port == "" {
			port = strconv.Itoa(int(b.Service.Port.Number))
		}
		name := ing.Namespace + "-" + b.Service.Name + "-" + port
		if be := backends[name]; be != nil {
			return be
		}
		be := &ingBackend{name: name, ns: ing.Namespace, svc: b.Service.Name, port: port,
			https:           strings.EqualFold(ing.Annotations[annBackendProtocol], "HTTPS"),
			serviceUpstream: ing.Annotations[annServiceUpstream] == "true"}
		backends[name] = be
		key := ing.Namespace + "/" + b.Service.Name
		svc := in.services[key]
		if svc == nil {
			warn(`"Error obtaining Endpoints for Service" err="no object matching key \"%s\" in local store" service=%q`, key, key)
			return be
		}
		var sp *v1.ServicePort
		for i, p := range svc.Spec.Ports {
			if (b.Service.Port.Name != "" && p.Name == b.Service.Port.Name) || (b.Service.Port.Name == "" && p.Port == b.Service.Port.Number) {
				sp = &svc.Spec.Ports[i]
			}
		}
		if sp == nil {
			warn(`"Service does not have any active Endpoint" service=%q port=%q`, key, port)
			return be
		}
		if be.serviceUpstream && svc.Spec.ClusterIP != "" && svc.Spec.ClusterIP != v1.ClusterIPNone {
			be.servers = []string{net.JoinHostPort(svc.Spec.ClusterIP, strconv.Itoa(int(sp.Port)))}
			return be
		}
		seen := map[string]bool{}
		for _, es := range in.slices[key] {
			for _, ep := range es.Ports {
				if ep.Port == nil || ptrString(ep.Name) != sp.Name {
					continue
				}
				for _, e := range es.Endpoints {
					if e.Conditions.Ready != nil && !*e.Conditions.Ready {
						continue
					}
					for _, a := range e.Addresses {
						hp := net.JoinHostPort(a, strconv.Itoa(int(*ep.Port)))
						if !seen[hp] {
							seen[hp] = true
							be.servers = append(be.servers, hp)
						}
					}
				}
			}
		}
		sort.Strings(be.servers)
		if len(be.servers) == 0 {
			warn(`"Service does not have any active Endpoint" service=%q`, key)
		}
		return be
	}
	add := func(s *ingServer, l *ingLoc) {
		if !s.seen[l.key()] {
			s.seen[l.key()] = true
			s.locs = append(s.locs, l)
		}
	}
	for _, ing := range ings {
		if b := ing.Spec.DefaultBackend; b != nil {
			be := backend(ing, b)
			if len(ing.Spec.Rules) == 0 {
				// An Ingress with a backend and no rule: the catch-all.
				if s := server("_"); !s.defBESet {
					s.defBE, s.defBESet = be, true
				}
			}
			for _, r := range ing.Spec.Rules {
				// Otherwise the default of the root of its hosts.
				if s := server(ruleHost(r)); !s.defBESet {
					s.defBE, s.defBESet = be, true
				}
			}
		}
		for _, r := range ing.Spec.Rules {
			s := server(ruleHost(r))
			if r.HTTP == nil {
				continue
			}
			for _, p := range r.HTTP.Paths {
				be := backend(ing, &p.Backend)
				pt := netv1.PathTypeImplementationSpecific
				if p.PathType != nil {
					pt = *p.PathType
				}
				path := p.Path
				if path == "" {
					path = "/"
				}
				var locs []*ingLoc
				switch {
				case pt == netv1.PathTypeExact:
					locs = append(locs, &ingLoc{path: path, mod: "="})
				case pt == netv1.PathTypePrefix && path != "/":
					// Prefix matches element-wise: /foo/ and everything below,
					// and /foo exactly.
					locs = append(locs, &ingLoc{path: strings.TrimSuffix(path, "/") + "/"},
						&ingLoc{path: strings.TrimSuffix(path, "/"), mod: "="})
				default:
					locs = append(locs, &ingLoc{path: path})
				}
				for _, l := range locs {
					if s.regex {
						l.mod = "~*"
					}
					l.ing, l.be, l.rewrite = ing, be, ing.Annotations[annRewriteTarget]
					l.redirect = (s.tls && ing.Annotations[annSSLRedirect] != "false") || ing.Annotations[annForceSSLRedirect] == "true"
					add(s, l)
				}
			}
		}
	}
	for _, s := range servers {
		root := &ingLoc{path: "/", be: s.defBE, redirect: s.tls}
		if s.regex {
			root.mod = "~*"
		}
		add(s, root)
		// Longest path first: the order of the regex locations.
		sort.SliceStable(s.locs, func(i, j int) bool {
			a, b := s.locs[i], s.locs[j]
			if len(a.path) != len(b.path) {
				return len(a.path) > len(b.path)
			}
			return a.key() < b.key()
		})
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w(ingConfHead)
	var names []string
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		be := backends[name]
		if len(be.servers) == 0 {
			continue
		}
		w("\tupstream %s {", name)
		for _, srv := range be.servers {
			w("\t\tserver %s max_fails=0 fail_timeout=0;", srv)
		}
		w("\t}\n")
	}
	var hosts []string
	for h := range servers {
		if h != "_" {
			hosts = append(hosts, h)
		}
	}
	// Exact names before wildcards: nginx prefers them, and the server
	// picked first by name wins here.
	sort.Slice(hosts, func(i, j int) bool {
		wi, wj := strings.HasPrefix(hosts[i], "*"), strings.HasPrefix(hosts[j], "*")
		if wi != wj {
			return wj
		}
		return hosts[i] < hosts[j]
	})
	renderServer(w, servers["_"])
	for _, h := range hosts {
		renderServer(w, servers[h])
	}
	w("}")
	conf := b.String()
	sum := sha256.Sum256([]byte(conf))
	out.conf = "\n# Configuration checksum: " + strconv.FormatUint(binary.BigEndian.Uint64(sum[:8]), 10) + "\n" + conf
	return out
}

const ingConfHead = `
# setup custom paths that do not require root access
pid /tmp/nginx/nginx.pid;

daemon off;

worker_processes 1;

worker_rlimit_nofile 1047552;

worker_shutdown_timeout 240s ;

events {
	multi_accept        on;
	worker_connections  16384;
	use                 epoll;
}

http {
	include /etc/nginx/mime.types;
	default_type text/html;

	server_tokens off;

	keepalive_timeout  75s;
	keepalive_requests 1000;

	client_max_body_size 1m;

	log_format upstreaminfo '$remote_addr - $remote_user [$time_local] "$request" $status $body_bytes_sent "$http_referer" "$http_user_agent" $request_length $request_time [$proxy_upstream_name] [$proxy_alternative_upstream_name] $upstream_addr $upstream_response_length $upstream_response_time $upstream_status $req_id';

	access_log /var/log/nginx/access.log upstreaminfo;

	error_log  /var/log/nginx/error.log notice;

	ssl_protocols TLSv1.2 TLSv1.3;
`

// renderServer writes the server blocks of a host: one for both ports, or,
// when a location redirects plain http, one per port.
func renderServer(w func(string, ...any), s *ingServer) {
	split := false
	for _, l := range s.locs {
		split = split || l.redirect
	}
	if !split {
		renderServerBlock(w, s, true, true)
		return
	}
	renderServerBlock(w, s, true, false)
	renderServerBlock(w, s, false, true)
}

func renderServerBlock(w func(string, ...any), s *ingServer, http, https bool) {
	def := ""
	if s.host == "_" {
		def = " default_server"
	}
	w("\t## start server %s", s.host)
	w("\tserver {")
	w("\t\tserver_name %s ;\n", s.host)
	if http {
		w("\t\tlisten 80%s reuseport backlog=4096 ;", def)
		w("\t\tlisten [::]:80%s reuseport backlog=4096 ;", def)
	}
	if https {
		w("\t\tlisten 443%s reuseport backlog=4096 ssl http2 ;", def)
		w("\t\tlisten [::]:443%s reuseport backlog=4096 ssl http2 ;", def)
		w("\n\t\t# PEM sha: %s", "-")
		w("\t\tssl_certificate     %s;", s.cert)
		w("\t\tssl_certificate_key %s;", s.cert)
	}
	w("")
	w("\t\tset $proxy_upstream_name \"-\";")
	w("\t\tset $req_id $request_id;")
	for _, l := range s.locs {
		renderLocation(w, l, http && !https)
	}
	if s.host == "_" {
		w("\t\t# health checks in cloud providers require the use of port 80")
		w("\t\tlocation /healthz {")
		w("\t\t\taccess_log off;")
		w("\t\t\treturn 200;")
		w("\t\t}\n")
	}
	w("\t}")
	w("\t## end server %s\n", s.host)
}

func renderLocation(w func(string, ...any), l *ingLoc, plainHTTP bool) {
	switch l.mod {
	case "~*":
		w("\t\tlocation ~* \"^%s\" {", l.path)
	case "=":
		w("\t\tlocation = %s {", l.path)
	default:
		w("\t\tlocation %s {", l.path)
	}
	ns, name, svc, port := "", "", "", ""
	if l.ing != nil {
		ns, name = l.ing.Namespace, l.ing.Name
	}
	upstream := "upstream-default-backend"
	if l.be != nil {
		svc, port, upstream = l.be.svc, l.be.port, l.be.name
	}
	w("\t\t\tset $namespace      %q;", ns)
	w("\t\t\tset $ingress_name   %q;", name)
	w("\t\t\tset $service_name   %q;", svc)
	w("\t\t\tset $service_port   %q;", port)
	w("\t\t\tset $location_path  %q;\n", l.path)
	w("\t\t\tset $proxy_upstream_name %q;", upstream)
	w("\t\t\tset $proxy_host          $proxy_upstream_name;")
	w("\t\t\tset $pass_access_scheme  $scheme;")
	w("\t\t\tset $pass_server_port    $server_port;")
	w("\t\t\tset $best_http_host      $http_host;")
	w("\t\t\tset $pass_port           $pass_server_port;")
	w("\t\t\tset $proxy_alternative_upstream_name \"\";\n")
	switch {
	case plainHTTP && l.redirect:
		w("\t\t\treturn 308 https://$host$request_uri;")
	case l.be == nil:
		w("\t\t\treturn 404;")
	case len(l.be.servers) == 0:
		// No ready endpoint: what the balancer of ingress-nginx answers.
		w("\t\t\treturn 503;")
	default:
		w("\t\t\tproxy_set_header Host               $best_http_host;")
		w("\t\t\tproxy_set_header X-Request-ID       $req_id;")
		w("\t\t\tproxy_set_header X-Real-IP          $remote_addr;")
		w("\t\t\tproxy_set_header X-Forwarded-For    $remote_addr;")
		w("\t\t\tproxy_set_header X-Forwarded-Host   $best_http_host;")
		w("\t\t\tproxy_set_header X-Forwarded-Port   $pass_port;")
		w("\t\t\tproxy_set_header X-Forwarded-Proto  $pass_access_scheme;")
		w("\t\t\tproxy_set_header X-Forwarded-Scheme $pass_access_scheme;")
		w("\t\t\tproxy_set_header X-Scheme           $pass_access_scheme;")
		w("\t\t\tproxy_set_header X-Original-URI     $request_uri;\n")
		if l.rewrite != "" {
			w("\t\t\trewrite \"(?i)%s\" %s break;", l.path, l.rewrite)
		}
		scheme := "http"
		if l.be.https {
			scheme = "https"
		}
		w("\t\t\tproxy_pass %s://%s;", scheme, l.be.name)
	}
	w("\t\t}\n")
}

func ptrString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// ingSecretPEM checks a TLS Secret and adds its PEM file (certificate and
// key) to the output. host, if set, must be a name of the certificate.
func ingSecretPEM(in *ingInput, key, host string, out *ingOutput) (string, error) {
	sec := in.secrets[key]
	if sec == nil {
		return "", fmt.Errorf("local SSL certificate %s was not found", key)
	}
	crt, k := sec.Data[v1.TLSCertKey], sec.Data[v1.TLSPrivateKeyKey]
	if len(crt) == 0 || len(k) == 0 {
		return "", fmt.Errorf("secret %s has no keys %s and %s", key, v1.TLSCertKey, v1.TLSPrivateKeyKey)
	}
	pair, err := tls.X509KeyPair(crt, k)
	if err != nil {
		return "", fmt.Errorf("unexpected error creating SSL Cert: %v", err)
	}
	if host != "" && pair.Leaf != nil {
		h := host
		if strings.HasPrefix(h, "*.") {
			h = "wildcard" + h[1:]
		}
		if err := pair.Leaf.VerifyHostname(h); err != nil {
			return "", fmt.Errorf("ssl certificate %s does not contain a Common Name or Subject Alternative Name for server %s", key, host)
		}
	}
	p := path.Join(ingressSSLDir, strings.ReplaceAll(key, "/", "-")+".pem")
	out.files[p] = strings.TrimRight(string(crt), "\n") + "\n" + strings.TrimRight(string(k), "\n") + "\n"
	return p, nil
}

// --- which Ingresses, which status ---

type ingressFlags struct {
	class, controllerClass                     string
	watchWithoutClass                          bool
	publishService, publishStatusAddress       string
	electionID, configmap, defaultSSL, watchNS string
	httpPort, httpsPort, healthzPort           int
	showVersion                                bool
	reportNodeInternalIP, updateStatus         bool
}

// ingressOwned decides whether the controller serves an Ingress: by its
// IngressClass, the legacy annotation, or --watch-ingress-without-class.
func ingressOwned(ing *netv1.Ingress, class func(name string) *netv1.IngressClass, f *ingressFlags) (string, error) {
	if a, ok := ing.Annotations[annIngressClass]; ok {
		if a != f.class {
			return "", fmt.Errorf("ingress class annotation is not equal to the expected by Ingress Controller")
		}
		return a, nil
	}
	if n := ing.Spec.IngressClassName; n != nil {
		ic := class(*n)
		if ic == nil || ic.Spec.Controller != f.controllerClass {
			return "", fmt.Errorf("ingress does not contain a valid IngressClass")
		}
		return *n, nil
	}
	if f.watchWithoutClass {
		return "", nil
	}
	return "", fmt.Errorf("ingress does not contain a valid IngressClass")
}

// statusFromAddresses parses --publish-status-address.
func statusFromAddresses(list string) []netv1.IngressLoadBalancerIngress {
	var out []netv1.IngressLoadBalancerIngress
	for _, a := range strings.Split(list, ",") {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, ingressLB(a))
		}
	}
	return sortLB(out)
}

func ingressLB(a string) netv1.IngressLoadBalancerIngress {
	if net.ParseIP(a) != nil {
		return netv1.IngressLoadBalancerIngress{IP: a}
	}
	return netv1.IngressLoadBalancerIngress{Hostname: a}
}

// statusFromService is the status for --publish-service, as upstream.
func statusFromService(svc *v1.Service) []netv1.IngressLoadBalancerIngress {
	var out []netv1.IngressLoadBalancerIngress
	switch svc.Spec.Type {
	case v1.ServiceTypeExternalName:
		out = append(out, netv1.IngressLoadBalancerIngress{Hostname: svc.Spec.ExternalName})
	case v1.ServiceTypeClusterIP:
		out = append(out, ingressLB(svc.Spec.ClusterIP))
	case v1.ServiceTypeNodePort:
		if len(svc.Spec.ExternalIPs) == 0 {
			out = append(out, ingressLB(svc.Spec.ClusterIP))
		}
		for _, ip := range svc.Spec.ExternalIPs {
			out = append(out, ingressLB(ip))
		}
	case v1.ServiceTypeLoadBalancer:
		for _, lb := range svc.Status.LoadBalancer.Ingress {
			if lb.IP != "" {
				out = append(out, netv1.IngressLoadBalancerIngress{IP: lb.IP})
			} else if lb.Hostname != "" {
				out = append(out, netv1.IngressLoadBalancerIngress{Hostname: lb.Hostname})
			}
		}
		for _, ip := range svc.Spec.ExternalIPs {
			out = append(out, ingressLB(ip))
		}
	}
	return sortLB(out)
}

// nodeAddress is the address of a node that runs a controller pod: its
// ExternalIP, else its InternalIP.
func nodeAddress(node *v1.Node, internal bool) string {
	var ext, in string
	for _, a := range node.Status.Addresses {
		switch {
		case a.Type == v1.NodeExternalIP && ext == "":
			ext = a.Address
		case a.Type == v1.NodeInternalIP && in == "":
			in = a.Address
		}
	}
	if ext != "" && !internal {
		return ext
	}
	return in
}

func sortLB(list []netv1.IngressLoadBalancerIngress) []netv1.IngressLoadBalancerIngress {
	seen := map[string]bool{}
	var out []netv1.IngressLoadBalancerIngress
	for _, lb := range list {
		if k := lb.IP + "|" + lb.Hostname; !seen[k] {
			seen[k] = true
			out = append(out, lb)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP+out[i].Hostname < out[j].IP+out[j].Hostname })
	return out
}

func sameLB(a, b []netv1.IngressLoadBalancerIngress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IP != b[i].IP || a[i].Hostname != b[i].Hostname {
			return false
		}
	}
	return true
}

// --- the program ---

// klog writes lines in the format of klog, like the real controller:
// structured ones (InfoS) with the message quoted, printf ones as they are.
type klog struct{ w io.Writer }

type rawJSON string

func (l klog) printf(sev byte, src, msg string) {
	fmt.Fprintf(l.w, "%c%s       7 %s] %s\n", sev, time.Now().UTC().Format("0102 15:04:05.000000"), src, msg)
}

func (l klog) log(sev byte, src, msg string, kv ...any) {
	var b strings.Builder
	fmt.Fprintf(&b, "%q", msg)
	for i := 0; i+1 < len(kv); i += 2 {
		switch v := kv[i+1].(type) {
		case string:
			fmt.Fprintf(&b, " %s=%q", kv[i], v)
		case rawJSON:
			fmt.Fprintf(&b, " %s=%s", kv[i], string(v))
		default:
			fmt.Fprintf(&b, " %s=%v", kv[i], v)
		}
	}
	l.printf(sev, src, b.String())
}

func (l klog) info(src, msg string, kv ...any)  { l.log('I', src, msg, kv...) }
func (l klog) warn(src, msg string, kv ...any)  { l.log('W', src, msg, kv...) }
func (l klog) error(src, msg string, kv ...any) { l.log('E', src, msg, kv...) }

const ingressBanner = `-------------------------------------------------------------------------------
NGINX Ingress controller
  Release:       ` + ingressRelease + `
  Build:         built into Tabernetes (Go, js/wasm)
  Repository:    https://github.com/kubernetes/ingress-nginx
  nginx version: nginx/` + ingressNginxVersion + `

-------------------------------------------------------------------------------
`

type ingController struct {
	f       ingressFlags
	log     klog
	hc      interp.HandlerContext
	real    func(string) string
	client  kubernetes.Interface
	factory informers.SharedInformerFactory
	rec     record.EventRecorder
	podName string
	podNS   string
	self    *v1.Pod
	kick    chan struct{}
	inst    *nginxInst
	conf    string
	files   map[string]string
	warned  map[string]bool
	healthy bool
	mu      sync.Mutex
	leader  bool
	addrs   []netv1.IngressLoadBalancerIngress
	addrsAt time.Time
	errw    *syncWriter
}

func ingressControllerProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	c := &ingController{hc: hc, real: containerFS(ctx), kick: make(chan struct{}, 1), files: map[string]string{}, warned: map[string]bool{}}
	c.errw = &syncWriter{w: hc.Stderr}
	c.log = klog{c.errw}
	fs := pflag.NewFlagSet("nginx-ingress-controller", pflag.ContinueOnError)
	fs.ParseErrorsAllowlist.UnknownFlags = true
	fs.SetOutput(hc.Stderr)
	f := &c.f
	fs.StringVar(&f.class, "ingress-class", "nginx", "Name of the ingress class this controller satisfies.")
	fs.StringVar(&f.controllerClass, "controller-class", "k8s.io/ingress-nginx", "Ingress Class Controller value this Ingress satisfies.")
	fs.BoolVar(&f.watchWithoutClass, "watch-ingress-without-class", false, "Define if Ingress Controller should also watch for Ingresses without an IngressClass or the annotation specified.")
	fs.StringVar(&f.publishService, "publish-service", "", "Service fronting the Ingress controller.")
	fs.StringVar(&f.publishStatusAddress, "publish-status-address", "", "Customized address (or addresses, separated by comma) to set as the load-balancer status of Ingress objects this controller satisfies.")
	fs.StringVar(&f.electionID, "election-id", "ingress-controller-leader", "Election id to use for Ingress status updates.")
	fs.StringVar(&f.configmap, "configmap", "", "Name of the ConfigMap containing custom global configurations for the controller.")
	fs.StringVar(&f.defaultSSL, "default-ssl-certificate", "", "Secret containing a SSL certificate to be used by the default HTTPS server (catch-all).")
	fs.StringVar(&f.watchNS, "watch-namespace", "", "Namespace the controller watches for updates to Kubernetes objects.")
	fs.IntVar(&f.httpPort, "http-port", 80, "Port to use for servicing HTTP traffic.")
	fs.IntVar(&f.httpsPort, "https-port", 443, "Port to use for servicing HTTPS traffic.")
	fs.IntVar(&f.healthzPort, "healthz-port", 10254, "Port to use for the healthz endpoint.")
	fs.BoolVar(&f.reportNodeInternalIP, "report-node-internal-ip-address", false, "Set the load-balancer status of Ingress objects to internal Node addresses instead of external.")
	fs.BoolVar(&f.updateStatus, "update-status", true, "Update the load-balancer status of Ingress objects this controller satisfies.")
	fs.BoolVar(&f.showVersion, "version", false, "Show release information about the NGINX Ingress controller and exit.")
	if err := fs.Parse(args); err != nil {
		if err == pflag.ErrHelp {
			return nil
		}
		return interp.ExitStatus(2)
	}
	fmt.Fprint(hc.Stdout, ingressBanner)
	if f.showVersion {
		return nil
	}
	c.podName, c.podNS = getenv(hc, "POD_NAME"), getenv(hc, "POD_NAMESPACE")
	if c.podName == "" {
		c.podName = podHostname(ctx)
	}
	if c.podNS == "" {
		c.podNS = "default"
	}
	if f.httpPort != 80 || f.httpsPort != 443 {
		c.log.printf('W', "flags.go:333", "--http-port and --https-port other than 80 and 443 are not supported here; using 80 and 443")
	}
	c.log.printf('W', "client_config.go:667", "Neither --kubeconfig nor --master was specified.  Using the inClusterConfig.  This might not work.")
	cfg, err := c.restConfig(ctx)
	if err == nil {
		c.log.info("main.go:205", "Creating API client", "host", cfg.Host)
		c.client, err = kubernetes.NewForConfig(cfg)
	}
	var ver string
	if err == nil {
		var v interface{ String() string }
		v, err = c.client.Discovery().ServerVersion()
		if err == nil {
			ver = v.String()
		}
	}
	if err != nil {
		c.log.printf('F', "main.go:107", fmt.Sprintf("Error while initiating a connection to the Kubernetes API server. This could mean the cluster is misconfigured (e.g. it has invalid API server certificates or Service Accounts configuration). Reason: %v\nRefer to the troubleshooting guide for more information: https://kubernetes.github.io/ingress-nginx/troubleshooting/", err))
		return interp.ExitStatus(255)
	}
	major, minor, _ := strings.Cut(strings.TrimPrefix(ver, "v"), ".")
	minor, _, _ = strings.Cut(minor, ".")
	c.log.info("main.go:248", "Running in Kubernetes cluster", "major", major, "minor", minor, "git", ver, "state", "clean", "platform", "linux/"+runtime.GOARCH)
	if err := c.fakeCertificate(); err != nil {
		c.log.log('F', "main.go:101", "unexpected error generating SSL certificate", "err", err.Error())
		return interp.ExitStatus(255)
	}
	c.log.info("main.go:101", "SSL fake certificate created", "file", ingressFakeCert)

	bc := record.NewBroadcaster()
	defer bc.Shutdown()
	bc.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: c.client.CoreV1().Events("")})
	bc.StartEventWatcher(func(e *v1.Event) {
		c.log.printf('I', "event.go:377", fmt.Sprintf("Event(%#v): type: '%s' reason: '%s' %s", e.InvolvedObject, e.Type, e.Reason, e.Message))
	})
	c.rec = bc.NewRecorder(scheme.Scheme, v1.EventSource{Component: "nginx-ingress-controller"})
	if self, err := c.client.CoreV1().Pods(c.podNS).Get(ctx, c.podName, metav1.GetOptions{}); err == nil {
		c.self = self
	}

	go c.healthz(ctx)
	c.log.info("nginx.go:271", "Starting NGINX Ingress controller")
	if !c.watch(ctx) {
		return nil
	}
	c.log.info("nginx.go:317", "Starting NGINX process")
	go c.lead(ctx)
	defer func() {
		if c.inst != nil {
			c.inst.stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			c.log.info("nginx.go:394", "Shutting down controller queues")
			return nil
		case <-c.kick:
		}
		// Changes within this time end up in one sync.
		if !sleepCtx(ctx, 250*time.Millisecond) {
			return nil
		}
		select {
		case <-c.kick:
		default:
		}
		if err := c.sync(ctx); err != nil {
			return err
		}
	}
}

// restConfig is the in-cluster configuration of the pod: the token and CA of
// its service account, the apiserver through the kubernetes service, dialed
// from the pod, so RBAC and NetworkPolicy apply.
func (c *ingController) restConfig(ctx context.Context) (*rest.Config, error) {
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token := c.real(saDir + "/token")
	if _, err := os.Stat(token); err != nil {
		return nil, fmt.Errorf("open %s/token: no such file or directory", saDir)
	}
	host, port := getenv(c.hc, "KUBERNETES_SERVICE_HOST"), getenv(c.hc, "KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		host, port = "10.96.0.1", "443"
	}
	pe := procOf(ctx)
	return &rest.Config{
		Host:            "https://" + net.JoinHostPort(host, port),
		BearerTokenFile: token,
		TLSClientConfig: rest.TLSClientConfig{CAFile: c.real(saDir + "/ca.crt")},
		UserAgent:       "nginx-ingress-controller/" + ingressRelease + " (linux/" + runtime.GOARCH + ") ingress-nginx/tabernetes",
		Dial: func(dctx context.Context, network, addr string) (net.Conn, error) {
			if pe != nil {
				dctx = withProc(dctx, pe)
			}
			return dialCluster(dctx, network, addr)
		},
	}, nil
}

// fakeCertificate is the self-signed certificate of the default server.
// ECDSA: an RSA key takes ages in single-threaded wasm.
func (c *ingController) fakeCertificate() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"Acme Co"}, CommonName: "Kubernetes Ingress Controller Fake Certificate"},
		DNSNames:              []string{"ingress.local"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	b := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})...)
	return c.writeFile(ingressFakeCert, string(b))
}

func (c *ingController) writeFile(p, content string) error {
	rp := c.real(p)
	if err := os.MkdirAll(path.Dir(rp), 0o755); err != nil {
		return err
	}
	return os.WriteFile(rp, []byte(content), 0o644)
}

func (c *ingController) trigger() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *ingController) class(name string) *netv1.IngressClass {
	ic, err := c.factory.Networking().V1().IngressClasses().Lister().Get(name)
	if err != nil {
		return nil
	}
	return ic
}

// watch starts the informers and waits for their caches.
func (c *ingController) watch(ctx context.Context) bool {
	var opts []informers.SharedInformerOption
	if c.f.watchNS != "" {
		opts = append(opts, informers.WithNamespace(c.f.watchNS))
	}
	c.factory = informers.NewSharedInformerFactoryWithOptions(c.client, 0, opts...)
	kick := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { c.trigger() },
		UpdateFunc: func(any, any) { c.trigger() },
		DeleteFunc: func(any) { c.trigger() },
	}
	ing := c.factory.Networking().V1().Ingresses().Informer()
	synced := []cache.InformerSynced{ing.HasSynced}
	for _, inf := range []cache.SharedIndexInformer{
		c.factory.Networking().V1().IngressClasses().Informer(),
		c.factory.Core().V1().Services().Informer(),
		c.factory.Discovery().V1().EndpointSlices().Informer(),
		c.factory.Core().V1().Secrets().Informer(),
	} {
		inf.AddEventHandler(kick)
		synced = append(synced, inf.HasSynced)
	}
	c.factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return false
	}
	// Added after the caches: the existing Ingresses come as adds, with
	// their IngressClasses known.
	ing.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { c.ingressEvent(obj.(*netv1.Ingress)) },
		UpdateFunc: func(_, obj any) { c.ingressEvent(obj.(*netv1.Ingress)) },
		DeleteFunc: func(any) { c.trigger() },
	})
	c.trigger()
	return true
}

func (c *ingController) ingressEvent(ing *netv1.Ingress) {
	defer c.trigger()
	key := ing.Namespace + "/" + ing.Name
	class, err := ingressOwned(ing, c.class, &c.f)
	if err != nil {
		c.log.info("store.go:432", "Ignoring ingress because of error while validating ingress class", "ingress", key, "error", err.Error())
		return
	}
	if class != "" {
		c.log.info("store.go:440", "Found valid IngressClass", "ingress", key, "ingressclass", class)
	} else {
		c.log.info("store.go:440", "Found valid IngressClass", "ingress", key, "ingressclass", "_")
	}
	c.rec.Eventf(ing, v1.EventTypeNormal, "Sync", "Scheduled for sync")
}

// owned lists the Ingresses of this controller.
func (c *ingController) owned() []*netv1.Ingress {
	all, _ := c.factory.Networking().V1().Ingresses().Lister().List(labels.Everything())
	var out []*netv1.Ingress
	for _, ing := range all {
		if _, err := ingressOwned(ing, c.class, &c.f); err == nil && ing.DeletionTimestamp == nil {
			out = append(out, ing)
		}
	}
	return out
}

func (c *ingController) input(owned []*netv1.Ingress) *ingInput {
	in := &ingInput{ingresses: owned, services: map[string]*v1.Service{}, slices: map[string][]*discoveryv1.EndpointSlice{},
		secrets: map[string]*v1.Secret{}, defaultSSL: c.f.defaultSSL, fakeCert: ingressFakeCert}
	svcs, _ := c.factory.Core().V1().Services().Lister().List(labels.Everything())
	for _, s := range svcs {
		in.services[s.Namespace+"/"+s.Name] = s
	}
	slices, _ := c.factory.Discovery().V1().EndpointSlices().Lister().List(labels.Everything())
	for _, es := range slices {
		if svc := es.Labels[discoveryv1.LabelServiceName]; svc != "" {
			in.slices[es.Namespace+"/"+svc] = append(in.slices[es.Namespace+"/"+svc], es)
		}
	}
	secrets, _ := c.factory.Core().V1().Secrets().Lister().List(labels.Everything())
	for _, s := range secrets {
		in.secrets[s.Namespace+"/"+s.Name] = s
	}
	return in
}

// sync renders the configuration, reloads nginx when it changed and
// updates the status of the Ingresses.
func (c *ingController) sync(ctx context.Context) error {
	owned := c.owned()
	out := renderIngressConf(c.input(owned))
	warned := map[string]bool{}
	for _, w := range out.warnings {
		if !c.warned[w] {
			c.log.printf('W', "controller.go:1216", w)
		}
		warned[w] = true
	}
	c.warned = warned
	for p, content := range out.files {
		if c.files[p] != content {
			if err := c.writeFile(p, content); err != nil {
				c.log.error("controller.go:262", "Unexpected failure writing certificate", "err", err.Error())
			}
		}
	}
	c.files = out.files
	if out.conf != c.conf {
		c.log.info("controller.go:196", "Configuration changes detected, backend reload required")
		if err := c.reload(ctx, out.conf); err != nil {
			c.log.error("controller.go:211", "Unexpected failure reloading the backend", "err", err.Error())
			if c.self != nil {
				c.rec.Eventf(c.self, v1.EventTypeWarning, "RELOAD", "Error reloading NGINX: %v", err)
			}
		} else {
			c.conf = out.conf
			c.log.info("controller.go:216", "Backend successfully reloaded")
			if c.self != nil {
				c.rec.Event(c.self, v1.EventTypeNormal, "RELOAD", "NGINX reload triggered due to a change in configuration")
			}
		}
	}
	if c.inst == nil {
		return interp.ExitStatus(1) // nginx did not start
	}
	c.mu.Lock()
	c.healthy = true
	leader := c.leader
	c.mu.Unlock()
	if leader && c.f.updateStatus {
		c.updateStatus(ctx, owned)
	}
	return nil
}

// reload tests the new configuration in a file of its own, like nginx -t,
// then writes nginx.conf and starts or reloads nginx.
func (c *ingController) reload(ctx context.Context, conf string) error {
	tmp := fmt.Sprintf("/tmp/nginx/nginx-cfg%d", time.Now().UnixNano())
	if err := c.writeFile(tmp, conf); err != nil {
		return err
	}
	defer os.Remove(c.real(tmp))
	cfg, err := ngxLoad(c.real, tmp, "")
	if err != nil {
		return fmt.Errorf("\n-------------------------------------------------------------------------------\nError: exit status 1\nnginx: [emerg] %v\nnginx: configuration file %s test failed\n-------------------------------------------------------------------------------", err, tmp)
	}
	if err := c.writeFile(ingressConfFile, conf); err != nil {
		return err
	}
	if c.inst == nil {
		nctx, cancel := context.WithCancel(ctx)
		n := &nginxInst{ctx: nctx, hc: c.hc, stop: cancel, real: c.real, file: ingressConfFile, version: ingressNginxVersion,
			ports: map[string]bool{}, errw: c.errw, outw: &syncWriter{w: c.hc.Stdout}}
		n.cfg.Store(cfg)
		if err := n.listen(cfg); err != nil {
			cancel()
			return err
		}
		if pe := procOf(ctx); pe != nil && pe.c != nil {
			// nginx -s reload and nginx -T in kubectl exec find it.
			nginxRunning.Store(pe.c, n)
			context.AfterFunc(nctx, func() { nginxRunning.CompareAndDelete(pe.c, n) })
		}
		c.inst = n
		return nil
	}
	if err := c.inst.listen(cfg); err != nil {
		return err
	}
	c.inst.cfg.Store(cfg)
	return nil
}

// healthz serves the probes of the manifest: /healthz answers once the first
// configuration is loaded.
func (c *ingController) healthz(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		ok := c.healthy
		c.mu.Unlock()
		if !ok {
			http.Error(w, "nginx is not running yet", http.StatusInternalServerError)
			return
		}
		io.WriteString(w, "ok")
	})
	serveHTTP(ctx, c.hc, "nginx-ingress-controller", mux, ":"+strconv.Itoa(c.f.healthzPort))
}

// lead holds the Lease of --election-id: only the leader writes the status
// of the Ingresses.
func (c *ingController) lead(ctx context.Context) {
	name := c.f.electionID
	c.log.printf('I', "leaderelection.go:257", fmt.Sprintf("attempting to acquire leader lease %s/%s...", c.podNS, name))
	leases := c.client.CoordinationV1().Leases(c.podNS)
	const duration = 30
	for {
		now := metav1.NewMicroTime(time.Now())
		lease, err := leases.Get(ctx, name, metav1.GetOptions{})
		holder := ""
		switch {
		case apierrors.IsNotFound(err):
			lease = &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.podNS}, Spec: coordinationv1.LeaseSpec{
				HolderIdentity: &c.podName, LeaseDurationSeconds: ptrInt32(duration), AcquireTime: &now, RenewTime: &now, LeaseTransitions: ptrInt32(0)}}
			if _, err = leases.Create(ctx, lease, metav1.CreateOptions{}); err == nil {
				holder = c.podName
			}
		case err == nil:
			holder = ptrString(lease.Spec.HolderIdentity)
			expired := lease.Spec.RenewTime == nil || time.Since(lease.Spec.RenewTime.Time) > time.Duration(ptrInt32Value(lease.Spec.LeaseDurationSeconds, duration))*time.Second
			if holder == c.podName || expired || holder == "" {
				if holder != c.podName {
					lease.Spec.AcquireTime = &now
					lease.Spec.LeaseTransitions = ptrInt32(ptrInt32Value(lease.Spec.LeaseTransitions, 0) + 1)
				}
				lease.Spec.HolderIdentity, lease.Spec.RenewTime, lease.Spec.LeaseDurationSeconds = &c.podName, &now, ptrInt32(duration)
				if _, err = leases.Update(ctx, lease, metav1.UpdateOptions{}); err == nil {
					holder = c.podName
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		was := c.leader
		c.leader = holder == c.podName
		now2 := c.leader
		c.mu.Unlock()
		if now2 && !was {
			c.log.printf('I', "leaderelection.go:271", fmt.Sprintf("successfully acquired lease %s/%s", c.podNS, name))
			c.log.info("status.go:85", "New leader elected", "identity", c.podName)
			c.trigger()
		}
		if err != nil && !apierrors.IsConflict(err) {
			c.log.printf('E', "leaderelection.go:436", fmt.Sprintf("error retrieving resource lock %s/%s: %v", c.podNS, name, err))
		}
		if !sleepCtx(ctx, 10*time.Second) {
			return
		}
	}
}

func ptrInt32(v int32) *int32 { return &v }

func ptrInt32Value(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

// statusAddresses is the address the Ingresses get, looked up at most every
// 30 seconds.
func (c *ingController) statusAddresses(ctx context.Context) []netv1.IngressLoadBalancerIngress {
	if c.addrs != nil && time.Since(c.addrsAt) < 30*time.Second {
		return c.addrs
	}
	var addrs []netv1.IngressLoadBalancerIngress
	switch {
	case c.f.publishStatusAddress != "":
		addrs = statusFromAddresses(c.f.publishStatusAddress)
	case c.f.publishService != "":
		ns, name, ok := strings.Cut(c.f.publishService, "/")
		if !ok {
			ns, name = c.podNS, c.f.publishService
		}
		svc, err := c.client.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			c.log.error("status.go:239", "Error getting Service", "err", err.Error(), "service", c.f.publishService)
			return nil
		}
		addrs = statusFromService(svc)
	default:
		// The nodes that run controller pods: the pods with the labels of
		// this one.
		sel := map[string]string{}
		if c.self != nil {
			for k, v := range c.self.Labels {
				if k != "pod-template-hash" && k != "controller-revision-hash" && k != "pod-template-generation" {
					sel[k] = v
				}
			}
		}
		pods, err := c.client.CoreV1().Pods(c.podNS).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(sel).String()})
		if err != nil {
			c.log.error("status.go:265", "Error listing pods", "err", err.Error())
			return nil
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName == "" || p.DeletionTimestamp != nil || p.Status.Phase != v1.PodRunning {
				continue
			}
			node, err := c.client.CoreV1().Nodes().Get(ctx, p.Spec.NodeName, metav1.GetOptions{})
			if err != nil {
				continue
			}
			if a := nodeAddress(node, c.f.reportNodeInternalIP); a != "" {
				addrs = append(addrs, ingressLB(a))
			}
		}
		addrs = sortLB(addrs)
	}
	c.addrs, c.addrsAt = addrs, time.Now()
	return addrs
}

func (c *ingController) updateStatus(ctx context.Context, owned []*netv1.Ingress) {
	addrs := c.statusAddresses(ctx)
	if addrs == nil {
		return
	}
	for _, ing := range owned {
		cur := ing.Status.LoadBalancer.Ingress
		if sameLB(sortLB(cur), addrs) {
			continue
		}
		curJSON := "null"
		if len(cur) > 0 {
			b, _ := json.Marshal(cur)
			curJSON = string(b)
		}
		newJSON, _ := json.Marshal(addrs)
		c.log.info("status.go:304", "updating Ingress status", "namespace", ing.Namespace, "ingress", ing.Name, "currentValue", rawJSON(curJSON), "newValue", rawJSON(newJSON))
		upd := ing.DeepCopy()
		upd.Status.LoadBalancer.Ingress = addrs
		if _, err := c.client.NetworkingV1().Ingresses(ing.Namespace).UpdateStatus(ctx, upd, metav1.UpdateOptions{}); err != nil {
			c.log.warn("status.go:323", "error updating ingress rule", "err", err.Error())
		}
	}
}
