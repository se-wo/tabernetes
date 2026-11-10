package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// readLine reads what the program gets next, with a timeout.
func readLine(t *testing.T, p *pty) (string, error) {
	t.Helper()
	type res struct {
		s   string
		err error
	}
	ch := make(chan res, 1)
	go func() {
		b := make([]byte, 256)
		n, err := p.Read(b)
		ch <- res{string(b[:n]), err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(5 * time.Second):
		t.Fatal("no input for the program")
		return "", nil
	}
}

func TestPtyLineDiscipline(t *testing.T) {
	in, keys := io.Pipe()
	var out syncBuffer
	interrupted := make(chan struct{}, 1)
	p := newPty(context.Background(), in, &out, func() { interrupted <- struct{}{} })

	io.WriteString(keys, "lsx\x7f -l\r")
	if s, err := readLine(t, p); s != "ls -l\n" || err != nil {
		t.Fatalf("line = %q, %v; want \"ls -l\\n\"", s, err)
	}
	// Arrow keys are dropped, Ctrl-U clears, Ctrl-W drops a word.
	io.WriteString(keys, "abc\x1b[Ad\x15echo one two\x17three\r")
	if s, _ := readLine(t, p); s != "echo one three\n" {
		t.Fatalf("line = %q, want \"echo one three\\n\"", s)
	}
	// Ctrl-C drops the line and interrupts.
	io.WriteString(keys, "sleep\x03")
	select {
	case <-interrupted:
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-C did not interrupt")
	}
	// Ctrl-D on an empty line is one end of file; the input goes on.
	io.WriteString(keys, "\x04")
	if _, err := readLine(t, p); err != io.EOF {
		t.Fatalf("Ctrl-D: err = %v, want EOF", err)
	}
	io.WriteString(keys, "ü\r")
	if s, _ := readLine(t, p); s != "ü\n" {
		t.Fatalf("line = %q, want \"ü\\n\"", s)
	}
	keys.Close()
	if _, err := readLine(t, p); err != io.EOF {
		t.Fatalf("closed input: err = %v, want EOF", err)
	}

	// The program writes "\n", the terminal gets "\r\n".
	io.WriteString(p, "a\nb\r\nc\n")
	erase := func(n int) string { return strings.Repeat("\b \b", n) }
	want := "lsx" + erase(1) + " -l\r\n" +
		"abcd" + erase(4) + "echo one two" + erase(3) + "three\r\n" +
		"sleep^C\r\n" + "ü\r\n" + "a\r\nb\r\nc\r\n"
	if got := out.String(); got != want {
		t.Fatalf("terminal output\n got %q\nwant %q", got, want)
	}
}

func TestShellPromptDefault(t *testing.T) {
	r, err := newPodRunner(nil, "/", strings.NewReader(""), io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader("true"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if got := shellPrompt(context.Background(), "/bin/sh", r); got != "/ # " {
		t.Errorf("sh prompt = %q", got)
	}
	if got := shellPrompt(context.Background(), "bash", r); got != "root@localhost:/# " {
		t.Errorf("bash prompt = %q", got)
	}
}

// The shell wraps stdin in a type of its own; ttyOf and the terminal
// shell need the reader they gave it.
func TestUnwrapStdin(t *testing.T) {
	want := strings.NewReader("")
	var got io.Reader
	r, err := interp.New(interp.StdIO(want, io.Discard, io.Discard), interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		return func(ctx context.Context, args []string) error {
			got = interp.HandlerCtx(ctx).Stdin
			return nil
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader("x"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if unwrapStdin(got) != io.Reader(want) {
		t.Fatalf("unwrapStdin(%T) is not the reader of the shell", got)
	}
	if unwrapStdin(termStdin) != termStdin {
		t.Fatal("unwrapStdin changed termStdin")
	}
}

// An interactive shell on a TTY: Ctrl-C ends the running command but not
// the shell, exit ends the shell with its status.
func TestInteractiveShellOnTTY(t *testing.T) {
	in, keys := io.Pipe()
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pe := &procEnv{}
	pe.tty = newPty(ctx, in, &out, cancel)
	done := make(chan int, 1)
	go func() { done <- runArgv(ctx, pe, []string{"sh"}, nil, "/", pe.tty, pe.tty, pe.tty) }()

	waitFor := func(s string) {
		t.Helper()
		for range 500 {
			if strings.Contains(out.String(), s) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("no %q in the output %q", s, out.String())
	}
	waitFor("/ # ")
	io.WriteString(keys, "sleep 60\r")
	time.Sleep(100 * time.Millisecond)
	io.WriteString(keys, "\x03")
	io.WriteString(keys, "echo still here\r")
	waitFor("still here\r\n/ # ")
	io.WriteString(keys, "exit 3\r")
	select {
	case code := <-done:
		if code != 3 {
			t.Fatalf("exit 3: status %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("exit did not end the shell; output %q", out.String())
	}
}
