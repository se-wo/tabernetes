package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/component-helpers/storage/ephemeral"
	"k8s.io/utils/ptr"
)

// pvcProtectionFinalizer keeps a claim that is in use from going away.
const pvcProtectionFinalizer = "kubernetes.io/pvc-protection"

// claimMount is a persistentVolumeClaim or generic ephemeral volume of a pod,
// resolved to a directory. The directory is outside the pod directory: the
// data survives the pod and is shared with other pods of the node.
type claimMount struct {
	dir      string
	readOnly bool
	pv       string
}

// mountError is a failure of the volume plugin after the claim was processed.
// The kubelet reports it as its own FailedMount event.
type mountError struct{ error }

func (e mountError) Unwrap() error { return e.error }

// claimOf returns the claim behind a persistentVolumeClaim or ephemeral volume.
func claimOf(pod *v1.Pod, v *v1.Volume) (claimName string, readOnly, ephemeralVol, ok bool) {
	switch {
	case v.PersistentVolumeClaim != nil:
		return v.PersistentVolumeClaim.ClaimName, v.PersistentVolumeClaim.ReadOnly, false, true
	case v.Ephemeral != nil:
		return ephemeral.VolumeClaimName(pod, v), false, true, true
	}
	return "", false, false, false
}

// volumeMounted tells whether a container of the pod mounts the volume.
func volumeMounted(pod *v1.Pod, name string) bool {
	has := func(ms []v1.VolumeMount) bool {
		return slices.ContainsFunc(ms, func(m v1.VolumeMount) bool { return m.Name == name })
	}
	for _, c := range pod.Spec.Containers {
		if has(c.VolumeMounts) {
			return true
		}
	}
	for _, c := range pod.Spec.InitContainers {
		if has(c.VolumeMounts) {
			return true
		}
	}
	for _, c := range pod.Spec.EphemeralContainers {
		if has(c.VolumeMounts) {
			return true
		}
	}
	return false
}

