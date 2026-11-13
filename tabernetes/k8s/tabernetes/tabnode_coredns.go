package main

// CoreDNS on the tab node. The Corefile of kube-system/coredns applies to the
// names of the tab (hosts and rewrite name; the rest has no meaning in a
// browser, which has no DNS), the built-in image registry.k8s.io/coredns/
// coredns runs as a pod with logs, health, ready, metrics and DNS over TCP,
// and a tab CoreDNS pod leaves the node once a Docker node can run the real
// one (pods there need UDP).

import (
	"bufio"
	"context"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"mvdan.cc/sh/v3/interp"
)

const (
	corednsVersion = "1.15.0"
	corednsGo      = "go1.25.4"
	corednsCommit  = "e4f0a9d"
)

// Registered here and not in tabnode_images.go. Nothing reads these before
// every init has run.
func init() {
	imagePrograms["coredns"] = corednsProg
	imageDefs = append(imageDefs, &imageDef{
		img:   builtinImage{name: "coredns", entrypoint: []string{"/coredns"}},
		repos: []string{"coredns/coredns", "coredns"}, short: true,
		registry: "registry.k8s.io", tag: "v" + corednsVersion, size: 22_000_000,
	})
}

// --- Corefile ---

// cfNode is a line of a Corefile: args[0] is the directive (or, at the top
// level, the first key of a server block), kids are the lines of its block.
type cfNode struct {
	args []string
	kids []*cfNode
}

// cfTokens splits a line into words: quotes group, # starts a comment.
func cfTokens(line string) ([]string, error) {
	var toks []string
	var cur strings.Builder
	has, quoted := false, false
	flush := func() {
		if has {
			toks = append(toks, cur.String())
		}
		cur.Reset()
		has = false
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quoted:
			if c == '"' {
				quoted = false
			} else {
				cur.WriteByte(c)
			}
		case c == '"':
			quoted, has = true, true
		case c == '#' && !has:
			flush()
			return toks, nil
		case c == ' ' || c == '\t' || c == '\r':
			flush()
		default:
			cur.WriteByte(c)
			has = true
		}
	}
	if quoted {
		return nil, errors.New("unterminated quote")
	}
	flush()
	return toks, nil
}

