package main

// The parts of the pod lifecycle that the kubelet drives on its own: the
// postStart and preStop hooks of containers (pkg/kubelet/lifecycle and
// killContainer in pkg/kubelet/kuberuntime), the stop of a whole pod with its
// grace period, spec.activeDeadlineSeconds (pkg/kubelet/active_deadline.go)
// and ephemeral containers (kubectl debug).
//
// There are no signals in the tab: a container stops when its context ends,
// which is the SIGKILL of a real runtime. So a stop is the preStop hook,
// bounded by the grace period, and then the kill.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
)

const (
	deadlineReason  = "DeadlineExceeded"
	deadlineMessage = "Pod was active on the node longer than the specified deadline"
)

// runHook runs a lifecycle handler of the container like the handler runner
// of the kubelet. msg describes a failure in the words of the kubelet.
// tcpSocket is deprecated for hooks and fails, like upstream.
func (c *ctr) runHook(ctx context.Context, proc *process, h *v1.LifecycleHandler) (msg string, err error) {
	pod, ip := c.w.podInfo()
	podName := fmt.Sprintf("%s_%s(%s)", pod.Name, pod.Namespace, pod.UID)
	switch {
	case h.Exec != nil:
		out, err := c.execHook(ctx, proc, h.Exec.Command)
		if err != nil {
			return fmt.Sprintf("Exec lifecycle hook (%v) for Container %q in Pod %q failed - error: %v, message: %q", h.Exec.Command, c.spec.Name, podName, err, out), err
		}
	case h.HTTPGet != nil:
		if err := c.httpHook(ctx, h.HTTPGet, ip); err != nil {
			return fmt.Sprintf("HTTP lifecycle hook (%s) for Container %q in Pod %q failed - error: %v", h.HTTPGet.Path, c.spec.Name, podName, err), err
		}
	case h.Sleep != nil:
		select {
		case <-ctx.Done():
			err := errors.New("container terminated before sleep hook finished")
			return fmt.Sprintf("Sleep lifecycle hook (%d) for Container %q in Pod %q failed - error: %v", h.Sleep.Seconds, c.spec.Name, podName, err), err
		case <-time.After(time.Duration(h.Sleep.Seconds) * time.Second):
		}
	default:
		err := fmt.Errorf("invalid handler: %v", h)
		return fmt.Sprintf("Cannot run handler: %v", err), err
	}
	return "", nil
}

// execHook runs cmd in a new shell of the running container, like an exec.
// Its output goes to the caller, not to the log of the container. out is
// stdout and stderr, the error carries stderr, like ExecSync of a runtime.
func (c *ctr) execHook(ctx context.Context, proc *process, cmd []string) (out string, err error) {
	c.w.mu.Lock()
	env, dir, root, wasm := c.env, c.dir, c.root, c.wasm
	c.w.mu.Unlock()
	if wasm {
		return "", errors.New("a WebAssembly module in a Web Worker has no exec")
	}
	select {
	case <-proc.done:
		return "", fmt.Errorf("container %q is not running", c.spec.Name)
	default:
	}
	// An exec dies with its container.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-proc.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	_, ip := c.w.podInfo()
	var all, stderr capBuf
	pe := &procEnv{w: c.w, c: c, root: root, ip: ip, cpu: newThrottle(c.spec.Resources)}
	code := runArgv(ctx, pe, cmd, env, dir, strings.NewReader(""), &all, io.MultiWriter(&all, &stderr))
	if code != 0 {
		return all.String(), fmt.Errorf("command '%s' exited with %d: %s", strings.Join(cmd, " "), code, stderr.String())
	}
	return all.String(), nil
}

// httpHook sends the GET of an httpGet hook. Unlike a probe, any answer is a
// success; only a failed request fails the hook.
func (c *ctr) httpHook(ctx context.Context, a *v1.HTTPGetAction, ip string) error {
	req, err := c.httpGetRequest(ctx, a, ip, "lifecycle")
	if err != nil {
		return err
	}
	resp, err := hookClient.Do(req)
	discardBody(resp)
	if isHTTPResponseError(err) {
		// An HTTPS hook that hits a plain HTTP server is retried with HTTP,
		// like the kubelet does.
		req := req.Clone(ctx)
		req.URL.Scheme = "http"
		req.Header.Del("Authorization")
		resp, httpErr := hookClient.Do(req)
		discardBody(resp)
		if httpErr == nil {
			c.w.event(v1.EventTypeWarning, "LifecycleHTTPFallback", "request to HTTPS lifecycle hook %s got HTTP response, retry with HTTP succeeded", req.URL.Host)
			err = nil
		}
	}
	return err
}

