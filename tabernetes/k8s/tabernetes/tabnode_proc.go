package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// procEnv is what a command inside a container knows about its container.
// It travels in the context, so the coreutils map paths through the root
// file system of the container and the network commands dial from the pod.
type procEnv struct {
	w    *podWorker
	c    *ctr
	root *rootFS
	ip   string
	cpu  *throttle
}

type procKey struct{}

func withProc(ctx context.Context, pe *procEnv) context.Context {
	return context.WithValue(ctx, procKey{}, pe)
}

func procOf(ctx context.Context) *procEnv {
	pe, _ := ctx.Value(procKey{}).(*procEnv)
	return pe
}

// realPath maps a path of the command to the in-memory file system: through
// the container's root when the command runs in a pod.
func realPath(ctx context.Context, dir, p string) string {
	if !path.IsAbs(p) {
		p = path.Join(dir, p)
	}
	if pe := procOf(ctx); pe != nil && pe.root != nil {
		return pe.root.real(p)
	}
	return p
}

// runArgv runs a command line in a new shell of the container and returns
// its exit code.
func runArgv(ctx context.Context, pe *procEnv, argv, env []string, dir string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		return 0
	}
	var src strings.Builder
	for i, a := range argv {
		q, err := syntax.Quote(a, syntax.LangBash)
		if err != nil {
			fmt.Fprintf(stderr, "tabernetes: argument %d: %v\n", i, err)
			return 126
		}
		if i > 0 {
			src.WriteByte(' ')
		}
		src.WriteString(q)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(src.String()), "")
	if err != nil {
		fmt.Fprintf(stderr, "tabernetes: %v\n", err)
		return 126
	}
	r, err := newPodRunner(env, dir, stdin, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "tabernetes: %v\n", err)
		return 126
	}
	ctx = withProc(ctx, pe)
	return exitStatus(ctx, r.Run(ctx, f), stderr)
}

func exitStatus(ctx context.Context, err error, stderr io.Writer) int {
	if ctx.Err() != nil {
		return 137
	}
	if st, ok := interp.IsExitStatus(err); ok {
		return int(st)
	}
	if err != nil {
		fmt.Fprintf(stderr, "sh: %v\n", err)
		return 1
	}
	return 0
}

// newPodRunner is a shell for a container. Its file system handlers go
// through the root of the container (from the context).
func newPodRunner(env []string, dir string, stdin io.Reader, stdout, stderr io.Writer) (*interp.Runner, error) {
	r, err := interp.New(
		interp.Dir("/"),
		interp.Env(expand.ListEnviron(env...)),
		interp.StdIO(stdin, stdout, stderr),
		interp.CallHandler(podCall),
		interp.ExecHandlers(podExec),
		interp.OpenHandler(podOpen),
		interp.ReadDirHandler2(func(ctx context.Context, p string) ([]fs.DirEntry, error) {
			return os.ReadDir(realPath(ctx, interp.HandlerCtx(ctx).Dir, p))
		}),
		interp.StatHandler(func(ctx context.Context, p string, follow bool) (fs.FileInfo, error) {
			rp := realPath(ctx, interp.HandlerCtx(ctx).Dir, p)
			if follow {
				return os.Stat(rp)
			}
			return os.Lstat(rp)
		}),
		interp.AccessHandler(func(ctx context.Context, p string, mode interp.AccessMode) error {
			_, err := os.Stat(realPath(ctx, interp.HandlerCtx(ctx).Dir, p))
			return err
		}),
	)
	if err != nil {
		return nil, err
	}
	// interp.Dir checks the directory in the real file system; the
	// directory of a container only exists through its root.
	r.Dir = dir
	return r, nil
}

func podOpen(ctx context.Context, p string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	switch p {
	case "/dev/null":
		return devNull{}, nil
	case "/dev/stdout":
		return nopCloser{interp.HandlerCtx(ctx).Stdout}, nil
	case "/dev/stderr", "/dev/termination-log":
		return nopCloser{interp.HandlerCtx(ctx).Stderr}, nil
	}
	rp := realPath(ctx, interp.HandlerCtx(ctx).Dir, p)
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0 {
		if pe := procOf(ctx); pe != nil && pe.root.readOnlyAt(p, interp.HandlerCtx(ctx).Dir) {
			return nil, &fs.PathError{Op: "open", Path: p, Err: errors.New("read-only file system")}
		}
	}
	return os.OpenFile(rp, flag, perm)
}