func parseCorefileTree(text string) (*cfNode, error) {
	root := &cfNode{}
	stack := []*cfNode{root}
	for i, line := range strings.Split(text, "\n") {
		toks, err := cfTokens(line)
		if err != nil {
			return nil, fmt.Errorf("Corefile:%d: %v", i+1, err)
		}
		if len(toks) == 0 {
			continue
		}
		if toks[0] == "}" {
			if len(toks) > 1 || len(stack) == 1 {
				return nil, fmt.Errorf("Corefile:%d: unexpected }", i+1)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		open := toks[len(toks)-1] == "{"
		if open {
			toks = toks[:len(toks)-1]
		}
		if len(toks) == 0 {
			return nil, fmt.Errorf("Corefile:%d: unexpected {", i+1)
		}
		if len(stack) == 1 && !open {
			return nil, fmt.Errorf("Corefile:%d: %q is outside a server block", i+1, toks[0])
		}
		n := &cfNode{args: toks}
		top := stack[len(stack)-1]
		top.kids = append(top.kids, n)
		if open {
			stack = append(stack, n)
		}
	}
	if len(stack) > 1 {
		return nil, errors.New("Corefile: missing }")
	}
	return root, nil
}

// rewriteRule is "rewrite name [exact|prefix|suffix|substring|regex] FROM TO".
type rewriteRule struct {
	kind     string
	from, to string
	re       *regexp.Regexp
	stop     bool
}

func (r *rewriteRule) apply(name string) (string, bool) {
	switch r.kind {
	case "exact":
		if name == r.from {
			return r.to, true
		}
	case "prefix":
		if rest, ok := strings.CutPrefix(name, r.from); ok {
			return r.to + rest, true
		}
	case "suffix":
		if rest, ok := strings.CutSuffix(name, r.from); ok {
			return rest + r.to, true
		}
	case "substring":
		if strings.Contains(name, r.from) {
			return strings.Replace(name, r.from, r.to, 1), true
		}
	case "regex":
		// CoreDNS matches the name with its trailing dot.
		if r.re.MatchString(name + ".") {
			return strings.TrimSuffix(r.re.ReplaceAllString(name+".", r.to), "."), true
		}
	}
	return name, false
}

// corefile is what the tab uses of a Corefile.
type corefile struct {
	keys    []string            // the keys of all server blocks, as written
	plugins map[string][]string // arguments of the plugins of the block for zone "."
	hosts   map[string][]string // name -> IPv4 addresses
	rewrite []*rewriteRule
}

var regexGroup = regexp.MustCompile(`\{(\d+)\}`)

func blockZone(key string) string {
	key = strings.TrimPrefix(key, "dns://")
	if i := strings.LastIndex(key, ":"); i >= 0 {
		if _, err := fmt.Sscanf(key[i+1:], "%d", new(int)); err == nil {
			key = key[:i]
		}
	}
	return key
}

func parseCorefile(text string) (*corefile, error) {
	root, err := parseCorefileTree(text)
	if err != nil {
		return nil, err
	}
	cf := &corefile{plugins: map[string][]string{}, hosts: map[string][]string{}}
	var block *cfNode
	for _, b := range root.kids {
		for _, k := range b.args {
			k = strings.TrimSuffix(k, ",")
			if k == "" {
				continue
			}
			cf.keys = append(cf.keys, k)
			if block == nil && blockZone(k) == "." {
				block = b
			}
		}
	}
	if block == nil {
		return cf, nil
	}
	for _, d := range block.kids {
		if _, ok := cf.plugins[d.args[0]]; !ok {
			cf.plugins[d.args[0]] = d.args[1:]
		}
		switch d.args[0] {
		case "hosts":
			// Only inline entries: the tab has no /etc/hosts of the node
			// to read, and nothing re-reads a file.
			for _, e := range d.kids {
				if ip := net.ParseIP(e.args[0]); ip != nil && ip.To4() != nil {
					for _, name := range e.args[1:] {
						name = strings.TrimSuffix(strings.ToLower(name), ".")
						cf.hosts[name] = append(cf.hosts[name], ip.String())
					}
				}
			}
		case "rewrite":
			a := d.args[1:]
			stop := true
			if len(a) > 0 && (a[0] == "stop" || a[0] == "continue") {
				stop = a[0] == "stop"
				a = a[1:]
			}
			if len(a) < 3 || a[0] != "name" {
				continue // type, class, ttl, edns0, answer: nothing to do here
			}
			a = a[1:]
			r := &rewriteRule{kind: "exact", stop: stop}
			switch len(a) {
			case 2:
			case 3:
				r.kind, a = a[0], a[1:]
				if r.kind != "exact" && r.kind != "prefix" && r.kind != "suffix" && r.kind != "substring" && r.kind != "regex" {
					return nil, fmt.Errorf("Corefile: rewrite name: unknown match type %q", r.kind)
				}
			default:
				return nil, fmt.Errorf("Corefile: rewrite name: wrong number of arguments")
			}
			r.from, r.to = strings.ToLower(a[0]), strings.ToLower(a[1])
			if r.kind == "regex" {
				if r.re, err = regexp.Compile(r.from); err != nil {
					return nil, fmt.Errorf("Corefile: rewrite name: %v", err)
				}
				r.to = regexGroup.ReplaceAllString(r.to, "$${$1}")
			} else {
				r.from, r.to = strings.TrimSuffix(r.from, "."), strings.TrimSuffix(r.to, ".")
			}
			cf.rewrite = append(cf.rewrite, r)
		}
	}
	return cf, nil
}

// name applies the rewrites and then the hosts of the Corefile to a fully
// qualified name; ips is empty when hosts does not know it.
func (cf *corefile) name(name string) (string, []string) {
	if cf == nil {
		return name, nil
	}
	for _, r := range cf.rewrite {
		if n, ok := r.apply(name); ok {
			name = n
			if r.stop {
				break
			}
		}
	}
	return name, cf.hosts[name]
}

// corefileState is the Corefile of kube-system/coredns as the tab saw it
// last. cf is the last Corefile that parsed; err is why text did not.
type corefileState struct {
	text, hash string
	cf         *corefile
	err        error
}

func corefileHash(text string) string {
	sum := sha512.Sum512([]byte(text))
	return hex.EncodeToString(sum[:])
}

func (s *serviceProxy) setCorefile(text string) {
	old := s.corefile.Load()
	if old != nil && old.text == text {
		return
	}
	st := &corefileState{text: text, hash: corefileHash(text)}
	st.cf, st.err = parseCorefile(text)
	if st.err != nil && old != nil {
		st.cf = old.cf // a broken Corefile keeps the old rules
	}
	s.corefile.Store(st)
}

// watchCorefile follows the ConfigMap coredns. A factory of its own keeps
// the cache to this one object.
func (s *serviceProxy) watchCorefile(ctx context.Context) {
	factory := informers.NewSharedInformerFactoryWithOptions(s.client, 0,
		informers.WithNamespace(metav1.NamespaceSystem),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.FieldSelector = fields.OneTermEqualSelector("metadata.name", "coredns").String()
		}))
	update := func(obj any) {
		if cm, ok := obj.(*v1.ConfigMap); ok {
			s.setCorefile(cm.Data["Corefile"])
		}
	}
	factory.Core().V1().ConfigMaps().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    update,
		UpdateFunc: func(_, obj any) { update(obj) },
		DeleteFunc: func(any) { s.corefile.Store(nil) },
	})
	factory.Start(ctx.Done())
}