func isHTTPResponseError(err error) bool {
	var urlErr *url.Error
	return errors.As(err, &urlErr) && strings.Contains(urlErr.Err.Error(), "server gave HTTP response to HTTPS client")
}

func discardBody(resp *http.Response) {
	if resp == nil {
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 10<<10))
	resp.Body.Close()
}

// hookClient sends the requests of httpGet hooks. Like the one of the
// kubelet, it does not verify certificates (there is nothing to verify them
// against) and follows redirects on the same host only. The dial examines the
// loopback table (section 8.11, TC15).
var hookClient = &http.Client{
	Transport: &http.Transport{
		DialContext:       dialFromNodeHost,
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Hostname() != via[0].URL.Hostname() {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	},
}

// capBuf keeps the first 4 KiB written to it: hook output ends up in events.
type capBuf struct {
	mu sync.Mutex
	b  []byte
}

func (b *capBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n := 4<<10 - len(b.b); n > 0 {
		b.b = append(b.b, p[:min(n, len(p))]...)
	}
	return len(p), nil
}

func (b *capBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.b)
}

// postStart runs the postStart hook of a new run of the container. Like the
// kubelet, a failed hook kills the run; postStart reports whether it did.
func (c *ctr) postStart(ctx context.Context, proc *process) (failed bool) {
	lc := c.spec.Lifecycle
	if lc == nil || lc.PostStart == nil {
		return false
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A pod that stops ends the hook: the kubelet cancels the sync of the
	// pod, too.
	defer context.AfterFunc(c.w.ctx, cancel)()
	msg, err := c.runHook(ctx, proc, lc.PostStart)
	if err == nil || c.w.ctx.Err() != nil {
		return false
	}
	c.w.ctrEvent(c, v1.EventTypeWarning, "FailedPostStartHook", "%s", msg)
	pod, _ := c.w.podInfo()
	c.stopProc(proc, time.Now().Add(killGrace(pod, nil)), "FailedPostStartHook", false)
	return true
}

// stopProc stops a run of the container like killContainer of the kubelet:
// the preStop hook first, within the grace period that ends at by, and then
// the kill. The rest of the grace period would be the time between SIGTERM
// and SIGKILL, which the tab does not have. msg is the message of the
// Killing event and the kill. ordered holds back the kill of a sidecar until
// the containers after it stopped, for the stop of the whole pod.
func (c *ctr) stopProc(proc *process, by time.Time, msg string, ordered bool) {
	select {
	case <-proc.done:
		return
	default:
	}
	if !proc.stopping.CompareAndSwap(false, true) {
		// Another stop runs the hook already; this one only keeps its own
		// deadline.
		select {
		case <-proc.done:
		case <-time.After(time.Until(by)):
			proc.kill(msg)
		case <-c.w.killCtx.Done():
			proc.kill(msg)
		}
		return
	}
	if msg == "" {
		c.w.ctrEvent(c, v1.EventTypeNormal, "Killing", "Stopping container %s", c.spec.Name)
	} else {
		c.w.ctrEvent(c, v1.EventTypeNormal, "Killing", "%s", msg)
	}
	if lc := c.spec.Lifecycle; lc != nil && lc.PreStop != nil && time.Until(by) > 0 {
		ctx, cancel := context.WithDeadline(c.w.killCtx, by)
		hookMsg, err := c.runHook(ctx, proc, lc.PreStop)
		// A hook that runs out of grace period is no failure of its own:
		// the kill follows, as planned.
		if err != nil && ctx.Err() == nil {
			c.w.ctrEvent(c, v1.EventTypeWarning, "FailedPreStopHook", "%s", hookMsg)
		}
		cancel()
	}
	if ordered && c.sidecar {
		c.w.waitForTurn(c, by)
	}
	proc.kill(msg)
}

// waitForTurn holds back the kill of a sidecar until the regular containers
// and the sidecars after it stopped, like the termination ordering of the
// kubelet: a sidecar outlives what it serves. It gives up at by.
func (w *podWorker) waitForTurn(c *ctr, by time.Time) {
	timeout := time.After(time.Until(by))
	after := false
	for _, o := range w.ctrs {
		if o == c {
			after = true
			continue
		}
		if o.init || (o.sidecar && !after) {
			continue
		}
		w.mu.Lock()
		p := o.proc
		w.mu.Unlock()
		if p == nil {
			continue
		}
		select {
		case <-p.done:
		case <-timeout:
			return
		case <-w.killCtx.Done():
			return
		}
	}
}

// stopAll stops the pod like the kubelet kills a pod: nothing starts any
// more, and every running container gets its preStop hook and is killed,
// all within grace. It returns when the containers stopped; whatever runs
// when the grace period is over is killed on the spot. A second stop may
// only shorten the grace period.
func (w *podWorker) stopAll(grace time.Duration) {
	by := time.Now().Add(grace)
	w.mu.Lock()
	if !w.stopping || by.Before(w.stopBy) {
		w.stopBy = by
	}
	w.stopping = true
	w.mu.Unlock()
	w.cancel()
	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		w.ewg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return
	case <-time.After(time.Until(by)):
	}
	w.kill()
	select {
	case <-done:
	case <-time.After(3 * time.Second): // runOnce waits 2s for a killed run
	}
}

