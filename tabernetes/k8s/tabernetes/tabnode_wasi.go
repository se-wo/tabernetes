package main

// WebAssembly pods. A pod with runtimeClassName wasmtime runs its WASI
// preview 1 module in a Web Worker of the browser, like runwasi's
// containerd-shim-wasmtime runs it on a real node. Every module gets a
// thread and a memory of its own, the control plane stays on the main
// thread. The page provides the runtime as globalThis.tabernetesWasi
// (web/src/wasi.js):
//
//	mirror: string                        base URL of the same-origin OCI mirror, "" for none
//	load(source): Promise<{module} | {component} | {unsupported: string}>
//	start({module, args, env, preopens, stdin}): {onStdout, onStderr, onExit, write, kill}
//	start({component: true, name}): {onMessage, post, kill, close}
//
// A WASI preview 2 component runs in a pod worker of its own, which talks
// with the Go side only through the messages of tabnode_wasi_msg.go
// (docs/wasi-sockets.md, section 8.3). This file runs a command component
// (wasi:cli/run) or the function of an entrypoint, with the statuses of
// runwasi. A proxy component (wasi:http/incoming-handler) gets the proxy
// server of tabnode_wasi_proxy.go.
//
// Browsers cannot pull from most registries: ghcr.io and Docker Hub send no
// CORS headers. Images come from a mirror next to the page instead, a
// static directory with a registry-like layout (wasm-images/build.sh):
//
//	<mirror>/<registry>/<repository>/manifests/<tag>   a manifest or an index
//	<mirror>/blobs/sha256/<hex>                        manifests, configs and layers by digest
//
// An image that is not in the mirror is pulled from its registry with the
// OCI distribution API, which works for registries that send CORS headers.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall/js"

	v1 "k8s.io/api/core/v1"
)

// wasmRuntimeClasses are the handlers of runwasi's shims. All of them run
// the module in the browser.
var wasmRuntimeClasses = map[string]bool{"wasmtime": true, "wasmedge": true, "wasmer": true, "wamr": true, "wasm": true}

// isWasmPod reports whether the pod runs WebAssembly modules.
func isWasmPod(pod *v1.Pod) bool {
	return pod.Spec.RuntimeClassName != nil && wasmRuntimeClasses[*pod.Spec.RuntimeClassName]
}

// Media types of layers that are a wasm module themselves (wasm OCI
// artifacts). runwasi takes the first two, the others come from the CNCF
// wasm OCI artifact spec and older tools (wasm-to-oci).
var wasmLayerTypes = map[string]bool{
	"application/vnd.bytecodealliance.wasm.component.layer.v0+wasm": true,
	"application/wasm":                                  true,
	"application/vnd.wasm.content.layer.v1+wasm":        true,
	"application/vnd.w3c.wasm.module.v1+wasm":           true,
	"application/vnd.module.wasm.content.layer.v1+wasm": true,
}

// Media types of file system layers. runwasi images keep the .wasm file in
// such a layer and name it in the entrypoint.
var tarLayerTypes = map[string]string{
	"application/vnd.oci.image.layer.v1.tar":                       "tar",
	"application/vnd.oci.image.layer.v1.tar+gzip":                  "tar+gzip",
	"application/vnd.docker.image.rootfs.diff.tar.gzip":            "tar+gzip",
	"application/vnd.oci.image.layer.nondistributable.v1.tar":      "tar",
	"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip": "tar+gzip",
}

// wasmImage is a resolved image: where its module is and how to run it.
type wasmImage struct {
	ref        string
	id         string // repository@digest of the manifest
	entrypoint []string
	cmd        []string
	env        []string
	workdir    string
	artifact   bool        // the module is a layer, not a file in the layers
	title      string      // file name of the module layer, if annotated
	layers     []wasmLayer // the module layer, or the tar layers from the bottom up
	headers    map[string]string
}

type wasmLayer struct {
	url, digest, kind string // kind: wasm, tar, tar+gzip
}

var wasmImages = struct {
	sync.Mutex
	m map[string]*wasmImage
}{m: map[string]*wasmImage{}}

// wasmImageID is the imageID of a container status for a pulled wasm image,
// "" before the pull.
func wasmImageID(ref string) string {
	wasmImages.Lock()
	defer wasmImages.Unlock()
	if img := wasmImages.m[ref]; img != nil {
		return img.id
	}
	return ""
}