// lookupName answers one fully qualified name: the Corefile first (rewrite,
// hosts), then the services.
func (s *serviceProxy) lookupName(name string) (dnsAnswer, bool) {
	chargeDNS()
	if st := s.corefile.Load(); st != nil {
		var ips []string
		if name, ips = st.cf.name(name); len(ips) > 0 {
			return dnsAnswer{name: name, ips: ips}, true
		}
	}
	return s.lookupFQDN(name, 0)
}

// --- DNS over TCP ---

type corednsStats struct {
	mu        sync.Mutex
	req, resp map[string]uint64
}

func (st *corednsStats) count(typ, rcode string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.req == nil {
		st.req, st.resp = map[string]uint64{}, map[string]uint64{}
	}
	st.req[typ]++
	st.resp[rcode]++
}

func (st *corednsStats) metrics() string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "# HELP coredns_build_info A metric with a constant '1' value labeled by version, revision, and goversion from which CoreDNS was built.\n# TYPE coredns_build_info gauge\ncoredns_build_info{goversion=%q,revision=%q,version=%q} 1\n", corednsGo, corednsCommit, corednsVersion)
	b.WriteString("# HELP coredns_dns_requests_total Counter of DNS requests made per zone, protocol and family.\n# TYPE coredns_dns_requests_total counter\n")
	for _, k := range sortedKeys(st.req) {
		fmt.Fprintf(&b, "coredns_dns_requests_total{family=\"1\",proto=\"tcp\",server=\"dns://:53\",type=%q,zone=\".\"} %d\n", k, st.req[k])
	}
	b.WriteString("# HELP coredns_dns_responses_total Counter of response status codes.\n# TYPE coredns_dns_responses_total counter\n")
	for _, k := range sortedKeys(st.resp) {
		fmt.Fprintf(&b, "coredns_dns_responses_total{plugin=\"\",rcode=%q,server=\"dns://:53\",zone=\".\"} %d\n", k, st.resp[k])
	}
	return b.String()
}

