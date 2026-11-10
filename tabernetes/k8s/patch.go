// Command patch prepares a Kubernetes checkout for GOOS=js GOARCH=wasm:
//
//	go run . <kubernetes checkout>
//
// kube-apiserver, kube-controller-manager, kube-scheduler and kubectl then
// build and run in the browser. The command
//
//   - excludes unix-only files from js with build tags,
//   - copies _overlay/ (js stubs and helpers) into the tree,
//   - applies a few content patches,
//   - vendors mvdan.cc/sh (the terminal's shell) at MVDAN_SH_VERSION,
//   - vendors the engine of sigs.k8s.io/kube-network-policies at
//     KUBE_NETWORK_POLICIES_VERSION,
//   - vendors Helm (helm.sh/helm/v4) at HELM_VERSION with every module and
//     package it needs that Kubernetes does not vendor already (vendorGraph):
//     modules both use stay at the version of Kubernetes, and
//   - copies tabernetes/ (the main package) to cmd/tabernetes.
//
// Every patch fails when its target is gone. A Kubernetes update shows exactly
// what needs attention.
package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"go/build/constraint"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

//go:embed all:_overlay all:tabernetes
var files embed.FS

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run . <kubernetes checkout>")
		os.Exit(2)
	}
	if err := os.Chdir(os.Args[1]); err != nil {
		fail("%v", err)
	}

	// Exclude unix-only files from js. The js variant comes from _overlay/ or
	// is a copy of the windows variant.
	for _, f := range []string{
		"vendor/github.com/coreos/go-systemd/v22/journal/journal_unix.go",
		"vendor/go.etcd.io/etcd/client/pkg/v3/fileutil/lock_flock.go",
		"vendor/go.etcd.io/etcd/client/pkg/v3/fileutil/lock_unix.go",
		"vendor/github.com/moby/term/term_unix.go",
		"vendor/github.com/moby/term/termios_unix.go",
		"vendor/github.com/moby/term/termios_nonbsd.go",
		"pkg/probe/dialer_others.go",
	} {
		buildTag(f, "(%s) && !js")
	}
	for unix, windows := range map[string]string{
		"vendor/github.com/coreos/go-systemd/v22/journal/journal_unix.go": "vendor/github.com/coreos/go-systemd/v22/journal/journal_windows.go",
		"staging/src/k8s.io/apiserver/pkg/server/options/serving_unix.go": "staging/src/k8s.io/apiserver/pkg/server/options/serving_windows.go",
		"staging/src/k8s.io/kubectl/pkg/util/umask.go":                    "staging/src/k8s.io/kubectl/pkg/util/umask_windows.go",
		"pkg/scheduler/backend/cache/debugger/signal.go":                  "pkg/scheduler/backend/cache/debugger/signal_windows.go",
		"staging/src/k8s.io/kubectl/pkg/util/term/resizeevents.go":        "staging/src/k8s.io/kubectl/pkg/util/term/resizeevents_windows.go",
	} {
		jsFromWindows(unix, windows)
	}
	// kubectl edit: no editor process in js. _overlay/ has a launcher that
	// runs the editor in-process (the vi of cmd/tabernetes).
	buildTag("staging/src/k8s.io/kubectl/pkg/cmd/util/editor/launcher_others.go", "(%s) && !js")

	// The plain file system helpers work in js as well.
	buildTag("pkg/util/filesystem/util_unix.go", "%s || js")

	copyTree("_overlay", ".")

	// mount-utils: move the unix syscalls into mount_helper_sys_{unix,js}.go.
	edit("staging/src/k8s.io/mount-utils/mount_helper_unix.go",
		"\t\terr = syscall.Access(path, syscall.F_OK)\n\t\tif err == nil {",
		"\t\tif accessExists(path) {",
		"\t\tuname := unix.Utsname{}\n\t\terr := unix.Uname(&uname)\n\t\tif err != nil {\n\t\t\treturn\n\t\t}\n\n\t\tend := bytes.IndexByte(uname.Release[:], 0)\n\t\tv := bytes.SplitN(uname.Release[:end]",
		"\t\trelease, ok := unameRelease()\n\t\tif !ok {\n\t\t\treturn\n\t\t}\n\n\t\tv := bytes.SplitN(release",
		"\t\"golang.org/x/sys/unix\"\n", "",
	)

	// kubectl: no SIGHUP/SIGQUIT in js, and no syscall.Exec for plugins.
	edit("staging/src/k8s.io/kubectl/pkg/util/interrupt/interrupt.go",
		"syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT",
		"syscall.SIGINT, syscall.SIGTERM",
	)
	edit("staging/src/k8s.io/kubectl/pkg/cmd/plugin.go",
		"return syscall.Exec(executablePath,",
		"return syscallExec(executablePath,",
	)
	dropUnusedImport("staging/src/k8s.io/kubectl/pkg/cmd/plugin.go", "syscall")

	// RSA key generation is painfully slow in single-threaded wasm: use ECDSA
	// for self-signed certificates.
	edit("staging/src/k8s.io/client-go/util/cert/cert.go",
		"rsa.GenerateKey(cryptorand.Reader, 2048)",
		"ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)",
		"\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.RSAPrivateKeyBlockType, Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {",
		"\tprivDER, err := x509.MarshalECPrivateKey(priv)\n\tif err != nil {\n\t\treturn nil, nil, err\n\t}\n\tif err := pem.Encode(&keyBuffer, &pem.Block{Type: keyutil.ECPrivateKeyBlockType, Bytes: privDER}); err != nil {",
		"\t\"crypto/rsa\"\n",
		"\t\"crypto/ecdsa\"\n\t\"crypto/elliptic\"\n",
	)

	// wasm has no preemption: give the informer a chance to catch up instead of
	// busy-looping.
	edit("pkg/registry/flowcontrol/ensurer/strategy.go",
		"\t\tklog.V(5).InfoS(fmt.Sprintf(\"Something created the %T concurrently\", bootstrap), \"type\", configurationType, \"name\", name)\n\t}",
		"\t\tklog.V(5).InfoS(fmt.Sprintf(\"Something created the %T concurrently\", bootstrap), \"type\", configurationType, \"name\", name)\n\t\ttime.Sleep(100 * time.Millisecond)\n\t}",
	)
	addImport("pkg/registry/flowcontrol/ensurer/strategy.go", "time")

	// All components run in one process: make the signal handler setup
	// idempotent.
	edit("staging/src/k8s.io/apiserver/pkg/server/signal.go",
		"func SetupSignalContext() context.Context {\n\tclose(onlyOneSignalHandler) // panics when called twice\n",
		"func SetupSignalContext() context.Context {\n\tsignalOnce.Do(func() { signalCtx = setupSignalContext() })\n\treturn signalCtx\n}\n\nvar (\n\tsignalOnce sync.Once\n\tsignalCtx  context.Context\n)\n\nfunc setupSignalContext() context.Context {\n\tclose(onlyOneSignalHandler) // panics when called twice\n",
		"\t\"os/signal\"\n",
		"\t\"os/signal\"\n\t\"sync\"\n",
	)

	// js has no fsnotify. The certificate and CA file watchers retry every
	// minute and log an error each time. The periodic check before the watch
	// still reloads the files, so skip the watch quietly.
	for _, f := range []string{
		"staging/src/k8s.io/apiserver/pkg/server/dynamiccertificates/dynamic_cafile_content.go",
		"staging/src/k8s.io/apiserver/pkg/server/dynamiccertificates/dynamic_serving_content.go",
	} {
		edit(f,
			"\tw, err := fsnotify.NewWatcher()\n\tif err != nil {\n\t\treturn fmt.Errorf(\"error creating fsnotify watcher: %v\", err)\n\t}",
			"\tw, err := fsnotify.NewWatcher()\n\tif err != nil {\n\t\treturn nil // tabernetes: no fsnotify in js\n\t}",
		)
	}

	// The proxy subresources of services and pods, webhooks and aggregated
	// APIs dial from the apiserver. Without a dialer, net/http in js uses
	// fetch(), which cannot reach the in-process network of the pods.
	edit("pkg/controlplane/apiserver/config.go",
		"\tvar proxyDialerFn utilnet.DialFunc\n",
		"\tproxyDialerFn := utilnet.DialFunc(proxyDial) // tabernetes\n",
	)
	write("pkg/controlplane/apiserver/tabernetes.go", `package apiserver

import (
	"context"
	"net"
)

// proxyDial dials pods and services in the in-process network of the wasm
// for the apiserver. Added by tabernetes (k8s/patch.go).
func proxyDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}
`)

	// The shell of the terminal. Its only other dependencies, golang.org/x/sys
	// and golang.org/x/term, are vendored by Kubernetes already.
	vendorModule("mvdan.cc/sh/v3", os.Getenv("MVDAN_SH_VERSION"),
		"expand", "fileutil", "internal", "interp", "pattern", "syntax")

	// jq, yq and awk of the shell (tabernetes/coreutils_jq.go, coreutils_awk.go).
	vendorModule("github.com/itchyny/gojq", os.Getenv("GOJQ_VERSION"), "")
	vendorModule("github.com/itchyny/timefmt-go", os.Getenv("TIMEFMT_GO_VERSION"), "")
	vendorModule("github.com/benhoyt/goawk", os.Getenv("GOAWK_VERSION"),
		"interp", "parser", "lexer", "internal/ast", "internal/compiler", "internal/resolver")

	// The NetworkPolicy engine of the tab nodes (tabernetes/tabnode_netpol.go,
	// docs/networking-policy.md): its decision packages, not its nftables
	// dataplane. Kubernetes vendors their other dependencies already.
	vendorModule("sigs.k8s.io/kube-network-policies", os.Getenv("KUBE_NETWORK_POLICIES_VERSION"),
		"pkg/api", "pkg/network", "pkg/networkpolicy", "pkg/podinfo", "plugins/npa-v1alpha2")
	// ClusterNetworkPolicy: its types, clientset, informers and listers.
	vendorModule("sigs.k8s.io/network-policy-api", os.Getenv("NETWORK_POLICY_API_VERSION"),
		"apis/v1alpha1", "apis/v1alpha2",
		"pkg/client/applyconfiguration/apis/v1alpha1", "pkg/client/applyconfiguration/apis/v1alpha2",
		"pkg/client/applyconfiguration/internal",
		"pkg/client/clientset/versioned", "pkg/client/clientset/versioned/scheme",
		"pkg/client/clientset/versioned/typed/apis/v1alpha1", "pkg/client/clientset/versioned/typed/apis/v1alpha2",
		"pkg/client/clientset/versioned/fake",
		"pkg/client/clientset/versioned/typed/apis/v1alpha1/fake", "pkg/client/clientset/versioned/typed/apis/v1alpha2/fake",
		"pkg/client/applyconfiguration",
		"pkg/client/informers/externalversions", "pkg/client/informers/externalversions/internalinterfaces",
		"pkg/client/informers/externalversions/apis", "pkg/client/informers/externalversions/apis/v1alpha1",
		"pkg/client/informers/externalversions/apis/v1alpha2",
		"pkg/client/listers/apis/v1alpha1", "pkg/client/listers/apis/v1alpha2")
	// netlink.go needs mdlayher/netlink and only serves pkg/dataplane and
	// pkg/dns, which are not vendored.
	remove("vendor/sigs.k8s.io/kube-network-policies/pkg/network/netlink.go")
	// syscall of js has no IPPROTO_{DSTOPTS,HOPOPTS,ROUTING,FRAGMENT,SCTP}:
	// the IANA numbers.
	edit("vendor/sigs.k8s.io/kube-network-policies/pkg/network/packet.go",
		"syscall.IPPROTO_DSTOPTS", "60",
		"syscall.IPPROTO_HOPOPTS", "0",
		"syscall.IPPROTO_ROUTING", "43",
		"syscall.IPPROTO_FRAGMENT", "44",
		"syscall.IPPROTO_SCTP", "132",
	)

	// Helm, in-process like kubectl (tabernetes/helm.go). Its module graph is
	// too big for a list of packages: vendorGraph works it out.
	vendorGraph("helm.sh/helm/v4", os.Getenv("HELM_VERSION"), "pkg/cmd", "pkg/kube", "pkg/getter")
	patchHelm()

	// A background job ("k port-forward ... &") runs with the context of its
	// command line, which ends with the line. The hook lets tabernetes give each
	// job its own context (tabernetes/jobs.go).
	edit("vendor/mvdan.cc/sh/v3/interp/runner.go",
		"\t\tgo func() {\n\t\t\tr2.Run(ctx, &st2)\n",
		"\t\tctx, done := BackgroundContext(ctx, &st2) // tabernetes\n\t\tgo func() {\n\t\t\tdefer done()\n\t\t\tr2.Run(ctx, &st2)\n",
	)
	write("vendor/mvdan.cc/sh/v3/interp/tabernetes.go", `package interp

import (
	"context"

	"mvdan.cc/sh/v3/syntax"
)

// BackgroundContext returns the context of a background job and the func
// called when the job ends. Added by tabernetes (k8s/patch.go).
var BackgroundContext = func(ctx context.Context, stmt *syntax.Stmt) (context.Context, func()) {
	return ctx, func() {}
}
`)

	// kubectl cp from a pod calls CheckErr in a goroutine of its own. Here that
	// is a panic nobody recovers, which ends the whole tab. Hand the error to
	// the reader of the pipe instead: the command reports it.
	edit("staging/src/k8s.io/kubectl/pkg/cmd/cp/cp.go",
		"\t\tdefer t.outStream.Close()\n\t\tcmdutil.CheckErr(t.o.execute(options))",
		"\t\tt.outStream.CloseWithError(t.o.execute(options)) // tabernetes: no CheckErr in a goroutine",
	)

	// port-forward and proxy serve until the process ends. Here the process is
	// the browser tab, so stop them with the command's context (Ctrl-C,
	// kill %1). port-forward has RunPortForwardContext, the command does not
	// use it.
	edit("staging/src/k8s.io/kubectl/pkg/cmd/portforward/portforward.go",
		"cmdutil.CheckErr(opts.RunPortForward())", "cmdutil.CheckErr(opts.RunPortForwardContext(cmd.Context()))",
	)
	// The status line under the terminal links every forwarded port
	// (tabernetes/forwards.go).
	edit("staging/src/k8s.io/kubectl/pkg/cmd/portforward/portforward.go",
		"\treturn fw.ForwardPorts()\n}",
		`	stopped := make(chan struct{}) // tabernetes: report the ports while they listen
	defer close(stopped)
	go func() {
		select {
		case <-opts.ReadyChannel:
		case <-stopped:
			return
		}
		ports, err := fw.GetPorts()
		if err != nil {
			return
		}
		done := OnForward(opts.Namespace, opts.PodName, ports)
		<-stopped
		done()
	}()
	return fw.ForwardPorts()
}`,
	)
	write("staging/src/k8s.io/kubectl/pkg/cmd/portforward/tabernetes.go", `package portforward

import "k8s.io/client-go/tools/portforward"

// OnForward is called when the local ports listen and returns the func called
// when they stop. Added by tabernetes (k8s/patch.go).
var OnForward = func(namespace, pod string, ports []portforward.ForwardedPort) func() {
	return func() {}
}
`)
	edit("staging/src/k8s.io/kubectl/pkg/cmd/proxy/proxy.go",
		"import (\n", "import (\n\t\"context\"\n",
		"cmdutil.CheckErr(o.RunProxy())", "cmdutil.CheckErr(o.runProxyContext(cmd.Context()))",
		"func (o ProxyOptions) RunProxy() error {\n",
		"func (o ProxyOptions) RunProxy() error { return o.runProxyContext(context.Background()) }\n\nfunc (o ProxyOptions) runProxyContext(ctx context.Context) error {\n",
		"\treturn server.ServeOnListener(l)\n",
		"\tstop := context.AfterFunc(ctx, func() { l.Close() })\n\tdefer stop()\n\tif err := server.ServeOnListener(l); ctx.Err() == nil {\n\t\treturn err\n\t}\n\treturn nil\n",
	)

	// The tabernetes main package.
	if err := os.RemoveAll("cmd/tabernetes"); err != nil {
		fail("%v", err)
	}
	copyTree("tabernetes", "cmd/tabernetes")
}

