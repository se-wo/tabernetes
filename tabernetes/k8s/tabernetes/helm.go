package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/url"
	"os"
	"path"
	"slices"
	"strings"
	"syscall/js"

	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	helmcmd "helm.sh/helm/v4/pkg/cmd"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/kube"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// helm runs Helm's command line (helm.sh/helm/v4/pkg/cmd) in-process, like
// kubectl. Releases are Secrets in the cluster, repositories, the cache and
// the charts of "helm create" are files in the in-memory file system.
//
// Helm keeps its settings, the default slog logger and the environment it
// reads them from in process globals, so one helm runs at a time.
var helmMu = make(chan struct{}, 1)

// chartMirrors maps chart repositories that the browser cannot fetch (no
// CORS headers) to a copy under charts/ next to the page (site/charts.mjs).
// Repositories on GitHub Pages send CORS headers and need none.
var chartMirrors = []struct{ repo, oci, mirror string }{
	{"https://charts.bitnami.com/bitnami", "registry-1.docker.io/bitnamicharts", "bitnami"},
}

func init() {
	// The field manager of server-side apply, as the helm binary sets it.
	kube.ManagedFieldsManager = "helm"
	getter.RewriteURL = func(href string) string { return rewriteChartURL(chartsBase(), href) }
	getter.FetchError = fetchError
}

// helmCmd is "helm" in the terminal.
func helmCmd(ctx context.Context, hc interp.HandlerContext, args []string) error {
	env := map[string]string{}
	hc.Env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.IsSet() {
			env[name] = vr.String()
		}
		return true
	})
	stdin := hc.Stdin
	if unwrapStdin(stdin) == termStdin {
		stdin = nil // helm does not read the terminal: no prompts
	}
	done := make(chan int, 1)
	go func() { done <- runHelm(ctx, hc.Dir, env, args, stdin, hc.Stdout, hc.Stderr) }()
	select {
	case code := <-done:
		return exitCode(code)
	case <-ctx.Done():
		// helm notices on its next write or request; don't wait for that.
		return interp.ExitStatus(130)
	}
}

// helmComplete runs "helm __complete ..." for the tab completion.
func helmComplete(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runHelm(ctx, "", map[string]string{"HOME": "/root", "KUBECONFIG": "/root/.kube/config"}, args, stdin, stdout, stderr)
}

// runHelm runs one helm invocation in dir (none: the current directory) with
// the environment env of the shell and returns its exit code.
func runHelm(ctx context.Context, dir string, env map[string]string, args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	// Wait for another helm (helm install --wait &), unless canceled: tab
	// completion gives up after a while.
	select {
	case helmMu <- struct{}{}:
		defer func() { <-helmMu }()
	case <-ctx.Done():
		return 130
	}
	out, errOut := ctxWriter{ctx, stdout}, ctxWriter{ctx, stderr}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(helmcmd.FatalError); ok {
				fmt.Fprintf(errOut, "Error: %s\n", strings.TrimSuffix(string(e), "\n"))
				code = 1
			} else {
				fmt.Fprintf(errOut, "helm panicked: %v\n", r)
				code = 2
			}
		}
		if ctx.Err() != nil {
			code = 130
		}
	}()

	// Relative paths: values files, chart directories, helm create/package.
	if dir != "" {
		if err := os.Chdir(dir); err != nil {
			fmt.Fprintf(errOut, "helm: %v\n", err)
			return 1
		}
	}
	defer setHelmEnv(env)()
	// Helm reads "-f -" and --password-stdin from os.Stdin.
	if stdin != nil && helmReadsStdin(args) {
		b, _ := io.ReadAll(stdin)
		f, err := os.CreateTemp("", "helm-stdin-")
		if err != nil {
			fmt.Fprintf(errOut, "helm: %v\n", err)
			return 1
		}
		defer os.Remove(f.Name())
		f.Write(b)
		f.Seek(0, io.SeekStart)
		old := os.Stdin
		os.Stdin = f
		defer func() { os.Stdin = old; f.Close() }()
	}
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	args = rewriteOCIChart(chartsBase(), args)

	// Helm logs with the default slog logger (warnings like "this chart is
	// deprecated"). It goes to the terminal while helm runs. slog.SetDefault
	// routes the log package through it as well, so restore both.
	oldLogger, oldLogOut, oldLogFlags := slog.Default(), log.Writer(), log.Flags()
	defer func() {
		slog.SetDefault(oldLogger)
		log.SetOutput(oldLogOut)
		log.SetFlags(oldLogFlags)
	}()
	logSetup := func(debug bool) {
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(errOut, &slog.HandlerOptions{
			Level: level,
			ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
				if a.Key == slog.TimeKey {
					return slog.Attr{}
				}
				return a
			},
		})))
	}
	oldStderr := chartutil.Stderr
	chartutil.Stderr = errOut
	defer func() { chartutil.Stderr = oldStderr }()
	helmcmd.Context = func() context.Context { return ctx }
	defer func() { helmcmd.Context = context.Background }()

	helmcmd.ResetSettings()
	if args == nil {
		args = []string{}
	}
	cmd, err := helmcmd.NewRootCmd(out, args, logSetup)
	if err != nil {
		fmt.Fprintf(errOut, "Error: %v\n", err)
		return 1
	}
	cmd.SetArgs(args)
	cmd.SetIn(stdin)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	if err := cmd.ExecuteContext(ctx); err != nil {
		if cerr, ok := errors.AsType[helmcmd.CommandError](err); ok {
			return cerr.ExitCode
		}
		return 1
	}
	return 0
}