func sortedKeys(m map[string]uint64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// answerDNS answers one query message with what the service proxy knows. The
// query is a FQDN, so the search path of the client is not applied again.
func (s *serviceProxy) answerDNS(req []byte, st *corednsStats) ([]byte, error) {
	var p dnsmessage.Parser
	h, err := p.Start(req)
	if err != nil {
		return nil, err
	}
	q, err := p.Question()
	if err != nil {
		return nil, err
	}
	name := strings.ToLower(q.Name.String())
	bare := strings.TrimSuffix(name, ".")
	resp := dnsmessage.Header{ID: h.ID, Response: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true,
		Authoritative: bare == clusterDomain || strings.HasSuffix(bare, "."+clusterDomain) || strings.HasSuffix(bare, ".in-addr.arpa")}
	rhdr := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 30}
	var answers []dnsmessage.Resource
	switch {
	case h.OpCode != 0:
		resp.RCode = dnsmessage.RCodeNotImplemented
	case q.Type == dnsmessage.TypePTR && strings.HasSuffix(bare, ".in-addr.arpa"):
		parts := strings.Split(strings.TrimSuffix(bare, ".in-addr.arpa"), ".")
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		target, ok := s.reverse(strings.Join(parts, "."))
		ptr, perr := dnsmessage.NewName(target + ".")
		if !ok || perr != nil {
			resp.RCode = dnsmessage.RCodeNameError
			break
		}
		rhdr.Type = dnsmessage.TypePTR
		answers = append(answers, dnsmessage.Resource{Header: rhdr, Body: &dnsmessage.PTRResource{PTR: ptr}})
	default:
		a, ok := s.lookup(name, "")
		if !ok {
			resp.RCode = dnsmessage.RCodeNameError
			break
		}
		owner := q.Name
		if a.cname != "" && (q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeAAAA || q.Type == dnsmessage.TypeCNAME) {
			target, perr := dnsmessage.NewName(a.cname + ".")
			if perr == nil {
				rhdr.Type = dnsmessage.TypeCNAME
				answers = append(answers, dnsmessage.Resource{Header: rhdr, Body: &dnsmessage.CNAMEResource{CNAME: target}})
				owner = target
			}
		}
		if q.Type == dnsmessage.TypeA {
			for _, ip := range a.ips {
				if v4 := net.ParseIP(ip).To4(); v4 != nil {
					r := dnsmessage.AResource{}
					copy(r.A[:], v4)
					answers = append(answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: owner, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 30}, Body: &r})
				}
			}
		}
	}
	rcode := map[dnsmessage.RCode]string{dnsmessage.RCodeSuccess: "NOERROR", dnsmessage.RCodeNameError: "NXDOMAIN", dnsmessage.RCodeNotImplemented: "NOTIMP"}[resp.RCode]
	st.count(strings.TrimPrefix(q.Type.String(), "Type"), rcode)
	b := dnsmessage.NewBuilder(make([]byte, 2, 512), resp)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(q); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	for _, r := range answers {
		switch body := r.Body.(type) {
		case *dnsmessage.AResource:
			err = b.AResource(r.Header, *body)
		case *dnsmessage.CNAMEResource:
			err = b.CNAMEResource(r.Header, *body)
		case *dnsmessage.PTRResource:
			err = b.PTRResource(r.Header, *body)
		}
		if err != nil {
			return nil, err
		}
	}
	msg, err := b.Finish()
	if err != nil {
		return nil, err
	}
	binary.BigEndian.PutUint16(msg, uint16(len(msg)-2))
	return msg, nil
}