// vendorModule adds packages of a module that Kubernetes does not use to the
// vendor directory, the way "go mod vendor" would: go.mod, go.sum,
// vendor/modules.txt and the sources without tests. The package "" is the
// root package of the module.
func vendorModule(path, version string, pkgs ...string) {
	if version == "" {
		fail("vendor %s: no version set", path)
	}
	cmd := exec.Command("go", "mod", "download", "-json", path+"@"+version)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fail("vendor %s: go mod download: %v", path, err)
	}
	var mod struct{ Dir, GoMod, Sum, GoModSum string }
	if err := json.Unmarshal(out, &mod); err != nil {
		fail("vendor %s: %v", path, err)
	}
	goVersion := regexp.MustCompile(`(?m)^go (\S+)`).FindStringSubmatch(read(mod.GoMod))
	if goVersion == nil {
		fail("vendor %s: no go directive in go.mod", path)
	}

	modules := read("vendor/modules.txt")
	if strings.Contains(modules, "\n# "+path+" ") {
		fail("vendor %s: Kubernetes vendors it already, drop vendorModule", path)
	}
	entry := fmt.Sprintf("# %s %s\n## explicit; go %s\n", path, version, goVersion[1])
	for _, p := range pkgs {
		entry += strings.TrimSuffix(path+"/"+p, "/") + "\n"
	}
	// modules.txt is sorted by module path.
	at := len(modules)
	for _, m := range regexp.MustCompile(`(?m)^# (\S+) `).FindAllStringSubmatchIndex(modules, -1) {
		if modules[m[2]:m[3]] > path {
			at = m[0]
			break
		}
	}
	write("vendor/modules.txt", modules[:at]+entry+modules[at:])
	gomod := read("go.mod")
	if !strings.Contains(gomod, "\nrequire (\n") {
		fail("go.mod: patch target not found: require block")
	}
	write("go.mod", strings.Replace(gomod, "\nrequire (\n", "\nrequire (\n\t"+path+" "+version+"\n", 1))
	write("go.sum", read("go.sum")+fmt.Sprintf("%s %s %s\n%s %s/go.mod %s\n", path, version, mod.Sum, path, version, mod.GoModSum))

	dst := filepath.Join("vendor", path)
	for _, p := range pkgs {
		entries, err := os.ReadDir(filepath.Join(mod.Dir, p))
		if err != nil {
			fail("vendor %s: %v", path, err)
		}
		must(os.MkdirAll(filepath.Join(dst, p), 0o755))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(mod.Dir, p, e.Name()))
			must(err)
			// "go mod vendor" drops files with an ignore build tag as well.
			if bytes.Contains(b, []byte("//go:build ignore")) {
				continue
			}
			must(os.WriteFile(filepath.Join(dst, p, e.Name()), b, 0o644))
		}
	}
	// goawk calls it LICENSE.txt.
	for _, name := range []string{"LICENSE", "LICENSE.txt"} {
		if b, err := os.ReadFile(filepath.Join(mod.Dir, name)); err == nil {
			must(os.WriteFile(filepath.Join(dst, name), b, 0o644))
			return
		}
	}
	fail("vendor %s: no LICENSE", path)
}

