package main

// CPU and memory accounting of the containers on a tab node: what the cgroups
// of a real node count, and what the kubelet serves on /stats/summary and
// /metrics/resource (tabnode_stats.go).
//
// A container here is a set of goroutines, and Go cannot tell how long a
// goroutine ran. So the numbers are a model, built from what the node does
// know:
//
//   - CPU: the throttle (tabnode_proc.go) sees every command of a shell. Two
//     commands close together mean the container kept the thread; that time
//     counts. A command after a pause (sleep, a read) counts a small fixed
//     cost. Programs of the built-in images charge their work themselves:
//     Work for CPU-bound work like a request of hpa-example, ChargeCPU for
//     the cost of a request. A running container also burns a little CPU on
//     its own (the idle rate of its image).
//   - limits.cpu works like the CFS quota of a cgroup: per period of 100ms
//     a container gets limit x 100ms of CPU. Work waits for the next period
//     when the quota is used up; ChargeCPU counts no more than the quota.
//   - Memory: the working set of a container is the base size of its image
//     (the resident memory of the real program) plus what its programs
//     allocate with AddMemory.
//
// The counters belong to a run of a container, like the cgroup of a real
// container: a restart (a new container ID) starts them at zero again.

import (
	"context"
	"math"
	"path"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
)

const (
	// cfsPeriod is the period of the CPU quota, like cpu.cfs_period_us.
	cfsPeriod = 100 * time.Millisecond
	// commandCost is the CPU time a single command costs: a fork and exec on
	// a real node.
	commandCost = 50 * time.Microsecond
	// rateWindow is the window of the CPU rate (usageNanoCores), like the
	// housekeeping interval of cAdvisor.
	rateWindow = 10 * time.Second
)

// imageProfile is how a built-in image looks to the accounting: the
// resident memory of the real program and the CPU it uses while idle.
type imageProfile struct {
	memory uint64  // bytes
	idle   float64 // cores
}

// imageProfiles are by the short name of a built-in image. Numbers from
// `kubectl top` of the real images on a kind cluster, rounded.
var imageProfiles = map[string]imageProfile{
	"nginx":       {3 << 20, 0.001},
	"httpd":       {6 << 20, 0.001},
	"hpa-example": {9 << 20, 0.001},
	"redis":       {3 << 20, 0.002},
	"pause":       {256 << 10, 0},
	"whoami":      {3 << 20, 0.0005},
	"http-echo":   {2 << 20, 0.0005},
	"echo-server": {12 << 20, 0.001},
	"agnhost":     {6 << 20, 0.0005},
	"podinfo":     {10 << 20, 0.001},
	"hello-app":   {2 << 20, 0.0005},
	"kubectl":     {20 << 20, 0},
}

// defaultProfile is a container that runs its command in the shell: a
// BusyBox, Alpine or Debian shell with a few programs.
var defaultProfile = imageProfile{memory: 512 << 10}

// wasmProfile is a WebAssembly container: the runtime of the worker.
var wasmProfile = imageProfile{memory: 2 << 20}

// ctrUsage is a sample of the counters of a container, pod or node.
type ctrUsage struct {
	Time time.Time
	// StartTime is the start of the run the counters belong to.
	StartTime time.Time
	// CPU time since the start, like cpuacct.usage.
	UsageCoreNanoSeconds uint64
	// CPU rate over the last rateWindow, in billionths of a core.
	UsageNanoCores uint64
	// The working set: what the kernel cannot drop, like
	// memory.current - inactive_file. RSS and usage are the same here.
	WorkingSetBytes uint64
}

// ctrAcct is the account of one run of a container.
type ctrAcct struct {
	mu      sync.Mutex
	id      string // the container ID of the run
	start   time.Time
	limit   float64 // limits.cpu in cores, 0 without a limit
	profile imageProfile

	cpu      time.Duration // since start
	idleAt   time.Time     // idle CPU is counted up to here
	period   time.Time     // start of the current CFS period
	inPeriod time.Duration // CPU of the current period
	alloc    int64         // AddMemory

	samples []cpuSample // for UsageNanoCores, oldest first
}

type cpuSample struct {
	t   time.Time
	cpu time.Duration
}

func newCtrAcct(id string, spec v1.Container, profile imageProfile, now time.Time) *ctrAcct {
	a := &ctrAcct{id: id, start: now, idleAt: now, period: now, profile: profile}
	if q, ok := spec.Resources.Limits[v1.ResourceCPU]; ok && q.MilliValue() > 0 {
		a.limit = float64(q.MilliValue()) / 1000
	}
	return a
}

var (
	acctMu sync.Mutex
	accts  = map[*ctr]*ctrAcct{}
)

// acctOf is the account of the current run of a container. A new run (a
// new container ID) gets a new account.
func acctOf(c *ctr) *ctrAcct {
	if c == nil {
		return nil
	}
	var id string
	var img *builtinImage
	var wasm bool
	if c.w != nil {
		c.w.mu.Lock()
		id, img, wasm = c.id, c.image, c.wasm
		c.w.mu.Unlock()
	} else {
		id, img, wasm = c.id, c.image, c.wasm
	}
	acctMu.Lock()
	defer acctMu.Unlock()
	a := accts[c]
	if a == nil || a.id != id {
		a = newCtrAcct(id, c.spec, profileOf(img, wasm), time.Now())
		accts[c] = a
	}
	return a
}

// forgetAcct drops the account of a container that is gone.
func forgetAcct(c *ctr) {
	acctMu.Lock()
	delete(accts, c)
	acctMu.Unlock()
}

