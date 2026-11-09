package main

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/moby/term"
)

// tty is the terminal for full screen programs (vi, kubectl edit) and for
// kubectl exec -it, run -it and attach -it. Normally the browser does the
// line editing and the shell gets complete lines. In raw mode every key goes
// straight to the running program instead.
var tty = &terminal{keys: make(chan string, 256)}

type terminal struct {
	mu      sync.Mutex
	out     io.Writer // the terminal itself, also when stdout is redirected
	held    []byte    // output of everything else while a full screen program runs
	keys    chan string
	raw     atomic.Bool
	alt     bool          // raw mode on the alternate screen (vi): hold other output
	rawDone chan struct{} // closes when the current raw mode ends
	cols    atomic.Int32
	rows    atomic.Int32
}

// resizeKey is sent to a raw mode program when the terminal changes size.
const resizeKey = "\x00resize"

func (t *terminal) setOutput(w io.Writer) {
	t.mu.Lock()
	t.out = w
	t.mu.Unlock()
}

func (t *terminal) Write(b []byte) (int, error) {
	t.mu.Lock()
	w := t.out
	t.mu.Unlock()
	if w == nil {
		return len(b), nil
	}
	return w.Write(b)
}

// holdWriter is the terminal for everything but the raw mode program. Its
// output (a warning of kubectl, for example) waits while the program owns the
// screen, so it does not end up in the middle of vi.
type holdWriter struct{ t *terminal }

func (h holdWriter) Write(b []byte) (int, error) {
	h.t.mu.Lock()
	if h.t.raw.Load() && h.t.alt {
		h.t.held = append(h.t.held, b...)
		h.t.mu.Unlock()
		return len(b), nil
	}
	h.t.mu.Unlock()
	return h.t.Write(b)
}

// input takes what the browser terminal sends while a command runs.
func (t *terminal) input(s string) {
	if !t.raw.Load() {
		return
	}
	select {
	case t.keys <- s:
	default: // the program does not read; drop instead of blocking the browser
	}
}

func (t *terminal) resize(cols, rows int) {
	t.cols.Store(int32(cols))
	t.rows.Store(int32(rows))
	t.input(resizeKey)
}

func (t *terminal) size() (cols, rows int) {
	cols, rows = int(t.cols.Load()), int(t.rows.Load())
	if cols < 20 {
		cols = 80
	}
	if rows < 5 {
		rows = 24
	}
	return cols, rows
}

// enterRaw switches to raw mode and the alternate screen. The returned
// function switches back.
func (t *terminal) enterRaw() func() {
	t.startRaw(true)
	io.WriteString(t, "\x1b[?1049h\x1b[H\x1b[2J")
	return func() {
		io.WriteString(t, "\x1b[?1049l")
		t.endRaw()
	}
}

// startRaw passes every key to the program from now on; alt holds the
// output of everything else (a full screen program owns the screen).
func (t *terminal) startRaw(alt bool) {
	for len(t.keys) > 0 {
		<-t.keys
	}
	t.mu.Lock()
	t.alt = alt
	t.rawDone = make(chan struct{})
	t.mu.Unlock()
	t.raw.Store(true)
}

func (t *terminal) endRaw() {
	t.mu.Lock()
	if !t.raw.Load() {
		t.mu.Unlock()
		return
	}
	t.raw.Store(false)
	close(t.rawDone)
	held := t.held
	t.held = nil
	t.alt = false
	t.mu.Unlock()
	t.Write(held)
}

// The terminal for kubectl -it. kubectl asks github.com/moby/term whether
// its stdin is a terminal, switches it to raw mode and polls its size; the
// js variant of that package (k8s/_overlay) asks these hooks. Only the stdin
// that the shell gives to kubectl in the foreground (ttyIn) and the output
// below have the file descriptor ttyFd.
const ttyFd = 1 << 30

func init() {
	term.JS.IsTerminal = func(fd uintptr) bool { return fd == ttyFd }
	term.JS.MakeRaw = func(uintptr) error {
		tty.startRaw(false)
		return nil
	}
	term.JS.Restore = func(uintptr) { tty.endRaw() }
	term.JS.Size = func(uintptr) (uint16, uint16) {
		cols, rows := tty.size()
		return uint16(cols), uint16(rows)
	}
	term.JS.Streams = func() (io.ReadCloser, io.Writer, io.Writer) {
		return &ttyIn{ctx: context.Background()}, ttyOut{}, ttyOut{}
	}
}

// ttyIn reads the keys of the browser terminal while it is in raw mode.
// Outside raw mode it is at its end, like the stdin of other commands.
type ttyIn struct {
	ctx context.Context
	buf []byte
}

func (r *ttyIn) Fd() uintptr  { return ttyFd }
func (r *ttyIn) Close() error { return nil }

func (r *ttyIn) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		tty.mu.Lock()
		done := tty.rawDone
		tty.mu.Unlock()
		if !tty.raw.Load() || done == nil {
			return 0, io.EOF
		}
		select {
		case s := <-tty.keys:
			if s != resizeKey {
				r.buf = []byte(s)
			}
		case <-done:
			return 0, io.EOF
		case <-r.ctx.Done():
			return 0, io.EOF
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// ttyOut writes to the browser terminal itself.
type ttyOut struct{}

func (ttyOut) Fd() uintptr                 { return ttyFd }
func (ttyOut) Write(b []byte) (int, error) { return tty.Write(b) }

// termStdin is the stdin of the shell of the terminal: commands that read it
// get end of file, only kubectl -it in the foreground reads keys (ttyIn).
type termStdinReader struct{}

func (termStdinReader) Read([]byte) (int, error) { return 0, io.EOF }

var termStdin io.Reader = termStdinReader{}

// key is one key press: a printable rune or the name of a special key.
type key struct {
	r    rune
	name string // "esc", "enter", "backspace", "tab", "up", "down", "left", "right", "home", "end", "pgup", "pgdown", "delete", "ctrl-x", "resize"
}

// parseKeys splits a chunk of terminal input into keys.
func parseKeys(s string) []key {
	if s == resizeKey {
		return []key{{name: "resize"}}
	}
	var keys []key
	r := []rune(s)
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case c == 0x1b:
			if i+2 < len(r) && (r[i+1] == '[' || r[i+1] == 'O') {
				j := i + 2
				for j < len(r) && (r[j] >= '0' && r[j] <= '9' || r[j] == ';') {
					j++
				}
				if j < len(r) {
					name := map[string]string{
						"A": "up", "B": "down", "C": "right", "D": "left", "H": "home", "F": "end",
						"1~": "home", "4~": "end", "7~": "home", "8~": "end",
						"3~": "delete", "5~": "pgup", "6~": "pgdown",
					}[string(r[i+2:j+1])]
					if name != "" {
						keys = append(keys, key{name: name})
					}
					i = j
					continue
				}
			}
			keys = append(keys, key{name: "esc"})
		case c == '\r' || c == '\n':
			keys = append(keys, key{name: "enter"})
			if c == '\r' && i+1 < len(r) && r[i+1] == '\n' {
				i++
			}
		case c == 0x7f || c == 0x08:
			keys = append(keys, key{name: "backspace"})
		case c == '\t':
			keys = append(keys, key{name: "tab"})
		case c < 0x20:
			keys = append(keys, key{name: "ctrl-" + string(rune('a'+c-1))})
		default:
			keys = append(keys, key{r: c})
		}
	}
	return keys
}
