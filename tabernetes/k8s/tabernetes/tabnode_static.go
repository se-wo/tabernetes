package main

// The static pods of the control plane node. kube-apiserver,
// kube-controller-manager, kube-scheduler and the etcd shim run as
// goroutines of this binary (main.go), not as containers. The tab node shows
// them the way a kubelet shows the static pods of kubeadm: a manifest in
// /etc/kubernetes/manifests and a mirror pod <component>-<node> in
// kube-system, created with the credentials of the node. The manifests are
// written for reading only; nothing watches them.
//
// The container of a static pod belongs to the component: its start time,
// ID and log stay when someone deletes the mirror pod; the manager creates
// it again, like a kubelet. Ready comes from the health endpoint of the
// component. Nothing restarts a component in-process: once one exits, its
// pod shows CrashLoopBackOff for good.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/component-base/version"
	"k8s.io/cri-streaming/pkg/streaming/portforward"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
	kubeadmconstants "k8s.io/kubernetes/cmd/kubeadm/app/constants"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/yaml"
)

const (
	staticPodManifests = "/etc/kubernetes/manifests"
	staticNamespace    = metav1.NamespaceSystem

	configHashKey   = "kubernetes.io/config.hash"
	configMirrorKey = "kubernetes.io/config.mirror"
	configSeenKey   = "kubernetes.io/config.seen"
	configSourceKey = "kubernetes.io/config.source"

	staticSyncPeriod = 10 * time.Second
)

// componentSpec is what the static pod of a component looks like, after
// the manifests of kubeadm.
type componentSpec struct {
	image       string
	cpu, memory string
	scheme      v1.URIScheme
	port        int32
	live, ready string // probe paths; ready "" for no readiness probe
	startup     string
	size        int64 // about the size of the upstream image
}

func controlPlaneSpec(name string) componentSpec {
	k8s := version.Get().GitVersion
	switch name {
	case "kube-apiserver":
		return componentSpec{image: "registry.k8s.io/kube-apiserver:" + k8s, cpu: "250m",
			scheme: v1.URISchemeHTTPS, port: 6443, live: "/livez", ready: "/readyz", startup: "/livez", size: 30_000_000}
	case "kube-controller-manager":
		return componentSpec{image: "registry.k8s.io/kube-controller-manager:" + k8s, cpu: "200m",
			scheme: v1.URISchemeHTTPS, port: 10257, live: "/healthz", startup: "/healthz", size: 27_000_000}
	case "kube-scheduler":
		return componentSpec{image: "registry.k8s.io/kube-scheduler:" + k8s, cpu: "100m",
			scheme: v1.URISchemeHTTPS, port: 10259, live: "/livez", ready: "/readyz", startup: "/livez", size: 21_000_000}
	case "etcd":
		return componentSpec{image: "registry.k8s.io/etcd:" + kubeadmconstants.DefaultEtcdVersion, cpu: "100m", memory: "100Mi",
			scheme: v1.URISchemeHTTP, port: 2381, live: "/livez", ready: "/readyz", startup: "/readyz", size: 23_000_000}
	}
	return componentSpec{}
}

// readyPath is the endpoint the manager checks for Ready.
func (s componentSpec) readyPath() string {
	if s.ready != "" {
		return s.ready
	}
	return s.live
}

