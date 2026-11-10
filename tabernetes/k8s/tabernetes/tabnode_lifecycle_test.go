package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

// testWorker is a pod worker without a node: enough for hooks and status.
func testWorker(t *testing.T, spec v1.PodSpec) *podWorker {
	t.Helper()
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"}, Spec: spec}
	w := newPodWorker(nil, pod)
	w.ip = "127.0.0.1"
	t.Cleanup(func() { w.cancel(); w.kill() })
	return w
}

func TestKillGrace(t *testing.T) {
	probe := &v1.Probe{TerminationGracePeriodSeconds: ptr.To[int64](3)}
	for _, tc := range []struct {
		name  string
		pod   v1.Pod
		probe *v1.Probe
		want  time.Duration
	}{
		{"default", v1.Pod{}, nil, 2 * time.Second},
		{"pod", v1.Pod{Spec: v1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](30)}}, nil, 30 * time.Second},
		{"probe", v1.Pod{Spec: v1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](30)}}, probe, 3 * time.Second},
		{"probe without its own", v1.Pod{Spec: v1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](30)}}, &v1.Probe{}, 30 * time.Second},
		{"deletion wins", v1.Pod{
			ObjectMeta: metav1.ObjectMeta{DeletionGracePeriodSeconds: ptr.To[int64](5)},
			Spec:       v1.PodSpec{TerminationGracePeriodSeconds: ptr.To[int64](30)},
		}, probe, 5 * time.Second},
	} {
		if got := killGrace(&tc.pod, tc.probe); got != tc.want {
			t.Errorf("%s: killGrace = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestActiveDeadline(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, ok := activeDeadline(start, nil); ok {
		t.Error("deadline without activeDeadlineSeconds")
	}
	if _, ok := activeDeadline(time.Time{}, ptr.To[int64](5)); ok {
		t.Error("deadline without a start time")
	}
	if at, ok := activeDeadline(start, ptr.To[int64](90)); !ok || !at.Equal(start.Add(90*time.Second)) {
		t.Errorf("activeDeadline = %v, %v", at, ok)
	}
}

func TestDeadlineExceededPhase(t *testing.T) {
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.initDone = true
	w.ctrs[0].state = v1.ContainerState{Running: &v1.ContainerStateRunning{}}
	w.ctrs[0].ready = true
	if st := w.buildStatus(); st.Phase != v1.PodRunning {
		t.Fatalf("phase %s, want Running", st.Phase)
	}
	w.failReason, w.failMessage = deadlineReason, deadlineMessage
	st := w.buildStatus()
	if st.Phase != v1.PodFailed || st.Reason != "DeadlineExceeded" || st.Message != deadlineMessage {
		t.Errorf("status %s %q %q, want Failed DeadlineExceeded", st.Phase, st.Reason, st.Message)
	}
	for _, c := range st.Conditions {
		if c.Type == v1.PodReady && c.Status != v1.ConditionFalse {
			t.Errorf("a failed pod is ready")
		}
	}
}

func TestEphemeralContainers(t *testing.T) {
	w := testWorker(t, v1.PodSpec{
		Containers: []v1.Container{{Name: "app", Image: "nginx"}},
		EphemeralContainers: []v1.EphemeralContainer{{
			EphemeralContainerCommon: v1.EphemeralContainerCommon{Name: "debugger", Image: "busybox", Stdin: true},
			TargetContainerName:      "app",
		}},
	})
	c := w.container("debugger")
	if c == nil || !c.ephemeral || c.stdin == nil {
		t.Fatalf("debugger: %+v", c)
	}
	if got := c.fieldPath(); got != "spec.ephemeralContainers{debugger}" {
		t.Errorf("fieldPath %s", got)
	}
	if w.defaultContainer().spec.Name != "app" {
		t.Error("an ephemeral container is the default container")
	}

	// kubectl debug adds one more through the informer.
	pod, _ := w.podInfo()
	pod = pod.DeepCopy()
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, v1.EphemeralContainer{
		EphemeralContainerCommon: v1.EphemeralContainerCommon{Name: "debugger-2", Image: "busybox"},
	})
	w.update(pod)
	if w.container("debugger-2") == nil || len(w.ephemeral) != 2 {
		t.Fatal("update did not add the second ephemeral container")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	w.initDone = true
	w.ctrs[0].state = v1.ContainerState{Running: &v1.ContainerStateRunning{}}
	w.ctrs[0].ready = true
	c.state = v1.ContainerState{Terminated: &v1.ContainerStateTerminated{ExitCode: 1}}
	c.finished = true
	st := w.buildStatus()
	if st.Phase != v1.PodRunning {
		t.Errorf("phase %s: ephemeral containers must not count", st.Phase)
	}
	if len(st.ContainerStatuses) != 1 || len(st.EphemeralContainerStatuses) != 2 {
		t.Fatalf("statuses: %d regular, %d ephemeral", len(st.ContainerStatuses), len(st.EphemeralContainerStatuses))
	}
	es := st.EphemeralContainerStatuses[1]
	if es.Name != "debugger-2" || es.State.Waiting == nil || es.State.Waiting.Reason != "ContainerCreating" || es.Started != nil || es.Ready {
		t.Errorf("status of a new ephemeral container: %+v", es)
	}
	for _, cond := range st.Conditions {
		if cond.Type == v1.PodReady && cond.Status != v1.ConditionTrue {
			t.Errorf("ephemeral containers must not affect readiness")
		}
	}
}

func TestRunHook(t *testing.T) {
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	c := w.ctrs[0]
	proc := newProcess(func() {})

	if msg, err := c.runHook(context.Background(), proc, &v1.LifecycleHandler{Sleep: &v1.SleepAction{Seconds: 0}}); err != nil {
		t.Errorf("sleep 0: %v %s", err, msg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg, err := c.runHook(ctx, proc, &v1.LifecycleHandler{Sleep: &v1.SleepAction{Seconds: 5}})
	if err == nil || msg != `Sleep lifecycle hook (5) for Container "app" in Pod "p_ns(uid)" failed - error: container terminated before sleep hook finished` {
		t.Errorf("sleep cut short: %v %s", err, msg)
	}
	// tcpSocket is no handler for hooks: it fails at run time.
	msg, err = c.runHook(context.Background(), proc, &v1.LifecycleHandler{TCPSocket: &v1.TCPSocketAction{Port: intstr.FromInt32(80)}})
	if err == nil || !strings.HasPrefix(msg, "Cannot run handler: invalid handler: ") {
		t.Errorf("tcpSocket: %v %s", err, msg)
	}
}

func TestExecHook(t *testing.T) {
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	c := w.ctrs[0]
	c.env, c.dir, c.root = []string{"HOME=/root"}, "/", &rootFS{base: t.TempDir()}
	proc := newProcess(func() {})

	if out, err := c.execHook(context.Background(), proc, []string{"sh", "-c", "echo hello"}); err != nil || out != "hello\n" {
		t.Errorf("exec: %q %v", out, err)
	}
	cmd := []string{"sh", "-c", "echo out; echo err >&2; exit 3"}
	msg, err := c.runHook(context.Background(), proc, &v1.LifecycleHandler{Exec: &v1.ExecAction{Command: cmd}})
	want := `Exec lifecycle hook ([sh -c echo out; echo err >&2; exit 3]) for Container "app" in Pod "p_ns(uid)" failed - error: command 'sh -c echo out; echo err >&2; exit 3' exited with 3: err` + "\n" + `, message: "out\nerr\n"`
	if err == nil || msg != want {
		t.Errorf("failed exec:\n got %s\nwant %s", msg, want)
	}

	proc.exit(0)
	if _, err := c.execHook(context.Background(), proc, []string{"true"}); err == nil {
		t.Error("exec in a container that is not running")
	}
}

func TestHTTPHook(t *testing.T) {
	got := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		// Unlike a probe, any answer is a success.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)

	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app", Ports: []v1.ContainerPort{{Name: "http", ContainerPort: int32(p)}}}}})
	c := w.ctrs[0]
	h := &v1.LifecycleHandler{HTTPGet: &v1.HTTPGetAction{
		Path: "/hook?drain=1", Port: intstr.FromString("http"),
		HTTPHeaders: []v1.HTTPHeader{{Name: "Host", Value: "example.com"}, {Name: "X-Token", Value: "a"}},
	}}
	if msg, err := c.runHook(context.Background(), nil, h); err != nil {
		t.Fatalf("http hook: %v %s", err, msg)
	}
	r := <-got
	if r.URL.Path != "/hook" || r.URL.RawQuery != "drain=1" || r.Host != "example.com" || r.Header.Get("X-Token") != "a" ||
		r.Header.Get("User-Agent") != "kube-lifecycle/tabernetes" || r.Header.Get("Accept") != "*/*" {
		t.Errorf("request %s %s host %s headers %v", r.Method, r.URL, r.Host, r.Header)
	}

	srv.Close()
	msg, err := c.runHook(context.Background(), nil, h)
	if err == nil || !strings.HasPrefix(msg, `HTTP lifecycle hook (/hook?drain=1) for Container "app" in Pod "p_ns(uid)" failed - error: `) {
		t.Errorf("hook to a closed port: %v %s", err, msg)
	}
}

func TestHTTPGetRequest(t *testing.T) {
	c := &ctr{spec: v1.Container{Name: "app", Ports: []v1.ContainerPort{{Name: "web", ContainerPort: 8080}}}}
	for _, tc := range []struct {
		a    v1.HTTPGetAction
		want string
	}{
		{v1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("web")}, "http://10.244.0.5:8080/healthz"},
		{v1.HTTPGetAction{Path: "healthz", Port: intstr.FromInt32(81), Host: "10.0.0.1", Scheme: v1.URISchemeHTTPS}, "https://10.0.0.1:81/healthz"},
		{v1.HTTPGetAction{Port: intstr.FromString("9090")}, "http://10.244.0.5:9090"},
	} {
		req, err := c.httpGetRequest(context.Background(), &tc.a, "10.244.0.5", "probe")
		if err != nil || req.URL.String() != tc.want {
			t.Errorf("%+v: %v %v, want %s", tc.a, req.URL, err, tc.want)
		}
	}
	for _, port := range []intstr.IntOrString{intstr.FromString("nope"), intstr.FromInt32(0), intstr.FromInt32(70000)} {
		if _, err := c.httpGetRequest(context.Background(), &v1.HTTPGetAction{Port: port}, "10.244.0.5", "probe"); err == nil {
			t.Errorf("port %v: no error", port)
		}
	}
}

// The status of the tab node keeps the claim statuses that the resourceclaim
// controller and the scheduler wrote: NodeRestriction refuses an update by a
// node that changes them.
func TestStatusKeepsClaimStatuses(t *testing.T) {
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pod.Status.ResourceClaimStatuses = []v1.PodResourceClaimStatus{{Name: "gpu", ResourceClaimName: ptr.To("p-gpu-abcde")}}
	w.pod.Status.ExtendedResourceClaimStatus = &v1.PodExtendedResourceClaimStatus{
		ResourceClaimName: "p-extended-resources-abcde",
		RequestMappings:   []v1.ContainerExtendedResourceRequest{{ContainerName: "app", ResourceName: "tabernetes.io/webgpu", RequestName: "container-0-request-0"}},
	}
	st := w.buildStatus()
	if len(st.ResourceClaimStatuses) != 1 || ptr.Deref(st.ResourceClaimStatuses[0].ResourceClaimName, "") != "p-gpu-abcde" {
		t.Errorf("resourceClaimStatuses = %+v, want the one of the pod", st.ResourceClaimStatuses)
	}
	if st.ExtendedResourceClaimStatus == nil || st.ExtendedResourceClaimStatus.ResourceClaimName != "p-extended-resources-abcde" {
		t.Errorf("extendedResourceClaimStatus = %+v, want the one of the pod", st.ExtendedResourceClaimStatus)
	}
}
