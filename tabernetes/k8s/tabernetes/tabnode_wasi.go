package main

// WebAssembly pods. A pod with runtimeClassName wasmtime runs its WASI
// preview 1 module in a Web Worker of the browser, like runwasi's
// containerd-shim-wasmtime runs it on a real node. Every module gets a
// thread and a memory of its own, the control plane stays on the main
// thread. The page provides the runtime as globalThis.tabernetesWasi
// (web/src/wasi.js):
//
//	mirror: string                        base URL of the same-origin OCI mirror, "" for none
//	load(source): Promise<{module} | {unsupported: string}>
//	start({module, args, env, preopens, stdin}): {onStdout, onStderr, onExit, write, kill}
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
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
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
	img, mod, err := c.pullWasm(pullCtx, rt)
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

	preopens, err := wasmPreopens(root)
	if err != nil {
		return nil, err
	}
	opts := js.Global().Get("Object").New()
	opts.Set("module", mod)
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

// pullWasm resolves the image and loads its module. Pull errors are
// retried with a back-off like the kubelet does (ErrImagePull,
// ImagePullBackOff), so the error is a non-pull error or the end of ctx.
func (c *ctr) pullWasm(ctx context.Context, rt js.Value) (img *wasmImage, mod js.Value, err error) {
	err = c.pullWithBackOff(ctx, func() error {
		var err error
		img, mod, err = c.pullWasmOnce(ctx, rt)
		return err
	}, func(err error) bool {
		var unsupported unsupportedError
		return !errors.As(err, &unsupported)
	})
	if err != nil {
		return nil, js.Undefined(), err
	}
	return img, mod, nil
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
	res, err := await(ctx, rt.Call("load", img.source()))
	if err != nil {
		return nil, js.Undefined(), fmt.Errorf("loading the module of %s: %v", ref, err)
	}
	if u := res.Get("unsupported"); u.Type() == js.TypeString {
		return nil, js.Undefined(), unsupportedError{fmt.Sprintf("%s: %s", ref, u.String())}
	}
	wasmImages.Lock()
	wasmImages.m[ref] = img
	wasmImages.Unlock()
	return img, res.Get("module"), nil
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
	walk := func(p *pre, dir string) error {
		return filepath.WalkDir(dir, func(real string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dir, real)
			if rel == "." {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				p.dirs.Call("push", rel)
				return nil
			}
			return addFile(p, rel, real)
		})
	}
	rootPre := newPre("/", false)
	if err := walk(rootPre, root.base); err != nil {
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
		if err := walk(newPre(m.path, m.readOnly), m.real); err != nil {
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