// startWasm runs the container as a WASI module in a Web Worker.
func (c *ctr) startWasm(ctx context.Context, logs *logBuf) (*process, error) {
	rt := js.Global().Get("tabernetesWasi")
	if rt.Type() != js.TypeObject {
		return nil, errors.New("this page has no WebAssembly runtime (globalThis.tabernetesWasi), so it cannot run pods with runtimeClassName " + c.runtimeClass() + ". Join a Docker node with runwasi instead.")
	}
	// ctx ends with the kill; the pull already ends when the pod stops.
	pullCtx, stopPull := context.WithCancel(ctx)
	unhook := context.AfterFunc(c.w.ctx, stopPull)
	img, res, err := c.pullWasm(pullCtx, rt)
	unhook()
	stopPull()
	if err != nil {
		return nil, err
	}

	c.w.mu.Lock()
	argv, env, dir, root := c.argv, c.env, c.dir, c.root
	c.w.mu.Unlock()
	// The command of the container replaces the entrypoint of the image,
	// its args the cmd. The first word names the module file, like in
	// runwasi, and is argv[0] of the module.
	if len(c.spec.Command) == 0 {
		args := img.cmd
		if len(c.spec.Args) > 0 {
			args = argv
		}
		argv = append(append([]string{}, img.entrypoint...), args...)
		if img.artifact && len(img.entrypoint) == 0 {
			argv = append([]string{img.moduleName()}, args...)
		}
	}
	if c.spec.WorkingDir == "" && img.workdir != "" {
		dir = img.workdir
	}
	// Image env first, the container overrides it. Go and wasi-libc
	// programs find their working directory in PWD.
	vars := map[string]bool{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		vars[k] = true
	}
	var all []string
	for _, kv := range img.env {
		if k, _, _ := strings.Cut(kv, "="); !vars[k] {
			all = append(all, kv)
		}
	}
	all = append(all, env...)
	if !vars["PWD"] {
		all = append(all, "PWD="+dir)
	}

	// A module and a component get the same copy of the files (FS1, FS4).
	preopens, err := wasmPreopens(root)
	if err != nil {
		return nil, err
	}
	if comp := res.Get("component"); comp.Type() == js.TypeObject {
		return c.startComponent(ctx, logs, rt, comp, argv, all, dir, preopens)
	}
	opts := js.Global().Get("Object").New()
	opts.Set("module", res.Get("module"))
	opts.Set("args", jsStrings(argv))
	opts.Set("env", jsStrings(all))
	opts.Set("preopens", preopens)
	opts.Set("stdin", c.stdin != nil)
	opts.Set("name", c.w.namespace+"/"+c.w.name+"/"+c.spec.Name)
	// The WebGPU device of its claims, if any (tabnode_gpu.go).
	opts.Set("gpu", c.w.gpuForWorker(c.spec.Name))

	ctx, cancel := context.WithCancel(ctx)
	p := newProcess(cancel)
	h := rt.Call("start", opts)
	var funcs []js.Func
	cb := func(f func(a []js.Value)) js.Func {
		fn := js.FuncOf(func(_ js.Value, a []js.Value) any { f(a); return nil })
		funcs = append(funcs, fn)
		return fn
	}
	stdout, stderr := logs.writer("stdout"), logs.writer("stderr")
	h.Call("onStdout", cb(func(a []js.Value) { stdout.Write(jsBytes(a[0])) }))
	h.Call("onStderr", cb(func(a []js.Value) { stderr.Write(jsBytes(a[0])) }))
	// The linear memory of the module counts for limits.memory.
	h.Call("onMem", cb(func(a []js.Value) { go c.chargeMem(p, "wasm", int64(a[0].Int())) }))
	h.Call("onExit", cb(func(a []js.Value) {
		if len(a) > 1 && a[1].Type() == js.TypeString {
			stderr.Write([]byte(a[1].String() + "\n"))
		}
		go func() {
			for _, fn := range funcs {
				fn.Release()
			}
		}()
		cancel()
		p.exit(a[0].Int())
	}))
	// kill and the end of the pod terminate the worker: exit code 137.
	context.AfterFunc(ctx, func() { h.Call("kill") })
	if c.stdin != nil {
		go func() {
			r, buf := c.stdin.reader(ctx), make([]byte, 32<<10)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					arr := js.Global().Get("Uint8Array").New(n)
					js.CopyBytesToJS(arr, buf[:n])
					h.Call("write", arr)
				}
				if err != nil {
					h.Call("write", js.Null())
					return
				}
			}
		}()
	}
	return p, nil
}

func (c *ctr) runtimeClass() string {
	pod, _ := c.w.podInfo()
	if pod.Spec.RuntimeClassName != nil {
		return *pod.Spec.RuntimeClassName
	}
	return ""
}