// staticManifest is the manifest of a component in /etc/kubernetes/manifests,
// like the one kubeadm writes. args is its real command line.
func staticManifest(name string, args []string) *v1.Pod {
	s := controlPlaneSpec(name)
	probe := func(path string, delay, period, failures int32) *v1.Probe {
		if path == "" {
			return nil
		}
		return &v1.Probe{
			ProbeHandler: v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{
				Host: "127.0.0.1", Path: path, Port: intstr.FromInt32(s.port), Scheme: s.scheme,
			}},
			InitialDelaySeconds: delay, TimeoutSeconds: 15, PeriodSeconds: period, FailureThreshold: failures,
		}
	}
	requests := v1.ResourceList{v1.ResourceCPU: resource.MustParse(s.cpu)}
	if s.memory != "" {
		requests[v1.ResourceMemory] = resource.MustParse(s.memory)
	}
	ctr := v1.Container{
		Name:            name,
		Image:           s.image,
		ImagePullPolicy: v1.PullIfNotPresent,
		Command:         append([]string{name}, args...),
		Resources:       v1.ResourceRequirements{Requests: requests},
		LivenessProbe:   probe(s.live, 10, 10, 8),
		ReadinessProbe:  probe(s.ready, 0, 1, 3),
		StartupProbe:    probe(s.startup, 10, 10, 24),
	}
	pod := &v1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: staticNamespace,
			Labels:    map[string]string{"component": name, "tier": "control-plane"},
		},
		Spec: v1.PodSpec{
			HostNetwork:       true,
			PriorityClassName: "system-node-critical",
			SecurityContext:   &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}},
			RestartPolicy:     v1.RestartPolicyAlways,
		},
	}
	// hostPath volumes for the files the command line names, nothing more.
	var certs bool
	kubeconfigs := map[string]bool{}
	for _, a := range args {
		_, v, ok := strings.Cut(a, "=")
		switch {
		case !ok:
		case strings.HasPrefix(v, dataDir+"/") || v == dataDir:
			certs = true
		case filepath.Dir(v) == filepath.Dir(dataDir) && strings.HasSuffix(v, ".conf"):
			kubeconfigs[v] = true
		}
	}
	addVolume := func(name, path string, typ v1.HostPathType) {
		pod.Spec.Volumes = append(pod.Spec.Volumes, v1.Volume{Name: name, VolumeSource: v1.VolumeSource{
			HostPath: &v1.HostPathVolumeSource{Path: path, Type: ptr.To(typ)},
		}})
		ctr.VolumeMounts = append(ctr.VolumeMounts, v1.VolumeMount{Name: name, MountPath: path, ReadOnly: true})
	}
	if certs {
		addVolume("k8s-certs", dataDir, v1.HostPathDirectoryOrCreate)
	}
	for path := range kubeconfigs { // one per component
		addVolume("kubeconfig", path, v1.HostPathFileOrCreate)
	}
	pod.Spec.Containers = []v1.Container{ctr}
	return pod
}

// staticPodOf is the pod the kubelet makes of a manifest: the node name in
// its name and spec, and a toleration of every NoExecute taint.
func staticPodOf(manifest *v1.Pod, nodeName string) *v1.Pod {
	pod := manifest.DeepCopy()
	pod.Name = manifest.Name + "-" + nodeName
	pod.Spec.NodeName = nodeName
	pod.Spec.Tolerations = append(pod.Spec.Tolerations, v1.Toleration{Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoExecute})
	return pod
}

// staticPodHash is the config hash of a static pod: equal pods, equal hash.
func staticPodHash(pod *v1.Pod) string {
	b, err := json.Marshal(pod)
	must(err)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// mirrorPodOf is the mirror pod of a static pod in the API. NodeRestriction
// lets a node create it with exactly one owner reference: its Node, as
// controller.
func mirrorPodOf(static *v1.Pod, hash string, seen time.Time, nodeName string, nodeUID types.UID) *v1.Pod {
	pod := static.DeepCopy()
	pod.TypeMeta = metav1.TypeMeta{}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[configHashKey] = hash
	pod.Annotations[configMirrorKey] = hash
	pod.Annotations[configSeenKey] = seen.Format(time.RFC3339Nano)
	pod.Annotations[configSourceKey] = "file"
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "Node", Name: nodeName, UID: nodeUID, Controller: ptr.To(true),
	}}
	return pod
}

// staticPod is the static pod of one component.
type staticPod struct {
	c           *component
	spec        componentSpec
	manifest    *v1.Pod // the file
	pod         *v1.Pod // the kubelet's view of it
	hash        string
	seen        time.Time
	containerID string

	// under staticPods.mu
	mirror     *v1.Pod // the mirror pod as last seen, nil when there is none
	ready      bool
	readySince time.Time
	written    v1.PodStatus // the status last written to the mirror pod writtenUID
	writtenUID types.UID
}

func newStaticPod(c *component, nodeName string) *staticPod {
	manifest := staticManifest(c.name, c.args)
	pod := staticPodOf(manifest, nodeName)
	return &staticPod{
		c:           c,
		spec:        controlPlaneSpec(c.name),
		manifest:    manifest,
		pod:         pod,
		hash:        staticPodHash(pod),
		seen:        time.Now(),
		containerID: "tabernetes://" + randToken(32),
		readySince:  c.start,
	}
}

