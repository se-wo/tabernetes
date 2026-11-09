package main

import (
	"context"
	"io"
	"sync"
	"time"
)

// logBuf keeps the output of one run of a container, line by line, like the
// log file of a container runtime. Old lines go when it grows past logLimit.
type logBuf struct {
	mu      sync.Mutex
	lines   []logLine
	size    int
	partial map[string][]byte
	closed  bool
	changed chan struct{} // closed and replaced on every new line
	dropped int           // lines dropped from the front
}

type logLine struct {
	t      time.Time
	stream string
	b      []byte // with the trailing newline
}

const logLimit = 2 << 20

func newLogBuf() *logBuf {
	return &logBuf{partial: map[string][]byte{}, changed: make(chan struct{})}
}

type logWriter struct {
	l      *logBuf
	stream string
}

func (l *logBuf) writer(stream string) io.Writer { return logWriter{l, stream} }

func (w logWriter) Write(b []byte) (int, error) {
	l := w.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return len(b), nil
	}
	buf := append(l.partial[w.stream], b...)
	added := false
	for {
		i := indexByte(buf, '\n')
		if i < 0 {
			break
		}
		l.addLocked(w.stream, append([]byte(nil), buf[:i+1]...))
		buf = buf[i+1:]
		added = true
	}
	// A very long line without a newline is cut, like the CRI does at 16k.
	if len(buf) > 16<<10 {
		l.addLocked(w.stream, append(append([]byte(nil), buf...), '\n'))
		buf = nil
		added = true
	}
	l.partial[w.stream] = append([]byte(nil), buf...)
	if added {
		close(l.changed)
		l.changed = make(chan struct{})
	}
	return len(b), nil
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func (l *logBuf) addLocked(stream string, b []byte) {
	l.lines = append(l.lines, logLine{t: time.Now(), stream: stream, b: b})
	l.size += len(b)
	for l.size > logLimit && len(l.lines) > 1 {
		l.size -= len(l.lines[0].b)
		l.lines = l.lines[1:]
		l.dropped++
	}
}

// close flushes partial lines; followers stop.
func (l *logBuf) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	for stream, b := range l.partial {
		if len(b) > 0 {
			l.addLocked(stream, append(b, '\n'))
		}
	}
	l.partial = nil
	l.closed = true
	close(l.changed)
}

type logOptions struct {
	follow     bool
	tail       int // -1: all
	since      time.Time
	timestamps bool
	limitBytes int64 // 0: no limit
}

// copyTo writes the log to w with the options of kubectl logs.
func (l *logBuf) copyTo(ctx context.Context, w io.Writer, o logOptions) error {
	l.mu.Lock()
	start := 0
	if o.tail >= 0 && len(l.lines) > o.tail {
		start = len(l.lines) - o.tail
	}
	pos := l.dropped + start // absolute index of the next line
	l.mu.Unlock()

	var written int64
	flusher, _ := w.(interface{ Flush() })
	for {
		l.mu.Lock()
		idx := pos - l.dropped
		if idx < 0 {
			idx = 0
		}
		lines := append([]logLine(nil), l.lines[idx:]...)
		pos = l.dropped + len(l.lines)
		changed, closed := l.changed, l.closed
		l.mu.Unlock()
		for _, ln := range lines {
			if !o.since.IsZero() && ln.t.Before(o.since) {
				continue
			}
			b := ln.b
			if o.timestamps {
				b = append([]byte(ln.t.UTC().Format(time.RFC3339Nano)+" "), b...)
			}
			if o.limitBytes > 0 && written+int64(len(b)) > o.limitBytes {
				b = b[:o.limitBytes-written]
			}
			n, err := w.Write(b)
			written += int64(n)
			if err != nil {
				return err
			}
			if o.limitBytes > 0 && written >= o.limitBytes {
				return nil
			}
		}
		if flusher != nil && len(lines) > 0 {
			flusher.Flush()
		}
		if !o.follow || closed {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
	}
}