// pullWasm resolves the image and loads its module or component: the result
// of load, {module} or {component}. Pull errors are
// retried with a back-off like the kubelet does (ErrImagePull,
// ImagePullBackOff), so the error is a non-pull error or the end of ctx.
func (c *ctr) pullWasm(ctx context.Context, rt js.Value) (img *wasmImage, res js.Value, err error) {
	err = c.pullWithBackOff(ctx, func() error {
		var err error
		img, res, err = c.pullWasmOnce(ctx, rt)
		return err
	}, func(err error) bool {
		var unsupported unsupportedError
		return !errors.As(err, &unsupported)
	})
	if err != nil {
		return nil, js.Undefined(), err
	}
	return img, res, nil
}

type unsupportedError struct{ msg string }

func (e unsupportedError) Error() string { return e.msg }

func (c *ctr) pullWasmOnce(ctx context.Context, rt js.Value) (*wasmImage, js.Value, error) {
	ref := c.spec.Image
	wasmImages.Lock()
	img := wasmImages.m[ref]
	wasmImages.Unlock()
	if img == nil || (c.spec.ImagePullPolicy == v1.PullAlways && !strings.Contains(ref, "@")) {
		var err error
		if img, err = resolveWasmImage(ctx, ref, rt.Get("mirror").String()); err != nil {
			return nil, js.Undefined(), err
		}
	}
	src := img.source()
	// runwasi takes the function after "#" from the first word of the
	// command (path#function, CO3); the command of the container replaces
	// the entrypoint of the image. The file stays the file of the image.
	if fn := commandFunction(c.spec.Command); fn != "" && !img.artifact {
		p, _, _ := strings.Cut(src.Get("path").String(), "#")
		src.Set("path", p+"#"+fn)
	}
	res, err := await(ctx, rt.Call("load", src))
	if err != nil {
		return nil, js.Undefined(), fmt.Errorf("loading the module of %s: %v", ref, err)
	}
	if u := res.Get("unsupported"); u.Type() == js.TypeString {
		return nil, js.Undefined(), unsupportedError{fmt.Sprintf("%s: %s", ref, u.String())}
	}
	// Components start; startComponent gives the statuses of runwasi.
	wasmImages.Lock()
	wasmImages.m[ref] = img
	wasmImages.Unlock()
	return img, res, nil
}

// commandFunction gives the function after "#" in the first word of a
// command, or "".
func commandFunction(command []string) string {
	if len(command) == 0 {
		return ""
	}
	_, fn, _ := strings.Cut(command[0], "#")
	return fn
}

// source tells the page where the module is: a module layer, or a file in
// the tar layers (the topmost layer that has it wins).
func (img *wasmImage) source() js.Value {
	o := js.Global().Get("Object").New()
	layers := js.Global().Get("Array").New()
	for _, l := range img.layers {
		lo := js.Global().Get("Object").New()
		lo.Set("url", l.url)
		lo.Set("digest", l.digest)
		lo.Set("kind", l.kind)
		layers.Call("push", lo)
	}
	o.Set("layers", layers)
	headers := js.Global().Get("Object").New()
	for k, v := range img.headers {
		headers.Set(k, v)
	}
	o.Set("headers", headers)
	// The pod worker runs components (startComponent).
	o.Set("components", true)
	if !img.artifact {
		p := ""
		if len(img.entrypoint) > 0 {
			p = img.entrypoint[0]
		} else if len(img.cmd) > 0 {
			p = img.cmd[0]
		}
		if !path.IsAbs(p) {
			p = path.Join("/", img.workdir, p)
		}
		o.Set("path", strings.TrimPrefix(path.Clean(p), "/"))
	}
	return o
}

func (img *wasmImage) moduleName() string {
	if img.title != "" {
		return img.title
	}
	r, _ := parseImageRef(img.ref)
	return path.Base(r.repo) + ".wasm"
}

// imageRef is a parsed image reference, normalized like Docker does.
type imageRef struct{ registry, repo, tag, digest string }

func parseImageRef(s string) (imageRef, error) {
	var r imageRef
	if i := strings.Index(s, "@"); i >= 0 {
		s, r.digest = s[:i], s[i+1:]
		if h, ok := strings.CutPrefix(r.digest, "sha256:"); !ok || len(h) != 64 {
			return r, fmt.Errorf("invalid digest %q (only sha256 is supported)", r.digest)
		}
	}
	if i := strings.LastIndex(s, ":"); i > strings.LastIndex(s, "/") {
		s, r.tag = s[:i], s[i+1:]
	}
	if first, rest, ok := strings.Cut(s, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.registry, s = first, rest
	} else {
		r.registry = "docker.io"
	}
	if r.registry == "index.docker.io" || r.registry == "registry-1.docker.io" {
		r.registry = "docker.io"
	}
	if r.registry == "docker.io" && !strings.Contains(s, "/") {
		s = "library/" + s
	}
	r.repo = s
	if r.repo == "" {
		return r, fmt.Errorf("invalid image reference %q", s)
	}
	if r.tag == "" && r.digest == "" {
		r.tag = "latest"
	}
	return r, nil
}

