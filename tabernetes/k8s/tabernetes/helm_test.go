package main

import (
	"bytes"
	"context"
	"io/fs"
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

// bitnamiCharts is the directory of the mirrored charts (tabernetes/charts/bitnami).
// The tests run in the Kubernetes checkout (.build/kubernetes/cmd/tabernetes)
// or in k8s/tabernetes, so it looks upwards. helm changes the directory, so
// this is found once, when the tests start.
var bitnamiCharts = func() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for d := dir; ; d = filepath.Dir(d) {
		c := filepath.Join(d, "charts", "bitnami")
		if _, err := os.Stat(filepath.Join(c, "nginx", "Chart.yaml")); err == nil {
			return c
		}
		if d == filepath.Dir(d) {
			return ""
		}
	}
}()

func chartsDir(t *testing.T) string {
	t.Helper()
	if bitnamiCharts == "" {
		t.Fatal("charts/bitnami not found above the test directory")
	}
	return bitnamiCharts
}

// notAnywhere are words that must not show up in what the mirrored charts
// print: the charts must read like the real ones on a real cluster.
var notAnywhere = []string{"tabernetes", "browser", "terminal", "proxy svc/"}

func checkWords(t *testing.T, what, out string, words ...string) {
	t.Helper()
	low := strings.ToLower(out)
	for _, w := range append(notAnywhere, words...) {
		if strings.Contains(low, w) {
			t.Errorf("%s mentions %q:\n%s", what, w, out)
		}
	}
}

