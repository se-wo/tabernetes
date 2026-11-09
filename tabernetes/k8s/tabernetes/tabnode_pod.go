package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// podWorker runs one pod: volumes, init containers, containers, status.
type podWorker struct {
	n         *tabNode
	uid       types.UID
	namespace string
	name      string
	dir       string // real directory of the pod in the in-memory file system

	ctx    context.Context // ends when the pod stops
	cancel context.CancelFunc

	mu          sync.Mutex
	pod         *v1.Pod // latest from the informer
	ip          string
	started     time.Time
	ctrs        []*ctr // init containers first
	volumesOK   bool
	initDone    bool
	initFailed  bool
	terminating bool
	deleted     bool
	sent        *v1.PodStatus // last status written

	kick chan struct{}
	wg   sync.WaitGroup // container supervisors
}

func newPodWorker(n *tabNode, pod *v1.Pod) *podWorker {
	ctx, cancel := context.WithCancel(context.Background())
	w := &podWorker{
		n: n, uid: pod.UID, namespace: pod.Namespace, name: pod.Name, pod: pod.DeepCopy(),
		dir: filepath.Join(tabPodsDir, string(pod.UID)), ctx: ctx, cancel: cancel,
		started: time.Now(), kick: make(chan struct{}, 1),
	}
	for _, c := range pod.Spec.InitContainers {
		sidecar := c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways
		w.ctrs = append(w.ctrs, newCtr(w, c, !sidecar, sidecar))
	}
	for _, c := range pod.Spec.Containers {
		w.ctrs = append(w.ctrs, newCtr(w, c, false, false))
	}
	return w
}

func (w *podWorker) key() string { return w.namespace + "/" + w.name }

func (w *podWorker) event(typ, reason, format string, a ...any) {
	w.mu.Lock()
	pod := w.pod
	w.mu.Unlock()
	w.n.recorder.Eventf(pod, typ, reason, format, a...)
}

func (w *podWorker) ctrEvent(c *ctr, typ, reason, format string, a ...any) {
	w.mu.Lock()
	ref := &v1.ObjectReference{
		Kind: "Pod", Namespace: w.namespace, Name: w.name, UID: w.uid, APIVersion: "v1",
		ResourceVersion: w.pod.ResourceVersion, FieldPath: c.fieldPath(),
	}
	w.mu.Unlock()
	w.n.recorder.Eventf(ref, typ, reason, format, a...)
}

func (w *podWorker) markDirty() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// run is the life of the pod on this node.
func (w *podWorker) run() {
	go w.statusLoop()
	defer w.markDirty()

	ip, err := w.n.allocIP()
	for err != nil {
		w.event(v1.EventTypeWarning, "FailedCreatePodSandBox", "%v", err)
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		ip, err = w.n.allocIP()
	}
	w.mu.Lock()
	w.ip = ip
	w.mu.Unlock()
	w.markDirty()

	// Volumes may wait for a ConfigMap or Secret that is not there yet.
	for {
		err := w.setupVolumes(w.ctx)
		if err == nil {
			break
		}
		w.event(v1.EventTypeWarning, "FailedMount", "%v", err)
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	for _, c := range w.ctrs {
		if err := c.prepare(); err != nil {
			c.setWaiting("CreateContainerConfigError", err.Error())
			w.ctrEvent(c, v1.EventTypeWarning, "Failed", "Error: %v", err)
			// A missing ConfigMap key may show up later, like on a real node.
			for err != nil {
				select {
				case <-w.ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
				err = c.prepare()
			}
		}
	}
	w.mu.Lock()
	w.volumesOK = true
	w.mu.Unlock()
	w.markDirty()

	// Init containers run one after the other, sidecars keep running.
	for _, c := range w.ctrs {
		if !c.init && !c.sidecar {
			continue
		}
		if c.sidecar {
			w.supervise(c)
			continue
		}
		if !w.runInit(c) {
			return
		}
	}
	w.mu.Lock()
	w.initDone = true
	w.mu.Unlock()
	w.markDirty()

	for _, c := range w.ctrs {
		if !c.init && !c.sidecar {
			w.supervise(c)
		}
	}
	w.wg.Wait()
}

// runInit runs an init container until it succeeds. false: the pod failed or
// stops.
func (w *podWorker) runInit(c *ctr) bool {
	for {
		code, ok := c.runOnce(w.ctx)
		if !ok {
			return false
		}
		if code == 0 {
			c.markFinished()
			return true
		}
		if w.restartPolicy() == v1.RestartPolicyNever {
			c.markFinished()
			w.mu.Lock()
			w.initFailed = true
			w.mu.Unlock()
			w.markDirty()
			return false
		}
		if !c.backoff(w.ctx) {
			return false
		}
	}
}

func (w *podWorker) restartPolicy() v1.RestartPolicy {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pod.Spec.RestartPolicy
}

// supervise runs a container and restarts it by the restart policy.
func (w *podWorker) supervise(c *ctr) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			code, ok := c.runOnce(w.ctx)
			if !ok {
				return
			}
			policy := w.restartPolicy()
			if c.sidecar {
				policy = v1.RestartPolicyAlways
			}
			if policy == v1.RestartPolicyNever || (policy == v1.RestartPolicyOnFailure && code == 0) {
				c.markFinished()
				return
			}
			if !c.backoff(w.ctx) {
				return
			}
		}
	}()
}

