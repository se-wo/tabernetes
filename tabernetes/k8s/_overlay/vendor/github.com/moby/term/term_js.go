//go:build js

package term

import (
	"errors"
	"io"
	"os"
)

var ErrInvalidState = errors.New("Invalid terminal state")

type terminalState struct{}
type Termios struct{}

// JS connects this package to the terminal of a program that runs in
// js/wasm, which has no file descriptors of a tty: Tabernetes sets it, so
// that kubectl exec -it and run -it find a terminal. A stream is that
// terminal when it has a method Fd() that IsTerminal accepts.
var JS struct {
	IsTerminal func(fd uintptr) bool
	MakeRaw    func(fd uintptr) error
	Restore    func(fd uintptr)
	Size       func(fd uintptr) (width, height uint16)
	Streams    func() (io.ReadCloser, io.Writer, io.Writer)
}

func stdStreams() (io.ReadCloser, io.Writer, io.Writer) {
	if JS.Streams != nil {
		return JS.Streams()
	}
	return os.Stdin, os.Stdout, os.Stderr
}
func getFdInfo(in interface{}) (uintptr, bool) {
	if f, ok := in.(interface{ Fd() uintptr }); ok {
		fd := f.Fd()
		return fd, isTerminal(fd)
	}
	return 0, false
}
func getWinsize(fd uintptr) (*Winsize, error) {
	if JS.Size != nil && isTerminal(fd) {
		w, h := JS.Size(fd)
		return &Winsize{Height: h, Width: w}, nil
	}
	return &Winsize{Height: 24, Width: 120}, nil
}
func setWinsize(fd uintptr, ws *Winsize) error { return nil }
func isTerminal(fd uintptr) bool               { return JS.IsTerminal != nil && JS.IsTerminal(fd) }
func restoreTerminal(fd uintptr, state *State) error {
	if JS.Restore != nil && isTerminal(fd) {
		JS.Restore(fd)
	}
	return nil
}
func saveState(fd uintptr) (*State, error)       { return &State{}, nil }
func disableEcho(fd uintptr, state *State) error { return nil }
func setRawTerminal(fd uintptr) (*State, error)  { return makeRaw(fd) }
func setRawTerminalOutput(uintptr) (*State, error) {
	return &State{}, nil
}
func makeRaw(fd uintptr) (*State, error) {
	if JS.MakeRaw != nil && isTerminal(fd) {
		if err := JS.MakeRaw(fd); err != nil {
			return nil, err
		}
	}
	return &State{}, nil
}