func (sp *staticPod) image() string { return sp.spec.image }

// imageID is repo@digest of the image, like the ones the node lists.
func (sp *staticPod) imageID() string {
	repo, tag := splitImage(sp.spec.image)
	return repo + "@" + imageDigest(repo, tag)
}

func splitImage(image string) (repo, tag string) {
	slash := strings.LastIndex(image, "/") + 1
	if name, tag, ok := strings.Cut(image[slash:], ":"); ok {
		return image[:slash] + name, tag
	}
	return image, "latest"
}

// status is the pod status of the static pod: the container of the
// component, Ready from its health endpoint.
func (sp *staticPod) status(ready bool, readySince time.Time, ip string, qos v1.PodQOSClass) v1.PodStatus {
	t := func(t time.Time) metav1.Time { return metav1.NewTime(t.Truncate(time.Second)) }
	start := t(sp.c.start)
	exitedAt := sp.c.exited()
	ready = ready && exitedAt.IsZero()
	name := sp.c.name
	cs := v1.ContainerStatus{
		Name:        name,
		Image:       sp.image(),
		ImageID:     sp.imageID(),
		ContainerID: sp.containerID,
		Ready:       ready,
		Started:     ptr.To(exitedAt.IsZero()),
	}
	if exitedAt.IsZero() {
		cs.State.Running = &v1.ContainerStateRunning{StartedAt: start}
	} else {
		cs.State.Waiting = &v1.ContainerStateWaiting{
			Reason:  "CrashLoopBackOff",
			Message: fmt.Sprintf("back-off 5m0s restarting failed container=%s pod=%s_%s(%s)", name, sp.pod.Name, sp.pod.Namespace, sp.hash),
		}
		cs.LastTerminationState.Terminated = &v1.ContainerStateTerminated{
			ExitCode: 1, Reason: "Error", StartedAt: start, FinishedAt: t(exitedAt), ContainerID: sp.containerID,
		}
	}
	readyStatus, reason, msg := v1.ConditionTrue, "", ""
	if !ready {
		readyStatus, reason, msg = v1.ConditionFalse, "ContainersNotReady", "containers with unready status: ["+name+"]"
	}
	since := t(readySince)
	return v1.PodStatus{
		Phase: v1.PodRunning,
		Conditions: []v1.PodCondition{
			{Type: v1.PodReadyToStartContainers, Status: v1.ConditionTrue, LastTransitionTime: start},
			{Type: v1.PodInitialized, Status: v1.ConditionTrue, LastTransitionTime: start},
			{Type: v1.PodReady, Status: readyStatus, Reason: reason, Message: msg, LastTransitionTime: since},
			{Type: v1.ContainersReady, Status: readyStatus, Reason: reason, Message: msg, LastTransitionTime: since},
			{Type: v1.PodScheduled, Status: v1.ConditionTrue, LastTransitionTime: start},
		},
		HostIP:            ip,
		HostIPs:           []v1.HostIP{{IP: ip}},
		PodIP:             ip,
		PodIPs:            []v1.PodIP{{IP: ip}},
		StartTime:         &start,
		QOSClass:          qos,
		ContainerStatuses: []v1.ContainerStatus{cs},
	}
}

// staticPods keeps the mirror pods of the control plane in the API.
type staticPods struct {
	n    *tabNode
	pods []*staticPod
	hc   *http.Client
	kick chan struct{}

	mu sync.Mutex
}

func newStaticPods(n *tabNode, comps []*component) *staticPods {
	m := &staticPods{
		n:    n,
		kick: make(chan struct{}, 1),
		hc: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			DialContext:         (&net.Dialer{}).DialContext,           // Go networking, not fetch()
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, // like a kubelet probe
			MaxIdleConnsPerHost: 1,
			IdleConnTimeout:     time.Minute, // longer than the check period: one connection per component
		}},
	}
	for _, c := range comps {
		m.pods = append(m.pods, newStaticPod(c, tabNodeName))
	}
	return m
}

