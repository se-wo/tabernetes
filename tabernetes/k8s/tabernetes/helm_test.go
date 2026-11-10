package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteChartURL(t *testing.T) {
	const base = "https://example.org/tab/charts/"
	for _, tc := range []struct{ in, want string }{
		{"https://charts.bitnami.com/bitnami", base + "bitnami"},
		{"https://charts.bitnami.com/bitnami/", base + "bitnami/"},
		{"https://charts.bitnami.com/bitnami/index.yaml", base + "bitnami/index.yaml"},
		{"https://charts.bitnami.com/bitnami/nginx-22.1.0.tgz", base + "bitnami/nginx-22.1.0.tgz"},
		{"https://charts.bitnami.com/bitnamifoo/index.yaml", "https://charts.bitnami.com/bitnamifoo/index.yaml"},
		{"https://stefanprodan.github.io/podinfo/index.yaml", "https://stefanprodan.github.io/podinfo/index.yaml"},
	} {
		if got := rewriteChartURL(base, tc.in); got != tc.want {
			t.Errorf("rewriteChartURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := rewriteChartURL("", "https://charts.bitnami.com/bitnami/index.yaml"); got != "https://charts.bitnami.com/bitnami/index.yaml" {
		t.Errorf("without a page: %q", got)
	}
}

func TestRewriteOCIChart(t *testing.T) {
	const base = "https://example.org/charts/"
	for _, tc := range []struct{ in, want string }{
		{"install y oci://registry-1.docker.io/bitnamicharts/nginx", "install y nginx --repo https://example.org/charts/bitnami"},
		{"show values oci://registry-1.docker.io/bitnamicharts/redis:23.2.0", "show values redis --repo https://example.org/charts/bitnami --version 23.2.0"},
		{"install y oci://ghcr.io/x/nginx", "install y oci://ghcr.io/x/nginx"},
	} {
		if got := strings.Join(rewriteOCIChart(base, strings.Fields(tc.in)), " "); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHelmEnv(t *testing.T) {
	os.Setenv("HELM_NAMESPACE", "stale")
	defer os.Unsetenv("HELM_NAMESPACE")
	restore := setHelmEnv(map[string]string{"HOME": "/home/x", "KUBECONFIG": "/k", "HELM_DRIVER": "configmap", "XDG_DATA_HOME": "/xdg", "PATH": "/bin"})
	for name, want := range map[string]string{
		"HOME": "/home/x", "KUBECONFIG": "/k", "HELM_DRIVER": "configmap",
		"HELM_CACHE_HOME": "/home/x/.cache/helm", "HELM_CONFIG_HOME": "/home/x/.config/helm",
		"HELM_DATA_HOME": "", // XDG_DATA_HOME wins
		"HELM_NAMESPACE": "", // not in the shell
	} {
		if got := os.Getenv(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	restore()
	if got := os.Getenv("HELM_NAMESPACE"); got != "stale" {
		t.Errorf("HELM_NAMESPACE not restored: %q", got)
	}
	if _, ok := os.LookupEnv("HELM_CACHE_HOME"); ok {
		t.Error("HELM_CACHE_HOME not removed")
	}
}

// helm runs one helm in dir with HOME in dir.
func helm(t *testing.T, dir string, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := runHelm(context.Background(), dir, map[string]string{"HOME": dir}, args, nil, &out, &out)
	return out.String(), code
}

func TestHelmChart(t *testing.T) {
	dir := t.TempDir()
	if out, code := helm(t, dir, "create", "mychart"); code != 0 || !strings.Contains(out, "Creating mychart") {
		t.Fatalf("create: %q (exit %d)", out, code)
	}
	if _, err := os.Stat(filepath.Join(dir, "mychart", "Chart.yaml")); err != nil {
		t.Fatal(err)
	}
	out, code := helm(t, dir, "template", "mychart", "--set", "replicaCount=3")
	if code != 0 || !strings.Contains(out, "replicas: 3") || !strings.Contains(out, "kind: Deployment") {
		t.Errorf("template: %q (exit %d)", out, code)
	}
	if out, code := helm(t, dir, "lint", "mychart"); code != 0 || !strings.Contains(out, "1 chart(s) linted, 0 chart(s) failed") {
		t.Errorf("lint: %q (exit %d)", out, code)
	}
	if out, code := helm(t, dir, "package", "mychart"); code != 0 || !strings.Contains(out, "mychart-0.1.0.tgz") {
		t.Errorf("package: %q (exit %d)", out, code)
	}
	// The settings follow the environment of each invocation.
	if out, code := helm(t, dir, "env"); code != 0 || !strings.Contains(out, `HELM_CACHE_HOME="`+dir+`/.cache/helm"`) {
		t.Errorf("env: %q (exit %d)", out, code)
	}
}

func TestHelmErrors(t *testing.T) {
	dir := t.TempDir()
	out, code := helm(t, dir, "template", "x", "./missing")
	if code != 1 || !strings.HasPrefix(out, "Error: ") {
		t.Errorf("missing chart: %q (exit %d)", out, code)
	}
	if out, code := helm(t, dir, "nosuchcommand"); code == 0 {
		t.Errorf("unknown command: %q (exit %d)", out, code)
	}
	if out, code := helm(t, dir, "version", "--short"); code != 0 || !strings.HasPrefix(out, "v") {
		t.Errorf("version: %q (exit %d)", out, code)
	}
}

func TestHelmReadsStdin(t *testing.T) {
	for args, want := range map[string]bool{
		"install x c -f -":                  true,
		"install x c --values=-":            true,
		"registry login r --password-stdin": true,
		"install x c -f values.yaml":        false,
	} {
		if got := helmReadsStdin(strings.Fields(args)); got != want {
			t.Errorf("%q: %v", args, got)
		}
	}
}
