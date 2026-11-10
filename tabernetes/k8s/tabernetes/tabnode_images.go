package main

// The built-in images of the tab node: images that run without pulling
// anything, because their programs are Go code in the wasm binary. Each one
// behaves like the real image as far as a tutorial notices: the same
// ENTRYPOINT/CMD (so command: and args: override them the same way), ports,
// paths, flags, environment variables and response formats.
//
// Images with a shell as CMD (busybox, alpine, ubuntu, ...) need no program:
// their commands run in the shell of the tab (tabnode_proc.go).
//
// nginx and httpd are in tabnode_images_web.go, redis in
// tabnode_images_redis.go.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/pflag"
	v1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/interp"
)

type imageProgram = func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error

// imagePrograms are the programs of the built-in images, by base name.
// Filled in init: some programs look up commands in this map.
var imagePrograms = map[string]imageProgram{}

func init() {
	for name, p := range map[string]imageProgram{
		"docker-entrypoint.sh": dockerEntrypoint,
		"entrypoint.sh":        dockerEntrypoint,
		"pause":                pauseProg,
		"whoami":               whoamiProg,
		"http-echo":            httpEchoProg,
		"echo-server":          echoServerProg,
		"echoserver":           echoserverProg,
		"agnhost":              agnhostProg,
		"podinfo":              podinfoProg,
		"node":                 nodeProg,
		"hello-app":            helloAppProg,
		"hello":                helloWorldProg,
		"apache2-foreground":   hpaExampleProg,
		"nginx":                nginxProg,
		"nginx-debug":          nginxProg,
		"httpd":                httpdProg,
		"httpd-foreground":     httpdProg,
		"apachectl":            httpdProg,
		"redis-server":         redisServerProg,
		"redis-cli":            redisCliProg,
	} {
		imagePrograms[name] = p
	}
}

// imageDef is a built-in image and how it is known.
type imageDef struct {
	img      builtinImage
	repos    []string // repository paths without registry and "library/"
	short    bool     // also match the last path element on any registry
	registry string   // for node.status.images, "" is docker.io
	tag      string   // the tag in node.status.images
	size     int64
	// tune adapts the image to the tag of the reference, e.g. the version
	// in its environment.
	tune func(img *builtinImage, tag string)
}

func (d *imageDef) canonical() string {
	repo := d.repos[0]
	switch {
	case d.registry != "":
		return d.registry + "/" + repo
	case !strings.Contains(repo, "/"):
		return "docker.io/library/" + repo
	}
	return "docker.io/" + repo
}

// osRelease is /etc/os-release of an image: the fields of the real
// distribution and a variant that says where it really runs.
func osRelease(fields string) string {
	return fields + "VARIANT=\"Tabernetes built-in image (runs in the shell of the browser tab)\"\nVARIANT_ID=tabernetes\n"
}

var (
	debianRelease = osRelease(`PRETTY_NAME="Debian GNU/Linux 13 (trixie)"
NAME="Debian GNU/Linux"
VERSION_ID="13"
VERSION="13 (trixie)"
VERSION_CODENAME=trixie
DEBIAN_VERSION_FULL=13.1
ID=debian
HOME_URL="https://www.debian.org/"
SUPPORT_URL="https://www.debian.org/support"
BUG_REPORT_URL="https://bugs.debian.org/"
`)
	alpineRelease = osRelease(`NAME="Alpine Linux"
ID=alpine
VERSION_ID=3.22.2
PRETTY_NAME="Alpine Linux v3.22"
HOME_URL="https://alpinelinux.org/"
BUG_REPORT_URL="https://gitlab.alpinelinux.org/alpine/aports/-/issues"
`)
	alpineFiles = map[string]string{"/etc/os-release": alpineRelease, "/etc/alpine-release": "3.22.2\n"}
	debianFiles = map[string]string{"/etc/os-release": debianRelease, "/etc/debian_version": "13.1\n"}
)

func rhelFiles(release, osr string) map[string]string {
	return map[string]string{"/etc/os-release": osRelease(osr), "/etc/redhat-release": release + "\n", "/etc/system-release": release + "\n"}
}

// shellImage is an image whose CMD is a shell: it needs no program.
func shellImage(name string, cmd string, files map[string]string, size int64, tag string, repos ...string) *imageDef {
	return &imageDef{
		img:   builtinImage{name: name, cmd: []string{cmd}, files: files},
		repos: append([]string{name}, repos...), short: true, tag: tag, size: size,
	}
}

func versionEnv(key string) func(img *builtinImage, tag string) {
	return func(img *builtinImage, tag string) {
		if !fullVersion.MatchString(tag) {
			return
		}
		env := append([]string{}, img.env...)
		for i, kv := range env {
			if strings.HasPrefix(kv, key+"=") {
				env[i] = key + "=" + tag
			}
		}
		img.env = env
	}
}

var fullVersion = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