// patchHelm makes Helm's command line (helm.sh/helm/v4/pkg/cmd) and its
// HTTP getter fit a process that runs one helm after the other next to the
// control plane (tabernetes/helm.go).
func patchHelm() {
	const cmd = "vendor/helm.sh/helm/v4/pkg/cmd/"
	// The settings are a package variable, read from the environment once.
	// Every helm of the terminal starts with the environment of its shell.
	// cobra.OnInitialize adds to a global list that every cobra command of
	// the process runs, kubectl included: tie it to the root command of this
	// helm instead.
	edit(cmd+"root.go", "\tcobra.OnInitialize(func() {", "\tonInitialize(cmd, func() { // tabernetes")
	// log.Fatal ends the process, which is the browser tab: panic instead,
	// runHelm recovers.
	entries, err := os.ReadDir(cmd)
	must(err)
	fatal := 0
	for _, e := range entries {
		f := cmd + e.Name()
		if e.IsDir() {
			continue
		}
		if s := read(f); strings.Contains(s, "log.Fatal(") {
			edit(f, "log.Fatal(", "logFatal(")
			if !regexp.MustCompile(`\blog\.[A-Z]`).MatchString(read(f)) {
				dropUnusedImport(f, "log")
			}
			fatal++
		}
	}
	if fatal == 0 {
		fail("%s: patch target not found: log.Fatal", cmd)
	}
	// install and upgrade stop on SIGINT, which a tab has not: Ctrl-C ends
	// the context of the command.
	edit(cmd+"install.go", "ctx := context.Background()", "ctx := Context() // tabernetes")
	edit(cmd+"upgrade.go", "ctx := context.Background()", "ctx := Context() // tabernetes")
	write(cmd+"tabernetes.go", `package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"helm.sh/helm/v4/pkg/cli"
)

// Added by tabernetes (k8s/patch.go).

// ResetSettings reads the settings from the environment again, as a new
// helm process would.
func ResetSettings() { settings = cli.New() }

// Context is the context of the running helm: install and upgrade stop with it.
var Context = context.Background

// FatalError is what log.Fatal panics with.
type FatalError string

func logFatal(v ...any) { panic(FatalError(fmt.Sprint(v...))) }

// onInitialize runs fn before the command, like cobra.OnInitialize, but for
// the commands of root only.
func onInitialize(root *cobra.Command, fn func()) {
	pre := root.PersistentPreRun
	root.PersistentPreRun = func(c *cobra.Command, args []string) {
		fn()
		if pre != nil {
			pre(c, args)
		}
	}
}
`)

	// Helm takes the Kubernetes version of "helm template" and "helm version"
	// from the version of k8s.io/client-go in the build info. Here it is a
	// staging module, v0.0.0: k8s/build.sh sets the real one.
	const version = "vendor/helm.sh/helm/v4/internal/version/"
	edit(version+"clientgo.go", "\tm := info.Deps[idx]\n\n\treturn m.Version, nil",
		"\tm := info.Deps[idx]\n\tif clientGoVersion != \"\" { // tabernetes\n\t\treturn clientGoVersion, nil\n\t}\n\n\treturn m.Version, nil")
	write(version+"tabernetes.go", `package version

// clientGoVersion is the version of k8s.io/client-go, set with -ldflags -X.
// Added by tabernetes (k8s/patch.go).
var clientGoVersion = ""
`)

	// Chart repositories: net/http of js fetches with fetch(), so a
	// repository must allow CORS. RewriteURL sends well-known ones to a copy
	// next to the page, FetchError explains a failed fetch().
	const getter = "vendor/helm.sh/helm/v4/pkg/getter/"
	edit(getter+"httpgetter.go",
		"\treq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, href, http.NoBody)",
		"\thref = RewriteURL(href) // tabernetes\n\treq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, href, http.NoBody)",
		"\tresp, err := client.Do(req)\n\tif err != nil {\n\t\treturn nil, err\n\t}",
		"\treq.Header.Del(\"User-Agent\") // tabernetes: the browser sends its own, another one needs a CORS preflight\n\tresp, err := client.Do(req)\n\tif err != nil {\n\t\treturn nil, FetchError(href, err) // tabernetes\n\t}",
	)
	write(getter+"tabernetes.go", `package getter

// Added by tabernetes (k8s/patch.go).

// RewriteURL maps the URL of a chart repository file before the download.
var RewriteURL = func(href string) string { return href }

// FetchError turns a failed download into the error helm shows.
var FetchError = func(href string, err error) error { return err }
`)

	// js has no flock(2), and the browser tab is the only process: locks in
	// the process (helm repo add locks repositories.yaml).
	buildTag("vendor/github.com/gofrs/flock/flock_others.go", "(%s) && !js")
	write("vendor/github.com/gofrs/flock/flock_js.go", `//go:build js

package flock

import "sync"

// Added by tabernetes (k8s/patch.go): js has no flock(2), and the browser
// tab is the only process, so the locks live in the process.

var (
	locksMu sync.Mutex
	locks   = map[string]*pathLock{}
)

type pathLock struct {
	cond    *sync.Cond
	writer  bool
	readers int
}

func lockOf(path string) *pathLock {
	l := locks[path]
	if l == nil {
		l = &pathLock{cond: sync.NewCond(&locksMu)}
		locks[path] = l
	}
	return l
}

func (f *Flock) Lock() error {
	_, err := f.lock(true, true)
	return err
}

func (f *Flock) RLock() error {
	_, err := f.lock(false, true)
	return err
}

func (f *Flock) TryLock() (bool, error) { return f.lock(true, false) }

func (f *Flock) TryRLock() (bool, error) { return f.lock(false, false) }

func (f *Flock) lock(exclusive, wait bool) (bool, error) {
	f.m.Lock()
	defer f.m.Unlock()
	if f.l || (!exclusive && f.r) {
		return true, nil
	}
	locksMu.Lock()
	defer locksMu.Unlock()
	l := lockOf(f.path)
	for l.writer || (exclusive && l.readers > 0) {
		if !wait {
			return false, nil
		}
		l.cond.Wait()
	}
	if exclusive {
		l.writer, f.l = true, true
	} else {
		l.readers, f.r = l.readers+1, true
	}
	return true, nil
}

func (f *Flock) Unlock() error {
	f.m.Lock()
	defer f.m.Unlock()
	if !f.l && !f.r {
		return nil
	}
	locksMu.Lock()
	l := lockOf(f.path)
	if f.l {
		l.writer = false
	} else {
		l.readers--
	}
	l.cond.Broadcast()
	locksMu.Unlock()
	f.reset()
	return nil
}
`)
}