// serveDNS answers queries of RFC 1035 TCP framing on ln until ctx ends.
func (s *serviceProxy) serveDNS(ctx context.Context, ln net.Listener, st *corednsStats) {
	context.AfterFunc(ctx, func() { ln.Close() })
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			r := bufio.NewReader(c)
			for {
				var n [2]byte
				if _, err := io.ReadFull(r, n[:]); err != nil {
					return
				}
				req := make([]byte, binary.BigEndian.Uint16(n[:]))
				if _, err := io.ReadFull(r, req); err != nil {
					return
				}
				chargeDNS()
				resp, err := s.answerDNS(req, st)
				if err != nil {
					return
				}
				if _, err := c.Write(resp); err != nil {
					return
				}
			}
		}()
	}
}

// --- the image ---

// corednsProg is registry.k8s.io/coredns/coredns: the log and the endpoints
// of the real one, answering from the service proxy of the tab.
func corednsProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	version := corednsVersion
	if t := strings.TrimPrefix(imageTag(ctx), "v"); fullVersion.MatchString(t) {
		version = t
	}
	fs := flag.NewFlagSet("coredns", flag.ContinueOnError)
	fs.SetOutput(hc.Stderr)
	conf := fs.String("conf", "Corefile", "Corefile to load")
	ver := fs.Bool("version", false, "show version")
	fs.Bool("quiet", false, "quiet mode (no initialization output)")
	fs.String("dns.port", "53", "default port")
	if err := fs.Parse(args); err != nil {
		return interp.ExitStatus(2)
	}
	banner := fmt.Sprintf("CoreDNS-%s\nlinux/amd64, %s, %s\n", version, corednsGo, corednsCommit)
	if *ver {
		fmt.Fprint(hc.Stdout, banner)
		return nil
	}
	var proxy *serviceProxy
	if theTabNode != nil {
		proxy = theTabNode.net
	}
	if proxy == nil {
		fmt.Fprintln(hc.Stderr, "coredns: the service proxy is not running")
		return interp.ExitStatus(1)
	}
	// The mounted file is what the kubelet put there; later changes come
	// from the ConfigMap (the volume is not refreshed on the tab).
	text := ""
	if st := proxy.corefile.Load(); st != nil && st.err == nil {
		text = st.text
	} else if b, err := os.ReadFile(realPath(ctx, hc.Dir, *conf)); err == nil {
		text = string(b)
	} else {
		fmt.Fprintf(hc.Stderr, "Corefile: %v\n", unwrap(err))
		return interp.ExitStatus(1)
	}
	cf, err := parseCorefile(text)
	if err != nil {
		fmt.Fprintln(hc.Stderr, err)
		return interp.ExitStatus(1)
	}
	log := newLogger(hc.Stdout, 0)
	for _, k := range cf.keys {
		log.Println(k)
	}
	hash := corefileHash(text)
	log.Printf("[INFO] plugin/reload: Running configuration SHA512 = %s", hash)
	fmt.Fprint(hc.Stdout, banner)

	defer addCoreDNS(acctFrom(ctx))()
	stats := &corednsStats{}
	errs := make(chan error, 4)
	serve := func(addr string, h http.Handler) bool {
		ln, err := listenPod(ctx, addr)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "listen tcp %s: %v\n", addr, unwrap(err))
			return false
		}
		go func() { errs <- serveListeners(ctx, hc, "coredns", h, ln) }()
		return true
	}
	text200 := func(body func() string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body()) })
	}
	// Like the real one, only the plugins of the Corefile open their ports.
	for _, ep := range []struct{ plugin, def string }{
		{"health", ":8080"}, {"ready", ":8181"}, {"prometheus", ":9153"},
	} {
		pargs, ok := cf.plugins[ep.plugin]
		if !ok {
			continue
		}
		addr := ep.def
		if len(pargs) > 0 && strings.Contains(pargs[0], ":") {
			addr = pargs[0]
		}
		var h http.Handler
		switch ep.plugin {
		case "health":
			mux := http.NewServeMux()
			mux.Handle("/health", text200(func() string { return "OK" }))
			h = mux
		case "ready":
			mux := http.NewServeMux()
			mux.Handle("/ready", text200(func() string { return "OK" }))
			h = mux
		default:
			mux := http.NewServeMux()
			mux.Handle("/metrics", text200(stats.metrics))
			h = mux
		}
		if !serve(addr, h) {
			return interp.ExitStatus(1)
		}
	}
	ln, err := listenPod(ctx, ":53")
	if err != nil {
		fmt.Fprintf(hc.Stderr, "listen tcp :53: %v\n", unwrap(err))
		return interp.ExitStatus(1)
	}
	go func() { proxy.serveDNS(ctx, ln, stats); errs <- nil }()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errs:
			if err == nil && ctx.Err() != nil {
				return nil
			}
			return interp.ExitStatus(1)
		case <-tick.C:
			st := proxy.corefile.Load()
			if st == nil || st.hash == hash {
				continue
			}
			hash = st.hash
			log.Println("[INFO] Reloading")
			if st.err != nil {
				log.Printf("[ERROR] Restart failed: %v", st.err)
				continue
			}
			log.Printf("[INFO] plugin/reload: Running configuration SHA512 = %s", hash)
			log.Println("[INFO] Reloading complete")
		}
	}
}

