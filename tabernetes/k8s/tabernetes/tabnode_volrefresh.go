package main

import (
	"context"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// volumeRefreshPeriod is how often the volumes are recomputed, like the
// sync frequency of the kubelet.
var volumeRefreshPeriod = 60 * time.Second

// volState is what a pod worker remembers between volume refreshes.
type volState struct {
	mu        sync.Mutex
	immutable map[string]any // "configmap/name" or "secret/name": fetched once
	tokens    map[string]cachedToken
	lastErr   map[string]string // by volume: the last message that became an event
}

type cachedToken struct {
	token           string
	issued, expires time.Time
}

// tokenNeedsRenewal is the rule of the kubelet's token manager: renew when
// 80 % of the lifetime is over or the token is older than 24 hours.
func tokenNeedsRenewal(issued, expires, now time.Time) bool {
	ttl := expires.Sub(issued)
	return now.After(issued.Add(ttl*8/10)) || now.Sub(issued) > 24*time.Hour
}

func (w *podWorker) cachedToken(key string) (string, bool) {
	w.vs.mu.Lock()
	defer w.vs.mu.Unlock()
	t, ok := w.vs.tokens[key]
	if !ok || tokenNeedsRenewal(t.issued, t.expires, time.Now()) {
		return "", false
	}
	return t.token, true
}

func (w *podWorker) storeToken(key, token string, issued, expires time.Time) {
	w.vs.mu.Lock()
	defer w.vs.mu.Unlock()
	if w.vs.tokens == nil {
		w.vs.tokens = map[string]cachedToken{}
	}
	w.vs.tokens[key] = cachedToken{token, issued, expires}
}

// getConfigMap fetches a ConfigMap. An immutable one never changes, so it is
// fetched once.
func (w *podWorker) getConfigMap(ctx context.Context, name string) (*v1.ConfigMap, error) {
	key := "configmap/" + name
	w.vs.mu.Lock()
	c, ok := w.vs.immutable[key]
	w.vs.mu.Unlock()
	if ok {
		return c.(*v1.ConfigMap), nil
	}
	cm, err := w.n.client.CoreV1().ConfigMaps(w.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if cm.Immutable != nil && *cm.Immutable {
		w.keepImmutable(key, cm)
	}
	return cm, nil
}

func (w *podWorker) getSecret(ctx context.Context, name string) (*v1.Secret, error) {
	key := "secret/" + name
	w.vs.mu.Lock()
	c, ok := w.vs.immutable[key]
	w.vs.mu.Unlock()
	if ok {
		return c.(*v1.Secret), nil
	}
	s, err := w.n.client.CoreV1().Secrets(w.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if s.Immutable != nil && *s.Immutable {
		w.keepImmutable(key, s)
	}
	return s, nil
}

func (w *podWorker) keepImmutable(key string, obj any) {
	w.vs.mu.Lock()
	defer w.vs.mu.Unlock()
	if w.vs.immutable == nil {
		w.vs.immutable = map[string]any{}
	}
	w.vs.immutable[key] = obj
}

// refreshVolumes rewrites the configMap, secret, projected and downwardAPI
// volumes until the pod stops, so a changed ConfigMap, a new label or a
// renewed token reaches the containers.
func (w *podWorker) refreshVolumes() {
	t := time.NewTicker(volumeRefreshPeriod)
	defer t.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
		}
		w.refreshVolumesOnce()
	}
}

func (w *podWorker) refreshVolumesOnce() {
	pod, ip := w.podInfo()
	for _, v := range pod.Spec.Volumes {
		if w.ctx.Err() != nil {
			return
		}
		if !isFileVolume(&v) {
			continue
		}
		_, err := w.syncVolume(w.ctx, pod, ip, v)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		w.vs.mu.Lock()
		if w.vs.lastErr == nil {
			w.vs.lastErr = map[string]string{}
		}
		changed := w.vs.lastErr[v.Name] != msg
		w.vs.lastErr[v.Name] = msg
		w.vs.mu.Unlock()
		// The old content stays; say so once per message.
		if err != nil && changed && w.ctx.Err() == nil {
			w.event(v1.EventTypeWarning, "FailedMount", "volume %q: %v", v.Name, err)
		}
	}
}