var imageDefs = []*imageDef{
	{
		img: builtinImage{
			name:       "nginx",
			entrypoint: []string{"/docker-entrypoint.sh"},
			cmd:        []string{"nginx", "-g", "daemon off;"},
			env:        []string{"NGINX_VERSION=1.29.3", "NJS_VERSION=0.9.4", "NJS_RELEASE=1~trixie", "PKG_RELEASE=1~trixie", "DYNPKG_RELEASE=1~trixie"},
			files:      nginxFiles,
		},
		repos: []string{"nginx"}, short: true, tag: "latest", size: 72_324_501,
		tune: versionEnv("NGINX_VERSION"),
	},
	{
		img: builtinImage{
			name:       "nginx-hello",
			entrypoint: []string{"/docker-entrypoint.sh"},
			cmd:        []string{"nginx", "-g", "daemon off;"},
			env:        []string{"NGINX_VERSION=1.29.3", "NJS_VERSION=0.9.4", "NJS_RELEASE=1~trixie", "PKG_RELEASE=1~trixie", "DYNPKG_RELEASE=1~trixie"},
			files:      nginxHelloFiles,
		},
		repos: []string{"nginxdemos/hello"}, tag: "latest", size: 72_400_112,
		tune: nginxHelloTag,
	},
	{
		img: builtinImage{
			name:    "httpd",
			cmd:     []string{"httpd-foreground"},
			workdir: "/usr/local/apache2",
			env: []string{
				"PATH=/usr/local/apache2/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"HTTPD_PREFIX=/usr/local/apache2", "HTTPD_VERSION=2.4.65",
			},
			files: httpdFiles,
		},
		repos: []string{"httpd"}, short: true, tag: "latest", size: 45_312_177,
		tune: versionEnv("HTTPD_VERSION"),
	},
	{
		img:   builtinImage{name: "pause", entrypoint: []string{"/pause"}},
		repos: []string{"pause"}, registry: "registry.k8s.io", tag: "3.10.1", size: 320_368,
	},
	shellImage("busybox", "sh", nil, 2_224_358, "latest", "radial/busyboxplus"),
	shellImage("alpine", "/bin/sh", alpineFiles, 3_802_452, "latest"),
	shellImage("ubuntu", "/bin/bash", map[string]string{
		"/etc/os-release": osRelease(`PRETTY_NAME="Ubuntu 24.04.3 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.3 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
UBUNTU_CODENAME=noble
`),
		"/etc/lsb-release":    "DISTRIB_ID=Ubuntu\nDISTRIB_RELEASE=24.04\nDISTRIB_CODENAME=noble\nDISTRIB_DESCRIPTION=\"Ubuntu 24.04.3 LTS\"\n",
		"/etc/debian_version": "trixie/sid\n",
	}, 29_723_147, "latest"),
	shellImage("debian", "bash", debianFiles, 48_481_335, "latest"),
	shellImage("fedora", "/bin/bash", rhelFiles("Fedora release 43 (Forty Three)", `NAME="Fedora Linux"
VERSION="43 (Container Image)"
ID=fedora
VERSION_ID=43
PRETTY_NAME="Fedora Linux 43 (Container Image)"
HOME_URL="https://fedoraproject.org/"
`), 61_208_993, "latest"),
	{
		img:   builtinImage{name: "bash", entrypoint: []string{"docker-entrypoint.sh"}, cmd: []string{"bash"}, env: []string{"_BASH_VERSION=5.3"}, files: alpineFiles},
		repos: []string{"bash"}, short: true, tag: "latest", size: 6_117_901,
	},
	shellImage("centos", "/bin/bash", rhelFiles("CentOS Stream release 10", `NAME="CentOS Stream"
VERSION="10 (Coughlan)"
ID="centos"
ID_LIKE="rhel fedora"
VERSION_ID="10"
PRETTY_NAME="CentOS Stream 10 (Coughlan)"
HOME_URL="https://centos.org/"
`), 76_904_202, "latest", "centos/centos"),
	shellImage("rockylinux", "/bin/bash", rhelFiles("Rocky Linux release 9.6 (Blue Onyx)", `NAME="Rocky Linux"
VERSION="9.6 (Blue Onyx)"
ID="rocky"
ID_LIKE="rhel centos fedora"
VERSION_ID="9.6"
PRETTY_NAME="Rocky Linux 9.6 (Blue Onyx)"
HOME_URL="https://rockylinux.org/"
`), 67_311_806, "9", "rockylinux/rockylinux"),
	shellImage("almalinux", "/bin/bash", rhelFiles("AlmaLinux release 10.0 (Purple Lion)", `NAME="AlmaLinux"
VERSION="10.0 (Purple Lion)"
ID="almalinux"
ID_LIKE="rhel centos fedora"
VERSION_ID="10.0"
PRETTY_NAME="AlmaLinux 10.0 (Purple Lion)"
HOME_URL="https://almalinux.org/"
`), 70_512_330, "latest"),
	shellImage("amazonlinux", "/bin/bash", rhelFiles("Amazon Linux release 2023.9 (Amazon Linux)", `NAME="Amazon Linux"
VERSION="2023"
ID="amzn"
ID_LIKE="fedora"
VERSION_ID="2023"
PRETTY_NAME="Amazon Linux 2023.9"
HOME_URL="https://aws.amazon.com/linux/amazon-linux-2023/"
`), 52_011_437, "latest"),
	shellImage("oraclelinux", "/bin/bash", rhelFiles("Oracle Linux Server release 9.6", `NAME="Oracle Linux Server"
VERSION="9.6"
ID="ol"
ID_LIKE="fedora"
VERSION_ID="9.6"
PRETTY_NAME="Oracle Linux Server 9.6"
HOME_URL="https://linux.oracle.com/"
`), 89_120_544, "9"),
	shellImage("ubi9/ubi", "/bin/bash", rhelFiles("Red Hat Enterprise Linux release 9.6 (Plow)", `NAME="Red Hat Enterprise Linux"
VERSION="9.6 (Plow)"
ID="rhel"
ID_LIKE="fedora"
VERSION_ID="9.6"
PRETTY_NAME="Red Hat Enterprise Linux 9.6 (Plow)"
HOME_URL="https://www.redhat.com/"
`), 79_101_221, "latest", "ubi9", "ubi9/ubi-minimal", "ubi8/ubi", "ubi8", "ubi10/ubi", "ubi10"),
	shellImage("archlinux", "/usr/bin/bash", map[string]string{"/etc/os-release": osRelease(`NAME="Arch Linux"
PRETTY_NAME="Arch Linux"
ID=arch
BUILD_ID=rolling
HOME_URL="https://archlinux.org/"
`)}, 160_221_908, "latest"),
	shellImage("opensuse/leap", "/bin/bash", map[string]string{"/etc/os-release": osRelease(`NAME="openSUSE Leap"
VERSION="15.6"
ID="opensuse-leap"
ID_LIKE="suse opensuse"
VERSION_ID="15.6"
PRETTY_NAME="openSUSE Leap 15.6"
HOME_URL="https://www.opensuse.org/"
`)}, 113_870_120, "latest", "opensuse/tumbleweed"),
	{
		img:   builtinImage{name: "nicolaka/netshoot", cmd: []string{"zsh"}, files: alpineFiles},
		repos: []string{"nicolaka/netshoot"}, tag: "latest", size: 213_501_877,
	},
	{
		img:   builtinImage{name: "traefik/whoami", entrypoint: []string{"/whoami"}},
		repos: []string{"traefik/whoami", "containous/whoami"}, tag: "v1.11.0", size: 3_617_002,
	},
	{
		img:   builtinImage{name: "hashicorp/http-echo", entrypoint: []string{"/http-echo"}},
		repos: []string{"hashicorp/http-echo"}, tag: "1.0.0", size: 3_642_517,
	},
	{
		img: builtinImage{name: "ealen/echo-server", entrypoint: []string{"echo-server"}, workdir: "/app",
			env: []string{"NODE_VERSION=22.20.0", "PORT=80"}, files: alpineFiles},
		repos: []string{"ealen/echo-server"}, tag: "latest", size: 59_018_336,
	},
	{
		img:   builtinImage{name: "kicbase/echo-server", entrypoint: []string{"/bin/echo-server"}},
		repos: []string{"kicbase/echo-server", "jmalloc/echo-server"}, tag: "1.0", size: 4_939_776,
	},
	{
		img:   builtinImage{name: "echoserver", cmd: []string{"echoserver"}, files: map[string]string{"/usr/local/bin/run.sh": "#!/bin/sh\nexec echoserver\n"}},
		repos: []string{"echoserver", "google_containers/echoserver", "google-containers/echoserver"}, short: true,
		registry: "registry.k8s.io", tag: "1.10", size: 46_102_430,
	},
	{
		img:   builtinImage{name: "agnhost", entrypoint: []string{"/agnhost"}, cmd: []string{"pause"}},
		repos: []string{"e2e-test-images/agnhost", "agnhost"}, short: true,
		registry: "registry.k8s.io", tag: "2.53", size: 50_826_141,
	},
	{
		img:   builtinImage{name: "podinfo", cmd: []string{"./podinfo"}, workdir: "/home/app", files: alpineFiles},
		repos: []string{"stefanprodan/podinfo"}, registry: "ghcr.io", tag: "6.9.2", size: 23_154_710,
	},
	{
		img:   builtinImage{name: "curlimages/curl", entrypoint: []string{"/entrypoint.sh"}, cmd: []string{"curl"}, workdir: "/home/curl_user", files: alpineFiles},
		repos: []string{"curlimages/curl"}, tag: "8.16.0", size: 24_811_204,
	},
	{
		img:   builtinImage{name: "bitnami/kubectl", entrypoint: []string{"kubectl"}, cmd: []string{"--help"}, files: debianFiles},
		repos: []string{"bitnami/kubectl"}, tag: "latest", size: 98_017_610,
	},
	{
		img:   builtinImage{name: "kubectl", entrypoint: []string{"kubectl"}},
		repos: []string{"kubectl", "rancher/kubectl"}, short: true, registry: "registry.k8s.io", tag: "v1.37.2", size: 58_301_004,
	},
	{
		img:   builtinImage{name: "kubernetes-bootcamp", cmd: []string{"/bin/sh", "-c", "node server.js"}, env: []string{"NODE_VERSION=6.3.0"}},
		repos: []string{"google-samples/kubernetes-bootcamp", "jocatalin/kubernetes-bootcamp"}, short: true,
		registry: "gcr.io", tag: "v1", size: 211_336_459,
	},
	{
		img:   builtinImage{name: "hello-app", cmd: []string{"/hello-app"}},
		repos: []string{"google-samples/hello-app", "google-samples/containers/gke/hello-app"}, short: true,
		registry: "us-docker.pkg.dev", tag: "1.0", size: 9_978_428,
	},
	{
		img:   builtinImage{name: "hpa-example", cmd: []string{"apache2-foreground"}, workdir: "/var/www/html"},
		repos: []string{"hpa-example", "kubernetes-e2e-test-images/hpa-example"}, short: true,
		registry: "registry.k8s.io", tag: "latest", size: 186_301_222,
	},
	{
		img:   builtinImage{name: "hello-world", cmd: []string{"/hello"}},
		repos: []string{"hello-world"}, short: true, tag: "latest", size: 10_072,
	},
	{
		img: builtinImage{name: "redis", entrypoint: []string{"docker-entrypoint.sh"}, cmd: []string{"redis-server"}, workdir: "/data",
			env: []string{"REDIS_VERSION=8.2.2"}, files: debianFiles},
		repos: []string{"redis"}, short: true, tag: "latest", size: 52_604_115,
		tune: versionEnv("REDIS_VERSION"),
	},
}

