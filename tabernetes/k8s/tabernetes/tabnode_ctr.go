package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// ctr is one container of a pod. Its fields are guarded by the mutex of the
// pod worker.
type ctr struct {
	w       *podWorker
	spec    v1.Container
	init    bool // a classic init container: runs once, before the others
	sidecar bool // an init container with restartPolicy Always

	state     v1.ContainerState
	last      v1.ContainerState
	restarts  int32
	ready     bool
	startupOK bool
	finished  bool // terminated and not restarted again
	id        string
	logs      *logBuf
	prev      *logBuf
	runStart  time.Time
	delay     time.Duration // the next crash loop back-off
	proc      *process
	stdin     *stdinHub

	// set by prepare
	image *builtinImage
	wasm  bool
	env   []string
	argv  []string
	root  *rootFS
	dir   string
}

func newCtr(w *podWorker, spec v1.Container, init, sidecar bool) *ctr {
	c := &ctr{w: w, spec: spec, init: init, sidecar: sidecar, logs: newLogBuf()}
	c.state.Waiting = &v1.ContainerStateWaiting{Reason: "ContainerCreating"}
	if init || sidecar {
		c.state.Waiting.Reason = "PodInitializing"
	}
	if spec.Stdin {
		c.stdin = newStdinHub(spec.StdinOnce)
	}
	return c
}

func (c *ctr) fieldPath() string {
	if c.init || c.sidecar {
		return fmt.Sprintf("spec.initContainers{%s}", c.spec.Name)
	}
	return fmt.Sprintf("spec.containers{%s}", c.spec.Name)
}

func (c *ctr) setWaiting(reason, msg string) {
	c.w.mu.Lock()
	c.state = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: reason, Message: msg}}
	c.w.mu.Unlock()
	c.w.markDirty()
}

func (c *ctr) isRunning() bool {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	return c.state.Running != nil
}

// final reports whether the container ended for good. w.mu is held.
func (c *ctr) final() bool { return c.finalLocked() }

func (c *ctr) finalLocked() bool { return c.finished && c.state.Terminated != nil }

func (c *ctr) markFinished() {
	c.w.mu.Lock()
	c.finished = true
	c.w.mu.Unlock()
	c.w.markDirty()
}

// status is the container status. w.mu is held.
func (c *ctr) status() v1.ContainerStatus {
	cs := v1.ContainerStatus{
		Name:                 c.spec.Name,
		State:                *c.state.DeepCopy(),
		LastTerminationState: *c.last.DeepCopy(),
		Ready:                c.ready,
		RestartCount:         c.restarts,
		Image:                c.spec.Image,
		ImageID:              imageID(c.spec.Image, c.image, c.wasm),
		ContainerID:          c.id,
	}
	if !c.init {
		cs.Started = ptr.To(c.state.Running != nil && c.startupOK)
	}
	return cs
}

// prepare works out the environment, the command line and the file system
// view of the container. It runs once the volumes exist.
func (c *ctr) prepare() error {
	pod, ip := c.w.podInfo()
	c.wasm = isWasmPod(pod)
	if !c.wasm {
		c.image = lookupImage(c.spec.Image)
	}
	env, err := c.w.containerEnv(pod, ip, &c.spec, c.image)
	if err != nil {
		return err
	}
	vars := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		vars[k] = v
	}
	argv := expandAll(c.spec.Command, vars)
	args := expandAll(c.spec.Args, vars)
	if len(c.spec.Command) == 0 && c.image != nil {
		argv = c.image.entrypoint
		if len(c.spec.Args) == 0 {
			args = c.image.cmd
		}
	}
	argv = append(append([]string{}, argv...), args...)

	root, err := c.w.rootFS(c, pod)
	if err != nil {
		return err
	}
	dir := c.spec.WorkingDir
	if dir == "" && c.image != nil {
		dir = c.image.workdir
	}
	if dir == "" {
		dir = "/"
	}
	os.MkdirAll(root.real(dir), 0o755)

	c.w.mu.Lock()
	c.env, c.argv, c.root, c.dir = env, argv, root, dir
	c.w.mu.Unlock()
	return nil
}

