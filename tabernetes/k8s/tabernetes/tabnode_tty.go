package main

import (
	"context"
	"io"
	"reflect"
	"strings"
	"sync"
	"unicode/utf8"
)

// pty is the terminal of a container with tty: true and of kubectl exec -t.
// A real pseudo terminal has a line discipline in the kernel: in canonical
// mode it echoes what is typed, handles backspace, Ctrl-U and Ctrl-W, hands
// complete lines to the program, turns Ctrl-C into SIGINT and Ctrl-D into
// end of file, and its output turns "\n" into "\r\n". The programs of the tab
// node are Go code without a kernel, so pty does that for them.
type pty struct {
	out   *lockedWriter // to the client: echo and output
	lines chan []byte   // complete input; nil is one end of file (Ctrl-D)
	done  chan struct{} // the input ended for good

	mu      sync.Mutex
	pending []byte
	intr    func() // what Ctrl-C interrupts

	line []rune // the line being typed
	esc  int    // 0, or the state of an escape sequence being skipped
}

// newPty reads the keys of the client from in until it ends or ctx is done.
// intr is what Ctrl-C interrupts while no shell prompt waits for a line.
func newPty(ctx context.Context, in io.Reader, out io.Writer, intr func()) *pty {
	p := &pty{
		out:   &lockedWriter{w: out},
		lines: make(chan []byte, 64),
		done:  make(chan struct{}),
		intr:  intr,
	}
	go p.readKeys(ctx, in)
	return p
}

func (p *pty) readKeys(ctx context.Context, in io.Reader) {
	defer close(p.done)
	if in == nil {
		return
	}
	stop := context.AfterFunc(ctx, func() {
		if c, ok := in.(io.Closer); ok {
			c.Close()
		}
	})
	defer stop()
	buf := make([]byte, 4096)
	var rest []byte
	for {
		n, err := in.Read(buf)
		if n > 0 {
			b := append(rest, buf[:n]...)
			rest = nil
			for len(b) > 0 {
				r, size := utf8.DecodeRune(b)
				if r == utf8.RuneError && size == 1 && !utf8.FullRune(b) {
					rest = append(rest, b...) // the rest of the rune comes with the next read
					break
				}
				p.key(r)
				b = b[size:]
			}
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}

// key applies one typed rune, like the canonical mode of a tty.
func (p *pty) key(r rune) {
	// Escape sequences (arrow keys) have no meaning to a line without an
	// editor: skip them instead of echoing ^[[A.
	switch p.esc {
	case 1:
		p.esc = 0
		if r == '[' || r == 'O' {
			p.esc = 2
		}
		return
	case 2:
		if r >= 0x40 && r <= 0x7e {
			p.esc = 0
		}
		return
	}
	switch r {
	case 0x1b:
		p.esc = 1
	case '\r', '\n':
		p.echo("\r\n")
		p.send([]byte(string(p.line) + "\n"))
		p.line = p.line[:0]
	case 0x7f, 0x08: // backspace
		if n := len(p.line); n > 0 {
			p.line = p.line[:n-1]
			p.echo("\b \b")
		}
	case 0x15: // Ctrl-U
		p.echo(strings.Repeat("\b \b", len(p.line)))
		p.line = p.line[:0]
	case 0x17: // Ctrl-W
		n := len(p.line)
		for n > 0 && p.line[n-1] == ' ' {
			n--
		}
		for n > 0 && p.line[n-1] != ' ' {
			n--
		}
		p.echo(strings.Repeat("\b \b", len(p.line)-n))
		p.line = p.line[:n]
	case 0x03, 0x1c: // Ctrl-C, Ctrl-\
		p.echo("^C\r\n")
		p.line = p.line[:0]
		p.mu.Lock()
		intr := p.intr
		p.mu.Unlock()
		if intr != nil {
			intr()
		}
	case 0x04: // Ctrl-D: end of file on an empty line, else the line as it is
		if len(p.line) == 0 {
			p.send(nil)
		} else {
			p.send([]byte(string(p.line)))
			p.line = p.line[:0]
		}
	default:
		if r < 0x20 && r != '\t' {
			return
		}
		p.line = append(p.line, r)
		p.echo(string(r))
	}
}

func (p *pty) echo(s string) { io.WriteString(p.out, s) }

func (p *pty) send(b []byte) {
	select {
	case p.lines <- b:
	default: // nobody reads: drop like a full tty buffer
	}
}

// Read is the stdin of the program.
func (p *pty) Read(b []byte) (int, error) {
	p.mu.Lock()
	if len(p.pending) > 0 {
		n := copy(b, p.pending)
		p.pending = p.pending[n:]
		p.mu.Unlock()
		return n, nil
	}
	p.mu.Unlock()
	var line []byte
	select {
	case line = <-p.lines:
	case <-p.done:
		select {
		case line = <-p.lines:
		default:
			return 0, io.EOF
		}
	}
	if line == nil {
		return 0, io.EOF
	}
	n := copy(b, line)
	p.mu.Lock()
	p.pending = append(p.pending, line[n:]...)
	p.mu.Unlock()
	return n, nil
}

// setIntr makes Ctrl-C interrupt f until the returned func restores the
// one before.
func (p *pty) setIntr(f func()) func() {
	p.mu.Lock()
	old := p.intr
	p.intr = f
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.intr = old
		p.mu.Unlock()
	}
}

// Write is the output of the program: "\n" becomes "\r\n".
func (p *pty) Write(b []byte) (int, error) {
	p.out.mu.Lock()
	defer p.out.mu.Unlock()
	var s []byte
	for _, c := range b {
		if c == '\n' && p.out.last != '\r' {
			s = append(s, '\r')
		}
		s = append(s, c)
		p.out.last = c
	}
	if _, err := p.out.w.Write(s); err != nil {
		return 0, err
	}
	return len(b), nil
}

type lockedWriter struct {
	mu   sync.Mutex
	w    io.Writer
	last byte
}

func (l *lockedWriter) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(b) > 0 {
		l.last = b[len(b)-1]
	}
	return l.w.Write(b)
}

