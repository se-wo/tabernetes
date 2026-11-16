package main

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Background jobs, such as "k port-forward deploy/web 8080:80 &".
//
// mvdan.cc/sh hands a job the context of its command line, and that context
// ends with the line, so every job would stop right away. k8s/patch.go adds
// the hook interp.BackgroundContext, and a job gets its own context here:
// Ctrl-C leaves it alone, "kill %1" ends it.

type job struct {
	id     int
	cmd    string
	cancel context.CancelFunc
}

type jobTable struct {
	mu   sync.Mutex
	list []*job // by id
}

var jobs jobTable

func init() { interp.BackgroundContext = jobs.start }

// start registers a job and returns its context and the func the shell calls
// when the job ends. A job of a container is not a job of the terminal:
// podJob.
func (t *jobTable) start(ctx context.Context, stmt *syntax.Stmt) (context.Context, func()) {
	if pe := procOf(ctx); pe != nil && pe.c != nil {
		return podJob(ctx, pe)
	}
	var cmd strings.Builder
	syntax.NewPrinter(syntax.SingleLine(true)).Print(&cmd, stmt)
	ctx, cancel := context.WithCancel(context.WithValue(context.WithoutCancel(ctx), jobKey{}, true))
	t.mu.Lock()
	j := &job{id: 1, cmd: cmd.String(), cancel: cancel}
	if n := len(t.list); n > 0 {
		j.id = t.list[n-1].id + 1
	}
	t.list = append(t.list, j)
	t.mu.Unlock()
	fmt.Fprintf(holdWriter{tty}, "[%d] %s\n", j.id, j.cmd)
	return ctx, func() { t.stop(j) }
}

type jobKey struct{}

// inJob reports whether ctx is the context of a background job.
func inJob(ctx context.Context) bool { b, _ := ctx.Value(jobKey{}).(bool); return b }

func (t *jobTable) stop(j *job) {
	j.cancel()
	t.mu.Lock()
	t.list = slices.DeleteFunc(t.list, func(o *job) bool { return o == j })
	t.mu.Unlock()
}

// stopAll ends every job, for "exit".
func (t *jobTable) stopAll() {
	t.mu.Lock()
	list := slices.Clone(t.list)
	t.mu.Unlock()
	for _, j := range list {
		t.stop(j)
	}
}

// find resolves a job spec: %1, %% or %+ (the newest).
func (t *jobTable) find(spec string) *job {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.list) == 0 {
		return nil
	}
	if spec == "%%" || spec == "%+" {
		return t.list[len(t.list)-1]
	}
	id, err := strconv.Atoi(strings.TrimPrefix(spec, "%"))
	if err != nil || !strings.HasPrefix(spec, "%") {
		return nil
	}
	for _, j := range t.list {
		if j.id == id {
			return j
		}
	}
	return nil
}

func jobsCmd(hc interp.HandlerContext) error {
	jobs.mu.Lock()
	defer jobs.mu.Unlock()
	for _, j := range jobs.list {
		fmt.Fprintf(hc.Stdout, "[%d]  Running  %s\n", j.id, j.cmd)
	}
	return nil
}

// killCmd ends jobs. Signals do not exist here, so "-9" and the like are
// accepted and ignored.
func killCmd(hc interp.HandlerContext, args []string) error {
	var specs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-s" || a == "-n":
			i++
		case strings.HasPrefix(a, "-") && len(a) > 1:
		default:
			specs = append(specs, a)
		}
	}
	if len(specs) == 0 {
		fmt.Fprintln(hc.Stderr, "usage: kill %<job> (see jobs)")
		return interp.ExitStatus(2)
	}
	code := 0
	for _, spec := range specs {
		j := jobs.find(spec)
		if j == nil {
			fmt.Fprintf(hc.Stderr, "kill: %s: no such job (see jobs)\n", spec)
			code = 1
			continue
		}
		jobs.stop(j)
	}
	return exitCode(code)
}

// podJob gives a background job of a container ("sh -c 'nc db 5432 &
// sleep 3600'") its context. On a node, every process of a container ends
// with the container: the runtime sends SIGTERM and, after the grace period,
// SIGKILL to all of them. Thus the job ends with the run of the container
// (startNative), not with the shell that started it, and not with the
// terminal: when the run is killed, its context ends, and with it the
// connections of the job (pipeConn, ncUDPCopy and the like close them).
//
// A job of the main process has the context of the run. A job of an exec, a
// hook or a probe outlives its shell, like a process that its parent left
// behind, but not the run of the container.
func podJob(ctx context.Context, pe *procEnv) (context.Context, func()) {
	p := pe.proc
	if p == nil {
		pe.c.w.mu.Lock()
		p = pe.c.proc
		pe.c.w.mu.Unlock()
	}
	if p == nil || p.ctx == nil {
		// No run (a wasm run, or a run that ended): the job ends with its
		// shell.
		return context.WithCancel(ctx)
	}
	if pe.proc == nil {
		ctx = context.WithoutCancel(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	p.jobs.add()
	return ctx, func() {
		stop()
		cancel()
		p.jobs.done()
	}
}