// runOnce starts the container and waits until it exits. ok is false when
// the pod stops.
func (c *ctr) runOnce(ctx context.Context) (code int, ok bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	c.w.mu.Lock()
	argv, wasm, img, restarts := c.argv, c.wasm, c.image, c.restarts
	c.w.mu.Unlock()

	if !wasm && len(argv) == 0 {
		msg := fmt.Sprintf("image %q has no implementation in the browser tab, and the container has no command. Built in: %s. Give it a command (it runs in a POSIX shell), use runtimeClassName: wasmtime for a WebAssembly image, or join a Docker node.", c.spec.Image, strings.Join(builtinImageNames(), ", "))
		c.setWaiting("UnsupportedImage", msg)
		c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %s", msg)
		<-ctx.Done()
		return 0, false
	}
	if restarts == 0 {
		switch {
		case wasm:
			c.w.ctrEvent(c, v1.EventTypeNormal, "Pulling", "Pulling image %q", c.spec.Image)
		case img != nil && img.name != "":
			c.w.ctrEvent(c, v1.EventTypeNormal, "Pulled", "Container image %q already present on machine (built into the browser tab)", c.spec.Image)
		default:
			c.w.ctrEvent(c, v1.EventTypeNormal, "Pulled", "Container image %q already present on machine (runs its command in the shell of the tab)", c.spec.Image)
		}
	}

	logs := newLogBuf()
	c.w.mu.Lock()
	if restarts > 0 || c.state.Terminated != nil {
		c.prev = c.logs
	}
	c.logs = logs
	c.id = "tabernetes://" + randToken(32)
	c.w.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var proc *process
	var err error
	if wasm {
		proc, err = c.startWasm(runCtx, logs)
	} else {
		proc, err = c.startNative(runCtx, logs)
	}
	if err != nil {
		now := metav1.Now()
		c.w.mu.Lock()
		c.state = v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
			ExitCode: 128, Reason: "StartError", Message: err.Error(), StartedAt: now, FinishedAt: now, ContainerID: c.id,
		}}
		c.last = c.state
		c.w.mu.Unlock()
		c.w.markDirty()
		c.w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %v", err)
		return 128, ctx.Err() == nil
	}
	if restarts == 0 && wasm {
		c.w.ctrEvent(c, v1.EventTypeNormal, "Pulled", "Successfully pulled image %q", c.spec.Image)
	}
	c.w.ctrEvent(c, v1.EventTypeNormal, "Created", "Created container: %s", c.spec.Name)
	c.w.ctrEvent(c, v1.EventTypeNormal, "Started", "Started container %s", c.spec.Name)

	started := metav1.Now()
	c.w.mu.Lock()
	c.proc = proc
	c.runStart = time.Now()
	c.state = v1.ContainerState{Running: &v1.ContainerStateRunning{StartedAt: started}}
	c.startupOK = c.spec.StartupProbe == nil
	c.ready = !c.init && c.spec.ReadinessProbe == nil && c.startupOK
	c.w.mu.Unlock()
	c.w.markDirty()

	if !c.init {
		go c.probe(runCtx, proc)
	}

	select {
	case <-proc.done:
	case <-ctx.Done():
		proc.kill("")
		select {
		case <-proc.done:
		case <-time.After(2 * time.Second):
			// A goroutine that ignores its context: the code is gone
			// with the tab only, but the container counts as stopped.
		}
	}
	code = proc.exitCode()
	reason := "Completed"
	if code != 0 {
		reason = "Error"
	}
	msg := proc.killReason()
	c.w.mu.Lock()
	c.proc = nil
	c.ready = false
	c.state = v1.ContainerState{Terminated: &v1.ContainerStateTerminated{
		ExitCode: int32(code), Reason: reason, Message: msg, StartedAt: started,
		FinishedAt: metav1.Now(), ContainerID: c.id,
	}}
	c.last = c.state
	c.w.mu.Unlock()
	logs.close()
	c.w.markDirty()
	return code, ctx.Err() == nil
}