// update takes a newer version of the pod from the informer.
func (w *podWorker) update(pod *v1.Pod) {
	w.mu.Lock()
	w.pod = pod.DeepCopy()
	start := pod.DeletionTimestamp != nil && !w.terminating
	if start {
		w.terminating = true
	}
	w.mu.Unlock()
	if start {
		go w.terminate(pod)
	}
}

// terminate stops the containers of a pod that is being deleted and then
// removes the pod object.
func (w *podWorker) terminate(pod *v1.Pod) {
	grace := 30 * time.Second
	if pod.DeletionGracePeriodSeconds != nil {
		grace = time.Duration(*pod.DeletionGracePeriodSeconds) * time.Second
	}
	for _, c := range w.ctrs {
		if c.isRunning() {
			w.ctrEvent(c, v1.EventTypeNormal, "Killing", "Stopping container %s", c.spec.Name)
		}
	}
	w.cancel()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
	}
	w.flushStatus()
	w.n.deletePod(pod)
}

// gone cleans up after the pod object is deleted.
func (w *podWorker) gone() {
	w.mu.Lock()
	w.deleted = true
	ip := w.ip
	w.mu.Unlock()
	w.cancel()
	go func() {
		w.wg.Wait()
		if ip != "" {
			w.n.releaseIP(ip)
		}
		os.RemoveAll(w.dir)
	}()
}

// statusLoop writes the pod status whenever it changes.
func (w *podWorker) statusLoop() {
	for {
		select {
		case <-w.kick:
		case <-time.After(30 * time.Second):
		}
		// Several changes in a row become one update.
		time.Sleep(50 * time.Millisecond)
		w.mu.Lock()
		deleted := w.deleted
		w.mu.Unlock()
		if deleted {
			return
		}
		w.flushStatus()
	}
}

