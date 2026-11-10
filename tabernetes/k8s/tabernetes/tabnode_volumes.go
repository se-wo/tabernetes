package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// volumeDir is the real directory of a volume of the pod.
func (w *podWorker) volumeDir(name string) (string, bool) {
	w.mu.Lock()
	pod := w.pod
	w.mu.Unlock()
	for _, v := range pod.Spec.Volumes {
		if v.Name != name {
			continue
		}
		if v.HostPath != nil {
			// The host is the tab: hostPath is the file system of the terminal.
			return v.HostPath.Path, true
		}
		return filepath.Join(w.dir, "volumes", name), true
	}
	return "", false
}

// setupVolumes writes the contents of every volume. Volume types without
// meaning in a browser tab (persistentVolumeClaim, nfs, csi, ...) fail with
// an event, like a volume that cannot be attached.
func (w *podWorker) setupVolumes(ctx context.Context) error {
	pod, ip := w.podInfo()
	for _, v := range pod.Spec.Volumes {
		dir, _ := w.volumeDir(v.Name)
		switch {
		case v.EmptyDir != nil:
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		case v.HostPath != nil:
			t := ptr.Deref(v.HostPath.Type, v1.HostPathUnset)
			if t == v1.HostPathDirectoryOrCreate || t == v1.HostPathUnset {
				os.MkdirAll(dir, 0o755)
			}
		case isFileVolume(&v):
			if _, err := w.syncVolume(ctx, pod, ip, v); err != nil {
				return fmt.Errorf("volume %q: %w", v.Name, err)
			}
		default:
			return fmt.Errorf("volume %q: this volume type does not exist in a browser tab (supported: emptyDir, configMap, secret, projected, downwardAPI, hostPath)", v.Name)
		}
	}
	return nil
}

// isFileVolume is a volume whose files the kubelet writes and refreshes.
func isFileVolume(v *v1.Volume) bool {
	return v.ConfigMap != nil || v.Secret != nil || v.DownwardAPI != nil || v.Projected != nil
}

// syncVolume computes the payload of a volume and writes it.
func (w *podWorker) syncVolume(ctx context.Context, pod *v1.Pod, ip string, v v1.Volume) (bool, error) {
	payload, err := w.volumePayload(ctx, pod, ip, v)
	if err != nil {
		return false, err
	}
	return w.writeVolume(v, payload)
}

// writeVolume is the one place that puts a payload on disk, for the first
// setup and for every refresh.
func (w *podWorker) writeVolume(v v1.Volume, payload map[string]volFile) (bool, error) {
	pod, _ := w.podInfo()
	dir, _ := w.volumeDir(v.Name)
	return writeAtomic(dir, payload, volumeHasSubPath(pod, v.Name))
}

// volumeHasSubPath: a subPath mount keeps pointing into the directory it
// resolved at start, so a refresh must not delete that one.
func volumeHasSubPath(pod *v1.Pod, name string) bool {
	has := func(cs []v1.Container) bool {
		for _, c := range cs {
			for _, m := range c.VolumeMounts {
				if m.Name == name && m.SubPath != "" {
					return true
				}
			}
		}
		return false
	}
	if has(pod.Spec.InitContainers) || has(pod.Spec.Containers) {
		return true
	}
	for _, e := range pod.Spec.EphemeralContainers {
		if has([]v1.Container{{VolumeMounts: e.VolumeMounts}}) {
			return true
		}
	}
	return false
}

// volumeMode is a defaultMode, 0644 when unset.
func volumeMode(m *int32) os.FileMode {
	if m == nil {
		return 0o644
	}
	return os.FileMode(*m).Perm()
}

// volumePayload is the files of a configMap, secret, downwardAPI or
// projected volume, with their modes.
func (w *podWorker) volumePayload(ctx context.Context, pod *v1.Pod, ip string, v v1.Volume) (map[string]volFile, error) {
	switch {
	case v.ConfigMap != nil:
		return w.configMapFiles(ctx, v.ConfigMap.Name, v.ConfigMap.Items, ptr.Deref(v.ConfigMap.Optional, false), volumeMode(v.ConfigMap.DefaultMode))
	case v.Secret != nil:
		return w.secretFiles(ctx, v.Secret.SecretName, v.Secret.Items, ptr.Deref(v.Secret.Optional, false), volumeMode(v.Secret.DefaultMode))
	case v.DownwardAPI != nil:
		return downwardFiles(pod, ip, v.DownwardAPI.Items, volumeMode(v.DownwardAPI.DefaultMode))
	case v.Projected != nil:
		def := volumeMode(v.Projected.DefaultMode)
		files := map[string]volFile{}
		for _, src := range v.Projected.Sources {
			var part map[string]volFile
			var err error
			switch {
			case src.ServiceAccountToken != nil:
				part, err = w.tokenFiles(ctx, pod, v.Name, src.ServiceAccountToken, def)
			case src.ConfigMap != nil:
				part, err = w.configMapFiles(ctx, src.ConfigMap.Name, src.ConfigMap.Items, ptr.Deref(src.ConfigMap.Optional, false), def)
			case src.Secret != nil:
				part, err = w.secretFiles(ctx, src.Secret.Name, src.Secret.Items, ptr.Deref(src.Secret.Optional, false), def)
			case src.DownwardAPI != nil:
				part, err = downwardFiles(pod, ip, src.DownwardAPI.Items, def)
			}
			if err != nil {
				return nil, err
			}
			for k, f := range part {
				files[k] = f
			}
		}
		return files, nil
	}
	return nil, nil
}