type nopCloser struct{ io.Writer }

func (nopCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (nopCloser) Close() error             { return nil }

func (r *rootFS) readOnlyAt(p, dir string) bool {
	if !path.IsAbs(p) {
		p = path.Join(dir, p)
	}
	p = path.Clean(p)
	for _, m := range r.mounts {
		if p == m.path || strings.HasPrefix(p, m.path+"/") {
			return m.readOnly
		}
	}
	return false
}

// podCall runs before every simple command. js/wasm has no preemption: a
// shell loop without a blocking command would freeze the whole tab, the
// control plane included. The throttle lets the rest of the tab run.
func podCall(ctx context.Context, args []string) ([]string, error) {
	if pe := procOf(ctx); pe != nil && pe.cpu != nil {
		if err := pe.cpu.tick(ctx); err != nil {
			return nil, err
		}
	}
	return args, nil
}

// throttle limits the CPU of a container. The whole tab has one thread:
// every command yields to the other goroutines (the control plane), and a
// container that runs a whole slice without blocking sleeps as long as it
// ran, so the browser gets its event loop back. That is half of the thread
// at most, less with a lower CPU limit (limits.cpu: 100m runs 10% of the
// time).
type throttle struct {
	streak time.Time // start of the current run without a pause
	last   time.Time // the previous command
	share  float64
}

func newThrottle(res v1.ResourceRequirements) *throttle {
	now := time.Now()
	t := &throttle{streak: now, last: now, share: 0.5}
	if q, ok := res.Limits[v1.ResourceCPU]; ok && q.MilliValue() > 0 && q.MilliValue() < 500 {
		t.share = float64(q.MilliValue()) / 1000
	}
	return t
}

const (
	slice = 10 * time.Millisecond
	// A gap this long between two commands means the container blocked
	// (sleep, a read, a pipe) and let everything else run already.
	idleGap = 2 * time.Millisecond
)

func (t *throttle) tick(ctx context.Context) error {
	// wasm has no preemption: without this, a busy container keeps the
	// thread for the whole slice and the apiserver waits.
	runtime.Gosched()
	now := time.Now()
	if now.Sub(t.last) > idleGap {
		t.streak = now
	}
	t.last = now
	if now.Sub(t.streak) < slice {
		return nil
	}
	pause := max(time.Duration(float64(slice)*(1-t.share)/t.share), time.Millisecond)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(pause):
	}
	t.streak, t.last = time.Now(), time.Now()
	return nil
}

var shells = map[string]bool{"sh": true, "bash": true, "ash": true, "dash": true, "zsh": true, "ksh": true}

// podExec runs the commands of a container: shells, kubectl, the programs of
// the built-in images and the coreutils of the terminal. Every command is
// found by its base name, so /bin/sh and /usr/bin/env work, too.
func podExec(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	var self interp.ExecHandlerFunc
	self = func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		name := path.Base(args[0])
		switch {
		case shells[name]:
			return runShell(ctx, hc, args)
		case name == "busybox":
			if len(args) < 2 {
				fmt.Fprintln(hc.Stdout, "BusyBox v1.37.0 (tabernetes) multi-call binary.")
				return nil
			}
			return self(ctx, args[1:])
		case name == "kubectl" || name == "k":
			return podKubectl(ctx, hc, args[1:])
		case imagePrograms[name] != nil:
			return imagePrograms[name](ctx, hc, args[1:], self)
		case podTools[name] != nil:
			return podTools[name](ctx, hc, args[1:], self)
		case coreutils[name] != nil:
			return coreutils[name](ctx, hc, args[1:], self)
		case interp.IsBuiltin(name):
			err := hc.Builtin(ctx, append([]string{name}, args[1:]...))
			if err != nil {
				var code int
				if _, scanErr := fmt.Sscanf(err.Error(), "builtin exit status %d", &code); scanErr == nil {
					return exitCode(code)
				}
			}
			return err
		}
		// A script in the file system of the container.
		if strings.Contains(args[0], "/") {
			if b, err := os.ReadFile(realPath(ctx, hc.Dir, args[0])); err == nil {
				return runScript(ctx, hc, args[0], b, args[1:])
			}
		}
		fmt.Fprintf(hc.Stderr, "sh: %s: not found\n", args[0])
		return interp.ExitStatus(127)
	}
	return self
}