// backoff waits before a restart, doubling from 10s up to 5 minutes like the
// kubelet. A container that ran for 10 minutes starts over at 10s.
func (c *ctr) backoff(ctx context.Context) bool {
	c.w.mu.Lock()
	if c.delay == 0 || time.Since(c.runStart) > 10*time.Minute {
		c.delay = 10 * time.Second
	} else if c.delay < 5*time.Minute {
		c.delay = min(2*c.delay, 5*time.Minute)
	}
	delay := c.delay
	c.state = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{
		Reason:  "CrashLoopBackOff",
		Message: fmt.Sprintf("back-off %s restarting failed container=%s pod=%s_%s(%s)", delay, c.spec.Name, c.w.name, c.w.namespace, c.w.uid),
	}}
	c.w.mu.Unlock()
	c.w.markDirty()
	c.w.ctrEvent(c, v1.EventTypeWarning, "BackOff", "Back-off restarting failed container %s in pod %s_%s(%s)", c.spec.Name, c.w.name, c.w.namespace, c.w.uid)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
	}
	c.w.mu.Lock()
	c.restarts++
	c.w.mu.Unlock()
	return true
}

// process is a running container: a goroutine or a Web Worker.
type process struct {
	done   chan struct{}
	code   atomic.Int32
	cancel context.CancelFunc
	reason atomic.Value // why it was killed
	killed atomic.Bool
}

func newProcess(cancel context.CancelFunc) *process {
	return &process{done: make(chan struct{}), cancel: cancel}
}

func (p *process) kill(reason string) {
	if p.killed.CompareAndSwap(false, true) {
		p.reason.Store(reason)
	}
	p.cancel()
}

func (p *process) exit(code int) {
	if p.killed.Load() {
		code = 137 // SIGKILL, like a container runtime reports it
	}
	p.code.Store(int32(code))
	close(p.done)
}

func (p *process) exitCode() int {
	select {
	case <-p.done:
		return int(p.code.Load())
	default:
		return 137
	}
}

func (p *process) killReason() string {
	if s, ok := p.reason.Load().(string); ok {
		return s
	}
	return ""
}

// startNative runs the command line of the container in a shell of its own.
func (c *ctr) startNative(ctx context.Context, logs *logBuf) (*process, error) {
	c.w.mu.Lock()
	argv, env, dir, root := c.argv, c.env, c.dir, c.root
	c.w.mu.Unlock()
	_, ip := c.w.podInfo()
	ctx, cancel := context.WithCancel(ctx)
	p := newProcess(cancel)
	pe := &procEnv{w: c.w, c: c, root: root, ip: ip, cpu: newThrottle(c.spec.Resources)}
	var stdin io.Reader = strings.NewReader("")
	if c.stdin != nil {
		stdin = c.stdin.reader(ctx)
	}
	go func() {
		code := runArgv(ctx, pe, argv, env, dir, stdin, logs.writer("stdout"), logs.writer("stderr"))
		p.exit(code)
	}()
	return p, nil
}

// rootFS is the file system view of a container: its own root directory
// with the volumes mounted into it. Paths of the container are mapped to
// paths of the in-memory file system of the tab.
type rootFS struct {
	base   string
	mounts []rootMount // longest path first
}

type rootMount struct {
	path, real string
	readOnly   bool
}

func (r *rootFS) real(p string) string {
	p = path.Clean("/" + p)
	for _, m := range r.mounts {
		if p == m.path || strings.HasPrefix(p, m.path+"/") {
			return m.real + p[len(m.path):]
		}
	}
	if p == "/" {
		return r.base
	}
	return r.base + p
}