func keyFiles(data map[string][]byte, items []v1.KeyToPath, kind, name string, def os.FileMode) (map[string]volFile, error) {
	files := map[string]volFile{}
	if len(items) == 0 {
		for k, b := range data {
			files[k] = volFile{b, def}
		}
		return files, nil
	}
	for _, it := range items {
		b, ok := data[it.Key]
		if !ok {
			return nil, fmt.Errorf("%s %q has no key %q", kind, name, it.Key)
		}
		files[it.Path] = volFile{b, itemMode(it.Mode, def)}
	}
	return files, nil
}

// itemMode is items[].mode, which overrides the defaultMode.
func itemMode(m *int32, def os.FileMode) os.FileMode {
	if m == nil {
		return def
	}
	return os.FileMode(*m).Perm()
}

func (w *podWorker) configMapFiles(ctx context.Context, name string, items []v1.KeyToPath, optional bool, def os.FileMode) (map[string]volFile, error) {
	cm, err := w.getConfigMap(ctx, name)
	if apierrors.IsNotFound(err) && optional {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("configmap %q: %w", name, err)
	}
	data := map[string][]byte{}
	for k, s := range cm.Data {
		data[k] = []byte(s)
	}
	for k, b := range cm.BinaryData {
		data[k] = b
	}
	return keyFiles(data, items, "configmap", name, def)
}

func (w *podWorker) secretFiles(ctx context.Context, name string, items []v1.KeyToPath, optional bool, def os.FileMode) (map[string]volFile, error) {
	s, err := w.getSecret(ctx, name)
	if apierrors.IsNotFound(err) && optional {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", name, err)
	}
	return keyFiles(s.Data, items, "secret", name, def)
}

func (w *podWorker) tokenFiles(ctx context.Context, pod *v1.Pod, volume string, src *v1.ServiceAccountTokenProjection, def os.FileMode) (map[string]volFile, error) {
	sa := pod.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	key := volume + "/" + src.Path
	if tok, ok := w.cachedToken(key); ok {
		return map[string]volFile{src.Path: {[]byte(tok), def}}, nil
	}
	tr := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: ptr.To(ptr.Deref(src.ExpirationSeconds, 3607)),
		BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
	}}
	if src.Audience != "" {
		tr.Spec.Audiences = []string{src.Audience}
	}
	issued := time.Now()
	res, err := w.n.client.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(ctx, sa, tr, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("service account token for %q: %w", sa, err)
	}
	w.storeToken(key, res.Status.Token, issued, res.Status.ExpirationTimestamp.Time)
	return map[string]volFile{src.Path: {[]byte(res.Status.Token), def}}, nil
}

func downwardFiles(pod *v1.Pod, ip string, items []v1.DownwardAPIVolumeFile, def os.FileMode) (map[string]volFile, error) {
	files := map[string]volFile{}
	for _, it := range items {
		var val string
		switch {
		case it.FieldRef != nil:
			v, err := fieldValue(pod, ip, it.FieldRef.FieldPath)
			if err != nil {
				return nil, err
			}
			val = v
		case it.ResourceFieldRef != nil:
			val = resourceValue(pod, it.ResourceFieldRef)
		}
		files[it.Path] = volFile{[]byte(val), itemMode(it.Mode, def)}
	}
	return files, nil
}