// vendorGraph vendors the packages pkgs of path@version and everything they
// import for js/wasm, the way "go mod vendor" would, without changing what
// Kubernetes vendors already. It resolves the module graph in a temporary
// module, where the staging modules are the ones of the checkout and every
// module of vendor/modules.txt is pinned to its version there. Then it adds
//
//   - the new modules: go.mod (path itself direct, the others indirect),
//     go.sum, an "## explicit" entry in vendor/modules.txt and the files of
//     their packages and their license files, and
//   - packages that Kubernetes does not use of modules it vendors, at the
//     version of Kubernetes.
//
// The temporary module needs the module proxy, like vendorModule.
func vendorGraph(path, version string, pkgs ...string) {
	if version == "" {
		fail("vendor %s: no version set", path)
	}
	modules := read("vendor/modules.txt")
	if strings.Contains(modules, "\n# "+path+" ") {
		fail("vendor %s: Kubernetes vendors it already, drop vendorGraph", path)
	}
	// The modules of Kubernetes and their packages.
	type vendored struct {
		version, replace string
		pkgs             map[string]bool
	}
	k8s := map[string]*vendored{}
	var cur *vendored
	for _, l := range strings.Split(modules, "\n") {
		switch {
		case strings.HasPrefix(l, "# "):
			f := strings.Fields(l)
			if len(f) < 3 {
				fail("vendor/modules.txt: patch target not found: module line %q", l)
			}
			cur = &vendored{version: f[2], pkgs: map[string]bool{}}
			if len(f) == 5 && f[3] == "=>" {
				cur.replace = f[4]
			}
			k8s[f[1]] = cur
		case l != "" && !strings.HasPrefix(l, "#") && cur != nil:
			cur.pkgs[l] = true
		}
	}

	checkout, err := os.Getwd()
	must(err)
	tmp, err := os.MkdirTemp("", "tabernetes-vendor-")
	must(err)
	defer os.RemoveAll(tmp)
	gomod := fmt.Sprintf("module tabernetes-vendor\n\ngo 1.26.0\n\nrequire %s %s\n\n", path, version)
	for _, m := range slices.Sorted(maps.Keys(k8s)) {
		switch v := k8s[m]; {
		case strings.HasPrefix(v.replace, "./"): // a staging module
			gomod += fmt.Sprintf("replace %s => %s\n", m, filepath.Join(checkout, v.replace))
		case v.replace != "":
			fail("vendor %s: %s is replaced by %s, vendorGraph does not handle that", path, m, v.replace)
		default:
			gomod += fmt.Sprintf("replace %s => %s %s\n", m, m, v.version)
		}
	}
	main := "package main\n\nimport (\n"
	for _, p := range pkgs {
		main += fmt.Sprintf("\t_ %q\n", path+"/"+p)
	}
	must(os.WriteFile(filepath.Join(tmp, "go.mod"), []byte(gomod), 0o644))
	must(os.WriteFile(filepath.Join(tmp, "main.go"), []byte(main+")\n\nfunc main() {}\n"), 0o644))
	gocmd := func(env []string, args ...string) []byte {
		cmd := exec.Command("go", args...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), append([]string{"GOWORK=off", "GOFLAGS=-mod=mod"}, env...)...)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			fail("vendor %s: go %s: %v", path, strings.Join(args, " "), err)
		}
		return out
	}
	// The packages the build needs.
	type pkg struct {
		ImportPath, Dir string
		Standard        bool
		EmbedFiles      []string
		Module          *struct {
			Path, Version, Dir, GoVersion string
			Main                          bool
			Replace                       *struct{ Path, Version string }
		}
	}
	type module struct {
		version, goVersion string
		pkgs               []string
	}
	added := map[string]*module{}  // new modules
	extra := map[string][]string{} // new packages of modules Kubernetes vendors
	dec := json.NewDecoder(bytes.NewReader(gocmd([]string{"GOOS=js", "GOARCH=wasm"}, "list", "-deps", "-json", ".")))
	for dec.More() {
		var p pkg
		must(dec.Decode(&p))
		m := p.Module
		if p.Standard || m == nil || m.Main {
			continue
		}
		if v := k8s[m.Path]; v != nil {
			if v.replace != "" || v.pkgs[p.ImportPath] {
				continue // a staging module or a package Kubernetes vendors
			}
			if m.Replace == nil || m.Replace.Version != v.version {
				fail("vendor %s: %s is not pinned to %s", path, m.Path, v.version)
			}
			copyPackage(p.Dir, filepath.Join("vendor", p.ImportPath), p.EmbedFiles)
			copyLicenses(m.Dir, p.Dir, filepath.Join("vendor", m.Path))
			extra[m.Path] = append(extra[m.Path], p.ImportPath)
			continue
		}
		if m.Replace != nil {
			fail("vendor %s: %s is replaced", path, m.Path)
		}
		a := added[m.Path]
		if a == nil {
			a = &module{version: m.Version, goVersion: m.GoVersion}
			added[m.Path] = a
		}
		a.pkgs = append(a.pkgs, p.ImportPath)
		copyPackage(p.Dir, filepath.Join("vendor", p.ImportPath), p.EmbedFiles)
		copyLicenses(m.Dir, p.Dir, filepath.Join("vendor", m.Path))
	}
	if added[path] == nil {
		fail("vendor %s: no package of it is needed", path)
	}

	// vendor/modules.txt: the new packages in the list of their module, the
	// new modules in sorted position.
	for m, ps := range extra {
		head := fmt.Sprintf("\n# %s %s\n", m, k8s[m].version)
		i := strings.Index(modules, head)
		if i < 0 {
			fail("vendor/modules.txt: patch target not found: %q", head)
		}
		start := i + len(head)
		end := len(modules) // the last module
		if j := strings.Index(modules[start:], "\n# "); j >= 0 {
			end = start + j + 1
		}
		var lines []string
		for _, l := range strings.Split(strings.TrimSuffix(modules[start:end], "\n"), "\n") {
			switch {
			case strings.HasPrefix(l, "## "):
				lines = append(lines, l)
			case l != "":
				ps = append(ps, l)
			}
		}
		slices.Sort(ps)
		modules = modules[:start] + strings.Join(append(lines, ps...), "\n") + "\n" + modules[end:]
	}
	var direct, indirect string
	for _, m := range slices.Sorted(maps.Keys(added)) {
		a := added[m]
		entry := fmt.Sprintf("# %s %s\n## explicit", m, a.version)
		if a.goVersion != "" {
			entry += "; go " + a.goVersion
		}
		slices.Sort(a.pkgs)
		entry += "\n" + strings.Join(a.pkgs, "\n") + "\n"
		at := len(modules)
		for _, i := range regexp.MustCompile(`(?m)^# (\S+) `).FindAllStringSubmatchIndex(modules, -1) {
			if modules[i[2]:i[3]] > m {
				at = i[0]
				break
			}
		}
		modules = modules[:at] + entry + modules[at:]
		if m == path {
			direct += "\t" + m + " " + a.version + "\n"
		} else {
			indirect += "\t" + m + " " + a.version + " // indirect\n"
		}
	}
	write("vendor/modules.txt", modules)

	// go.mod: the direct requirement in the first require block, the
	// indirect ones in the second, like "go mod tidy" does.
	s := read("go.mod")
	blocks := strings.SplitAfterN(s, "\nrequire (\n", 3)
	if len(blocks) != 3 {
		fail("go.mod: patch target not found: two require blocks")
	}
	write("go.mod", blocks[0]+direct+blocks[1]+indirect+blocks[2])
	// go.sum: the hashes of the module and of its go.mod.
	var args []string
	for _, m := range slices.Sorted(maps.Keys(added)) {
		args = append(args, m+"@"+added[m].version)
	}
	sum := read("go.sum")
	dec = json.NewDecoder(bytes.NewReader(gocmd(nil, append([]string{"mod", "download", "-json"}, args...)...)))
	for dec.More() {
		var d struct{ Path, Version, Sum, GoModSum string }
		must(dec.Decode(&d))
		if d.Sum == "" || d.GoModSum == "" {
			fail("vendor %s: no hashes for %s %s", path, d.Path, d.Version)
		}
		sum += fmt.Sprintf("%s %s %s\n%s %s/go.mod %s\n", d.Path, d.Version, d.Sum, d.Path, d.Version, d.GoModSum)
	}
	write("go.sum", sum)
}

