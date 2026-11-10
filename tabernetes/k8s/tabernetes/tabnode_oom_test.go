package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseSize(t *testing.T) {
	for _, tc := range []struct {
		in    string
		units string
		want  int64
		bad   bool
	}{
		{"250M", "bkmg", 250 << 20, false},
		{"1g", "bkmg", 1 << 30, false},
		{"1G", "bkmg", 1 << 30, false},
		{"100k", "bkmg", 100 << 10, false},
		{"512", "bkmg", 512, false},
		{"512b", "bkmg", 512, false},
		{"2t", "bkmgt", 2 << 40, false},
		{"2t", "bkmg", 0, true},
		{"", "bkmg", 0, true},
		{"M", "bkmg", 0, true},
		{"-1", "bkmg", 0, true},
		{"1.5G", "bkmg", 0, true},
	} {
		got, err := parseSize(tc.in, tc.units)
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("parseSize(%q, %q) = %d, %v; want %d, bad=%v", tc.in, tc.units, got, err, tc.want, tc.bad)
		}
	}
}

func TestParseSeconds(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"10": 10 * time.Second, "10s": 10 * time.Second, "2m": 2 * time.Minute, "1h": time.Hour,
		"1d": 24 * time.Hour, "1y": 365 * 24 * time.Hour, "0": 0,
	} {
		if got, err := parseSeconds(in); err != nil || got != want {
			t.Errorf("parseSeconds(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseSeconds("x"); err == nil {
		t.Error("parseSeconds(x) did not fail")
	}
}

func TestParseStress(t *testing.T) {
	a, err := parseStress([]string{"--vm", "2", "--vm-bytes=250M", "--vm-hang", "1", "-t", "10s", "-c", "3", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if a.vm != 2 || a.vmBytes != 250<<20 || a.vmHang != time.Second || a.timeout != 10*time.Second || a.cpu != 3 || !a.verbose {
		t.Errorf("parsed %+v", a)
	}
	a, err = parseStress([]string{"-m1", "--timeout=2m", "--vm-keep"})
	if err != nil || a.vm != 1 || a.timeout != 2*time.Minute || !a.vmKeep {
		t.Errorf("parsed %+v, %v", a, err)
	}
	if a.vmBytes != 256<<20 {
		t.Errorf("default vm-bytes = %d", a.vmBytes)
	}
	for _, bad := range [][]string{{"--bogus"}, {"--vm"}, {"--vm", "x"}, {"--verbose=1"}, {"stray"}} {
		if _, err := parseStress(bad); err == nil {
			t.Errorf("parseStress(%q) did not fail", bad)
		}
	}
}

func TestTerminatedReason(t *testing.T) {
	for _, tc := range []struct {
		code int
		oom  bool
		want string
	}{{0, false, "Completed"}, {1, false, "Error"}, {137, false, "Error"}, {137, true, "OOMKilled"}} {
		if got := terminatedReason(tc.code, tc.oom); got != tc.want {
			t.Errorf("terminatedReason(%d, %v) = %q, want %q", tc.code, tc.oom, got, tc.want)
		}
	}
}

// oomCtr is a container with limits.memory (0 for none) and a process.
func oomCtr(t *testing.T, limit string) (*ctr, *process, context.Context) {
	t.Helper()
	spec := v1.Container{Name: "app"}
	if limit != "" {
		spec.Resources.Limits = v1.ResourceList{v1.ResourceMemory: resource.MustParse(limit)}
	}
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{spec}})
	c := newCtr(w, spec, false, false)
	c.id = "tabernetes://" + t.Name()
	t.Cleanup(func() { forgetAcct(c) })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return c, newProcess(cancel), ctx
}

func TestChargeMem(t *testing.T) {
	c, p, ctx := oomCtr(t, "100Mi")
	base := acctOf(c).WorkingSet()
	c.chargeMem(p, "a", 60<<20)
	if p.oom.Load() || p.killed.Load() {
		t.Fatal("killed below the limit")
	}
	// A source is set, not added: the same bytes again change nothing.
	c.chargeMem(p, "a", 60<<20)
	if got := acctOf(c).WorkingSet(); got != base+60<<20 {
		t.Errorf("working set = %d, want %d", got, base+60<<20)
	}
	c.chargeMem(p, "a", 20<<20)
	if got := acctOf(c).WorkingSet(); got != base+20<<20 {
		t.Errorf("working set = %d, want %d", got, base+20<<20)
	}
	c.chargeMem(p, "b", 70<<20)
	if p.oom.Load() || ctx.Err() != nil {
		t.Fatal("killed below the limit with two sources")
	}
	c.chargeMem(p, "b", 90<<20)
	if !p.oom.Load() || !p.killed.Load() || ctx.Err() == nil {
		t.Errorf("not killed above the limit: oom=%v killed=%v", p.oom.Load(), p.killed.Load())
	}
	if p.killReason() != "" {
		t.Errorf("kill reason = %q, want none", p.killReason())
	}
}

func TestChargeMemNoLimit(t *testing.T) {
	c, p, ctx := oomCtr(t, "")
	c.chargeMem(p, "a", 8<<30)
	if p.oom.Load() || p.killed.Load() || ctx.Err() != nil {
		t.Error("a container without a limit was killed")
	}
	if memLimit(c.spec) != 0 {
		t.Error("memLimit without a limit")
	}
	c2, _, _ := oomCtr(t, "64Mi")
	if memLimit(c2.spec) != 64<<20 {
		t.Errorf("memLimit = %d", memLimit(c2.spec))
	}
}

// runInCtr runs a command line in the container, like the process of a pod.
func runInCtr(ctx context.Context, c *ctr, p *process, out *syncBuf, argv ...string) {
	pe := &procEnv{w: c.w, c: c, ip: "127.0.0.1", proc: p, cpu: newThrottle(c.spec.Resources)}
	go func() {
		p.exit(runArgv(ctx, pe, argv, nil, "/", strings.NewReader(""), out, out))
	}()
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestStressOOMKilled(t *testing.T) {
	c, p, ctx := oomCtr(t, "100Mi")
	var out syncBuf
	runInCtr(ctx, c, p, &out, "stress", "--vm", "1", "--vm-bytes", "250M", "--vm-hang", "1")
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("stress was not killed")
	}
	if !p.oom.Load() || p.exitCode() != 137 {
		t.Errorf("oom=%v exit=%d", p.oom.Load(), p.exitCode())
	}
	if !strings.Contains(out.String(), "dispatching hogs: 0 cpu, 0 io, 1 vm, 0 hdd") {
		t.Errorf("output %q", out.String())
	}
}

func TestStressUnderLimit(t *testing.T) {
	c, p, ctx := oomCtr(t, "100Mi")
	base := acctOf(c).WorkingSet()
	var out syncBuf
	runInCtr(ctx, c, p, &out, "stress", "--vm", "1", "--vm-bytes", "50M")
	select {
	case <-p.done:
		t.Fatalf("stress ended: %s", out.String())
	case <-time.After(time.Second):
	}
	if p.oom.Load() {
		t.Error("killed under the limit")
	}
	got := acctOf(c).WorkingSet()
	if want := base + 50<<20; got != want {
		t.Errorf("working set = %d, want %d", got, want)
	}
	p.kill("")
	<-p.done
	if got := acctOf(c).WorkingSet(); got != base {
		t.Errorf("working set after the end = %d, want %d", got, base)
	}
}

func TestStressTimeout(t *testing.T) {
	c, p, ctx := oomCtr(t, "")
	var out syncBuf
	runInCtr(ctx, c, p, &out, "stress", "-t", "1", "-c", "1", "-m", "1", "--vm-bytes", "32M")
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("stress did not end at the timeout")
	}
	if p.exitCode() != 0 || !strings.Contains(out.String(), "successful run completed in 1s") {
		t.Errorf("exit=%d output %q", p.exitCode(), out.String())
	}
}

