package main

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

var testArgs = []string{"--kubeconfig=" + kubeconfigPath("scheduler"), "--secure-port=10259", "--cert-dir=" + p("certs")}

func testComponent(name string, args []string) *component {
	return &component{name: name, args: args, start: time.Date(2026, 11, 10, 8, 0, 0, 123, time.UTC), logs: newLogBuf()}
}

func TestStaticPodManifest(t *testing.T) {
	sp := newStaticPod(testComponent("kube-scheduler", testArgs), "tab")
	if sp.manifest.Name != "kube-scheduler" || sp.pod.Name != "kube-scheduler-tab" || sp.pod.Namespace != "kube-system" {
		t.Errorf("names %s, %s/%s", sp.manifest.Name, sp.pod.Namespace, sp.pod.Name)
	}
	mirror := mirrorPodOf(sp.pod, sp.hash, sp.seen, "tab", "node-uid")
	for k, want := range map[string]string{
		configHashKey: sp.hash, configMirrorKey: sp.hash, configSourceKey: "file",
	} {
		if got := mirror.Annotations[k]; got != want {
			t.Errorf("annotation %s = %q, want %q", k, got, want)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, mirror.Annotations[configSeenKey]); err != nil {
		t.Errorf("config.seen: %v", err)
	}
	if want := []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: "tab", UID: "node-uid", Controller: ptr.To(true)}}; !reflect.DeepEqual(mirror.OwnerReferences, want) {
		t.Errorf("owner references %+v", mirror.OwnerReferences)
	}
	spec := mirror.Spec
	if !spec.HostNetwork || spec.NodeName != "tab" || spec.PriorityClassName != "system-node-critical" || spec.ServiceAccountName != "" {
		t.Errorf("spec %+v", spec)
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Operator != v1.TolerationOpExists || spec.Tolerations[0].Effect != v1.TaintEffectNoExecute {
		t.Errorf("tolerations %+v", spec.Tolerations)
	}
	if mirror.Labels["component"] != "kube-scheduler" || mirror.Labels["tier"] != "control-plane" {
		t.Errorf("labels %v", mirror.Labels)
	}
	c := spec.Containers[0]
	if want := append([]string{"kube-scheduler"}, testArgs...); !reflect.DeepEqual(c.Command, want) {
		t.Errorf("command %q, want %q", c.Command, want)
	}
	if c.ReadinessProbe == nil || c.ReadinessProbe.HTTPGet.Path != "/readyz" || c.ReadinessProbe.HTTPGet.Port.IntValue() != 10259 {
		t.Errorf("readiness probe %+v", c.ReadinessProbe)
	}
	// Volumes only for what the command line names; nothing references
	// an API object (NodeRestriction).
	var vols []string
	for _, v := range spec.Volumes {
		if v.HostPath == nil {
			t.Errorf("volume %s is no hostPath", v.Name)
		}
		vols = append(vols, v.Name+"="+v.HostPath.Path)
	}
	if want := []string{"k8s-certs=" + dataDir, "kubeconfig=" + kubeconfigPath("scheduler")}; !reflect.DeepEqual(vols, want) {
		t.Errorf("volumes %v, want %v", vols, want)
	}
	for _, vm := range c.VolumeMounts {
		if !vm.ReadOnly {
			t.Errorf("mount %s is writable", vm.Name)
		}
	}

	if again := newStaticPod(testComponent("kube-scheduler", testArgs), "tab"); again.hash != sp.hash {
		t.Errorf("hash %s, then %s for the same manifest", sp.hash, again.hash)
	}
	other := newStaticPod(testComponent("kube-scheduler", append(testArgs[:2:2], "--v=2")), "tab")
	if other.hash == sp.hash {
		t.Error("same hash for different arguments")
	}
	etcd := newStaticPod(testComponent("etcd", etcdArgs), "tab")
	if !strings.HasPrefix(etcd.image(), "registry.k8s.io/etcd:3.") || len(etcd.pod.Spec.Volumes) != 0 {
		t.Errorf("etcd: image %s, volumes %v", etcd.image(), etcd.pod.Spec.Volumes)
	}
}