// helmEnv lists the variables helm reads besides HELM_*.
var helmEnv = []string{"HOME", "KUBECONFIG", "NO_COLOR", "SOURCE_DATE_EPOCH"}

// setHelmEnv makes the process environment the one of the shell for the
// variables helm reads, with the directories of Linux under /root as
// defaults, and returns the func that restores it.
func setHelmEnv(env map[string]string) func() {
	vars := map[string]string{}
	for name, v := range env {
		if strings.HasPrefix(name, "HELM_") || strings.HasPrefix(name, "XDG_") || slices.Contains(helmEnv, name) {
			vars[name] = v
		}
	}
	home := vars["HOME"]
	if home == "" {
		home = "/root"
		vars["HOME"] = home
	}
	for helm, xdg := range map[string]string{
		"HELM_CACHE_HOME":  "XDG_CACHE_HOME",
		"HELM_CONFIG_HOME": "XDG_CONFIG_HOME",
		"HELM_DATA_HOME":   "XDG_DATA_HOME",
	} {
		if vars[helm] == "" && vars[xdg] == "" {
			vars[helm] = helmDefaultDir(home, helm)
		}
	}
	// Variables of the process that the shell does not have go away.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if _, ok := vars[name]; !ok && (strings.HasPrefix(name, "HELM_") || strings.HasPrefix(name, "XDG_") || slices.Contains(helmEnv, name)) {
			vars[name] = ""
		}
	}
	type saved struct {
		v  string
		ok bool
	}
	old := map[string]saved{}
	for name, v := range vars {
		ov, ok := os.LookupEnv(name)
		old[name] = saved{ov, ok}
		if v == "" {
			os.Unsetenv(name)
		} else {
			os.Setenv(name, v)
		}
	}
	return func() {
		for name, s := range old {
			if s.ok {
				os.Setenv(name, s.v)
			} else {
				os.Unsetenv(name)
			}
		}
	}
}

// helmDefaultDir is where helm on Linux keeps its files.
func helmDefaultDir(home, name string) string {
	switch name {
	case "HELM_CACHE_HOME":
		return path.Join(home, ".cache/helm")
	case "HELM_CONFIG_HOME":
		return path.Join(home, ".config/helm")
	}
	return path.Join(home, ".local/share/helm")
}

// helmReadsStdin reports whether helm reads stdin: "-f -" (values) or
// --password-stdin.
func helmReadsStdin(args []string) bool {
	for i, a := range args {
		switch {
		case a == "--password-stdin", a == "-f-", a == "--values=-":
			return true
		case a == "-" && i > 0 && (args[i-1] == "-f" || args[i-1] == "--values"):
			return true
		}
	}
	return false
}

// chartsBase is the URL of charts/ next to the page: the page says where
// (web/src/index.js), or else it is next to the document.
func chartsBase() string {
	if v := js.Global().Get("tabernetesCharts"); v.Type() == js.TypeString {
		return v.String()
	}
	if doc := js.Global().Get("document"); doc.Truthy() {
		if base := doc.Get("baseURI"); base.Type() == js.TypeString {
			if u, err := url.Parse(base.String()); err == nil {
				return u.ResolveReference(&url.URL{Path: "charts/"}).String()
			}
		}
	}
	return "" // no page (tests): no mirrors
}

// rewriteChartURL sends a download from a mirrored chart repository to its
// copy under base. Relative chart URLs of its index.yaml are resolved
// against the repository URL first, so they end up here, too.
func rewriteChartURL(base, href string) string {
	if base == "" {
		return href
	}
	for _, m := range chartMirrors {
		if rest, ok := strings.CutPrefix(href, m.repo); ok && (rest == "" || strings.HasPrefix(rest, "/") || strings.HasPrefix(rest, "?")) {
			return strings.TrimSuffix(base, "/") + "/" + m.mirror + rest
		}
	}
	return href
}

// rewriteOCIChart turns a chart of a mirrored OCI registry
// (oci://registry-1.docker.io/bitnamicharts/nginx[:version]) into the chart
// of the mirror: "nginx --repo <base>/bitnami [--version version]".
func rewriteOCIChart(base string, args []string) []string {
	if base == "" {
		return args
	}
	for i, a := range args {
		for _, m := range chartMirrors {
			name, ok := strings.CutPrefix(a, "oci://"+m.oci+"/")
			if !ok || name == "" || strings.Contains(name, "/") {
				continue
			}
			name, version, hasVersion := strings.Cut(name, ":")
			out := slices.Clone(args)
			out[i] = name
			out = append(out, "--repo", strings.TrimSuffix(base, "/")+"/"+m.mirror)
			if hasVersion {
				out = append(out, "--version", version)
			}
			return out
		}
	}
	return args
}

// fetchError explains a download that fetch() of the browser refused.
func fetchError(href string, err error) error {
	if !strings.Contains(err.Error(), "fetch() failed") {
		return err
	}
	host := href
	if u, perr := url.Parse(href); perr == nil {
		host = u.Host
	}
	return fmt.Errorf("%w (the browser downloads from %s with fetch(), which needs CORS headers from the server)", err, host)
}