// runShell implements sh [-c script [name [args...]]] [file [args...]], and
// sh without arguments, which reads its commands from stdin.
func runShell(ctx context.Context, hc interp.HandlerContext, args []string) error {
	rest := args[1:]
	var script *string
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") && rest[0] != "-" {
		flag := rest[0]
		rest = rest[1:]
		if flag == "--" {
			break
		}
		if strings.Contains(flag, "c") {
			if len(rest) == 0 {
				fmt.Fprintf(hc.Stderr, "%s: -c requires an argument\n", args[0])
				return interp.ExitStatus(2)
			}
			s := rest[0]
			script = &s
			rest = rest[1:]
		}
		// -e, -x, -u, -l, -i and friends are accepted and ignored.
	}
	switch {
	case script != nil:
		name := args[0]
		if len(rest) > 0 {
			name, rest = rest[0], rest[1:]
		}
		return runScript(ctx, hc, name, []byte(*script), rest)
	case len(rest) > 0 && rest[0] != "-":
		b, err := os.ReadFile(realPath(ctx, hc.Dir, rest[0]))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: can't open '%s': %v\n", args[0], rest[0], unwrap(err))
			return interp.ExitStatus(2)
		}
		return runScript(ctx, hc, rest[0], b, rest[1:])
	}
	// Commands from stdin, one after the other as they arrive: kubectl
	// attach and kubectl exec -i feed a shell this way.
	r, err := subRunner(hc)
	if err != nil {
		return err
	}
	var runErr error
	parseErr := syntax.NewParser().Interactive(stdinOf(hc), func(stmts []*syntax.Stmt) bool {
		for _, st := range stmts {
			runErr = r.Run(ctx, st)
			if r.Exited() || ctx.Err() != nil {
				return false
			}
		}
		return true
	})
	if parseErr != nil && runErr == nil {
		fmt.Fprintf(hc.Stderr, "%s: %v\n", args[0], parseErr)
		return interp.ExitStatus(2)
	}
	return statusOf(runErr)
}

func stdinOf(hc interp.HandlerContext) io.Reader {
	if hc.Stdin == nil {
		return strings.NewReader("")
	}
	return hc.Stdin
}

func runScript(ctx context.Context, hc interp.HandlerContext, name string, src []byte, params []string) error {
	src = bytes.TrimPrefix(src, []byte("\xef\xbb\xbf"))
	f, err := syntax.NewParser().Parse(bytes.NewReader(src), name)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "sh: %v\n", err)
		return interp.ExitStatus(2)
	}
	r, err := subRunner(hc)
	if err != nil {
		return err
	}
	r.Params = params
	return statusOf(r.Run(ctx, f))
}

// subRunner is a new shell with the exported variables of hc.
func subRunner(hc interp.HandlerContext) (*interp.Runner, error) {
	var env []string
	for name, vr := range hc.Env.Each {
		if vr.Exported && vr.IsSet() {
			env = append(env, name+"="+vr.String())
		}
	}
	return newPodRunner(env, hc.Dir, stdinOf(hc), hc.Stdout, hc.Stderr)
}

func statusOf(err error) error {
	if err == nil {
		return nil
	}
	if st, ok := interp.IsExitStatus(err); ok {
		return exitCode(int(st))
	}
	return err
}