func TestStaticPodStatus(t *testing.T) {
	sp := newStaticPod(testComponent("kube-apiserver", nil), "tab")
	ready := sp.c.start.Add(3 * time.Second)
	cond := func(st v1.PodStatus, typ v1.PodConditionType) v1.PodCondition {
		for _, c := range st.Conditions {
			if c.Type == typ {
				return c
			}
		}
		t.Fatalf("no condition %s", typ)
		return v1.PodCondition{}
	}

	st := sp.status(true, ready, "10.0.0.1", v1.PodQOSBurstable)
	cs := st.ContainerStatuses[0]
	if st.Phase != v1.PodRunning || st.PodIP != "10.0.0.1" || st.HostIP != "10.0.0.1" || st.QOSClass != v1.PodQOSBurstable {
		t.Errorf("status %+v", st)
	}
	if !cs.Ready || cs.State.Running == nil || !cs.State.Running.StartedAt.Time.Equal(sp.c.start.Truncate(time.Second)) || cs.RestartCount != 0 || !*cs.Started {
		t.Errorf("container status %+v", cs)
	}
	if !strings.HasPrefix(cs.ContainerID, "tabernetes://") || !strings.HasPrefix(cs.ImageID, "registry.k8s.io/kube-apiserver@sha256:") {
		t.Errorf("ids %s %s", cs.ContainerID, cs.ImageID)
	}
	if c := cond(st, v1.PodReady); c.Status != v1.ConditionTrue || !c.LastTransitionTime.Time.Equal(ready.Truncate(time.Second)) {
		t.Errorf("ready %+v", c)
	}

	st = sp.status(false, ready, "10.0.0.1", "")
	if cs := st.ContainerStatuses[0]; cs.Ready || cs.State.Running == nil {
		t.Errorf("not ready: %+v", cs)
	}
	if c := cond(st, v1.ContainersReady); c.Status != v1.ConditionFalse || c.Reason != "ContainersNotReady" {
		t.Errorf("containers ready %+v", c)
	}

	sp.c.setExited(context.Canceled)
	st = sp.status(true, ready, "10.0.0.1", "")
	cs = st.ContainerStatuses[0]
	if cs.Ready || cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" ||
		cs.LastTerminationState.Terminated == nil || cs.LastTerminationState.Terminated.ExitCode != 1 || cs.LastTerminationState.Terminated.Reason != "Error" {
		t.Errorf("exited: %+v", cs)
	}
	if c := cond(st, v1.PodReady); c.Status != v1.ConditionFalse {
		t.Errorf("ready after exit %+v", c)
	}
}