// start writes the manifests, marks the node as control plane node and
// keeps the mirror pods until ctx ends.
func (m *staticPods) start(ctx context.Context) error {
	if err := os.MkdirAll(staticPodManifests, 0o755); err != nil {
		return err
	}
	for _, sp := range m.pods {
		b, err := yaml.Marshal(sp.manifest)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(staticPodManifests, sp.c.name+".yaml"), b, 0o600); err != nil {
			return err
		}
	}
	// NodeRestriction keeps a kubelet from setting node-role labels; kubeadm
	// sets them with admin credentials, too.
	patch := fmt.Sprintf(`{"metadata":{"labels":{%q:"","node.kubernetes.io/exclude-from-external-load-balancers":""}}}`, controlPlaneRole)
	if _, err := m.n.admin.CoreV1().Nodes().Patch(ctx, tabNodeName, types.MergePatchType, []byte(patch), metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("labeling node %s: %w", tabNodeName, err)
	}
	go m.run(ctx)
	return nil
}

func (m *staticPods) run(ctx context.Context) {
	m.probe(ctx)
	tick := time.NewTicker(staticSyncPeriod)
	defer tick.Stop()
	for {
		m.syncAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
		case <-tick.C:
			m.probe(ctx)
		}
	}
}

func (m *staticPods) wake() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// byName finds the static pod of namespace/name, nil for none.
func (m *staticPods) byName(namespace, name string) *staticPod {
	if m == nil || namespace != staticNamespace {
		return nil
	}
	for _, sp := range m.pods {
		if sp.pod.Name == name {
			return sp
		}
	}
	return nil
}

// observe takes a pod of the node informer. It reports whether the pod
// belongs to the manager; such pods do not reach the pod workers.
func (m *staticPods) observe(pod *v1.Pod) bool {
	sp := m.byName(pod.Namespace, pod.Name)
	if sp == nil {
		return false
	}
	m.mu.Lock()
	sp.mirror = pod
	m.mu.Unlock()
	m.wake()
	return true
}

// observeGone takes a deleted pod of the node informer, like observe.
func (m *staticPods) observeGone(pod *v1.Pod) bool {
	sp := m.byName(pod.Namespace, pod.Name)
	if sp == nil {
		return false
	}
	m.forget(sp, pod.UID)
	m.wake()
	return true
}

func (m *staticPods) forget(sp *staticPod, uid types.UID) {
	m.mu.Lock()
	if sp.mirror != nil && sp.mirror.UID == uid {
		sp.mirror = nil
	}
	m.mu.Unlock()
}

// probe checks the health endpoint of every component.
func (m *staticPods) probe(ctx context.Context) {
	for _, sp := range m.pods {
		ok := m.healthy(ctx, sp)
		m.mu.Lock()
		if ok != sp.ready {
			sp.ready, sp.readySince = ok, time.Now()
		}
		m.mu.Unlock()
	}
}

func (m *staticPods) healthy(ctx context.Context, sp *staticPod) bool {
	if !sp.c.exited().IsZero() {
		return false
	}
	url := fmt.Sprintf("%s://127.0.0.1:%d%s", strings.ToLower(string(sp.spec.scheme)), sp.spec.port, sp.spec.readyPath())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := m.hc.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, resp.Body) // the connection goes back to the pool
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func (m *staticPods) syncAll(ctx context.Context) {
	for _, sp := range m.pods {
		if err := m.sync(ctx, sp); err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "tab node: mirror pod %s/%s: %v\n", staticNamespace, sp.pod.Name, err)
		}
	}
}

// sync brings the mirror pod of sp in line: there, with the hash of the
// static pod, not being deleted, with the status of the component.
func (m *staticPods) sync(ctx context.Context, sp *staticPod) error {
	pods := m.n.client.CoreV1().Pods(staticNamespace)
	m.mu.Lock()
	mirror := sp.mirror
	m.mu.Unlock()
	if mirror != nil && (mirror.DeletionTimestamp != nil || mirror.Annotations[configMirrorKey] != sp.hash) {
		// A deleted or outdated mirror pod goes at once and comes back new.
		err := pods.Delete(ctx, mirror.Name, metav1.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](0),
			Preconditions:      &metav1.Preconditions{UID: &mirror.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return err
		}
		m.forget(sp, mirror.UID)
		mirror = nil
	}
	if mirror == nil {
		created, err := pods.Create(ctx, mirrorPodOf(sp.pod, sp.hash, sp.seen, tabNodeName, m.n.nodeUID), metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			// The old one is still there; the next round deletes it.
			if old, err := pods.Get(ctx, sp.pod.Name, metav1.GetOptions{}); err == nil {
				m.observeLocked(sp, old)
			}
			return nil
		}
		if err != nil {
			return err
		}
		m.observeLocked(sp, created)
		mirror = created
	}
	return m.writeStatus(ctx, sp, mirror)
}