// podKubectl runs kubectl with the service account of the pod, the way
// in-cluster config works: the token and CA of the projected volume, the
// apiserver through the kubernetes service.
func podKubectl(ctx context.Context, hc interp.HandlerContext, args []string) error {
	pe := procOf(ctx)
	const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	token := pe.root.real(saDir + "/token")
	if _, err := os.Stat(token); err != nil {
		fmt.Fprintln(hc.Stderr, "error: no service account token in "+saDir+" (automountServiceAccountToken: false?)")
		return interp.ExitStatus(1)
	}
	ns := pe.w.namespace
	if b, err := os.ReadFile(pe.root.real(saDir + "/namespace")); err == nil {
		ns = strings.TrimSpace(string(b))
	}
	kc := filepath.Join(pe.w.dir, "containers", pe.c.spec.Name, "kubeconfig")
	cfg := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: in-cluster
  cluster:
    server: https://10.96.0.1:443
    certificate-authority: %s
contexts:
- name: in-cluster
  context: {cluster: in-cluster, user: sa, namespace: %s}
current-context: in-cluster
users:
- name: sa
  user:
    tokenFile: %s
`, pe.root.real(saDir+"/ca.crt"), ns, token)
	if err := os.WriteFile(kc, []byte(cfg), 0o600); err != nil {
		fmt.Fprintf(hc.Stderr, "kubectl: %v\n", err)
		return interp.ExitStatus(1)
	}
	// kubectl resolves -f against the process directory, which is shared
	// with the terminal: hand it absolute paths of the tab instead.
	for i, a := range args {
		if (a == "-f" || a == "--filename") && i+1 < len(args) && args[i+1] != "-" && !strings.Contains(args[i+1], "://") {
			args[i+1] = realPath(ctx, hc.Dir, args[i+1])
		}
	}
	code := runKubectl(ctx, append([]string{"--kubeconfig=" + kc}, args...), hc.Stdin, hc.Stdout, hc.Stderr)
	return exitCode(code)
}

// podTools are commands that only make sense inside a container.
var podTools = map[string]func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error{
	"hostname": func(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
		pe := procOf(ctx)
		if len(args) > 0 && args[0] == "-i" {
			fmt.Fprintln(hc.Stdout, pe.ip)
			return nil
		}
		b, _ := os.ReadFile(pe.root.real("/etc/hostname"))
		fmt.Fprint(hc.Stdout, string(b))
		return nil
	},
	"uname": func(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
		if len(args) > 0 && (args[0] == "-a" || args[0] == "--all") {
			b, _ := os.ReadFile(procOf(ctx).root.real("/etc/hostname"))
			fmt.Fprintf(hc.Stdout, "Linux %s 6.0.0-tabernetes #1 SMP WebAssembly wasm32 GNU/Linux\n", strings.TrimSpace(string(b)))
			return nil
		}
		fmt.Fprintln(hc.Stdout, "Linux")
		return nil
	},
	"id": func(_ context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
		fmt.Fprintln(hc.Stdout, "uid=0(root) gid=0(root) groups=0(root)")
		return nil
	},
	"whoami": func(_ context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
		fmt.Fprintln(hc.Stdout, "root")
		return nil
	},
	"ps": func(ctx context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
		pe := procOf(ctx)
		pe.w.mu.Lock()
		argv := strings.Join(pe.c.argv, " ")
		pe.w.mu.Unlock()
		fmt.Fprintln(hc.Stdout, "PID   USER     COMMAND")
		fmt.Fprintf(hc.Stdout, "    1 root     %s\n", argv)
		return nil
	},
}

func init() {
	podTools["printenv"] = func(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
		if len(args) == 0 {
			for name, vr := range hc.Env.Each {
				if vr.Exported && vr.IsSet() {
					fmt.Fprintf(hc.Stdout, "%s=%s\n", name, vr.String())
				}
			}
			return nil
		}
		code := 0
		for _, a := range args {
			if vr := hc.Env.Get(a); vr.IsSet() {
				fmt.Fprintln(hc.Stdout, vr.String())
			} else {
				code = 1
			}
		}
		return exitCode(code)
	}
}

// expandAll applies the $(VAR) expansion of Kubernetes to command and args.
func expandAll(list []string, vars map[string]string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = expandVars(s, vars)
	}
	return out
}

// expandVars replaces $(VAR) with its value; $$ is a literal $ and unknown
// variables stay as they are.
func expandVars(s string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '(':
			end := strings.IndexByte(s[i+2:], ')')
			if end < 0 {
				b.WriteString(s[i:])
				return b.String()
			}
			name := s[i+2 : i+2+end]
			if v, ok := vars[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString(s[i : i+3+end])
			}
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String()
}