func TestMirrorPodSync(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	// The fake gives no UIDs; the apiserver does.
	var seq int
	var mu sync.Mutex
	client.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		seq++
		a.(k8stesting.CreateAction).GetObject().(*v1.Pod).UID = types.UID("uid-" + string(rune('0'+seq)))
		mu.Unlock()
		return false, nil, nil
	})
	n := &tabNode{client: client, admin: client, nodeUID: "node-uid"}
	m := newStaticPods(n, []*component{testComponent("kube-scheduler", testArgs)})
	sp := m.pods[0]
	m.mu.Lock()
	sp.ready, sp.readySince = true, sp.c.start.Add(time.Second)
	m.mu.Unlock()
	pods := client.CoreV1().Pods("kube-system")
	get := func() *v1.Pod {
		t.Helper()
		pod, err := pods.Get(ctx, "kube-scheduler-tab", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return pod
	}
	sync := func() {
		t.Helper()
		if err := m.sync(ctx, sp); err != nil {
			t.Fatal(err)
		}
	}

	sync()
	first := get()
	if first.Annotations[configMirrorKey] != sp.hash || len(first.Status.ContainerStatuses) != 1 || !first.Status.ContainerStatuses[0].Ready {
		t.Fatalf("created mirror pod %+v", first)
	}
	started := first.Status.ContainerStatuses[0].State.Running.StartedAt
	if !m.observe(first) {
		t.Fatal("the manager does not own its mirror pod")
	}
	if m.observe(&v1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "kube-scheduler-tab"}}) {
		t.Error("the manager owns a pod of another namespace")
	}

	// kubectl delete: a deletion timestamp. It goes at once and comes back.
	deleting := first.DeepCopy()
	deleting.DeletionTimestamp = ptr.To(metav1.Now())
	if _, err := pods.Update(ctx, deleting, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	m.observe(deleting)
	sync()
	second := get()
	if second.UID == first.UID || second.DeletionTimestamp != nil {
		t.Fatalf("not recreated: uid %s, deletion %v", second.UID, second.DeletionTimestamp)
	}
	if got := second.Status.ContainerStatuses[0]; !got.State.Running.StartedAt.Equal(&started) || got.ContainerID != first.Status.ContainerStatuses[0].ContainerID {
		t.Errorf("the container changed with the mirror pod: %+v", got)
	}

	// A wrong hash: replaced.
	wrong := second.DeepCopy()
	wrong.Annotations[configMirrorKey] = "outdated"
	if _, err := pods.Update(ctx, wrong, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	m.observe(wrong)
	sync()
	third := get()
	if third.UID == second.UID || third.Annotations[configMirrorKey] != sp.hash {
		t.Errorf("not replaced: uid %s, hash %s", third.UID, third.Annotations[configMirrorKey])
	}

	// Gone from the informer: created again.
	if err := pods.Delete(ctx, third.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if !m.observeGone(third) {
		t.Fatal("observeGone")
	}
	sync()
	if fourth := get(); fourth.UID == third.UID {
		t.Error("not created again after the delete")
	}

	// The status is written only when it changed.
	before := len(client.Actions())
	sync()
	if after := client.Actions(); len(after) != before {
		t.Errorf("unchanged status written again: %v", after[before:])
	}
	if list := m.podList(); len(list) != 1 || list[0].Name != "kube-scheduler-tab" {
		t.Errorf("pod list %v", list)
	}
	if imgs := m.images(); len(imgs) != 1 || !strings.HasPrefix(imgs[0].Names[1], "registry.k8s.io/kube-scheduler:") {
		t.Errorf("images %v", imgs)
	}
}

func TestLogSplit(t *testing.T) {
	var fallback bytes.Buffer
	var fbMu sync.Mutex
	split := logSplitter{fallback: writerFunc(func(p []byte) (int, error) {
		fbMu.Lock()
		defer fbMu.Unlock()
		return fallback.Write(p)
	})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		labelGoroutine("test-a")
		split.Write([]byte("root of a\n"))
		child := make(chan struct{})
		go func() {
			defer close(child)
			split.Write([]byte("child of a\n"))
			grandchild := make(chan struct{})
			go func() {
				defer close(grandchild)
				split.Write([]byte("grandchild of a\n"))
			}()
			<-grandchild
		}()
		<-child
	}()
	<-done
	other := make(chan struct{})
	go func() {
		defer close(other)
		labelGoroutine("test-b")
		split.Write([]byte("b\n"))
	}()
	<-other
	unlabelled := make(chan struct{})
	go func() {
		defer close(unlabelled)
		split.Write([]byte("nobody\n"))
	}()
	<-unlabelled

	read := func(name string) string {
		var b bytes.Buffer
		componentLogs(name).copyTo(context.Background(), &b, logOptions{tail: -1})
		return b.String()
	}
	if got, want := read("test-a"), "root of a\nchild of a\ngrandchild of a\n"; got != want {
		t.Errorf("log of a %q, want %q", got, want)
	}
	if got := read("test-b"); got != "b\n" {
		t.Errorf("log of b %q", got)
	}
	fbMu.Lock()
	defer fbMu.Unlock()
	for _, line := range []string{"root of a", "child of a", "grandchild of a", "b", "nobody"} {
		if !strings.Contains(fallback.String(), line+"\n") {
			t.Errorf("%q did not reach the fallback", line)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestTabNodeInfo(t *testing.T) {
	info := tabNodeInfo()
	all := strings.ToLower(strings.Join([]string{info.MachineID, info.SystemUUID, info.BootID, info.KernelVersion,
		info.OSImage, info.ContainerRuntimeVersion, info.KubeletVersion, info.OperatingSystem, info.Architecture}, " "))
	for _, bad := range []string{"browser", "mozilla", "chrome", "webassembly", "firefox", "safari"} {
		if strings.Contains(all, bad) {
			t.Errorf("node info names %q: %+v", bad, info)
		}
	}
	if !strings.HasPrefix(info.OSImage, "Tabernetes ") || info.KernelVersion != kernelVersion {
		t.Errorf("node info %+v", info)
	}
	if len(info.MachineID) != 32 || len(info.SystemUUID) != 36 || len(info.BootID) != 36 || strings.Count(info.BootID, "-") != 4 {
		t.Errorf("ids %q %q %q", info.MachineID, info.SystemUUID, info.BootID)
	}
	if again := tabNodeInfo(); again != info {
		t.Error("node info changes while the node runs")
	}
	if taints := tabNodeTaints(true); len(taints) != 2 || taints[0].Key != controlPlaneRole || taints[0].Effect != v1.TaintEffectPreferNoSchedule {
		t.Errorf("control plane taints %v", taints)
	}
	if taints := tabNodeTaints(false); len(taints) != 1 || taints[0].Key != tabTaintKey {
		t.Errorf("worker taints %v", taints)
	}
}

func TestEtcdLogLine(t *testing.T) {
	ts := time.Date(2026, 11, 10, 8, 0, 0, 123456000, time.UTC)
	got := string(etcdLogLine(ts, "info", "etcdshim/server.go:34", "finished scheduled compaction", "compact-revision", int64(42)))
	want := `{"level":"info","ts":"2026-11-10T08:00:00.123456Z","caller":"etcdshim/server.go:34","msg":"finished scheduled compaction","compact-revision":42}` + "\n"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// The node lifecycle controller may taint the node between the read and the
// write of the status. The write must not carry the spec that was read:
// NodeRestriction would see the node drop the taint and refuse it.
func TestNodeStatusKeepsTaints(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset(&v1.Node{ObjectMeta: metav1.ObjectMeta{Name: tabNodeName}})
	n := &tabNode{client: client}
	tainted := false
	client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := client.Tracker().Get(v1.SchemeGroupVersion.WithResource("nodes"), "", tabNodeName)
		if err != nil {
			return true, nil, err
		}
		read := obj.(*v1.Node).DeepCopy()
		if !tainted {
			tainted = true
			now := obj.(*v1.Node).DeepCopy()
			now.Spec.Taints = []v1.Taint{{Key: "node.kubernetes.io/not-ready", Effect: v1.TaintEffectNoSchedule}}
			if err := client.Tracker().Update(v1.SchemeGroupVersion.WithResource("nodes"), now, ""); err != nil {
				return true, nil, err
			}
		}
		return true, read, nil
	})
	if err := n.updateStatus(ctx); err != nil {
		t.Fatal(err)
	}
	for _, a := range client.Actions() {
		switch a := a.(type) {
		case k8stesting.UpdateAction:
			t.Errorf("the node is updated as a whole (%s %s)", a.GetVerb(), a.GetSubresource())
		case k8stesting.PatchAction:
			if a.GetSubresource() != "status" || strings.Contains(string(a.GetPatch()), "spec") || strings.Contains(string(a.GetPatch()), "taints") {
				t.Errorf("patch of %q: %s", a.GetSubresource(), a.GetPatch())
			}
		}
	}
	node, err := client.CoreV1().Nodes().Get(ctx, tabNodeName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Spec.Taints) != 1 || node.Spec.Taints[0].Key != "node.kubernetes.io/not-ready" {
		t.Errorf("taints after the status write: %v", node.Spec.Taints)
	}
	ready := false
	for _, c := range node.Status.Conditions {
		ready = ready || c.Type == v1.NodeReady && c.Status == v1.ConditionTrue
	}
	if !ready || node.Status.NodeInfo.KernelVersion != kernelVersion {
		t.Errorf("status %+v", node.Status)
	}
}
