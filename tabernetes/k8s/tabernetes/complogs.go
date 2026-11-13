package main

// Logs per control plane component. All components share one process and
// one klog; k8s/patch.go makes the stream of its text logger settable
// (logsapi.DefaultErrorStream), and boot() sets it to a logSplitter. The
// splitter finds the component of the goroutine that logs by its pprof
// labels: the root goroutine of each component gets a label set of its own
// (labelGoroutine), and the runtime hands the pointer of that set on to
// every goroutine started from there, children and grandchildren alike.
// The pointer is all the splitter looks at.

import (
	"context"
	"io"
	"runtime/pprof"
	"sync"
	"unsafe"
)

// runtimeGetProfLabel is the label set of the current goroutine (the
// runtime pushes it for runtime/pprof). The body is in no file: the empty
// complogs.s lets the compiler accept that.
//
//go:linkname runtimeGetProfLabel runtime/pprof.runtime_getProfLabel
func runtimeGetProfLabel() unsafe.Pointer

var (
	compLogsMu sync.Mutex
	compLogs   = map[string]*logBuf{}         // by component name
	labelLogs  = map[unsafe.Pointer]*logBuf{} // by label set
	labelNames = map[unsafe.Pointer]string{}  // component by label set
)

// componentLogs is the log of a component, the log of its container in
// its static pod (tabnode_static.go).
func componentLogs(name string) *logBuf {
	compLogsMu.Lock()
	defer compLogsMu.Unlock()
	l := compLogs[name]
	if l == nil {
		l = newLogBuf()
		compLogs[name] = l
	}
	return l
}

// labelGoroutine marks the current goroutine, and every goroutine it
// starts from now on, as part of the component name.
func labelGoroutine(name string) {
	logs := componentLogs(name)
	pprof.SetGoroutineLabels(pprof.WithLabels(context.Background(), pprof.Labels("component", name)))
	ptr := runtimeGetProfLabel()
	compLogsMu.Lock()
	labelLogs[ptr] = logs
	labelNames[ptr] = name
	compLogsMu.Unlock()
}

// goroutineComponent is the component the current goroutine belongs to, ""
// for none.
func goroutineComponent() string {
	ptr := runtimeGetProfLabel()
	if ptr == nil {
		return ""
	}
	compLogsMu.Lock()
	defer compLogsMu.Unlock()
	return labelNames[ptr]
}

// goroutineLogs is the log of the component the current goroutine belongs
// to, nil for none.
func goroutineLogs() *logBuf {
	ptr := runtimeGetProfLabel()
	if ptr == nil {
		return nil
	}
	compLogsMu.Lock()
	defer compLogsMu.Unlock()
	return labelLogs[ptr]
}

// logSplitter copies every write to the log of the component that writes
// it, and always to fallback (os.Stderr: the terminal builtin "logs" and
// the DevTools console show it). The text logger writes each entry with one
// Write in the goroutine that logs.
type logSplitter struct{ fallback io.Writer }

func (s logSplitter) Write(p []byte) (int, error) {
	if l := goroutineLogs(); l != nil {
		l.writer("stderr").Write(p)
	}
	return s.fallback.Write(p)
}