// --- handover to Docker nodes ---

const (
	handoverEvery    = 10 * time.Second
	handoverCooldown = 2 * time.Minute
)

// corednsEvictions is the decision of the handover: once a Docker node is
// Ready and schedulable, the CoreDNS pods of this node should go there, where
// CoreDNS speaks UDP. After an eviction it waits, so a scheduler that puts the
// new pod back on the tab does not start a loop.
func corednsEvictions(nodes []*v1.Node, pods []*v1.Pod, self string, now, last time.Time) []*v1.Pod {
	if !last.IsZero() && now.Sub(last) < handoverCooldown {
		return nil
	}
	docker := false
	for _, n := range nodes {
		if _, tab := n.Labels[tabTaintKey]; tab || n.Spec.Unschedulable {
			continue
		}
		for _, c := range n.Status.Conditions {
			if c.Type == v1.NodeReady && c.Status == v1.ConditionTrue {
				docker = true
			}
		}
	}
	if !docker {
		return nil
	}
	var out []*v1.Pod
	for _, p := range pods {
		if p.Namespace == metav1.NamespaceSystem && p.Labels["k8s-app"] == "kube-dns" && p.Spec.NodeName == self && p.DeletionTimestamp == nil {
			out = append(out, p)
		}
	}
	return out
}

// handoverCoreDNS runs on each tab node and evicts its CoreDNS pods when a
// Docker node can take over.
func (n *tabNode) handoverCoreDNS(ctx context.Context) {
	var last time.Time
	tick := time.NewTicker(handoverEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if !last.IsZero() && time.Since(last) < handoverCooldown {
			continue
		}
		pods, err := n.admin.CoreV1().Pods(metav1.NamespaceSystem).List(ctx, metav1.ListOptions{
			LabelSelector: "k8s-app=kube-dns",
			FieldSelector: fields.OneTermEqualSelector("spec.nodeName", tabNodeName).String(),
		})
		if err != nil || len(pods.Items) == 0 {
			continue
		}
		nodes, err := n.admin.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		var nl []*v1.Node
		for i := range nodes.Items {
			nl = append(nl, &nodes.Items[i])
		}
		var pl []*v1.Pod
		for i := range pods.Items {
			pl = append(pl, &pods.Items[i])
		}
		for _, p := range corednsEvictions(nl, pl, tabNodeName, time.Now(), last) {
			// The Eviction API respects PodDisruptionBudgets.
			err := n.admin.CoreV1().Pods(p.Namespace).EvictV1(ctx, &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{Name: p.Name, Namespace: p.Namespace}})
			if err != nil {
				fmt.Fprintf(os.Stderr, "tabernetes: evicting %s/%s for a Docker node: %v\n", p.Namespace, p.Name, err)
				continue
			}
			last = time.Now()
		}
	}
}