// TestHelmBitnamiNotes renders the NOTES.txt of the mirrored charts: the text
// of the real Bitnami charts, with the branch of the service type.
func TestHelmBitnamiNotes(t *testing.T) {
	charts := chartsDir(t)
	type tc struct {
		name, chart string
		set         []string
		want, not   []string
	}
	// What the other service types print.
	other := map[string][]string{
		"LoadBalancer": {"port-forward", "nodePort"},
		"ClusterIP":    {"nodePort", "-w "},
		"NodePort":     {"port-forward", "-w "},
	}
	const patient = "** Please be patient while the chart is being deployed **"
	nginx := func(name, typ string, want ...string) tc {
		return tc{name: name, chart: "nginx", set: []string{"service.type=" + typ}, not: other[typ], want: append([]string{
			"CHART NAME: nginx\nCHART VERSION: 22.1.0\n", "APP VERSION: ", "Bitnami Secure Images", patient,
			"NGINX can be accessed through the following DNS name from within your cluster:",
			"rel-nginx.default.svc.cluster.local (port 80)",
			"To access NGINX from outside the cluster, follow the steps below:",
			"1. Get the NGINX URL by running these commands:",
		}, want...)}
	}
	apache := func(name, typ string, want ...string) tc {
		return tc{name: name, chart: "apache", set: []string{"service.type=" + typ}, not: other[typ], want: append([]string{
			"CHART NAME: apache\nCHART VERSION: 11.4.0\n", "APP VERSION: ", "NOTICE: Starting August 28th, 2025", patient,
			"1. Get the Apache URL by running:",
		}, want...)}
	}
	redis := func(name string, set []string, want ...string) tc {
		typ, not := "ClusterIP", []string(nil)
		for _, v := range set {
			if t, ok := strings.CutPrefix(v, "master.service.type="); ok {
				typ = t
			}
			if v == "auth.enabled=false" {
				not = []string{"REDIS_PASSWORD", "To get your password"}
			}
		}
		if name == "NoAuth" {
			not = append(not, "most likely exposed")
		}
		return tc{name: name, chart: "redis", set: set, not: append(not, other[typ]...), want: append([]string{
			"CHART NAME: redis\nCHART VERSION: 23.2.0\n", "APP VERSION: ", "Bitnami Secure Images", patient,
			"Redis&reg; can be accessed via port 6379 on the following DNS name from within your cluster:",
			"rel-redis-master.default.svc.cluster.local",
			"To connect to your Redis&reg; server:",
			"1. Run a Redis&reg; pod that you can use as a client:",
			"kubectl exec --tty -i redis-client \\\n   --namespace default -- bash",
			"2. Connect using the Redis&reg; CLI:",
			"To connect to your database from outside the cluster execute the following commands:",
		}, want...)}
	}
	cases := []tc{
		nginx("LoadBalancer", "LoadBalancer", "NOTE: It may take a few minutes for the LoadBalancer IP to be available.",
			"-w rel-nginx'", "{.status.loadBalancer.ingress[0].ip}"),
		nginx("ClusterIP", "ClusterIP", "kubectl port-forward --namespace default svc/rel-nginx ${SERVICE_PORT}:${SERVICE_PORT} &"),
		nginx("NodePort", "NodePort", "{.spec.ports[0].nodePort}", "echo \"http://${NODE_IP}:${NODE_PORT}\""),
		apache("LoadBalancer", "LoadBalancer", "-w rel-apache **", "echo URL            : http://$SERVICE_IP/"),
		apache("ClusterIP", "ClusterIP", "kubectl port-forward --namespace default svc/rel-apache 8080:80", "http://127.0.0.1:8080/"),
		apache("NodePort", "NodePort", "{.spec.ports[0].nodePort}", "echo http://$NODE_IP:$NODE_PORT/"),
		redis("ClusterIP", nil, "To get your password run:", `-o jsonpath="{.data.redis-password}" | base64 -d)`,
			"--env REDIS_PASSWORD=$REDIS_PASSWORD", `REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli -h rel-redis-master`,
			"port-forward --namespace default svc/rel-redis-master 6379:6379 &", "redis-cli -h 127.0.0.1 -p 6379"),
		redis("NodePort", []string{"master.service.type=NodePort"}, "{.spec.ports[0].nodePort}", "redis-cli -h $NODE_IP -p $NODE_PORT"),
		redis("LoadBalancer", []string{"master.service.type=LoadBalancer"}, "NOTE: It may take a few minutes for the LoadBalancer IP to be available.",
			"-w rel-redis'", "redis-cli -h $SERVICE_IP -p 6379"),
		redis("NoAuth", []string{"auth.enabled=false"}, "redis-cli -h rel-redis-master\n"),
		redis("NoAuthLoadBalancer", []string{"auth.enabled=false", "master.service.type=LoadBalancer"},
			`By specifying "master.service.type=LoadBalancer" and "auth.enabled=false" you have`, "without any authentication"),
		redis("ExistingSecret", []string{"auth.existingSecret=mysecret", "auth.existingSecretPasswordKey=pw"},
			`secret --namespace default mysecret -o jsonpath="{.data.pw}"`),
	}
	for _, c := range cases {
		t.Run(c.chart+"/"+c.name, func(t *testing.T) {
			args := []string{"install", "rel", filepath.Join(charts, c.chart), "--dry-run=client"}
			for _, s := range c.set {
				args = append(args, "--set", s)
			}
			out, code := helm(t, t.TempDir(), args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			// helm prints the notes at the end of the dry run.
			_, out, ok := strings.Cut(out, "\nNOTES:\n")
			if !ok {
				t.Fatalf("no NOTES in the output")
			}
			for _, w := range c.want {
				if !strings.Contains(out, w) {
					t.Errorf("missing %q in:\n%s", w, out)
				}
			}
			for _, w := range c.not {
				if strings.Contains(out, w) {
					t.Errorf("must not print %q:\n%s", w, out)
				}
			}
			// The jsonpath template of the LoadBalancer branches is part of the text.
			const jsonpath = "{{ range (index .status.loadBalancer.ingress 0) }}{{ . }}{{ end }}"
			if strings.Contains(strings.ReplaceAll(out, jsonpath, ""), "{{") || strings.Contains(out, "<no value>") {
				t.Errorf("not rendered:\n%s", out)
			}
			checkWords(t, "NOTES", out, "wget", "no load balancer")
		})
	}
}

// TestHelmBitnamiShow checks that helm show prints nothing of this project.
func TestHelmBitnamiShow(t *testing.T) {
	charts := chartsDir(t)
	for _, chart := range []string{"nginx", "apache", "redis"} {
		for _, what := range []string{"chart", "readme", "values", "all"} {
			t.Run(chart+"/"+what, func(t *testing.T) {
				out, code := helm(t, t.TempDir(), "show", what, filepath.Join(charts, chart))
				if code != 0 || strings.TrimSpace(out) == "" {
					t.Fatalf("exit %d: %q", code, out)
				}
				checkWords(t, "helm show "+what, out, "wget", "no load balancer")
				if what == "readme" || what == "all" {
					if !strings.Contains(out, "helm install my-release oci://registry-1.docker.io/bitnamicharts/"+chart) {
						t.Errorf("README has no install command:\n%s", out)
					}
				}
				if what == "chart" && !strings.Contains(out, "home: https://bitnami.com") {
					t.Errorf("Chart.yaml: %s", out)
				}
				// helm prints the parsed metadata, not the comment of Chart.yaml.
				if what == "chart" && (strings.Contains(out, "not maintained by Broadcom") || strings.Contains(out, "Apache-2.0")) {
					t.Errorf("helm show chart prints the license comment: %s", out)
				}
			})
		}
	}
}

// TestHelmBitnamiNoTabernetes fails if any file of the mirrored charts names
// this project.
func TestHelmBitnamiNoTabernetes(t *testing.T) {
	root := filepath.Dir(chartsDir(t)) // charts/
	n := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		if strings.Contains(strings.ToLower(string(b)), "tabernetes") {
			t.Errorf("%s mentions Tabernetes", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 10 {
		t.Errorf("only %d files found below %s", n, root)
	}
}