func (r imageRef) reference() string {
	if r.digest != "" {
		return r.digest
	}
	return r.tag
}

// registryClient reads manifests and blobs from the mirror or a registry.
type registryClient struct {
	ref     imageRef
	mirror  string // base URL of the mirror, "" for the registry
	token   string
	scheme  string
	headers map[string]string
}

func (rc *registryClient) manifestURL(ref string) string {
	if rc.mirror != "" {
		if strings.HasPrefix(ref, "sha256:") {
			return rc.blobURL(ref)
		}
		return rc.mirror + rc.ref.registry + "/" + rc.ref.repo + "/manifests/" + url.PathEscape(ref)
	}
	return rc.registryURL() + "/manifests/" + ref
}

func (rc *registryClient) blobURL(digest string) string {
	if rc.mirror != "" {
		return rc.mirror + "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
	}
	return rc.registryURL() + "/blobs/" + digest
}

func (rc *registryClient) registryURL() string {
	host := rc.ref.registry
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	return rc.scheme + "://" + host + "/v2/" + rc.ref.repo
}

var errNotFound = errors.New("not found")

const manifestTypes = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

// get fetches a small document (a manifest or a config) and checks its
// digest when it is known.
func (rc *registryClient) get(ctx context.Context, u, accept, digest string) ([]byte, error) {
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accept)
		if rc.token != "" {
			req.Header.Set("Authorization", "Bearer "+rc.token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if rc.mirror == "" {
				return nil, corsError(rc.ref, err)
			}
			return nil, err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		switch {
		case resp.StatusCode == http.StatusUnauthorized && try == 0 && rc.mirror == "":
			if err := rc.login(ctx, resp.Header.Get("WWW-Authenticate")); err != nil {
				return nil, err
			}
			continue
		case resp.StatusCode == http.StatusNotFound:
			return nil, errNotFound
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
		}
		if digest != "" {
			if sum := sha256.Sum256(body); "sha256:"+hex.EncodeToString(sum[:]) != digest {
				return nil, fmt.Errorf("GET %s: the content does not match its digest %s", u, digest)
			}
		}
		return body, nil
	}
}

