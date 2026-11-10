package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

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
		if m, ok := w.claimMountFor(name); ok {
			return m.dir, true
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
// meaning in a browser tab (nfs, csi, ...) fail with
// an event, like a volume that cannot be attached.
func (w *podWorker) setupVolumes(ctx context.Context) error {
	if err := w.resolveClaims(ctx); err != nil {
		return err
	}
	pod, ip := w.podInfo()
	for _, v := range pod.Spec.Volumes {
		dir, _ := w.volumeDir(v.Name)
		var files map[string][]byte
		var err error
		switch {
		case v.EmptyDir != nil:
		case v.HostPath != nil:
			t := ptr.Deref(v.HostPath.Type, v1.HostPathUnset)
			if t == v1.HostPathDirectoryOrCreate || t == v1.HostPathUnset {
				os.MkdirAll(dir, 0o755)
			}
			continue
		case v.ConfigMap != nil:
			files, err = w.configMapFiles(ctx, v.ConfigMap.Name, v.ConfigMap.Items, ptr.Deref(v.ConfigMap.Optional, false))
		case v.Secret != nil:
			files, err = w.secretFiles(ctx, v.Secret.SecretName, v.Secret.Items, ptr.Deref(v.Secret.Optional, false))
		case v.DownwardAPI != nil:
			files, err = downwardFiles(pod, ip, v.DownwardAPI.Items)
		case v.Projected != nil:
			files = map[string][]byte{}
			for _, src := range v.Projected.Sources {
				var part map[string][]byte
				switch {
				case src.ServiceAccountToken != nil:
					part, err = w.tokenFiles(ctx, pod, src.ServiceAccountToken)
				case src.ConfigMap != nil:
					part, err = w.configMapFiles(ctx, src.ConfigMap.Name, src.ConfigMap.Items, ptr.Deref(src.ConfigMap.Optional, false))
				case src.Secret != nil:
					part, err = w.secretFiles(ctx, src.Secret.Name, src.Secret.Items, ptr.Deref(src.Secret.Optional, false))
				case src.DownwardAPI != nil:
					part, err = downwardFiles(pod, ip, src.DownwardAPI.Items)
				}
				if err != nil {
					break
				}
				for k, b := range part {
					files[k] = b
				}
			}
		case v.PersistentVolumeClaim != nil || v.Ephemeral != nil:
			continue
		default:
			return fmt.Errorf("volume %q: this volume type does not exist in a browser tab (supported: emptyDir, configMap, secret, projected, downwardAPI, hostPath, persistentVolumeClaim, ephemeral)", v.Name)
		}
		if err != nil {
			return fmt.Errorf("volume %q: %w", v.Name, err)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for name, b := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func keyFiles(data map[string][]byte, items []v1.KeyToPath, kind, name string) (map[string][]byte, error) {
	files := map[string][]byte{}
	if len(items) == 0 {
		for k, b := range data {
			files[k] = b
		}
		return files, nil
	}
	for _, it := range items {
		b, ok := data[it.Key]
		if !ok {
			return nil, fmt.Errorf("%s %q has no key %q", kind, name, it.Key)
		}
		files[it.Path] = b
	}
	return files, nil
}

func (w *podWorker) configMapFiles(ctx context.Context, name string, items []v1.KeyToPath, optional bool) (map[string][]byte, error) {
	cm, err := w.n.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, name, metav1.GetOptions{})
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
	return keyFiles(data, items, "configmap", name)
}

func (w *podWorker) secretFiles(ctx context.Context, name string, items []v1.KeyToPath, optional bool) (map[string][]byte, error) {
	s, err := w.n.client.CoreV1().Secrets(w.namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) && optional {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", name, err)
	}
	return keyFiles(s.Data, items, "secret", name)
}

func (w *podWorker) tokenFiles(ctx context.Context, pod *v1.Pod, src *v1.ServiceAccountTokenProjection) (map[string][]byte, error) {
	sa := pod.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	tr := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{
		ExpirationSeconds: ptr.To(ptr.Deref(src.ExpirationSeconds, 3607)),
		BoundObjectRef:    &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
	}}
	if src.Audience != "" {
		tr.Spec.Audiences = []string{src.Audience}
	}
	res, err := w.n.client.CoreV1().ServiceAccounts(pod.Namespace).CreateToken(ctx, sa, tr, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("service account token for %q: %w", sa, err)
	}
	return map[string][]byte{src.Path: []byte(res.Status.Token)}, nil
}

func downwardFiles(pod *v1.Pod, ip string, items []v1.DownwardAPIVolumeFile) (map[string][]byte, error) {
	files := map[string][]byte{}
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
		files[it.Path] = []byte(val)
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