func (w *podWorker) flushStatus() {
	for attempt := 0; attempt < 5; attempt++ {
		w.mu.Lock()
		if w.deleted {
			w.mu.Unlock()
			return
		}
		status := w.buildStatus()
		if w.sent != nil && statusEqual(w.sent, &status) {
			w.mu.Unlock()
			return
		}
		pod := w.pod.DeepCopy()
		w.mu.Unlock()
		pod.Status = status
		updated, err := w.n.client.CoreV1().Pods(w.namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
		if err == nil {
			w.mu.Lock()
			w.sent = &status
			if updated.ResourceVersion > w.pod.ResourceVersion || w.pod.ResourceVersion == pod.ResourceVersion {
				w.pod = updated
			}
			w.mu.Unlock()
			return
		}
		if apierrors.IsNotFound(err) {
			return
		}
		if apierrors.IsConflict(err) {
			if latest, err := w.n.client.CoreV1().Pods(w.namespace).Get(context.Background(), w.name, metav1.GetOptions{}); err == nil && latest.UID == w.uid {
				w.mu.Lock()
				w.pod = latest
				w.mu.Unlock()
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "tab node: status of %s: %v\n", w.key(), err)
		time.Sleep(time.Second)
	}
}

// statusEqual compares two statuses without the heartbeat-like fields.
func statusEqual(a, b *v1.PodStatus) bool {
	return reflect.DeepEqual(a, b)
}

// buildStatus computes the pod status. w.mu is held.
func (w *podWorker) buildStatus() v1.PodStatus {
	pod := w.pod
	st := v1.PodStatus{
		HostIP:   tabNodeIP,
		HostIPs:  []v1.HostIP{{IP: tabNodeIP}},
		QOSClass: pod.Status.QOSClass,
	}
	start := metav1.NewTime(w.started.Truncate(time.Second))
	st.StartTime = &start
	if w.ip != "" {
		st.PodIP = w.ip
		st.PodIPs = []v1.PodIP{{IP: w.ip}}
	}

	allReady, anyRunning, regular := true, false, 0
	allDone, allSucceeded := true, true
	for _, c := range w.ctrs {
		cs := c.status()
		if c.init || c.sidecar {
			st.InitContainerStatuses = append(st.InitContainerStatuses, cs)
			if c.sidecar && !cs.Ready {
				allReady = false
			}
			continue
		}
		regular++
		st.ContainerStatuses = append(st.ContainerStatuses, cs)
		if !cs.Ready {
			allReady = false
		}
		if cs.State.Running != nil {
			anyRunning = true
		}
		if c.final() {
			if cs.State.Terminated.ExitCode != 0 {
				allSucceeded = false
			}
		} else {
			allDone = false
		}
	}

	switch {
	case w.initFailed:
		st.Phase = v1.PodFailed
	case !w.initDone:
		st.Phase = v1.PodPending
	case regular > 0 && allDone:
		if allSucceeded {
			st.Phase = v1.PodSucceeded
		} else {
			st.Phase = v1.PodFailed
		}
	case anyRunning || w.anyRestarting():
		st.Phase = v1.PodRunning
	default:
		st.Phase = v1.PodPending
	}
	if st.Phase == v1.PodSucceeded || st.Phase == v1.PodFailed {
		allReady = false
	}

	cond := func(t v1.PodConditionType, ok bool, reason string) v1.PodCondition {
		c := v1.PodCondition{Type: t, Status: v1.ConditionFalse, Reason: reason}
		if ok {
			c.Status, c.Reason = v1.ConditionTrue, ""
		}
		c.LastTransitionTime = start
		for _, old := range pod.Status.Conditions {
			if old.Type == t && old.Status == c.Status {
				c.LastTransitionTime = old.LastTransitionTime
			}
		}
		if w.sent != nil {
			for _, old := range w.sent.Conditions {
				if old.Type == t && old.Status != c.Status {
					c.LastTransitionTime = metav1.NewTime(time.Now().Truncate(time.Second))
				}
			}
		}
		return c
	}
	initialized := w.initDone || st.Phase == v1.PodSucceeded
	reason := "ContainersNotReady"
	if st.Phase == v1.PodSucceeded || st.Phase == v1.PodFailed {
		reason = "PodCompleted"
	}
	st.Conditions = []v1.PodCondition{
		cond(v1.PodReadyToStartContainers, w.volumesOK && st.Phase != v1.PodSucceeded && st.Phase != v1.PodFailed, ""),
		cond(v1.PodInitialized, initialized, "ContainersNotInitialized"),
		cond(v1.PodReady, allReady && regular > 0, reason),
		cond(v1.ContainersReady, allReady && regular > 0, reason),
		cond(v1.PodScheduled, true, ""),
	}
	return st
}

func (w *podWorker) anyRestarting() bool {
	for _, c := range w.ctrs {
		if !c.init && c.restarts > 0 && !c.finalLocked() {
			return true
		}
	}
	return false
}

// podInfo is what a container sees of its pod.
func (w *podWorker) podInfo() (pod *v1.Pod, ip string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pod, w.ip
}

func (w *podWorker) container(name string) *ctr {
	for _, c := range w.ctrs {
		if c.spec.Name == name {
			return c
		}
	}
	return nil
}

// defaultContainer is the first regular container.
func (w *podWorker) defaultContainer() *ctr {
	for _, c := range w.ctrs {
		if !c.init && !c.sidecar {
			return c
		}
	}
	return nil
}