// parseRef splits an image reference into the repository path (without
// registry and "library/") and the tag ("" without one).
func parseRef(ref string) (repo, tag string) {
	ref = strings.ToLower(strings.TrimSpace(ref))
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		ref, tag = ref[:i], ref[i+1:]
	}
	if first, rest, ok := strings.Cut(ref, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		ref = rest
	}
	return strings.TrimPrefix(ref, "library/"), tag
}

// findImage finds the definition of an image reference: the repository path
// first, then the last path element for images known on any registry.
func findImage(ref string) (*imageDef, string) {
	repo, tag := parseRef(ref)
	if repo == "" {
		return nil, ""
	}
	for _, d := range imageDefs {
		for _, r := range d.repos {
			if repo == r {
				return d, tag
			}
		}
	}
	// bitnami/nginx and friends listen elsewhere and keep files elsewhere.
	if strings.HasPrefix(repo, "bitnami/") {
		return nil, ""
	}
	last := path.Base(repo)
	for _, d := range imageDefs {
		if d.short && last == path.Base(d.repos[0]) {
			return d, tag
		}
	}
	if strings.Contains(last, "pause") {
		for _, d := range imageDefs {
			if d.img.name == "pause" {
				return d, tag
			}
		}
	}
	return nil, ""
}

// lookupImage returns the built-in image for an image reference, or nil.
func lookupImage(ref string) *builtinImage {
	d, tag := findImage(ref)
	if d == nil {
		return nil
	}
	img := d.img
	if d.tune != nil {
		d.tune(&img, tag)
	}
	return &img
}

// builtinImageNames lists the short names of the built-in images, for messages.
func builtinImageNames() []string {
	var names []string
	for _, d := range imageDefs {
		names = append(names, d.img.name)
	}
	sort.Strings(names)
	return names
}

func imageDigest(canonical, tag string) string {
	sum := sha256.Sum256([]byte("tabernetes builtin image " + canonical + ":" + tag))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// builtinImageList is node.status.images.
func builtinImageList() []v1.ContainerImage {
	var list []v1.ContainerImage
	for _, d := range imageDefs {
		c := d.canonical()
		list = append(list, v1.ContainerImage{
			Names:     []string{c + "@" + imageDigest(c, d.tag), c + ":" + d.tag},
			SizeBytes: d.size,
		})
	}
	return list
}

// imageID is the imageID of a container status.
func imageID(ref string, img *builtinImage, wasm bool) string {
	if wasm {
		// repo@digest once the module is pulled (tabnode_wasi.go)
		if id := wasmImageID(ref); id != "" {
			return id
		}
		return "tabernetes://wasm/" + ref
	}
	if d, tag := findImage(ref); d != nil && img != nil {
		if tag == "" {
			tag = "latest"
		}
		return "tabernetes://builtin/" + d.img.name + "@" + imageDigest(d.canonical(), tag)
	}
	return "tabernetes://shell/" + ref
}

// --- helpers for the programs ---

// imageOf is the built-in image of the container a program runs in.
func imageOf(ctx context.Context) *builtinImage {
	if pe := procOf(ctx); pe != nil && pe.c != nil {
		return pe.c.image
	}
	return nil
}

func imageIs(ctx context.Context, name string) bool {
	img := imageOf(ctx)
	return img != nil && img.name == name
}

// imageTag is the tag of the image of the container, "" without one.
func imageTag(ctx context.Context) string {
	if pe := procOf(ctx); pe != nil && pe.c != nil {
		_, tag := parseRef(pe.c.spec.Image)
		return tag
	}
	return ""
}

// podHostname is the hostname of the pod (or of the tab in the terminal).
func podHostname(ctx context.Context) string {
	if pe := procOf(ctx); pe != nil {
		if b, err := os.ReadFile(pe.root.real("/etc/hostname")); err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
		return pe.w.name
	}
	h, _ := os.Hostname()
	return h
}

func podIP(ctx context.Context) string {
	if pe := procOf(ctx); pe != nil && pe.ip != "" {
		return pe.ip
	}
	return "127.0.0.1"
}

func getenv(hc interp.HandlerContext, name string) string {
	if vr := hc.Env.Get(name); vr.IsSet() {
		return vr.String()
	}
	return ""
}

func environ(hc interp.HandlerContext) []string {
	var env []string
	for name, vr := range hc.Env.Each {
		if vr.Exported && vr.IsSet() {
			env = append(env, name+"="+vr.String())
		}
	}
	sort.Strings(env)
	return env
}

// syncWriter writes whole lines of concurrent request handlers at once.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

func newLogger(w io.Writer, flags int) *log.Logger {
	return log.New(&syncWriter{w: w}, "", flags)
}

// serveHTTP serves h on the addresses of the pod until ctx ends or a
// listener fails. A failed listen is reported like a Go program does.
func serveHTTP(ctx context.Context, hc interp.HandlerContext, prog string, h http.Handler, addrs ...string) error {
	var lns []net.Listener
	for _, a := range addrs {
		ln, err := listenPod(ctx, a)
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			fmt.Fprintf(hc.Stderr, "%s: listen tcp %s: %v\n", prog, a, unwrap(err))
			return interp.ExitStatus(1)
		}
		lns = append(lns, ln)
	}
	return serveListeners(ctx, hc, prog, h, lns...)
}

func serveListeners(ctx context.Context, hc interp.HandlerContext, prog string, h http.Handler, lns ...net.Listener) error {
	srv := &http.Server{
		Handler:     h,
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    log.New(io.Discard, "", 0),
	}
	stop := context.AfterFunc(ctx, func() { srv.Close() })
	defer stop()
	errs := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errs <- srv.Serve(ln) }()
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-errs:
		srv.Close()
		if ctx.Err() != nil || err == http.ErrServerClosed {
			return nil
		}
		fmt.Fprintf(hc.Stderr, "%s: %v\n", prog, err)
		return interp.ExitStatus(1)
	}
}