// login gets an anonymous token, the flow of the OCI distribution spec:
// the 401 names the token service in WWW-Authenticate.
func (rc *registryClient) login(ctx context.Context, challenge string) error {
	if challenge == "" {
		return fmt.Errorf("%s wants a login, and the browser does not see its WWW-Authenticate header (not in Access-Control-Expose-Headers)", rc.ref.registry)
	}
	scheme, params, _ := strings.Cut(challenge, " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("%s wants %s authentication, only anonymous bearer tokens work", rc.ref.registry, scheme)
	}
	p := map[string]string{}
	for _, kv := range splitChallenge(params) {
		k, v, _ := strings.Cut(kv, "=")
		p[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
	}
	if p["realm"] == "" {
		return fmt.Errorf("%s: no realm in %q", rc.ref.registry, challenge)
	}
	q := url.Values{}
	if p["service"] != "" {
		q.Set("service", p["service"])
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + rc.ref.repo + ":pull"
	}
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p["realm"]+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return corsError(rc.ref, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("anonymous token for %s: %s (a private image cannot be pulled in the browser)", rc.ref.repo, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return err
	}
	rc.token = t.Token
	if rc.token == "" {
		rc.token = t.AccessToken
	}
	rc.headers = map[string]string{"Authorization": "Bearer " + rc.token}
	return nil
}

// splitChallenge splits the parameters of a WWW-Authenticate header at the
// commas outside quotes.
func splitChallenge(s string) []string {
	var out []string
	quoted, start := false, 0
	for i, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func corsError(r imageRef, err error) error {
	return fmt.Errorf("the browser cannot reach %s: %v. Registries like ghcr.io and Docker Hub send no CORS headers, so a web page may not read from them. Put the image into the OCI mirror of the page (wasm-images/build.sh) or run the pod on a Docker node", r.registry, err)
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
	} `json:"platform"`
}

type ociManifest struct {
	MediaType string          `json:"mediaType"`
	Manifests []ociDescriptor `json:"manifests"` // an index
	Config    ociDescriptor   `json:"config"`
	Layers    []ociDescriptor `json:"layers"`
}

// resolveWasmImage reads the manifest and the config of an image, from the
// mirror if it has the image, else from the registry.
func resolveWasmImage(ctx context.Context, ref, mirror string) (*wasmImage, error) {
	r, err := parseImageRef(ref)
	if err != nil {
		return nil, err
	}
	var rc *registryClient
	var body []byte
	if mirror != "" {
		if !strings.HasSuffix(mirror, "/") {
			mirror += "/"
		}
		rc = &registryClient{ref: r, mirror: mirror}
		body, err = rc.get(ctx, rc.manifestURL(r.reference()), manifestTypes, r.digest)
		if err != nil && !errors.Is(err, errNotFound) {
			return nil, fmt.Errorf("the OCI mirror of the page: %v", err)
		}
	}
	if mirror == "" || errors.Is(err, errNotFound) {
		rc = &registryClient{ref: r, scheme: "https"}
		if r.registry == "localhost" || strings.HasPrefix(r.registry, "localhost:") || strings.HasPrefix(r.registry, "127.0.0.1:") {
			rc.scheme = "http"
		}
		body, err = rc.get(ctx, rc.manifestURL(r.reference()), manifestTypes, r.digest)
		if errors.Is(err, errNotFound) {
			where := r.registry
			if mirror != "" {
				where = "the OCI mirror of the page (" + strings.TrimSuffix(mirror, "/") + ") nor " + r.registry
			}
			return nil, fmt.Errorf("image %s not found: neither %s has it", ref, where)
		}
		if err != nil {
			return nil, err
		}
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])

	var m ociManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest of %s: %v", ref, err)
	}
	if len(m.Manifests) > 0 {
		d, err := pickWasmPlatform(m.Manifests)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", ref, err)
		}
		if body, err = rc.get(ctx, rc.manifestURL(d.Digest), manifestTypes, d.Digest); err != nil {
			return nil, fmt.Errorf("manifest %s of %s: %v", d.Digest, ref, err)
		}
		digest = d.Digest
		m = ociManifest{}
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("manifest of %s: %v", ref, err)
		}
	}
	img := &wasmImage{ref: ref, id: r.registry + "/" + r.repo + "@" + digest, headers: rc.headers}

	if m.Config.Digest != "" && m.Config.Size < 4<<20 {
		b, err := rc.get(ctx, rc.blobURL(m.Config.Digest), "*/*", m.Config.Digest)
		if err != nil {
			return nil, fmt.Errorf("config of %s: %v", ref, err)
		}
		var cfg struct {
			Config struct {
				Entrypoint []string `json:"Entrypoint"`
				Cmd        []string `json:"Cmd"`
				Env        []string `json:"Env"`
				WorkingDir string   `json:"WorkingDir"`
			} `json:"config"`
		}
		// The config of a wasm artifact may be anything, even "{}".
		if json.Unmarshal(b, &cfg) == nil {
			img.entrypoint, img.cmd, img.env, img.workdir = cfg.Config.Entrypoint, cfg.Config.Cmd, cfg.Config.Env, cfg.Config.WorkingDir
		}
	}

	for _, l := range m.Layers {
		if wasmLayerTypes[l.MediaType] {
			img.artifact = true
			img.title = l.Annotations["org.opencontainers.image.title"]
			img.layers = []wasmLayer{{url: rc.blobURL(l.Digest), digest: l.Digest, kind: "wasm"}}
			// The layer named by the entrypoint wins, else the first.
			if len(img.entrypoint) == 0 || path.Base(img.entrypoint[0]) == img.title {
				break
			}
		}
	}
	if !img.artifact {
		for _, l := range m.Layers {
			kind, ok := tarLayerTypes[l.MediaType]
			if !ok {
				if strings.Contains(l.MediaType, "zstd") {
					return nil, unsupportedError{fmt.Sprintf("%s: zstd compressed layers are not supported in the browser (use gzip)", ref)}
				}
				continue
			}
			img.layers = append(img.layers, wasmLayer{url: rc.blobURL(l.Digest), digest: l.Digest, kind: kind})
		}
		if len(img.layers) == 0 {
			return nil, unsupportedError{fmt.Sprintf("%s has no wasm module layer and no file system layer", ref)}
		}
		if len(img.entrypoint) == 0 && len(img.cmd) == 0 {
			return nil, unsupportedError{fmt.Sprintf("%s: the image config has no entrypoint that names the .wasm file", ref)}
		}
	}
	return img, nil
}

// pickWasmPlatform chooses the wasm image of an index.
func pickWasmPlatform(ds []ociDescriptor) (ociDescriptor, error) {
	var seen []string
	for _, d := range ds {
		if d.Platform == nil {
			continue
		}
		if d.Platform.Architecture == "wasm" || d.Platform.OS == "wasip1" || d.Platform.OS == "wasi" {
			return d, nil
		}
		seen = append(seen, d.Platform.OS+"/"+d.Platform.Architecture)
	}
	if len(ds) == 1 {
		return ds[0], nil
	}
	return ociDescriptor{}, fmt.Errorf("the image index has no wasm platform (only %s)", strings.Join(seen, ", "))
}

