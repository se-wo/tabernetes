package main

// limits.memory of the containers of a tab node. It works like the memory
// cgroup of a real node: a container whose working set (tabnode_acct.go) passes
// the limit is killed and ends with the reason OOMKilled and exit code 137. The
// restart policy decides what comes next, like for any other exit. A real
// kubelet sends no event for it, and so does this one.
//
// What counts is what the programs say they hold: the memory of stress and
// stress-ng (tabnode_stress.go), the files of emptyDir volumes with
// medium: Memory (watchTmpfs) and the linear memory of a WebAssembly module
// (startWasm). An idle container never passes its limit, and a container
// without limits.memory is never killed.

import (
	"context"
	"os"
	"path/filepath"
	"time"

	v1 "k8s.io/api/core/v1"
)

// memLimit is limits.memory of a container in bytes, 0 without a limit.
func memLimit(spec v1.Container) int64 {
	if q, ok := spec.Resources.Limits[v1.ResourceMemory]; ok && q.Value() > 0 {
		return q.Value()
	}
	return 0
}

// terminatedReason is the reason of a container that ended with code.
func terminatedReason(code int, oom bool) string {
	switch {
	case oom:
		return "OOMKilled"
	case code != 0:
		return "Error"
	}
	return "Completed"
}

// chargeMem sets the memory a source of the run holds ("wasm", "stress",
// "tmpfs") to bytes. When the working set passes limits.memory, the process
// is killed, once. It must not run with the lock of the pod worker held.
func (c *ctr) chargeMem(p *process, source string, bytes int64) {
	if c == nil || p == nil {
		return
	}
	select {
	case <-p.done:
		// the run is over, its numbers are gone with it
		return
	default:
	}
	c.memSet(source, max(bytes, 0))
	limit := memLimit(c.spec)
	// A process killed for another reason (a stop, a failed probe) keeps
	// that reason.
	if limit > 0 && c.memUsage() > limit && !p.killed.Load() && p.oom.CompareAndSwap(false, true) {
		p.kill("")
	}
}

// watchTmpfs charges the files of the emptyDir volumes with medium: Memory
// that the container mounts, every second while the run lives.
func (c *ctr) watchTmpfs(ctx context.Context, p *process) {
	dirs := c.tmpfsDirs()
	if len(dirs) == 0 {
		return
	}
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		var sum int64
		for _, d := range dirs {
			sum += dirSize(d)
		}
		c.chargeMem(p, "tmpfs", sum)
		select {
		case <-ctx.Done():
			return
		case <-p.done:
			return
		case <-t.C:
		}
	}
}

// tmpfsDirs are the directories of the memory-backed volumes of the container.
func (c *ctr) tmpfsDirs() []string {
	pod, _ := c.w.podInfo()
	mem := map[string]bool{}
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir != nil && v.EmptyDir.Medium == v1.StorageMediumMemory {
			mem[v.Name] = true
		}
	}
	var dirs []string
	seen := map[string]bool{}
	for _, m := range c.spec.VolumeMounts {
		if !mem[m.Name] || seen[m.Name] {
			continue
		}
		seen[m.Name] = true
		if d, ok := c.w.volumeDir(m.Name); ok {
			dirs = append(dirs, d)
		}
	}
	return dirs
}

// dirSize is the sum of the sizes of the files below dir.
func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return nil
		}
		if fi, err := e.Info(); err == nil {
			n += fi.Size()
		}
		return nil
	})
	return n
}