func profileOf(img *builtinImage, wasm bool) imageProfile {
	switch {
	case wasm:
		return wasmProfile
	case img == nil:
		return defaultProfile
	}
	if p, ok := imageProfiles[path.Base(img.name)]; ok {
		return p
	}
	return defaultProfile
}

// acctFrom is the account of the container a command runs in, nil in the
// terminal.
func acctFrom(ctx context.Context) *ctrAcct {
	if pe := procOf(ctx); pe != nil && pe.c != nil {
		return acctOf(pe.c)
	}
	return nil
}

// ChargeCPU counts d of CPU time. With limits.cpu, no more than the quota of
// the current period counts: the rest is what the CFS throttling of a real
// node would have cut off.
func (a *ctrAcct) ChargeCPU(d time.Duration) {
	if a == nil || d <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chargeLocked(time.Now(), d)
}

// Work is CPU-bound work of d: it counts d and takes at least d, longer when
// the quota of limits.cpu is used up, like a throttled process. It ends early
// when ctx ends.
func (a *ctrAcct) Work(ctx context.Context, d time.Duration) error {
	if a == nil {
		return sleepCtxErr(ctx, d)
	}
	for d > 0 {
		a.mu.Lock()
		now := time.Now()
		a.rollLocked(now)
		step := d
		if a.limit > 0 {
			quota := time.Duration(a.limit * float64(cfsPeriod))
			left := quota - a.inPeriod
			if left <= 0 {
				// throttled until the next period
				wait := a.period.Add(cfsPeriod).Sub(now)
				a.mu.Unlock()
				if err := sleepCtxErr(ctx, max(wait, time.Millisecond)); err != nil {
					return err
				}
				continue
			}
			step = min(step, left)
		}
		a.chargeLocked(now, step)
		a.mu.Unlock()
		if err := sleepCtxErr(ctx, step); err != nil {
			return err
		}
		d -= step
	}
	return nil
}

func sleepCtxErr(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// rollLocked starts a new CFS period when the current one is over.
func (a *ctrAcct) rollLocked(now time.Time) {
	if now.Sub(a.period) >= cfsPeriod {
		a.period = now.Truncate(cfsPeriod)
		if a.period.Before(a.start) {
			a.period = a.start
		}
		a.inPeriod = 0
	}
}

func (a *ctrAcct) chargeLocked(now time.Time, d time.Duration) {
	a.rollLocked(now)
	if a.limit > 0 {
		quota := time.Duration(a.limit * float64(cfsPeriod))
		d = min(d, max(quota-a.inPeriod, 0))
	}
	a.inPeriod += d
	a.cpu += d
	nodeCPU.add(d)
}

// AddMemory changes the memory the programs of the container hold by delta
// bytes (negative frees) and returns the working set after it.
func (a *ctrAcct) AddMemory(delta int64) uint64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.alloc = max(a.alloc+delta, 0)
	return a.workingSetLocked()
}

// WorkingSet is the working set of the container in bytes.
func (a *ctrAcct) WorkingSet() uint64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.workingSetLocked()
}

func (a *ctrAcct) workingSetLocked() uint64 {
	return a.profile.memory + uint64(a.alloc)
}

// Usage samples the counters at now. running says whether the container
// runs: only then does its idle CPU count.
func (a *ctrAcct) Usage(now time.Time, running bool) ctrUsage {
	a.mu.Lock()
	defer a.mu.Unlock()
	if running && a.profile.idle > 0 && now.After(a.idleAt) {
		// a little jitter, so that idle containers do not show the same
		// number for ever
		jitter := 0.7 + 0.6*math.Abs(math.Sin(float64(now.UnixNano())/1e9))
		a.chargeLocked(now, time.Duration(float64(now.Sub(a.idleAt))*a.profile.idle*jitter))
	}
	if now.After(a.idleAt) {
		a.idleAt = now
	}
	// one sample per second at most, rateWindow back at most
	if n := len(a.samples); n == 0 || now.Sub(a.samples[n-1].t) >= time.Second {
		a.samples = append(a.samples, cpuSample{now, a.cpu})
	}
	for len(a.samples) > 2 && now.Sub(a.samples[1].t) >= rateWindow {
		a.samples = a.samples[1:]
	}
	var rate uint64
	if old := a.samples[0]; now.Sub(old.t) > 0 {
		rate = uint64(float64(a.cpu-old.cpu) / now.Sub(old.t).Seconds())
	}
	return ctrUsage{
		Time:                 now,
		StartTime:            a.start,
		UsageCoreNanoSeconds: uint64(a.cpu),
		UsageNanoCores:       rate,
		WorkingSetBytes:      a.workingSetLocked(),
	}
}

// sum adds the counters of the containers of a pod; the start time is the
// earliest.
func (u ctrUsage) sum(o ctrUsage) ctrUsage {
	if u.StartTime.IsZero() || (!o.StartTime.IsZero() && o.StartTime.Before(u.StartTime)) {
		u.StartTime = o.StartTime
	}
	if o.Time.After(u.Time) {
		u.Time = o.Time
	}
	u.UsageCoreNanoSeconds += o.UsageCoreNanoSeconds
	u.UsageNanoCores += o.UsageNanoCores
	u.WorkingSetBytes += o.WorkingSetBytes
	return u
}

// nodeCPU counts all CPU the containers of this node ever used, so the node
// counter never goes back when a container goes.
var nodeCPU cpuCounter

type cpuCounter struct {
	mu  sync.Mutex
	sum time.Duration
}

func (c *cpuCounter) add(d time.Duration) {
	c.mu.Lock()
	c.sum += d
	c.mu.Unlock()
}

func (c *cpuCounter) get() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sum
}