// wasmSnapshotLimit bounds the files copied into a worker.
const wasmSnapshotLimit = 64 << 20

// wasmPreopens copies the file system of the container for the module:
// "/" is its root (the files every container has), each directory volume
// mount is a preopened directory of its own. The copies live in the worker:
// writes of the module stay there and end with it.
func wasmPreopens(root *rootFS) (js.Value, error) {
	list := js.Global().Get("Array").New()
	total := 0
	type pre struct {
		guest    string
		readOnly bool
		files    js.Value
		dirs     js.Value
	}
	newPre := func(guest string, ro bool) *pre {
		p := &pre{guest: guest, readOnly: ro, files: js.Global().Get("Array").New(), dirs: js.Global().Get("Array").New()}
		o := js.Global().Get("Object").New()
		o.Set("guest", guest)
		o.Set("readOnly", ro)
		o.Set("files", p.files)
		o.Set("dirs", p.dirs)
		list.Call("push", o)
		return p
	}
	addFile := func(p *pre, rel, real string) error {
		b, err := os.ReadFile(real)
		if err != nil {
			return err
		}
		if total += len(b); total > wasmSnapshotLimit {
			return fmt.Errorf("the volumes of the container are larger than %d MiB, too large to copy into the worker", wasmSnapshotLimit>>20)
		}
		arr := js.Global().Get("Uint8Array").New(len(b))
		js.CopyBytesToJS(arr, b)
		p.files.Call("push", jsArray(rel, arr))
		return nil
	}
	// A volume is links (key -> ..data/key, ..data -> ..<ts>): follow them and
	// skip the ".." entries, which are the plumbing of the atomic writer.
	var walk func(p *pre, dir, prefix string) error
	walk = func(p *pre, dir, prefix string) error {
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			dir = r
		}
		return filepath.WalkDir(dir, func(real string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, real)
			if rel == "." {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(d.Name(), "..") {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if prefix != "" {
				rel = prefix + "/" + rel
			}
			if d.Type()&fs.ModeSymlink != 0 {
				st, err := os.Stat(real)
				if err != nil {
					return err
				}
				if st.IsDir() {
					r, err := filepath.EvalSymlinks(real)
					if err != nil {
						return err
					}
					p.dirs.Call("push", rel)
					return walk(p, r, rel)
				}
				return addFile(p, rel, real)
			}
			if d.IsDir() {
				p.dirs.Call("push", rel)
				return nil
			}
			return addFile(p, rel, real)
		})
	}
	rootPre := newPre("/", false)
	if err := walk(rootPre, root.base, ""); err != nil {
		return js.Undefined(), err
	}
	// A module finds the preopen with the longest prefix of a path.
	for _, m := range root.mounts {
		st, err := os.Stat(m.real)
		if err != nil {
			return js.Undefined(), fmt.Errorf("mount %s: %v", m.path, err)
		}
		if !st.IsDir() {
			// A file (subPath): it goes into the root.
			if err := addFile(rootPre, strings.TrimPrefix(m.path, "/"), m.real); err != nil {
				return js.Undefined(), err
			}
			continue
		}
		if err := walk(newPre(m.path, m.readOnly), m.real, ""); err != nil {
			return js.Undefined(), err
		}
	}
	return list, nil
}

func jsStrings(s []string) js.Value {
	a := js.Global().Get("Array").New()
	for _, x := range s {
		a.Call("push", x)
	}
	return a
}

func jsArray(v ...any) js.Value {
	a := js.Global().Get("Array").New()
	for _, x := range v {
		a.Call("push", x)
	}
	return a
}

func jsBytes(v js.Value) []byte {
	b := make([]byte, v.Get("length").Int())
	js.CopyBytesToGo(b, v)
	return b
}

// The interfaces that the pod worker gives a command component and the
// function of an entrypoint, by "<package>/<interface>". It is the linker of
// runwasi for these worlds (fact 5): WASI 0.2 without wasi:http. Each import
// must have a version 0.2.x.
var componentImports = map[string]bool{
	"cli/environment": true, "cli/exit": true, "cli/stdin": true, "cli/stdout": true, "cli/stderr": true,
	"cli/terminal-input": true, "cli/terminal-output": true, "cli/terminal-stdin": true, "cli/terminal-stdout": true, "cli/terminal-stderr": true,
	"clocks/monotonic-clock": true, "clocks/wall-clock": true,
	"filesystem/preopens": true, "filesystem/types": true,
	"io/error": true, "io/poll": true, "io/streams": true,
	"random/random": true, "random/insecure": true, "random/insecure-seed": true,
	"sockets/instance-network": true, "sockets/network": true, "sockets/ip-name-lookup": true,
	"sockets/tcp": true, "sockets/tcp-create-socket": true, "sockets/udp": true, "sockets/udp-create-socket": true,
}