// rootFS builds the file system of a container: the files of its image, a
// few files every container has, and its volume mounts.
func (w *podWorker) rootFS(c *ctr, pod *v1.Pod) (*rootFS, error) {
	base := filepath.Join(w.dir, "containers", c.spec.Name, "rootfs")
	for _, d := range []string{"tmp", "etc", "root", "var/log", "var/tmp", "usr/bin", "bin", "dev", "proc", "home"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			return nil, err
		}
	}
	hostname := pod.Name
	if pod.Spec.Hostname != "" {
		hostname = pod.Spec.Hostname
	}
	_, ip := w.podInfo()
	files := map[string]string{
		"/etc/hostname":    hostname + "\n",
		"/etc/hosts":       fmt.Sprintf("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n%s\t%s\n", ip, hostname),
		"/etc/resolv.conf": fmt.Sprintf("search %s.svc.cluster.local svc.cluster.local cluster.local\nnameserver 10.96.0.10\noptions ndots:5\n", pod.Namespace),
		"/etc/os-release":  "NAME=\"Tabernetes\"\nPRETTY_NAME=\"Tabernetes (a browser tab)\"\nID=tabernetes\n",
		"/etc/passwd":      "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/nonexistent:/bin/false\n",
	}
	if c.image != nil {
		for p, s := range c.image.files {
			files[p] = s
		}
	}
	for p, s := range files {
		real := filepath.Join(base, p)
		if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(real, []byte(s), 0o644); err != nil {
			return nil, err
		}
	}
	root := &rootFS{base: base}
	for _, m := range c.spec.VolumeMounts {
		real, ok := w.volumeDir(m.Name)
		if !ok {
			return nil, fmt.Errorf("volume %q of mount %s not found", m.Name, m.MountPath)
		}
		if m.SubPath != "" {
			real = filepath.Join(real, m.SubPath)
		}
		root.mounts = append(root.mounts, rootMount{path: path.Clean("/" + m.MountPath), real: real, readOnly: m.ReadOnly})
	}
	// longest first, so /a/b wins over /a
	for i := 1; i < len(root.mounts); i++ {
		for j := i; j > 0 && len(root.mounts[j].path) > len(root.mounts[j-1].path); j-- {
			root.mounts[j], root.mounts[j-1] = root.mounts[j-1], root.mounts[j]
		}
	}
	return root, nil
}

// stdinHub is the stdin of a container with stdin: true. kubectl attach
// writes into it.
type stdinHub struct {
	mu   sync.Mutex
	pr   *io.PipeReader
	pw   *io.PipeWriter
	once bool
}

func newStdinHub(once bool) *stdinHub {
	pr, pw := io.Pipe()
	return &stdinHub{pr: pr, pw: pw, once: once}
}

func (h *stdinHub) reader(ctx context.Context) io.Reader {
	h.mu.Lock()
	defer h.mu.Unlock()
	pr := h.pr
	context.AfterFunc(ctx, func() { pr.CloseWithError(io.EOF) })
	return pr
}

// attach copies in into the stdin of the container. With stdinOnce the
// stdin closes when the first attach ends.
func (h *stdinHub) attach(in io.Reader) {
	h.mu.Lock()
	pw := h.pw
	h.mu.Unlock()
	io.Copy(pw, in)
	if h.once {
		pw.Close()
	}
}

// builtinImage is an image the tab node runs without pulling anything: its
// programs are Go code in the wasm binary (tabnode_images.go).
type builtinImage struct {
	name       string            // short name, "nginx"
	entrypoint []string          // like the ENTRYPOINT of the image
	cmd        []string          // like the CMD of the image
	workdir    string            // like the WORKDIR of the image
	env        []string          // like the ENV of the image, "K=V"
	files      map[string]string // files of the image: path in the container -> content
}
