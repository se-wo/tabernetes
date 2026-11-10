package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func claimPod(vols ...v1.Volume) *v1.Pod {
	mounts := []v1.VolumeMount{}
	for _, v := range vols {
		mounts = append(mounts, v1.VolumeMount{Name: v.Name, MountPath: "/" + v.Name})
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "p", UID: "pod-uid"},
		Spec:       v1.PodSpec{Volumes: vols, Containers: []v1.Container{{Name: "c", VolumeMounts: mounts}}},
	}
}

func pvcVolume(name, claim string, readOnly bool) v1.Volume {
	return v1.Volume{Name: name, VolumeSource: v1.VolumeSource{
		PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: readOnly}}}
}

func boundClaim(name, pv string) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, UID: types.UID(name + "-uid")},
		Spec:       v1.PersistentVolumeClaimSpec{VolumeName: pv},
		Status:     v1.PersistentVolumeClaimStatus{Phase: v1.ClaimBound},
	}
}

func testPV(name, claim string, src v1.PersistentVolumeSource) *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: src,
			ClaimRef:               &v1.ObjectReference{Namespace: "ns", Name: claim, UID: types.UID(claim + "-uid")},
		},
	}
}

func hostPathPV(name, claim, path string) *v1.PersistentVolume {
	return testPV(name, claim, v1.PersistentVolumeSource{HostPath: &v1.HostPathVolumeSource{Path: path}})
}

func TestClaimHostPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	client := fake.NewClientset(boundClaim("c", "pv"), hostPathPV("pv", "c", dir))
	pod := claimPod(pvcVolume("d", "c", true))
	m, err := resolveClaim(context.Background(), client, pod, &pod.Spec.Volumes[0], true)
	if err != nil {
		t.Fatal(err)
	}
	if m.dir != dir || !m.readOnly || m.pv != "pv" {
		t.Fatalf("got %+v", m)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestClaimHostPathTypes(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		typ  v1.HostPathType
		path string
		want string
	}{
		{v1.HostPathDirectory, filepath.Join(base, "missing"), `MountVolume.SetUp failed for volume "pv" : hostPath type check failed: ` + filepath.Join(base, "missing") + ` is not a directory`},
		{v1.HostPathDirectory, base, ""},
		{v1.HostPathSocket, base, `MountVolume.SetUp failed for volume "pv" : hostPath type Socket is not supported in a browser tab (supported: Directory, DirectoryOrCreate)`},
	} {
		pv := hostPathPV("pv", "c", tc.path)
		pv.Spec.HostPath.Type = ptr.To(tc.typ)
		client := fake.NewClientset(boundClaim("c", "pv"), pv)
		pod := claimPod(pvcVolume("d", "c", false))
		_, err := resolveClaim(context.Background(), client, pod, &pod.Spec.Volumes[0], true)
		if got := claimErrText(err); got != tc.want {
			t.Errorf("type %s: got %q, want %q", tc.typ, got, tc.want)
		}
	}
}

func claimErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestClaimLocal(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	os.WriteFile(file, nil, 0o644)
	missing := filepath.Join(dir, "missing")
	local := func(p string) *v1.PersistentVolume {
		return testPV("pv", "c", v1.PersistentVolumeSource{Local: &v1.LocalVolumeSource{Path: p}})
	}
	for _, tc := range []struct{ path, want string }{
		{dir, ""},
		{missing, `MountVolume.NewMounter initialization failed for volume "pv" : path "` + missing + `" does not exist`},
		{file, `MountVolume.NewMounter initialization failed for volume "pv" : path "` + file + `" is not a directory`},
	} {
		client := fake.NewClientset(boundClaim("c", "pv"), local(tc.path))
		pod := claimPod(pvcVolume("d", "c", false))
		m, err := resolveClaim(context.Background(), client, pod, &pod.Spec.Volumes[0], true)
		if got := claimErrText(err); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.path, got, tc.want)
		}
		if err == nil && m.dir != tc.path {
			t.Errorf("dir %q", m.dir)
		}
	}
}