var componentImportRE = regexp.MustCompile(`^wasi:([a-z0-9-]+/[a-z0-9-]+)@0\.2\.[0-9]+$`)

// componentProxyImports are the interfaces that the linker of runwasi has
// for a proxy component in addition to componentImports (fact 4,
// instance.rs lines 263 to 265). A command component does not get them
// (CR6).
var componentProxyImports = map[string]bool{"http/types": true, "http/outgoing-handler": true}

// componentDefaultFunction is the function that runwasi calls when the
// entrypoint names no function (path#function).
const componentDefaultFunction = "_start"

// componentStatus gives the text of runwasi for a component that cannot
// start, or "". world is "command", "proxy" or "" (the world of
// component_world.js), imports are
// the names of the component with their versions, exports its exports as
// [name, kind], and fn the function of the entrypoint ("" for none).
//
// runwasi instantiates first: an import that its linker does not have gives
// "component imports instance ..." (CO11, CO12, CR6). Then, without
// wasi:cli/run, it looks for the function (CO4). The task exits with 137
// (section 9.2). A proxy component also gets wasi:http/types and
// wasi:http/outgoing-handler, and needs no function.
func componentStatus(world string, imports []string, exports [][2]string, fn string) string {
	for _, i := range imports {
		m := componentImportRE.FindStringSubmatch(i)
		if m == nil || !(componentImports[m[1]] || world == "proxy" && componentProxyImports[m[1]]) {
			return fmt.Sprintf("component imports instance `%s`, but a matching implementation was not found in the linker", i)
		}
	}
	if world == "command" || world == "proxy" {
		return ""
	}
	if fn == "" {
		fn = componentDefaultFunction
	}
	for _, e := range exports {
		if e[0] == fn && e[1] == "function" {
			return ""
		}
	}
	return fmt.Sprintf("component does not have exported function %q", fn)
}

func jsStringList(v js.Value) []string {
	if v.Type() != js.TypeObject {
		return nil
	}
	out := make([]string, v.Length())
	for i := range out {
		out[i] = v.Index(i).String()
	}
	return out
}

// componentExports gives the [[name, kind]] of the names of a component.
func componentExports(v js.Value) [][2]string {
	if v.Type() != js.TypeObject {
		return nil
	}
	out := make([][2]string, v.Length())
	for i := range out {
		out[i] = [2]string{v.Index(i).Index(0).String(), v.Index(i).Index(1).String()}
	}
	return out
}

// wasiCLIHandlers handle the CLI messages of a pod worker: they go to the
// container (section 8.3).
var wasiCLIHandlers = map[string]wasiHandler{
	"stdout": func(p *wasiPeer, m wasiMsg) {
		if p.cli != nil {
			p.cli.stdout.Write(m.Bytes("bytes"))
		}
	},
	"stderr": func(p *wasiPeer, m wasiMsg) {
		if p.cli != nil {
			p.cli.stderr.Write(m.Bytes("bytes"))
		}
	},
	"exit": func(p *wasiPeer, m wasiMsg) {
		if p.cli != nil {
			msg := ""
			if m.Has("error") {
				msg = m.Str("error")
			}
			p.cli.exit(m.Int("code"), msg)
		}
	},
	"mem": func(p *wasiPeer, m wasiMsg) {
		if p.cli != nil && p.cli.mem != nil {
			p.cli.mem(int64(m.Float("size")))
		}
	},
}

func init() {
	for typ, h := range wasiCLIHandlers {
		wasiHandle(typ, h)
	}
}