func TestStressNg(t *testing.T) {
	c, p, ctx := oomCtr(t, "100Mi")
	base := acctOf(c).WorkingSet()
	var out syncBuf
	runInCtr(ctx, c, p, &out, "stress-ng", "--vm", "1", "--vm-bytes", "80%", "--metrics-brief", "--vm-method", "all", "-t", "60s")
	select {
	case <-p.done:
		t.Fatalf("stress-ng ended: %s", out.String())
	case <-time.After(time.Second):
	}
	if p.oom.Load() {
		t.Error("killed under the limit")
	}
	if got, want := acctOf(c).WorkingSet(), base+80<<20; got != want {
		t.Errorf("working set = %d, want %d", got, want)
	}
	for _, want := range []string{"setting to a 60 secs run per stressor", "dispatching hogs: 1 vm"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}
	p.kill("")
	<-p.done

	c, p, ctx = oomCtr(t, "100Mi")
	runInCtr(ctx, c, p, &out, "stress-ng", "--vm", "2", "--vm-bytes", "60M", "-t", "60")
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Fatal("stress-ng was not killed")
	}
	if !p.oom.Load() {
		t.Error("two workers of 60M under 100Mi were not killed")
	}
}

func TestStressUsage(t *testing.T) {
	c, p, ctx := oomCtr(t, "")
	var out syncBuf
	runInCtr(ctx, c, p, &out, "stress")
	<-p.done
	if p.exitCode() != 0 || !strings.Contains(out.String(), "Usage: stress [OPTION [ARG]]") {
		t.Errorf("exit=%d output %q", p.exitCode(), out.String())
	}
}