// copyPackage copies the files of the package in src to dst like "go mod
// vendor": everything but tests and Go files that no build includes
// ("//go:build ignore"), and the files its //go:embed patterns need. It fails
// instead of changing a file that is there already.
func copyPackage(src, dst string, embeds []string) {
	entries, err := os.ReadDir(src)
	must(err)
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasSuffix(n, "_test.go") || n == "go.mod" || n == "go.sum" {
			continue
		}
		if strings.HasSuffix(n, ".go") && ignored(filepath.Join(src, n)) {
			continue
		}
		names = append(names, n)
	}
	for _, n := range append(names, embeds...) {
		b, err := os.ReadFile(filepath.Join(src, n))
		must(err)
		target := filepath.Join(dst, n)
		if old, err := os.ReadFile(target); err == nil {
			if !bytes.Equal(old, b) {
				fail("vendor: %s is there already with other content", target)
			}
			continue
		}
		must(os.MkdirAll(filepath.Dir(target), 0o755))
		must(os.WriteFile(target, b, 0o644))
	}
}

// ignored reports whether no build includes the Go file: its build
// constraint needs the tag "ignore" whatever the other tags are.
func ignored(file string) bool {
	b, err := os.ReadFile(file)
	must(err)
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "package ") {
			return false
		}
		if !constraint.IsGoBuild(l) {
			continue
		}
		expr, err := constraint.Parse(l)
		if err != nil {
			return false
		}
		// Any tag but "ignore" is whatever suits the expression best.
		var eval func(constraint.Expr, bool) bool
		eval = func(x constraint.Expr, want bool) bool {
			switch x := x.(type) {
			case *constraint.TagExpr:
				return x.Tag != "ignore" && want
			case *constraint.NotExpr:
				return !eval(x.X, !want)
			case *constraint.AndExpr:
				return eval(x.X, want) && eval(x.Y, want)
			case *constraint.OrExpr:
				return eval(x.X, want) || eval(x.Y, want)
			}
			return true
		}
		return !eval(expr, true)
	}
	return false
}