// startComponent runs a command component, or the function of the
// entrypoint of a component, in a pod worker of its own. The pod worker gets
// the component and its CLI in the message "start"; all other traffic goes
// through the messages of tabnode_wasi_msg.go.
//
// A component that runwasi cannot start (CO4, CO11, CO12, CR6) starts no
// pod worker. The container starts, writes the text of runwasi to stderr and
// exits with 137, as the task of runwasi does (section 9.2). runwasi writes
// the text only to the log of the shim; the tab node writes it to the log of
// the container, so that the name of the import shows (CO12).
//
// preopens is the copy of wasmPreopens: the pod worker gives it to the
// component through its files adapter (files.js, section 8.10).
func (c *ctr) startComponent(ctx context.Context, logs *logBuf, rt, comp js.Value, argv, env []string, dir string, preopens js.Value) (*process, error) {
	name := c.w.namespace + "/" + c.w.name + "/" + c.spec.Name
	names := comp.Get("names")
	world := ""
	if w := comp.Get("world"); w.Type() == js.TypeString {
		world = w.String()
	}
	fn := ""
	if e := comp.Get("entrypoint"); e.Type() == js.TypeString {
		fn = e.String()
	}
	if names.Type() != js.TypeObject {
		return nil, errors.New("the transpile result of the component has no names")
	}
	if why := componentStatus(world, jsStringList(names.Get("imports")), componentExports(names.Get("exports")), fn); why != "" {
		_, cancel := context.WithCancel(ctx)
		p := newProcess(cancel)
		logs.writer("stderr").Write([]byte(why + "\n"))
		cancel()
		p.exit(wasiErrorExit)
		return p, nil
	}
	opts := js.Global().Get("Object").New()
	opts.Set("component", true)
	opts.Set("name", name)

	ctx, cancel := context.WithCancel(ctx)
	p := newProcess(cancel)
	// A proxy component (section 8.6): the proxy server listens before the
	// pod worker starts. Its context has the procEnv of the pod, for
	// listenPod, NetworkPolicy and the account of the container (AC1).
	var proxyLn net.Listener
	var proxyCtx context.Context
	if world == "proxy" {
		_, ip := c.w.podInfo()
		proxyCtx = withProc(ctx, &procEnv{w: c.w, c: c, ip: ip, cpu: newThrottle(c.spec.Resources), proc: p})
		var why string
		proxyLn, env, why = startProxy(proxyCtx, env, ip)
		if why != "" {
			// PX33: the text of runwasi, and the exit code 137.
			logs.writer("stderr").Write([]byte(why + "\n"))
			cancel()
			p.exit(wasiErrorExit)
			return p, nil
		}
		// runwasi gives an instance no arguments and no preopened
		// directories (fact 4, PX13).
		argv, preopens = []string{}, js.Undefined()
	}
	h := rt.Call("start", opts)
	peer := newWasiPeer(name, func(msg, transfer js.Value) { h.Call("post", msg, transfer) })
	stdout, stderr := logs.writer("stdout"), logs.writer("stderr")
	var onMsg js.Func
	var once sync.Once
	peer.cli = &wasiCLI{
		stdout: stdout,
		stderr: stderr,
		exit: func(code int, msg string) {
			once.Do(func() {
				if msg != "" {
					stderr.Write([]byte(msg + "\n"))
				}
				// No more messages: the pod worker ends here.
				h.Call("close")
				peer.Close()
				go onMsg.Release()
				cancel()
				p.exit(code)
			})
		},
		// The memory of the instance counts for limits.memory.
		mem: func(n int64) { go c.chargeMem(p, "wasm", n) },
	}
	onMsg = js.FuncOf(func(_ js.Value, a []js.Value) any {
		if len(a) > 0 {
			peer.dispatch(a[0])
		}
		return nil
	})
	h.Call("onMessage", onMsg)
	// The cpu messages go to the account of this container (AC2).
	peer.State(wasiCPUState, func() any { return c })
	var px *wasiProxy
	if proxyLn != nil {
		px = newWasiProxy(proxyCtx, peer, func(code int) { peer.cli.exit(code, "") })
	}
	start := map[string]any{"component": comp, "args": argv, "env": env, "cwd": dir}
	if preopens.Type() != js.TypeUndefined {
		start["preopens"] = preopens
	}
	if err := peer.send("start", start); err != nil {
		peer.cli.exit(128, "the pod worker cannot start: "+err.Error())
		return p, nil
	}
	if px != nil {
		px.serve(proxyLn)
	}
	// stdin of the component (CO5). Without stdin in the container spec,
	// stdin ends at once, as /dev/null does.
	if c.stdin == nil {
		peer.send("stdin", map[string]any{"bytes": []byte{}, "end": true})
	} else {
		go func() {
			r, buf := c.stdin.reader(ctx), make([]byte, 32<<10)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					if peer.send("stdin", map[string]any{"bytes": append([]byte(nil), buf[:n]...), "end": false}) != nil {
						return
					}
				}
				if err != nil {
					peer.send("stdin", map[string]any{"bytes": []byte{}, "end": true})
					return
				}
			}
		}()
	}
	// kill and the end of the pod terminate the pod worker: exit code 137.
	context.AfterFunc(ctx, func() { h.Call("kill") })
	return p, nil
}

// wasiErrorExit is the exit code of runwasi for each error of a component
// that is not an exit (executor.rs of containerd-shim-wasm v0.6.1).
const wasiErrorExit = 137