// stopDeadline is the end of the grace period of the stop of the pod.
func (w *podWorker) stopDeadline() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stopBy
}

// killGrace is the grace period of a kill, like setTerminationGracePeriod
// of the kubelet: the one of a deletion wins, then the one of the probe that
// failed, then the one of the pod.
func killGrace(pod *v1.Pod, probe *v1.Probe) time.Duration {
	switch {
	case pod.DeletionGracePeriodSeconds != nil:
		return time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second
	case pod.Spec.TerminationGracePeriodSeconds != nil:
		if probe != nil && probe.TerminationGracePeriodSeconds != nil {
			return time.Duration(*probe.TerminationGracePeriodSeconds) * time.Second
		}
		return time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
	}
	return 2 * time.Second // minimumGracePeriodInSeconds of the kubelet
}

// activeDeadline is the time spec.activeDeadlineSeconds ends the pod. The
// clock starts with status.startTime, when the node took the pod.
func activeDeadline(start time.Time, seconds *int64) (time.Time, bool) {
	if seconds == nil || start.IsZero() {
		return time.Time{}, false
	}
	return start.Add(time.Duration(*seconds) * time.Second), true
}

// deadlineLoop enforces spec.activeDeadlineSeconds. The field can change
// (only to a lower value) while the pod runs, update wakes the loop.
func (w *podWorker) deadlineLoop() {
	for {
		w.mu.Lock()
		at, ok := activeDeadline(w.started, w.pod.Spec.ActiveDeadlineSeconds)
		w.mu.Unlock()
		var expired <-chan time.Time
		if ok {
			expired = time.After(time.Until(at))
		}
		select {
		case <-w.ctx.Done():
			return
		case <-w.specChanged:
		case <-expired:
			w.deadlineExceeded()
			return
		}
	}
}

// deadlineExceeded fails the pod like the active deadline handler of the
// kubelet: phase Failed with reason DeadlineExceeded, and every container
// stops (with its preStop hook) within the grace period of the pod.
func (w *podWorker) deadlineExceeded() {
	w.mu.Lock()
	phase := w.buildStatus().Phase
	if w.stopping || phase == v1.PodSucceeded || phase == v1.PodFailed {
		w.mu.Unlock()
		return
	}
	w.failReason, w.failMessage = deadlineReason, deadlineMessage
	pod := w.pod
	w.mu.Unlock()
	w.event(v1.EventTypeNormal, deadlineReason, "%s", deadlineMessage)
	w.markDirty()
	w.stopAll(killGrace(pod, nil))
	w.markDirty()
}

// addEphemeralLocked picks up the ephemeral containers (kubectl debug) of
// the spec that are new. w.mu is held.
func (w *podWorker) addEphemeralLocked(pod *v1.Pod) {
next:
	for _, ec := range pod.Spec.EphemeralContainers {
		for _, c := range w.ephemeral {
			if c.spec.Name == ec.Name {
				continue next
			}
		}
		// The fields that ephemeral containers do not have (ports,
		// probes, resources, lifecycle) are empty, validation sees to it.
		c := newCtr(w, v1.Container(ec.EphemeralContainerCommon), false, false)
		c.ephemeral = true
		w.ephemeral = append(w.ephemeral, c)
	}
}

// startEphemeral starts the ephemeral containers that did not run yet. Like
// the kubelet, it starts them once the pod has its volumes, even while init
// containers run, and never in a pod that stops.
func (w *podWorker) startEphemeral() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.volumesOK || w.stopping {
		return
	}
	for _, c := range w.ephemeral {
		if c.launched {
			continue
		}
		c.launched = true
		w.ewg.Add(1)
		go w.runEphemeral(c)
	}
}

// runEphemeral runs an ephemeral container once: it is never restarted.
func (w *podWorker) runEphemeral(c *ctr) {
	defer w.ewg.Done()
	if err := c.prepare(); err != nil {
		c.setWaiting("CreateContainerConfigError", err.Error())
		w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %v", err)
		for err != nil {
			select {
			case <-w.ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			err = c.prepare()
			if err != nil { // the kubelet repeats the event with every sync
				c.setWaiting("CreateContainerConfigError", err.Error())
				w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %v", err)
			}
		}
	}
	if _, ok := c.runOnce(); ok {
		c.markFinished()
	}
}