// copyLicenses copies the license files of a module ("go mod vendor" copies
// LICENSE, NOTICE, ... of the module root and of every directory down to a
// package) from modDir into dst, the module in vendor/.
func copyLicenses(modDir, pkgDir, dst string) {
	rel, err := filepath.Rel(modDir, pkgDir)
	must(err)
	dirs := []string{"."}
	for d := rel; d != "." && d != ""; d = filepath.Dir(d) {
		dirs = append(dirs, d)
	}
	for _, d := range dirs {
		entries, err := os.ReadDir(filepath.Join(modDir, d))
		must(err)
		for _, e := range entries {
			if e.IsDir() || !metaFile.MatchString(e.Name()) {
				continue
			}
			target := filepath.Join(dst, d, e.Name())
			if _, err := os.Stat(target); err == nil {
				continue
			}
			b, err := os.ReadFile(filepath.Join(modDir, d, e.Name()))
			must(err)
			must(os.MkdirAll(filepath.Dir(target), 0o755))
			must(os.WriteFile(target, b, 0o644))
		}
	}
}

// metaFile matches the files "go mod vendor" keeps outside of packages.
var metaFile = regexp.MustCompile(`(?i)^(AUTHORS|CONTRIBUTORS|COPYLEFT|COPYING|COPYRIGHT|LEGAL|LICENSE|NOTICE|PATENTS)`)