func TestClaimErrors(t *testing.T) {
	now := metav1.Now()
	unbound := boundClaim("unbound", "")
	unbound.Status.Phase = v1.ClaimPending
	emptyName := boundClaim("empty", "")
	deleting := boundClaim("deleting", "pv-deleting")
	deleting.DeletionTimestamp = &now
	protected := boundClaim("protected", "pv-protected")
	protected.DeletionTimestamp = &now
	protected.Finalizers = []string{pvcProtectionFinalizer}
	foreign := hostPathPV("pv-foreign", "foreign", t.TempDir())
	foreign.Spec.ClaimRef.UID = "other"
	noRef := hostPathPV("pv-noref", "noref", t.TempDir())
	noRef.Spec.ClaimRef = nil
	block := hostPathPV("pv-block", "block", t.TempDir())
	block.Spec.VolumeMode = ptr.To(v1.PersistentVolumeBlock)
	nfs := testPV("pv-nfs", "nfs", v1.PersistentVolumeSource{NFS: &v1.NFSVolumeSource{Server: "s", Path: "/p"}})
	client := fake.NewClientset(unbound, emptyName, deleting, protected, boundClaim("foreign", "pv-foreign"), foreign,
		boundClaim("noref", "pv-noref"), noRef, boundClaim("block", "pv-block"), block,
		boundClaim("nfs", "pv-nfs"), nfs, boundClaim("nopv", "gone"), hostPathPV("pv-protected", "protected", t.TempDir()))
	pre := "error processing PVC ns/"
	for _, tc := range []struct {
		claim, want string
		mounted     bool
	}{
		{"unbound", pre + "unbound: PVC is not bound", true},
		{"empty", pre + "empty: PVC has empty pvc.Spec.VolumeName", true},
		{"missing", pre + `missing: failed to fetch PVC from API server: persistentvolumeclaims "missing" not found`, true},
		{"deleting", pre + "deleting: PVC is being deleted", true},
		{"protected", "", true},
		{"nopv", pre + `nopv: failed to fetch PV gone from API server: persistentvolumes "gone" not found`, true},
		{"foreign", pre + "foreign: found PV object pv-foreign but its pv.Spec.ClaimRef.UID other does not point to claim.UID foreign-uid", true},
		{"noref", pre + "noref: found PV object pv-noref but it has a nil pv.Spec.ClaimRef indicating it is not yet bound to the claim", true},
		{"block", "volume d has volumeMode Block, but is specified in volumeMounts", true},
		{"block", "", false},
		{"nfs", `MountVolume.SetUp failed for volume "pv-nfs" : this volume type does not exist in a browser tab (supported for PersistentVolumes: hostPath, local)`, true},
	} {
		pod := claimPod(pvcVolume("d", tc.claim, false))
		_, err := resolveClaim(context.Background(), client, pod, &pod.Spec.Volumes[0], tc.mounted)
		if got := claimErrText(err); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.claim, got, tc.want)
		}
	}
}

func TestClaimEphemeral(t *testing.T) {
	pod := claimPod(v1.Volume{Name: "scratch", VolumeSource: v1.VolumeSource{Ephemeral: &v1.EphemeralVolumeSource{}}})
	claim := boundClaim("p-scratch", "pv")
	client := fake.NewClientset(claim, hostPathPV("pv", "p-scratch", t.TempDir()))
	_, err := resolveClaim(context.Background(), client, pod, &pod.Spec.Volumes[0], true)
	if err == nil || !strings.Contains(err.Error(), "pod is not owner") {
		t.Fatalf("claim not owned by the pod: %v", err)
	}
	owned := claim.DeepCopy()
	owned.OwnerReferences = []metav1.OwnerReference{{UID: pod.UID, Controller: ptr.To(true)}}
	client = fake.NewClientset(owned, hostPathPV("pv", "p-scratch", t.TempDir()))
	if _, err := resolveClaim(context.Background(), client, pod, &pod.Spec.Volumes[0], true); err != nil {
		t.Fatal(err)
	}
}

func TestResolveClaims(t *testing.T) {
	dir := t.TempDir()
	unbound := boundClaim("unbound", "")
	unbound.Status.Phase = v1.ClaimPending
	client := fake.NewClientset(boundClaim("ok", "pv"), hostPathPV("pv", "ok", dir), unbound)
	pod := claimPod(pvcVolume("b", "unbound", false), pvcVolume("a", "ok", true), pvcVolume("c", "missing", false),
		v1.Volume{Name: "e", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}})
	w := &podWorker{n: &tabNode{client: client}, pod: pod, dir: t.TempDir()}
	err := w.resolveClaims(context.Background())
	want := "Unable to attach or mount volumes: unmounted volumes=[b c], unattached volumes=[], failed to process volumes=[b c]: " +
		"error processing PVC ns/unbound: PVC is not bound; " +
		`error processing PVC ns/missing: failed to fetch PVC from API server: persistentvolumeclaims "missing" not found`
	if got := claimErrText(err); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if d, ok := w.volumeDir("a"); !ok || d != dir {
		t.Fatalf("volumeDir = %q, %v", d, ok)
	}
	if !w.claimReadOnly("a") || w.claimReadOnly("b") {
		t.Fatal("readOnly wrong")
	}
	if d, _ := w.volumeDir("e"); d != filepath.Join(w.dir, "volumes", "e") {
		t.Fatalf("emptyDir at %q", d)
	}

	// all resolved
	pod2 := claimPod(pvcVolume("a", "ok", false))
	w2 := &podWorker{n: &tabNode{client: client}, pod: pod2, dir: t.TempDir()}
	if err := w2.setupVolumes(context.Background()); err != nil {
		t.Fatal(err)
	}
}
