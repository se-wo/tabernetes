package main

// Temporary until #23's memSet/memUsage land; delete then.

import "sync"

var (
	memShimMu   sync.Mutex
	memShimSrcs = map[*ctrAcct]map[string]int64{}
)

// memSet sets the bytes of a named source of memory of the current run.
func (c *ctr) memSet(source string, bytes int64) {
	a := acctOf(c)
	if a == nil {
		return
	}
	memShimMu.Lock()
	m := memShimSrcs[a]
	if m == nil {
		m = map[string]int64{}
		memShimSrcs[a] = m
	}
	delta := bytes - m[source]
	m[source] = bytes
	memShimMu.Unlock()
	a.AddMemory(delta)
}

// memUsage is the working set of the current run.
func (c *ctr) memUsage() int64 {
	return int64(acctOf(c).WorkingSet())
}