func formatMap(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%q\n", k, m[k])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// fieldValue resolves a downward API field path.
func fieldValue(pod *v1.Pod, ip, fp string) (string, error) {
	switch fp {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "metadata.labels":
		return formatMap(pod.Labels), nil
	case "metadata.annotations":
		return formatMap(pod.Annotations), nil
	case "spec.nodeName":
		return tabNodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.hostIP", "status.hostIPs":
		return tabNodeIP, nil
	case "status.podIP", "status.podIPs":
		return ip, nil
	}
	for _, prefix := range []string{"metadata.labels", "metadata.annotations"} {
		if strings.HasPrefix(fp, prefix+"['") && strings.HasSuffix(fp, "']") {
			key := fp[len(prefix)+2 : len(fp)-2]
			if prefix == "metadata.labels" {
				return pod.Labels[key], nil
			}
			return pod.Annotations[key], nil
		}
	}
	return "", fmt.Errorf("unsupported field path %q", fp)
}

func resourceValue(pod *v1.Pod, ref *v1.ResourceFieldSelector) string {
	for _, c := range pod.Spec.Containers {
		if ref.ContainerName != "" && c.Name != ref.ContainerName {
			continue
		}
		kind, name, _ := strings.Cut(ref.Resource, ".")
		list := c.Resources.Limits
		if kind == "requests" {
			list = c.Resources.Requests
		}
		q, ok := list[v1.ResourceName(name)]
		if !ok {
			if name == "cpu" {
				return "1"
			}
			return "0"
		}
		div := ref.Divisor
		if div.IsZero() {
			if name == "cpu" {
				return strconv.FormatInt((q.MilliValue()+999)/1000, 10)
			}
			return strconv.FormatInt(q.Value(), 10)
		}
		if name == "cpu" {
			return strconv.FormatInt((q.MilliValue()+div.MilliValue()-1)/div.MilliValue(), 10)
		}
		return strconv.FormatInt((q.Value()+div.Value()-1)/div.Value(), 10)
	}
	return "0"
}

// containerEnv builds the environment of a container, in the order of the
// kubelet: defaults, image, service links, envFrom, env.
func (w *podWorker) containerEnv(pod *v1.Pod, ip string, c *v1.Container, img *builtinImage) ([]string, error) {
	ctx := w.ctx
	hostname := specHostname(pod)
	vars := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := vars[k]; !ok {
			order = append(order, k)
		}
		vars[k] = v
	}
	set("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	set("HOSTNAME", hostname)
	set("HOME", "/root")
	if img != nil {
		for _, kv := range img.env {
			k, v, _ := strings.Cut(kv, "=")
			set(k, v)
		}
	}
	if w.n.net != nil {
		links := pod.Spec.EnableServiceLinks == nil || *pod.Spec.EnableServiceLinks
		for _, kv := range w.n.net.serviceEnv(pod.Namespace, links) {
			k, v, _ := strings.Cut(kv, "=")
			set(k, v)
		}
	}
	for _, from := range c.EnvFrom {
		var data map[string]string
		switch {
		case from.ConfigMapRef != nil:
			cm, err := w.n.client.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, from.ConfigMapRef.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) && ptr.Deref(from.ConfigMapRef.Optional, false) {
					continue
				}
				return nil, fmt.Errorf("configmap %q not found", from.ConfigMapRef.Name)
			}
			data = cm.Data
		case from.SecretRef != nil:
			s, err := w.n.client.CoreV1().Secrets(pod.Namespace).Get(ctx, from.SecretRef.Name, metav1.GetOptions{})
			if err != nil {
				if apierrors.IsNotFound(err) && ptr.Deref(from.SecretRef.Optional, false) {
					continue
				}
				return nil, fmt.Errorf("secret %q not found", from.SecretRef.Name)
			}
			data = map[string]string{}
			for k, b := range s.Data {
				data[k] = string(b)
			}
		}
		keys := make([]string, 0, len(data))
		for k := range data {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			set(from.Prefix+k, data[k])
		}
	}
	for _, e := range c.Env {
		val := expandVars(e.Value, vars)
		if src := e.ValueFrom; src != nil {
			var err error
			switch {
			case src.FieldRef != nil:
				val, err = fieldValue(pod, ip, src.FieldRef.FieldPath)
			case src.ResourceFieldRef != nil:
				val = resourceValue(pod, src.ResourceFieldRef)
			case src.ConfigMapKeyRef != nil:
				ref := src.ConfigMapKeyRef
				cm, gerr := w.n.client.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
				v, ok := "", false
				if gerr == nil {
					v, ok = cm.Data[ref.Key]
				}
				if !ok {
					if ptr.Deref(ref.Optional, false) {
						continue
					}
					err = fmt.Errorf("couldn't find key %s in ConfigMap %s/%s", ref.Key, pod.Namespace, ref.Name)
				}
				val = v
			case src.SecretKeyRef != nil:
				ref := src.SecretKeyRef
				s, gerr := w.n.client.CoreV1().Secrets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
				var b []byte
				ok := false
				if gerr == nil {
					b, ok = s.Data[ref.Key]
				}
				if !ok {
					if ptr.Deref(ref.Optional, false) {
						continue
					}
					err = fmt.Errorf("couldn't find key %s in Secret %s/%s", ref.Key, pod.Namespace, ref.Name)
				}
				val = string(b)
			}
			if err != nil {
				return nil, err
			}
		}
		set(e.Name, val)
	}
	env := make([]string, 0, len(order))
	for _, k := range order {
		env = append(env, k+"="+vars[k])
	}
	return env, nil
}