// remove deletes a file that must exist.
func remove(path string) {
	if err := os.Remove(path); err != nil {
		fail("%s: patch target not found: %v", path, err)
	}
}

func must(err error) {
	if err != nil {
		fail("%v", err)
	}
}

func fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(os.Stderr, "patch: "+msg)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		// An annotation puts the failing patch at the top of the run and the PR.
		fmt.Printf("::error file=tabernetes/k8s/patch.go,title=Kubernetes patch failed::%s\n", msg)
	}
	os.Exit(1)
}

func read(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("%v", err)
	}
	return string(b)
}

func write(path, s string) {
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		fail("%v", err)
	}
}

// edit replaces each old with its new in path. pairs is old, new, old, new, ...
func edit(path string, pairs ...string) {
	s := read(path)
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			fail("%s: patch target not found: %q", path, pairs[i])
		}
		s = strings.ReplaceAll(s, pairs[i], pairs[i+1])
	}
	write(path, s)
}

var (
	goBuild   = regexp.MustCompile(`(?m)^//go:build (.+)$`)
	plusBuild = regexp.MustCompile(`(?m)^// \+build .*\n`)
)

// buildTag rewrites the //go:build line of path with format, where %s is the
// old expression. Legacy // +build lines are dropped.
func buildTag(path, format string) {
	s := read(path)
	m := goBuild.FindStringSubmatchIndex(s)
	if m == nil {
		fail("%s: patch target not found: //go:build line", path)
	}
	expr := s[m[2]:m[3]]
	if strings.Contains(expr, "js") {
		fail("%s: already patched: %s", path, expr)
	}
	s = s[:m[2]] + fmt.Sprintf(format, expr) + s[m[3]:]
	write(path, plusBuild.ReplaceAllString(s, ""))
}

// jsFromWindows excludes unix from js and copies windows to a _js.go file
// with a js build tag.
func jsFromWindows(unix, windows string) {
	if !strings.Contains(read(unix), "&& !js") {
		buildTag(unix, "(%s) && !js")
	}
	s := plusBuild.ReplaceAllString(goBuild.ReplaceAllString(read(windows), ""), "")
	write(strings.TrimSuffix(windows, "_windows.go")+"_js.go", "//go:build js\n\n"+strings.TrimLeft(s, "\n"))
}

func dropUnusedImport(path, pkg string) {
	if regexp.MustCompile(`\b` + pkg + `\.[A-Z]`).MatchString(read(path)) {
		fail("%s: %s is still used", path, pkg)
	}
	edit(path, "\t\""+pkg+"\"\n", "")
}

func addImport(path, pkg string) {
	if !strings.Contains(read(path), "\t\""+pkg+"\"\n") {
		edit(path, "import (\n", "import (\n\t\""+pkg+"\"\n")
	}
}

// copyTree copies the embedded directory src into dst.
func copyTree(src, dst string) {
	err := fs.WalkDir(files, src, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := files.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, strings.TrimPrefix(path, src+"/"))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
	if err != nil {
		fail("%v", err)
	}
}