func (m *staticPods) observeLocked(sp *staticPod, pod *v1.Pod) {
	m.mu.Lock()
	sp.mirror = pod
	m.mu.Unlock()
}

// desired is the status of mirror as the manager sees it now.
func (m *staticPods) desired(sp *staticPod, mirror *v1.Pod) v1.PodStatus {
	m.mu.Lock()
	ready, since := sp.ready, sp.readySince
	m.mu.Unlock()
	st := sp.status(ready, since, tabNodeIP, mirror.Status.QOSClass)
	st.ObservedGeneration = mirror.Generation
	return st
}

func (m *staticPods) writeStatus(ctx context.Context, sp *staticPod, mirror *v1.Pod) error {
	st := m.desired(sp, mirror)
	m.mu.Lock()
	same := sp.writtenUID == mirror.UID && equality.Semantic.DeepEqual(sp.written, st)
	m.mu.Unlock()
	if same {
		return nil
	}
	pods := m.n.client.CoreV1().Pods(staticNamespace)
	pod := mirror.DeepCopy()
	for range 3 {
		pod.Status = st
		updated, err := pods.UpdateStatus(ctx, pod, metav1.UpdateOptions{})
		if apierrors.IsConflict(err) {
			if pod, err = pods.Get(ctx, mirror.Name, metav1.GetOptions{}); err != nil {
				return err
			}
			if pod.UID != mirror.UID {
				return nil // replaced meanwhile; the next round writes the new one
			}
			continue
		}
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		m.mu.Lock()
		sp.written, sp.writtenUID = st, mirror.UID
		if sp.mirror != nil && sp.mirror.UID == updated.UID {
			sp.mirror = updated
		}
		m.mu.Unlock()
		return nil
	}
	return errors.New("status update conflicts")
}

// podList is the mirror pods for GET /pods of the kubelet API.
func (m *staticPods) podList() []v1.Pod {
	if m == nil {
		return nil
	}
	var list []v1.Pod
	for _, sp := range m.pods {
		m.mu.Lock()
		mirror := sp.mirror
		m.mu.Unlock()
		if mirror == nil {
			continue
		}
		pod := mirror.DeepCopy()
		pod.Status = m.desired(sp, mirror)
		list = append(list, *pod)
	}
	return list
}

// images are the images of the static pods for the node status.
func (m *staticPods) images() []v1.ContainerImage {
	if m == nil {
		return nil
	}
	var list []v1.ContainerImage
	for _, sp := range m.pods {
		list = append(list, v1.ContainerImage{Names: []string{sp.imageID(), sp.image()}, SizeBytes: sp.spec.size})
	}
	return list
}

// ---- kubelet API (tabnode_server.go) ----

// lookup finds the static pod and container of a kubelet API request. It
// reports false when the pod is no static pod; errors are answered.
func (m *staticPods) lookup(w http.ResponseWriter, r *http.Request, withCtr bool) (*staticPod, bool) {
	sp := m.byName(r.PathValue("ns"), r.PathValue("pod"))
	if sp == nil {
		return nil, false
	}
	m.mu.Lock()
	mirror := sp.mirror
	m.mu.Unlock()
	if uid := r.PathValue("uid"); uid != "" && (mirror == nil || types.UID(uid) != mirror.UID) {
		http.Error(w, fmt.Sprintf("pod %q does not exist", r.PathValue("pod")), http.StatusNotFound)
		return nil, true
	}
	if withCtr && r.PathValue("ctr") != sp.c.name {
		http.Error(w, fmt.Sprintf("container %q not found in pod %q", r.PathValue("ctr"), sp.pod.Name), http.StatusNotFound)
		return nil, true
	}
	return sp, true
}