// statusWriter remembers the status and size of a response, for access logs.
type statusWriter struct {
	http.ResponseWriter
	status int
	size   int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.size += n
	return n, err
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	w.Write(append(b, '\n'))
}

// knownCommand reports whether the shell of a container finds name.
func knownCommand(ctx context.Context, hc interp.HandlerContext, name string) bool {
	base := path.Base(name)
	if shells[base] || base == "kubectl" || base == "busybox" || imagePrograms[base] != nil || podTools[base] != nil || coreutils[base] != nil || interp.IsBuiltin(base) {
		return true
	}
	if strings.Contains(name, "/") {
		_, err := os.Stat(realPath(ctx, hc.Dir, name))
		return err == nil
	}
	return false
}

// --- entrypoints ---

// dockerEntrypoint is docker-entrypoint.sh and entrypoint.sh of the images
// of the Docker library: flags (and for curl unknown commands) go to the
// default command of the image, then the command runs. nginx prints what its
// /docker-entrypoint.d scripts do.
func dockerEntrypoint(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	img := imageOf(ctx)
	if img != nil && len(img.cmd) > 0 && len(args) > 0 {
		def := img.cmd[0]
		switch {
		case strings.HasPrefix(args[0], "-"):
			args = append([]string{def}, args...)
		case def == "redis-server" && strings.HasSuffix(args[0], ".conf"):
			args = append([]string{def}, args...)
		case img.name == "curlimages/curl" && !knownCommand(ctx, hc, args[0]):
			args = append([]string{def}, args...)
		}
	}
	if len(args) == 0 {
		return nil
	}
	if img != nil && img.name == "nginx" && (args[0] == "nginx" || args[0] == "nginx-debug") {
		nginxEntrypoint(ctx, hc)
	}
	return exec(ctx, args)
}

// --- pause ---

func pauseProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if len(args) > 0 && args[0] == "-v" {
		fmt.Fprintln(hc.Stdout, "pause.c v3.10.1")
		return nil
	}
	<-ctx.Done()
	return nil
}

// --- traefik/whoami ---

// whoamiProg is traefik/whoami. In other containers whoami is the usual
// command that prints the user.
func whoamiProg(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	if !imageIs(ctx, "traefik/whoami") {
		return podTools["whoami"](ctx, hc, args, exec)
	}
	fs := pflag.NewFlagSet("whoami", pflag.ContinueOnError)
	fs.SetOutput(hc.Stderr)
	defPort := "80"
	if p := getenv(hc, "WHOAMI_PORT_NUMBER"); p != "" {
		defPort = p
	}
	port := fs.String("port", defPort, "give me a port number")
	name := fs.String("name", getenv(hc, "WHOAMI_NAME"), "give me a name")
	verbose := fs.Bool("verbose", false, "enable the verbose mode")
	fs.String("cert", "", "give me a certificate")
	fs.String("key", "", "give me a key")
	fs.String("cacert", "", "give me a CA chain, enforces mutual TLS")
	if err := fs.Parse(goFlags(args)); err != nil {
		return interp.ExitStatus(2)
	}
	logger := newLogger(hc.Stdout, log.LstdFlags)
	hostname := podHostname(ctx)
	ip := podIP(ctx)
	ips := []string{"127.0.0.1", "::1", ip}

	var mu sync.Mutex
	health := http.StatusOK
	mux := http.NewServeMux()
	whoami := func(w http.ResponseWriter, r *http.Request) {
		if wait := r.URL.Query().Get("wait"); wait != "" {
			if d, err := time.ParseDuration(wait); err == nil {
				sleepCtx(r.Context(), d)
			}
		}
		if *name != "" {
			fmt.Fprintln(w, "Name:", *name)
		}
		fmt.Fprintln(w, "Hostname:", hostname)
		for _, a := range ips {
			fmt.Fprintln(w, "IP:", a)
		}
		fmt.Fprintln(w, "RemoteAddr:", r.RemoteAddr)
		r.Write(w)
		if ok, _ := strconv.ParseBool(r.URL.Query().Get("env")); ok {
			for _, kv := range environ(hc) {
				fmt.Fprintln(w, kv)
			}
		}
	}
	mux.HandleFunc("/", whoami)
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		data := map[string]any{
			"hostname": hostname, "ip": ips, "headers": r.Header, "url": r.URL.RequestURI(),
			"host": r.Host, "method": r.Method, "remoteAddr": r.RemoteAddr,
		}
		if *name != "" {
			data["name"] = *name
		}
		if ok, _ := strconv.ParseBool(r.URL.Query().Get("env")); ok {
			data["environ"] = environ(hc)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	})
	mux.HandleFunc("/bench", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "1")
	})
	mux.HandleFunc("/data", func(w http.ResponseWriter, r *http.Request) {
		size, _ := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
		if size <= 0 {
			size = 1
		}
		switch strings.ToUpper(r.URL.Query().Get("unit")) {
		case "KB":
			size <<= 10
		case "MB":
			size <<= 20
		case "GB":
			size <<= 30
		}
		size = min(size, 64<<20)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("|"))
		chunk := []byte(strings.Repeat("-", 4096))
		for left := max(size-2, 0); left > 0; left -= int64(len(chunk)) {
			if _, err := w.Write(chunk[:min(left, int64(len(chunk)))]); err != nil {
				return
			}
		}
		w.Write([]byte("|"))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var code int
			fmt.Fscan(r.Body, &code)
			if code < 100 || code > 999 {
				http.Error(w, "invalid status code", http.StatusBadRequest)
				return
			}
			logger.Printf("Update health check status code [%d]\n", code)
			health = code
			return
		}
		w.WriteHeader(health)
	})
	var h http.Handler = mux
	if *verbose {
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			logger.Println(r.Method, r.URL.RequestURI(), r.RemoteAddr)
			mux.ServeHTTP(w, r)
		})
	}
	logger.Printf("Starting up on port %s", *port)
	return serveHTTP(ctx, hc, "whoami", h, ":"+*port)
}

// goFlags turns the -flag spelling of Go programs into --flag for pflag.
func goFlags(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 2 {
			a = "-" + a
		}
		out[i] = a
	}
	return out
}

// --- hashicorp/http-echo ---

func httpEchoProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	fs := flag.NewFlagSet("http-echo", flag.ContinueOnError)
	fs.SetOutput(hc.Stderr)
	listen := fs.String("listen", ":5678", "address and port to listen")
	text := fs.String("text", "", "text to put on the webpage")
	status := fs.Int("status-code", 200, "http response code, e.g.: 200")
	version := fs.Bool("version", false, "display version information")
	if err := fs.Parse(args); err != nil {
		return interp.ExitStatus(2)
	}
	if *version {
		fmt.Fprintln(hc.Stderr, "http-echo v1.0.0")
		return nil
	}
	if *text == "" {
		*text = getenv(hc, "ECHO_TEXT")
	}
	if *text == "" {
		if len(fs.Args()) > 0 {
			fmt.Fprintf(hc.Stderr, "Too many arguments! %q\n", fs.Args())
		} else {
			fmt.Fprintln(hc.Stderr, "Missing -text option!")
		}
		return interp.ExitStatus(127)
	}
	logger := newLogger(hc.Stdout, log.LstdFlags)
	body := *text + "\n"
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`+"\n")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-App-Name", "http-echo")
		w.Header().Set("X-App-Version", "1.0.0")
		w.WriteHeader(*status)
		fmt.Fprint(w, body)
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		mux.ServeHTTP(sw, r)
		logger.Printf("%s %s \"%s %s %s\" %d %d %q %v\n", r.Host, r.RemoteAddr, r.Method, r.URL.RequestURI(), r.Proto, sw.code(), sw.size, r.UserAgent(), time.Since(start))
	})
	logger.Printf("[INFO] server is listening on %s\n", *listen)
	return serveHTTP(ctx, hc, "http-echo", h, *listen)
}

// --- echo servers ---

// echoServerProg is ealen/echo-server (JSON on port 80) and
// kicbase/echo-server, the jmalloc/echo-server of minikube (text on 8080).
func echoServerProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if imageIs(ctx, "ealen/echo-server") {
		return ealenEcho(ctx, hc)
	}
	port := getenv(hc, "PORT")
	if port == "" {
		port = "8080"
	}
	hostname := podHostname(ctx)
	logger := newLogger(hc.Stdout, 0)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Printf("%s | %s %s", r.RemoteAddr, r.Method, r.URL)
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Request served by %s\n\n", hostname)
		fmt.Fprintf(w, "%s %s %s\n\n", r.Proto, r.Method, r.URL.RequestURI())
		fmt.Fprintf(w, "Host: %s\n", r.Host)
		keys := make([]string, 0, len(r.Header))
		for k := range r.Header {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			for _, v := range r.Header[k] {
				fmt.Fprintf(w, "%s: %s\n", k, v)
			}
		}
		if b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(b) > 0 {
			fmt.Fprintf(w, "\n%s", b)
		}
	})
	logger.Printf("Echo server listening on port %s.", port)
	return serveHTTP(ctx, hc, "echo-server", h, ":"+port)
}

func ealenEcho(ctx context.Context, hc interp.HandlerContext) error {
	port := getenv(hc, "PORT")
	if port == "" {
		port = "80"
	}
	showEnv := getenv(hc, "ENABLE__ENVIRONMENT") != "false"
	showHost := getenv(hc, "ENABLE__HOST") != "false"
	showHTTP := getenv(hc, "ENABLE__HTTP") != "false"
	showReq := getenv(hc, "ENABLE__REQUEST") != "false"
	showHeader := getenv(hc, "ENABLE__HEADER") != "false"
	showCookies := getenv(hc, "ENABLE__COOKIES") != "false"
	logger := newLogger(hc.Stdout, 0)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		param := func(name string) string {
			if v := r.URL.Query().Get(name); v != "" {
				return v
			}
			return r.Header.Get("X-" + strings.ReplaceAll(name, "_", "-"))
		}
		if v := param("echo_time"); v != "" {
			if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
				sleepCtx(r.Context(), time.Duration(min(ms, 60000))*time.Millisecond)
			}
		}
		if v := param("echo_header"); v != "" {
			for _, kv := range strings.Split(v, ",") {
				if k, val, ok := strings.Cut(kv, ":"); ok {
					w.Header().Add(strings.TrimSpace(k), strings.TrimSpace(val))
				}
			}
		}
		code := http.StatusOK
		if v := param("echo_code"); v != "" {
			// "200-400-500" picks one of them at random.
			codes := strings.Split(v, "-")
			if c, err := strconv.Atoi(codes[time.Now().Nanosecond()%len(codes)]); err == nil && c >= 200 && c <= 599 {
				code = c
			}
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		logger.Printf(`{"level":"info","time":"%s","host":%q,"method":%q,"url":%q,"status":%d}`, time.Now().UTC().Format(time.RFC3339Nano), r.Host, r.Method, r.URL.RequestURI(), code)
		if v := param("echo_body"); v != "" {
			writeJSON(w, code, v)
			return
		}
		if v := param("echo_env_body"); v != "" {
			writeJSON(w, code, getenv(hc, v))
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		out := map[string]any{}
		if showHost {
			out["host"] = map[string]any{"hostname": host, "ip": "::ffff:" + remoteHost(r), "ips": []string{}}
		}
		if showHTTP {
			out["http"] = map[string]any{"method": r.Method, "baseUrl": "", "originalUrl": r.URL.RequestURI(), "protocol": "http"}
		}
		if showReq {
			query := map[string]any{}
			for k, v := range r.URL.Query() {
				if len(v) == 1 {
					query[k] = v[0]
				} else {
					query[k] = v
				}
			}
			cookies := map[string]string{}
			if showCookies {
				for _, c := range r.Cookies() {
					cookies[c.Name] = c.Value
				}
			}
			headers := map[string]string{}
			if showHeader {
				headers["host"] = r.Host
				for k, v := range r.Header {
					headers[strings.ToLower(k)] = strings.Join(v, ", ")
				}
			}
			var b any = map[string]any{}
			if len(body) > 0 {
				if json.Unmarshal(body, &b) != nil {
					b = string(body)
				}
			}
			out["request"] = map[string]any{"params": map[string]string{"0": r.URL.Path}, "query": query, "cookies": cookies, "body": b, "headers": headers}
		}
		if showEnv {
			env := map[string]string{}
			for _, kv := range environ(hc) {
				k, v, _ := strings.Cut(kv, "=")
				env[k] = v
			}
			out["environment"] = env
		}
		writeJSON(w, code, out)
	})
	logger.Printf("Listening on port %s.", port)
	return serveHTTP(ctx, hc, "echo-server", h, ":"+port)
}

// echoserverProg is registry.k8s.io/echoserver:1.10 (nginx with lua there).
func echoserverProg(ctx context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
	hostname := podHostname(ctx)
	logger := newLogger(hc.Stdout, 0)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		var b strings.Builder
		fmt.Fprintf(&b, "\n\nHostname: %s\n\nPod Information:\n", hostname)
		if node, pod := getenv(hc, "NODE_NAME"), getenv(hc, "POD_NAME"); node != "" || pod != "" {
			fmt.Fprintf(&b, "\tnode name:\t%s\n\tpod name:\t%s\n\tpod namespace:\t%s\n\tpod IP:\t%s\n", orDash(node), orDash(pod), orDash(getenv(hc, "POD_NAMESPACE")), orDash(getenv(hc, "POD_IP")))
		} else {
			b.WriteString("\t-no pod information available-\n")
		}
		b.WriteString("\nServer values:\n\tserver_version=nginx: 1.13.3 - lua: 10008\n")
		fmt.Fprintf(&b, "\nRequest Information:\n\tclient_address=%s\n\tmethod=%s\n\treal path=%s\n\tquery=%s\n\trequest_version=%d.%d\n\trequest_scheme=http\n\trequest_uri=http://%s%s\n",
			remoteHost(r), r.Method, r.URL.Path, r.URL.RawQuery, r.ProtoMajor, r.ProtoMinor, r.Host, r.URL.RequestURI())
		b.WriteString("\nRequest Headers:\n")
		hdr := map[string]string{"host": r.Host}
		for k, v := range r.Header {
			hdr[strings.ToLower(k)] = strings.Join(v, ", ")
		}
		keys := make([]string, 0, len(hdr))
		for k := range hdr {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\t%s=%s  \n", k, hdr[k])
		}
		b.WriteString("\nRequest Body:\n")
		if body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(body) > 0 {
			fmt.Fprintf(&b, "%s\n\n", body)
		} else {
			b.WriteString("\t-no body in request-\n\n\n")
		}
		w.Header().Set("Server", "echoserver")
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(sw, b.String())
		logger.Printf("%s - - [%s] \"%s %s %s\" %d %d \"%s\" \"%s\"", remoteHost(r), time.Now().UTC().Format("02/Jan/2006:15:04:05 -0700"), r.Method, r.URL.RequestURI(), r.Proto, sw.code(), sw.size, orDash(r.Referer()), orDash(r.UserAgent()))
	})
	fmt.Fprintln(hc.Stdout, "Generating self-signed cert")
	return serveHTTP(ctx, hc, "echoserver", h, ":8080")
}

// --- agnhost ---

// agnhostProg is the test image of Kubernetes: one binary, many
// subcommands.
func agnhostProg(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	if len(args) == 0 {
		fmt.Fprintln(hc.Stderr, "Usage:\n  agnhost [command]\n\nAvailable Commands:\n  connect\n  entrypoint-tester\n  liveness\n  logs-generator\n  netexec\n  pause\n  porter\n  serve-hostname\n  test-webserver\n\nUse \"agnhost [command] --help\" for more information about a command.")
		return nil
	}
	sub, rest := args[0], args[1:]
	fs := pflag.NewFlagSet("agnhost "+sub, pflag.ContinueOnError)
	fs.SetOutput(hc.Stderr)
	fs.ParseErrorsAllowlist.UnknownFlags = true
	logger := newLogger(hc.Stderr, log.LstdFlags)
	switch sub {
	case "pause":
		fmt.Fprintln(hc.Stdout, "Paused")
		<-ctx.Done()
		fmt.Fprintln(hc.Stdout, "Shutting down after receiving signal: terminated.")
		return nil
	case "entrypoint-tester":
		fmt.Fprintln(hc.Stdout, rest)
		return nil
	case "--version", "version":
		fmt.Fprintln(hc.Stdout, "2.53")
		return nil
	case "serve-hostname":
		port := fs.Int("port", 9376, "Port number.")
		fs.Bool("http", true, "Serve HTTP requests.")
		fs.Bool("tcp", false, "Serve raw over TCP.")
		fs.Bool("udp", false, "Serve raw over UDP.")
		exitAfter := fs.Bool("close", false, "Close connection per each HTTP request.")
		_ = exitAfter
		if err := fs.Parse(rest); err != nil {
			return interp.ExitStatus(2)
		}
		hostname := podHostname(ctx)
		logger.Printf("Serving on port %d.\n", *port)
		return serveHTTP(ctx, hc, "agnhost", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, hostname)
		}), fmt.Sprintf(":%d", *port))
	case "liveness":
		started := time.Now()
		mux := http.NewServeMux()
		mux.HandleFunc("/started", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(200)
			fmt.Fprint(w, time.Since(started).String())
		})
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
			d := time.Since(started)
			if d.Seconds() > 10 {
				w.WriteHeader(500)
				fmt.Fprintf(w, "error: %v", d.Seconds())
				return
			}
			w.WriteHeader(200)
			fmt.Fprint(w, "ok")
		})
		return serveHTTP(ctx, hc, "agnhost", mux, ":8080")
	case "test-webserver":
		port := fs.Int("port", 80, "Port number.")
		if err := fs.Parse(rest); err != nil {
			return interp.ExitStatus(2)
		}
		root := "/"
		if pe := procOf(ctx); pe != nil {
			root = pe.root.real("/")
		}
		logger.Printf("Serving on port %d.\n", *port)
		return serveHTTP(ctx, hc, "agnhost", http.FileServer(http.Dir(root)), fmt.Sprintf(":%d", *port))
	case "porter":
		// Serves the value of SERVE_PORT_<port> on <port>.
		var addrs []string
		vals := map[string]string{}
		for _, kv := range environ(hc) {
			k, v, _ := strings.Cut(kv, "=")
			if p, ok := strings.CutPrefix(k, "SERVE_PORT_"); ok {
				addrs = append(addrs, ":"+p)
				vals[p] = v
			}
		}
		if len(addrs) == 0 {
			fmt.Fprintln(hc.Stderr, "No ports to serve: set SERVE_PORT_<port>=<value>.")
			return interp.ExitStatus(1)
		}
		return serveHTTP(ctx, hc, "agnhost", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, p, _ := net.SplitHostPort(r.Context().Value(http.LocalAddrContextKey).(net.Addr).String())
			fmt.Fprint(w, vals[p])
		}), addrs...)
	case "logs-generator":
		total := fs.Int("log-lines-total", 0, "Total lines that should be generated by the end of the run")
		dur := fs.Duration("run-duration", 0, "Total duration of the run")
		if err := fs.Parse(rest); err != nil {
			return interp.ExitStatus(2)
		}
		if n := fs.Args(); len(n) > 0 {
			*total, _ = strconv.Atoi(n[0])
			if len(n) > 1 {
				*dur, _ = time.ParseDuration(n[1])
			}
		}
		if *total <= 0 {
			fmt.Fprintln(hc.Stderr, "Invalid line count")
			return interp.ExitStatus(1)
		}
		gap := *dur / time.Duration(*total)
		methods := []string{"GET", "POST", "PUT"}
		for i := 0; i < *total; i++ {
			fmt.Fprintf(hc.Stdout, "%s %d %s /api/v1/namespaces/default/pods/%d %d\n", time.Now().UTC().Format("I0102 15:04:05.000000"), i, methods[i%3], 1000+i*37%9000, 200+i*7%300)
			if !sleepCtx(ctx, max(gap, time.Millisecond)) {
				return nil
			}
		}
		return nil
	case "netexec":
		return agnhostNetexec(ctx, hc, fs, rest, exec)
	case "connect":
		return agnhostConnect(ctx, hc, fs, rest)
	}
	fmt.Fprintf(hc.Stderr, "Error: unknown command %q for \"agnhost\"\nRun 'agnhost --help' for usage.\n", sub)
	return interp.ExitStatus(1)
}

// agnhostConnect is "agnhost connect <host:port>", the probe of the
// NetworkPolicy e2e tests: exit 0 if the connection opens, else TIMEOUT,
// REFUSED or OTHER on stderr and exit 1.
func agnhostConnect(ctx context.Context, hc interp.HandlerContext, fs *pflag.FlagSet, args []string) error {
	timeout := fs.Duration("timeout", 0, "Maximum time before returning an error")
	protocol := fs.String("protocol", "tcp", "The protocol to use to perform the connection, can be tcp, udp or sctp")
	if err := fs.Parse(args); err != nil {
		return interp.ExitStatus(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(hc.Stderr, "Usage: agnhost connect <host:port> [--timeout=<duration>] [--protocol=<tcp|udp|sctp>]")
		return interp.ExitStatus(1)
	}
	if *protocol != "tcp" {
		fmt.Fprintf(hc.Stderr, "OTHER: protocol %s is not supported on the tab node\n", *protocol)
		return interp.ExitStatus(1)
	}
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	conn, err := dialCluster(ctx, "tcp", fs.Arg(0))
	if err == nil {
		conn.Close()
		return nil
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		fmt.Fprintln(hc.Stderr, "TIMEOUT")
	case refused(err):
		fmt.Fprintln(hc.Stderr, "REFUSED")
	default:
		fmt.Fprintf(hc.Stderr, "OTHER: %v\n", err)
	}
	return interp.ExitStatus(1)
}

func agnhostNetexec(ctx context.Context, hc interp.HandlerContext, fs *pflag.FlagSet, args []string, exec interp.ExecHandlerFunc) error {
	httpPort := fs.Int("http-port", 8080, "HTTP Listen Port")
	fs.Int("udp-port", 8081, "UDP Listen Port")
	fs.Int("sctp-port", -1, "SCTP Listen Port")
	delayShutdown := fs.Int("delay-shutdown", 0, "Seconds to delay shutdown on SIGTERM")
	_ = delayShutdown
	if err := fs.Parse(args); err != nil {
		return interp.ExitStatus(2)
	}
	logger := newLogger(hc.Stderr, log.LstdFlags)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exitWith := -1
	var mu sync.Mutex
	healthy := true
	started := time.Now()
	hostname := podHostname(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "NOW: %v", time.Now().UTC())
	})
	mux.HandleFunc("/hostname", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, hostname) })
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		if code, err := strconv.Atoi(r.FormValue("code")); err == nil {
			w.WriteHeader(code)
		}
		fmt.Fprint(w, r.FormValue("msg"))
	})
	mux.HandleFunc("/clientip", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.RemoteAddr) })
	mux.HandleFunc("/header", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Header.Get(r.FormValue("key"))) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ok := healthy
		mu.Unlock()
		if !ok {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, "ok, uptime %v", time.Since(started).Round(time.Second))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("/exit", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.FormValue("code"))
		if err != nil || code < 0 || code > 127 {
			http.Error(w, "argument 'code' must be an integer [0-127]", http.StatusBadRequest)
			return
		}
		logger.Printf("GET /exit?code=%d", code)
		w.WriteHeader(http.StatusOK)
		mu.Lock()
		exitWith = code
		mu.Unlock()
		go func() {
			// let the response go out first
			sleepCtx(ctx, 50*time.Millisecond)
			cancel()
		}()
	})
	mux.HandleFunc("/shutdown", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		exitWith = 0
		mu.Unlock()
		go func() { sleepCtx(ctx, 50*time.Millisecond); cancel() }()
	})
	mux.HandleFunc("/shell", func(w http.ResponseWriter, r *http.Request) {
		cmd := r.FormValue("shellCommand")
		if cmd == "" {
			cmd = r.FormValue("cmd")
		}
		var out syncBuffer
		code := runArgv(r.Context(), procOf(ctx), []string{"sh", "-c", cmd}, environ(hc), hc.Dir, strings.NewReader(""), &out, &out)
		res := map[string]string{"output": out.String()}
		if code != 0 {
			res["error"] = fmt.Sprintf("exit status %d", code)
			writeJSON(w, http.StatusExpectationFailed, res)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("/dial", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		host, port, request := q.Get("host"), q.Get("port"), q.Get("request")
		tries, _ := strconv.Atoi(q.Get("tries"))
		tries = max(tries, 1)
		proto := q.Get("protocol")
		if proto == "" {
			proto = "http"
		}
		var responses, errs []string
		for i := 0; i < tries; i++ {
			switch proto {
			case "http":
				req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/"+strings.TrimPrefix(request, "/"), nil)
				resp, err := clusterClient.Do(req)
				if err != nil {
					errs = append(errs, err.Error())
					continue
				}
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				responses = append(responses, string(b))
			case "tcp":
				c, err := dialCluster(r.Context(), "tcp", net.JoinHostPort(host, port))
				if err != nil {
					errs = append(errs, err.Error())
					continue
				}
				fmt.Fprintln(c, request)
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				b, _ := io.ReadAll(io.LimitReader(c, 1<<20))
				c.Close()
				responses = append(responses, string(b))
			default:
				http.Error(w, fmt.Sprintf("unsupported protocol %q", proto), http.StatusBadRequest)
				return
			}
		}
		res := map[string][]string{"responses": responses}
		if len(errs) > 0 {
			res["errors"] = errs
		}
		writeJSON(w, http.StatusOK, res)
	})
	logger.Printf("Started HTTP server on port %d", *httpPort)
	err := serveHTTP(ctx, hc, "agnhost", mux, fmt.Sprintf(":%d", *httpPort))
	mu.Lock()
	code := exitWith
	mu.Unlock()
	if code >= 0 {
		return exitCode(code)
	}
	return err
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// --- stefanprodan/podinfo ---

func podinfoProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	fs := pflag.NewFlagSet("podinfo", pflag.ContinueOnError)
	fs.SetOutput(hc.Stderr)
	fs.ParseErrorsAllowlist.UnknownFlags = true
	// Every flag can also be set as PODINFO_<FLAG> (viper).
	envDef := func(name, def string) string {
		if v := getenv(hc, "PODINFO_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))); v != "" {
			return v
		}
		return def
	}
	version := "6.9.2"
	if t := strings.TrimPrefix(imageTag(ctx), "v"); fullVersion.MatchString(t) {
		version = t
	}
	port := fs.String("port", envDef("port", "9898"), "HTTP port")
	metricsPort := fs.Int("port-metrics", 0, "metrics port")
	fs.String("host", envDef("host", ""), "host to bind service to")
	fs.Int("grpc-port", 0, "gRPC port")
	fs.String("level", envDef("level", "info"), "log level")
	fs.Bool("random-delay", false, "between 0 and 5 seconds random delay by default")
	fs.Bool("random-error", false, "1/3 chances of a random response error")
	uiMessage := fs.String("ui-message", envDef("ui-message", "greetings from podinfo v"+version), "UI message")
	uiColor := fs.String("ui-color", envDef("ui-color", "#34577c"), "UI color")
	uiLogo := fs.String("ui-logo", envDef("ui-logo", "https://raw.githubusercontent.com/stefanprodan/podinfo/gh-pages/cuddle_clap.gif"), "UI logo")
	fs.String("backend-url", "", "backend service URL")
	ver := fs.Bool("version", false, "get version number")
	if err := fs.Parse(args); err != nil {
		return interp.ExitStatus(2)
	}
	if *ver {
		fmt.Fprintln(hc.Stdout, version)
		return nil
	}
	if v := getenv(hc, "PODINFO_PORT_METRICS"); v != "" && *metricsPort == 0 {
		*metricsPort, _ = strconv.Atoi(v)
	}
	const revision = "tabernetes"
	hostname := podHostname(ctx)
	logger := newLogger(hc.Stdout, 0)
	zap := func(msg string, kv ...string) {
		var b strings.Builder
		fmt.Fprintf(&b, `{"level":"info","ts":"%s","caller":"podinfo/main.go:153","msg":%q`, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), msg)
		for i := 0; i+1 < len(kv); i += 2 {
			fmt.Fprintf(&b, ",%q:%q", kv[i], kv[i+1])
		}
		b.WriteString("}")
		logger.Print(b.String())
	}
	var mu sync.Mutex
	ready, healthy := true, true
	var requests int
	info := func() map[string]string {
		return map[string]string{
			"hostname": hostname, "version": version, "revision": revision, "color": *uiColor, "logo": *uiLogo,
			"message": *uiMessage, "goos": "linux", "goarch": "wasm", "runtime": runtime.Version(),
			"num_goroutine": strconv.Itoa(runtime.NumGoroutine()), "num_cpu": strconv.Itoa(runtime.NumCPU()),
		}
	}
	status := func(w http.ResponseWriter, ok bool) {
		if ok {
			writeJSON(w, http.StatusOK, map[string]string{"status": "OK"})
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "message": "not found"})
			return
		}
		if strings.Contains(r.Header.Get("Accept"), "text/html") && !strings.Contains(r.UserAgent(), "curl") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprintf(w, "<!DOCTYPE html><html><head><title>%s</title></head><body style=\"background:%s;color:#fff;font-family:sans-serif;text-align:center\"><h1>%s</h1><p>%s</p><p>podinfo v%s</p></body></html>\n",
				hostname, *uiColor, *uiMessage, hostname, version)
			return
		}
		writeJSON(w, http.StatusOK, info())
	})
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, info()) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"commit": revision, "version": version})
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { mu.Lock(); ok := healthy; mu.Unlock(); status(w, ok) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) { mu.Lock(); ok := ready; mu.Unlock(); status(w, ok) })
	toggle := func(p *bool, v bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			*p = v
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
		}
	}
	mux.HandleFunc("/readyz/enable", toggle(&ready, true))
	mux.HandleFunc("/readyz/disable", toggle(&ready, false))
	mux.HandleFunc("/healthz/enable", toggle(&healthy, true))
	mux.HandleFunc("/healthz/disable", toggle(&healthy, false))
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, environ(hc)) })
	mux.HandleFunc("/api/env", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, environ(hc)) })
	mux.HandleFunc("/headers", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, r.Header) })
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.WriteHeader(http.StatusAccepted)
		w.Write(b)
	})
	mux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.WriteHeader(http.StatusAccepted)
		w.Write(b)
	})
	mux.HandleFunc("/status/{code}", func(w http.ResponseWriter, r *http.Request) {
		code, err := strconv.Atoi(r.PathValue("code"))
		if err != nil || code < 100 || code > 599 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "message": "invalid status code"})
			return
		}
		writeJSON(w, code, map[string]int{"status": code})
	})
	mux.HandleFunc("/delay/{seconds}", func(w http.ResponseWriter, r *http.Request) {
		s, err := strconv.Atoi(r.PathValue("seconds"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "message": "invalid seconds"})
			return
		}
		sleepCtx(r.Context(), time.Duration(min(s, 300))*time.Second)
		writeJSON(w, http.StatusOK, map[string]int{"delay": s})
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := requests
		mu.Unlock()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "# HELP http_requests_total The total number of HTTP requests.\n# TYPE http_requests_total counter\nhttp_requests_total %d\n# HELP podinfo_info Podinfo version.\n# TYPE podinfo_info gauge\npodinfo_info{version=%q} 1\n", n, version)
	})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	exitWith := 0
	mux.HandleFunc("/panic", func(w http.ResponseWriter, r *http.Request) {
		zap("Panic command received")
		exitWith = 255
		cancel()
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
	zap("Starting podinfo", "version", version, "revision", revision, "port", *port)
	if *metricsPort > 0 {
		go serveHTTP(ctx, hc, "podinfo", mux, fmt.Sprintf(":%d", *metricsPort))
		zap("Starting HTTP Server.", "addr", fmt.Sprintf(":%d", *metricsPort))
	}
	zap("Starting HTTP Server.", "addr", ":"+*port)
	err := serveHTTP(ctx, hc, "podinfo", h, ":"+*port)
	if exitWith != 0 {
		return exitCode(exitWith)
	}
	return err
}

// --- tutorial apps ---

// nodeProg is node of the Kubernetes bootcamp image, whose CMD is
// "node server.js". There is no other node in the tab.
func nodeProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if !imageIs(ctx, "kubernetes-bootcamp") || len(args) != 1 || path.Base(args[0]) != "server.js" {
		if len(args) > 0 && args[0] == "--version" && imageIs(ctx, "kubernetes-bootcamp") {
			fmt.Fprintln(hc.Stdout, "v6.3.0")
			return nil
		}
		fmt.Fprintln(hc.Stderr, "sh: node: not found")
		return interp.ExitStatus(127)
	}
	v := strings.TrimPrefix(imageTag(ctx), "v")
	if v == "" || v == "latest" {
		v = "1"
	}
	hostname := podHostname(ctx)
	start := time.Now()
	var mu sync.Mutex
	n := 0
	fmt.Fprintf(hc.Stdout, "Kubernetes Bootcamp App Started At: %s | Running On:  %s \n\n", start.UTC().Format(time.RFC3339Nano), hostname)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		c := n
		mu.Unlock()
		fmt.Fprintf(hc.Stdout, "Running On: %s | Total Requests: %d | App Uptime: %.3f seconds | Log Time: %s\n", hostname, c, time.Since(start).Seconds(), time.Now().UTC().Format(time.RFC3339Nano))
		fmt.Fprintf(w, "Hello Kubernetes bootcamp! | Running on: %s | v=%s\n", hostname, v)
	})
	return serveHTTP(ctx, hc, "node", h, ":8080")
}

func helloAppProg(ctx context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
	port := getenv(hc, "PORT")
	if port == "" {
		port = "8080"
	}
	v := imageTag(ctx)
	if v == "" || v == "latest" {
		v = "1.0"
	}
	if !fullVersion.MatchString(v) {
		v += ".0"
	}
	hostname := podHostname(ctx)
	logger := newLogger(hc.Stderr, log.LstdFlags)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Printf("Serving request: %s", r.URL.Path)
		fmt.Fprintf(w, "Hello, world!\nVersion: %s\nHostname: %s\n", v, hostname)
	})
	logger.Printf("Server listening on port %s", port)
	return serveHTTP(ctx, hc, "hello-app", h, ":"+port)
}

func helloWorldProg(_ context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
	fmt.Fprint(hc.Stdout, `
Hello from Docker!
This message shows that your installation appears to be working correctly.

(Well, almost: this is the hello-world image built into Tabernetes. It ran as a
pod on the node inside your browser tab, no Docker involved.)

To generate this message, Tabernetes took the following steps:
 1. The scheduler placed the pod on the node "tab".
 2. The tab node found "hello-world" among its built-in images.
 3. It ran the program of the image, which produced this output.
 4. The output went to the container log, where kubectl logs reads it.

To try something more ambitious, you can run an nginx deployment with:
 $ kubectl create deployment web --image=nginx --replicas=3

For more examples and ideas, visit:
 https://kubernetes.io/docs/tutorials/
`)
	return nil
}

// hpaExampleProg is registry.k8s.io/hpa-example: php with apache, whose
// index.php burns some CPU and says OK!.
func hpaExampleProg(ctx context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
	if !imageIs(ctx, "hpa-example") {
		return httpdProg(ctx, hc, nil, nil)
	}
	ip := podIP(ctx)
	fmt.Fprintf(hc.Stderr, "AH00558: apache2: Could not reliably determine the server's fully qualified domain name, using %s. Set the 'ServerName' directive globally to suppress this message\n", ip)
	fmt.Fprintf(hc.Stderr, "[%s] [mpm_prefork:notice] [pid 1] AH00163: Apache/2.4.10 (Debian) PHP/5.6.14 configured -- resuming normal operations\n", apacheTime(time.Now().UTC()))
	logger := newLogger(hc.Stdout, 0)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The real page computes square roots for a while; a short sleep
		// keeps the tab responsive instead.
		sleepCtx(r.Context(), 20*time.Millisecond)
		w.Header().Set("Server", "Apache/2.4.10 (Debian)")
		w.Header().Set("X-Powered-By", "PHP/5.6.14")
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "OK!")
		logger.Printf("%s - - [%s] \"%s %s %s\" 200 3 \"%s\" \"%s\"", remoteHost(r), time.Now().UTC().Format("02/Jan/2006:15:04:05 -0700"), r.Method, r.URL.RequestURI(), r.Proto, orDash(r.Referer()), orDash(r.UserAgent()))
	})
	return serveHTTP(ctx, hc, "apache2", h, ":80")
}