// resolveClaim follows the claim of a volume to its PersistentVolume and
// prepares the directory, like the volume manager of the kubelet.
func resolveClaim(ctx context.Context, client kubernetes.Interface, pod *v1.Pod, v *v1.Volume, mounted bool) (claimMount, error) {
	claim, readOnly, isEphemeral, ok := claimOf(pod, v)
	if !ok {
		return claimMount{}, fmt.Errorf("volume %q is not a claim", v.Name)
	}
	wrap := func(err error) error {
		return fmt.Errorf("error processing PVC %s/%s: %v", pod.Namespace, claim, err)
	}
	pvc, err := client.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, claim, metav1.GetOptions{})
	if err != nil || pvc == nil {
		return claimMount{}, wrap(fmt.Errorf("failed to fetch PVC from API server: %v", err))
	}
	switch {
	case pvc.DeletionTimestamp != nil && !slices.Contains(pvc.Finalizers, pvcProtectionFinalizer):
		return claimMount{}, wrap(errors.New("PVC is being deleted"))
	case pvc.Status.Phase != v1.ClaimBound:
		return claimMount{}, wrap(errors.New("PVC is not bound"))
	case pvc.Spec.VolumeName == "":
		return claimMount{}, wrap(errors.New("PVC has empty pvc.Spec.VolumeName"))
	}
	if isEphemeral {
		if err := ephemeral.VolumeIsForPod(pod, pvc); err != nil {
			return claimMount{}, err
		}
	}
	name := pvc.Spec.VolumeName
	pv, err := client.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
	if err != nil || pv == nil {
		return claimMount{}, wrap(fmt.Errorf("failed to fetch PV %s from API server: %v", name, err))
	}
	if pv.Spec.ClaimRef == nil {
		return claimMount{}, wrap(fmt.Errorf("found PV object %s but it has a nil pv.Spec.ClaimRef indicating it is not yet bound to the claim", name))
	}
	if pv.Spec.ClaimRef.UID != pvc.UID {
		return claimMount{}, wrap(fmt.Errorf("found PV object %s but its pv.Spec.ClaimRef.UID %s does not point to claim.UID %s", name, pv.Spec.ClaimRef.UID, pvc.UID))
	}
	mode := ptr.Deref(pv.Spec.VolumeMode, v1.PersistentVolumeFilesystem)
	if mode != v1.PersistentVolumeFilesystem {
		if mounted {
			return claimMount{}, fmt.Errorf("volume %s has volumeMode %s, but is specified in volumeMounts", v.Name, mode)
		}
		return claimMount{pv: name, readOnly: readOnly}, nil
	}
	m := claimMount{readOnly: readOnly, pv: name}
	setup := func(format string, args ...any) error {
		return mountError{fmt.Errorf("MountVolume.SetUp failed for volume %q : %s", name, fmt.Sprintf(format, args...))}
	}
	switch {
	case pv.Spec.HostPath != nil:
		m.dir = pv.Spec.HostPath.Path
		switch t := ptr.Deref(pv.Spec.HostPath.Type, v1.HostPathUnset); t {
		case v1.HostPathUnset, v1.HostPathDirectoryOrCreate:
			if err := os.MkdirAll(m.dir, 0o755); err != nil {
				return claimMount{}, setup("mkdir %s: %v", m.dir, err)
			}
		case v1.HostPathDirectory:
			if st, err := os.Stat(m.dir); err != nil || !st.IsDir() {
				return claimMount{}, setup("hostPath type check failed: %s is not a directory", m.dir)
			}
		default:
			return claimMount{}, setup("hostPath type %s is not supported in a browser tab (supported: Directory, DirectoryOrCreate)", t)
		}
	case pv.Spec.Local != nil:
		m.dir = pv.Spec.Local.Path
		st, err := os.Stat(m.dir)
		if err != nil {
			return claimMount{}, mountError{fmt.Errorf("MountVolume.NewMounter initialization failed for volume %q : path %q does not exist", name, m.dir)}
		}
		if !st.IsDir() {
			return claimMount{}, mountError{fmt.Errorf("MountVolume.NewMounter initialization failed for volume %q : path %q is not a directory", name, m.dir)}
		}
	default:
		return claimMount{}, setup("this volume type does not exist in a browser tab (supported for PersistentVolumes: hostPath, local)")
	}
	return m, nil
}

// resolveClaims resolves the claim volumes of the pod. The error is the text
// of the FailedMount event.
func (w *podWorker) resolveClaims(ctx context.Context) error {
	pod, _ := w.podInfo()
	claims := map[string]claimMount{}
	var failed []string
	var errs []string
	var mountErr error
	for i := range pod.Spec.Volumes {
		v := &pod.Spec.Volumes[i]
		if _, _, _, ok := claimOf(pod, v); !ok {
			continue
		}
		m, err := resolveClaim(ctx, w.n.client, pod, v, volumeMounted(pod, v.Name))
		var me mountError
		switch {
		case err == nil:
			claims[v.Name] = m
		case errors.As(err, &me):
			if mountErr == nil {
				mountErr = err
			}
		default:
			failed = append(failed, v.Name)
			errs = append(errs, err.Error())
		}
	}
	w.mu.Lock()
	w.claims = claims
	w.mu.Unlock()
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("Unable to attach or mount volumes: unmounted volumes=%v, unattached volumes=%v, failed to process volumes=%v: %s",
			failed, []string{}, failed, strings.Join(errs, "; "))
	}
	return mountErr
}

// claimMountFor returns the resolved claim of a volume.
func (w *podWorker) claimMountFor(name string) (claimMount, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	m, ok := w.claims[name]
	return m, ok
}

// claimReadOnly tells whether the claim of a volume is mounted read-only.
func (w *podWorker) claimReadOnly(name string) bool {
	m, _ := w.claimMountFor(name)
	return m.readOnly
}