// serveLogs serves the log of a component; it reports whether the request
// was for a static pod.
func (m *staticPods) serveLogs(w http.ResponseWriter, r *http.Request, o logOptions, previous bool) bool {
	sp, ok := m.lookup(w, r, true)
	if sp == nil {
		return ok
	}
	if previous && sp.c.exited().IsZero() {
		http.Error(w, fmt.Sprintf("previous terminated container %q in pod %q not found", sp.c.name, sp.pod.Name), http.StatusBadRequest)
		return true
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	sp.c.logs.copyTo(r.Context(), w, o)
	return true
}

// serveStream serves exec (refused, the images of the control plane have
// no shell) and attach (the log from now on).
func (m *staticPods) serveStream(w http.ResponseWriter, r *http.Request, verb string, opts *remotecommand.Options) bool {
	sp, ok := m.lookup(w, r, true)
	if sp == nil {
		return ok
	}
	if !sp.c.exited().IsZero() {
		http.Error(w, fmt.Sprintf("container %q in pod %q is not running", sp.c.name, sp.pod.Name), http.StatusBadRequest)
		return true
	}
	uid := m.mirrorUID(sp)
	s := staticStreamer{sp}
	if verb == "exec" {
		cmd := r.URL.Query()["command"]
		if len(cmd) == 0 {
			http.Error(w, "no command given", http.StatusBadRequest)
			return true
		}
		remotecommand.ServeExec(w, r, s, sp.pod.Name, uid, sp.c.name, cmd, opts,
			streamIdleTimeout, streamCreationTimeout, remotecommand.SupportedStreamingProtocols)
		return true
	}
	remotecommand.ServeAttach(w, r, s, sp.pod.Name, uid, sp.c.name, opts,
		streamIdleTimeout, streamCreationTimeout, remotecommand.SupportedStreamingProtocols)
	return true
}

// servePortForward forwards to localhost of the node: the pods use the
// host network.
func (m *staticPods) servePortForward(w http.ResponseWriter, r *http.Request, opts *portforward.V4Options) bool {
	sp, ok := m.lookup(w, r, false)
	if sp == nil {
		return ok
	}
	portforward.ServePortForward(w, r, staticStreamer{sp}, sp.pod.Name, m.mirrorUID(sp), opts,
		streamIdleTimeout, streamCreationTimeout, portforward.SupportedProtocols)
	return true
}

func (m *staticPods) mirrorUID(sp *staticPod) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sp.mirror == nil {
		return ""
	}
	return string(sp.mirror.UID)
}

// staticStreamer is the container runtime side of a static pod.
type staticStreamer struct{ sp *staticPod }

// ExecInContainer fails like containerd for a distroless image: the
// control plane images have no shell, and here not even a file system.
func (s staticStreamer) ExecInContainer(_ context.Context, _, _, _ string, cmd []string, _ io.Reader, _, _ io.WriteCloser, _ bool, resize <-chan remotecommand.TerminalSize, _ time.Duration) error {
	go drainResize(resize)
	id := make([]byte, 32)
	rand.Read(id)
	return fmt.Errorf("failed to exec in container: failed to start exec %q: OCI runtime exec failed: exec failed: unable to start container process: exec: %q: executable file not found in $PATH: unknown",
		hex.EncodeToString(id), cmd[0])
}

// AttachContainer follows the log of the component.
func (s staticStreamer) AttachContainer(ctx context.Context, _, _, _ string, _ io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	go drainResize(resize)
	var stdout, stderr io.Writer
	if out != nil {
		stdout = out
	}
	if errOut != nil {
		stderr = errOut
	} else {
		stderr = stdout // a terminal has one output
	}
	if err := s.sp.c.logs.follow(ctx, stdout, stderr); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// PortForward connects to port on localhost of the node.
func (s staticStreamer) PortForward(ctx context.Context, _, _ string, port int32, stream io.ReadWriteCloser) error {
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		_, err := io.Copy(conn, stream)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok && err == nil {
			cw.CloseWrite()
		} else {
			conn.Close()
		}
	}()
	if _, err := io.Copy(stream, conn); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