// ttyOutput is the output of a container with tty: true. It goes to the
// log, which is line by line, and as it is to every kubectl attach: a prompt
// without a newline must show up right away.
type ttyOutput struct {
	log io.Writer

	mu   sync.Mutex
	subs map[*ttySub]struct{}
}

type ttySub struct{ w io.Writer }

func newTTYOutput(log io.Writer) *ttyOutput {
	return &ttyOutput{log: log, subs: map[*ttySub]struct{}{}}
}

func (t *ttyOutput) Write(b []byte) (int, error) {
	t.log.Write(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	for s := range t.subs {
		if _, err := s.w.Write(b); err != nil {
			delete(t.subs, s)
		}
	}
	return len(b), nil
}

// follow writes the output to w until ctx is done or done closes.
func (t *ttyOutput) follow(ctx context.Context, w io.Writer, done <-chan struct{}) {
	s := &ttySub{w: w}
	t.mu.Lock()
	t.subs[s] = struct{}{}
	t.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-done:
	}
	t.mu.Lock()
	delete(t.subs, s)
	t.mu.Unlock()
}

// ttyOf returns the terminal of a command whose stdin is that terminal, as
// isatty(0) would tell.
func ttyOf(ctx context.Context, stdin io.Reader) *pty {
	pe := procOf(ctx)
	if pe == nil || pe.tty == nil || unwrapStdin(stdin) != io.Reader(pe.tty) {
		return nil
	}
	return pe.tty
}

// unwrapStdin returns the reader a shell was given as stdin: on js,
// mvdan.cc/sh wraps it in a struct of its own with the reader embedded
// (interp.jsReader), once per nested shell.
func unwrapStdin(r io.Reader) io.Reader {
	for range 8 {
		v := reflect.ValueOf(r)
		if v.Kind() != reflect.Struct {
			return r
		}
		f := v.FieldByName("Reader")
		if !f.IsValid() || !f.CanInterface() {
			return r
		}
		in, ok := f.Interface().(io.Reader)
		if !ok || in == nil {
			return r
		}
		r = in
	}
	return r
}
