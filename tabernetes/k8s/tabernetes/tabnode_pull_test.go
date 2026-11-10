package main

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
)

func TestCheckImageRef(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for _, ref := range []string{
		"hello-world", "busybox", "alpine", "ubuntu", "debian", "redis", "httpd", "nginx", "bash", "centos",
		"nginx:1.27-alpine", "nginx:alpine", "nginx:stable-alpine-perl", "nginx:1.27.3-alpine3.20-slim", "nginx:mainline",
		"busybox:1.36", "busybox:1.36.1-musl", "busybox:stable-glibc", "redis:7", "redis:7.2-alpine", "httpd:2.4",
		"ubuntu:22.04", "ubuntu:jammy", "ubuntu:noble-20240827.1", "debian:bookworm-slim", "alpine:3.19", "mysql:8.0",
		"postgres:16-alpine", "node:lts-alpine", "python:3.12-slim-bookworm", "wordpress:6-php8.3-fpm-alpine",
		"rabbitmq:3-management-alpine", "traefik/whoami:v1.10", "registry.k8s.io/pause:3.9",
		"registry.k8s.io/e2e-test-images/agnhost:2.39", "curlimages/curl:8.5.0", "busybox" + digest, "nginx:nope" + digest,
		"traefik/traefik", "bitnami/nginx", "kodekloud/webapp-color", "lfccncf/arg-output", "quay.io/foo/bar:anything",
		"myregistry.example.com/team/app:v2", "gcr.io/some/thing:abc", "registry.k8s.io/other/thing", "ghcr.io/x/y",
		"mcr.microsoft.com/dotnet/aspnet:8.0", "docker.io/library/nginx:1.27", "openjdk:17.0.10_7-jdk", "", "adoptopenjdk:11",
		"mysql:doesnotexist", "traefik:mimolette", "node:iron-alpine", "radial/busyboxplus:curl", "debian:experimental",
		"oraclelinux:9-slim-fips", "archlinux:base-devel", "almalinux:9-minimal", "ubuntu:devel",
	} {
		if err := checkImageRef(ref); err != nil {
			t.Errorf("%q: %v", ref, err)
		}
	}
	for _, ref := range []string{
		"nginx:doesnotexist", "nginx:lates", "nginx:1.27-alpne", "ngnix", "ngnix:1.27", "busybx", "ubunut:22.04",
		"curlimages/crul", "traefik/whomai", "registry.k8s.io/e2e-test-images/agnhst",
	} {
		if checkImageRef(ref) == nil {
			t.Errorf("%q passes", ref)
		}
	}
	want := `rpc error: code = Unknown desc = failed to pull and unpack image "docker.io/library/ngnix:latest": failed to resolve reference "docker.io/library/ngnix:latest": pull access denied, repository does not exist or may require authorization: server message: insufficient_scope: authorization failed`
	if err := checkImageRef("ngnix"); err == nil || err.Error() != want {
		t.Errorf("message %v", err)
	}
	want = `rpc error: code = NotFound desc = failed to pull and unpack image "docker.io/library/nginx:doesnotexist": failed to resolve reference "docker.io/library/nginx:doesnotexist": docker.io/library/nginx:doesnotexist: not found`
	if err := checkImageRef("nginx:doesnotexist"); err == nil || err.Error() != want {
		t.Errorf("message %v", err)
	}
	if r, repo, tag, _ := fullRef("Registry.k8s.io/pause:3.10.1"); r != "registry.k8s.io" || repo != "pause" || tag != "3.10.1" {
		t.Errorf("fullRef %s %s %s", r, repo, tag)
	}
}

func TestDefaultPullPolicy(t *testing.T) {
	for image, want := range map[string]v1.PullPolicy{
		"nginx": v1.PullAlways, "nginx:latest": v1.PullAlways, "nginx:1.27": v1.PullIfNotPresent,
		"nginx@sha256:" + strings.Repeat("a", 64): v1.PullIfNotPresent, "localhost:5000/x": v1.PullAlways,
	} {
		if got := defaultPullPolicy(image); got != want {
			t.Errorf("%s: %s, want %s", image, got, want)
		}
	}
}

func TestPullWithBackOff(t *testing.T) {
	pullErrorWait, pullBackOffBase = 10*time.Millisecond, 20*time.Millisecond
	defer func() { pullErrorWait, pullBackOffBase = 2*time.Second, 10*time.Second }()
	rec := record.NewFakeRecorder(100)
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app", Image: "ngnix"}}})
	w.n = &tabNode{recorder: rec}
	c := w.ctrs[0]
	n := 0
	err := c.pullWithBackOff(context.Background(), func() error {
		if n++; n < 3 {
			return context.DeadlineExceeded
		}
		return nil
	}, nil)
	if err != nil || n != 3 {
		t.Fatalf("err %v after %d tries", err, n)
	}
	close(rec.Events)
	var got []string
	for e := range rec.Events {
		got = append(got, e)
	}
	all := strings.Join(got, "\n")
	for _, s := range []string{"Warning Failed Failed to pull image", "Warning Failed Error: ErrImagePull", "Normal BackOff Back-off pulling image", "Normal Pulling Pulling image"} {
		if !strings.Contains(all, s) {
			t.Errorf("no event %q in\n%s", s, all)
		}
	}
}

func TestImagesChanged(t *testing.T) {
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app", Image: "ngnix"}, {Name: "web", Image: "nginx"}}})
	app, web := w.ctrs[0], w.ctrs[1]
	app.state = v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}
	web.state = v1.ContainerState{Running: &v1.ContainerStateRunning{}}
	pod := w.pod.DeepCopy()
	pod.Spec.Containers[0].Image, pod.Spec.Containers[1].Image = "nginx:1.27", "nginx:1.28"
	w.mu.Lock()
	w.imagesChangedLocked(pod)
	w.mu.Unlock()
	if app.spec.Image != "nginx:1.27" {
		t.Errorf("waiting container has image %q, want nginx:1.27", app.spec.Image)
	}
	select {
	case <-app.newImage:
	default:
		t.Error("waiting container not woken")
	}
	if web.spec.Image != "nginx" || web.newImage != nil {
		t.Errorf("running container changed: %q", web.spec.Image)
	}
}

func TestPullTime(t *testing.T) {
	for _, ref := range []string{"nginx", "busybox", "kodekloud/webapp-color", "redis:7"} {
		if d := pullTime(ref); d < 300*time.Millisecond || d >= 900*time.Millisecond || d != pullTime(ref) {
			t.Errorf("%s: pullTime %v", ref, d)
		}
	}
}
