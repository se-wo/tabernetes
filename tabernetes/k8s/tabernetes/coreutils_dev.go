package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func init() { coreutils["dd"] = ddCmd }

// devFile returns the character devices the in-memory file system lacks:
// /dev/null, /dev/zero, /dev/urandom, /dev/random and /dev/full. Redirects
// (shell.go, tabnode_proc.go) and the inputs of the coreutils use it.
func devFile(p string) io.ReadWriteCloser {
	switch p {
	case "/dev/null":
		return devNull{}
	case "/dev/zero":
		return devZero{}
	case "/dev/urandom", "/dev/random":
		return devRandom{}
	case "/dev/full":
		return devFull{}
	}
	return nil
}

type devZero struct{}

func (devZero) Read(b []byte) (int, error)  { clear(b); return len(b), nil }
func (devZero) Write(b []byte) (int, error) { return len(b), nil }
func (devZero) Close() error                { return nil }

type devRandom struct{}

func (devRandom) Read(b []byte) (int, error)  { return rand.Read(b) }
func (devRandom) Write(b []byte) (int, error) { return len(b), nil }
func (devRandom) Close() error                { return nil }

type devFull struct{ devZero }

func (devFull) Write([]byte) (int, error) { return 0, syscall.ENOSPC }

// parseByteSize reads sizes like 512, 10K, 50M, 1G, 1MB and 1KiB the way
// head, dd and truncate do: K, M and G are powers of 1024, KB, MB and GB
// powers of 1000, and dd also knows c (1), w (2) and b (512).
func parseByteSize(s string) (int64, error) {
	mult := int64(1)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30},
		{"kB", 1000}, {"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"k", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
		{"c", 1}, {"w", 2}, {"b", 512},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) && len(s) > len(u.suffix) {
			s, mult = strings.TrimSuffix(s, u.suffix), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid number: '%s'", s)
	}
	return n * mult, nil
}

// ddOpen opens the files of dd: the devices, or a file through the root of
// the container. The single place where dd touches the file system.
func ddOpen(ctx context.Context, hc interp.HandlerContext, name string, flag int) (io.ReadWriteCloser, error) {
	if d := devFile(name); d != nil {
		return d, nil
	}
	return openAs(ctx, hc, name, flag, 0o644)
}

// ddCmd is dd with if=, of=, bs=, ibs=, obs=, count=, skip=, seek=,
// conv=notrunc and status=none|noxfer|progress. Without if= it reads stdin,
// without of= it writes stdout. The summary goes to stderr like GNU dd's.
func ddCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	op := map[string]string{}
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return fail(hc, 1, "dd: unrecognized operand '%s'", a)
		}
		switch k {
		case "if", "of", "bs", "ibs", "obs", "count", "skip", "seek", "conv", "status", "iflag", "oflag":
			op[k] = v
		default:
			return fail(hc, 1, "dd: unrecognized operand '%s'", a)
		}
	}
	size := func(k string, def int64) (int64, error) {
		v, ok := op[k]
		if !ok {
			return def, nil
		}
		n, err := parseByteSize(v)
		if err != nil {
			return 0, fmt.Errorf("dd: %v", err)
		}
		return n, nil
	}
	ibs, err := size("ibs", 512)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	obs, err := size("obs", 512)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if _, ok := op["bs"]; ok {
		if ibs, err = size("bs", 512); err != nil {
			return fail(hc, 1, "%v", err)
		}
		obs = ibs
	}
	if ibs == 0 || obs == 0 {
		return fail(hc, 1, "dd: invalid number: '0'")
	}
	count, err := size("count", -1)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	skip, err := size("skip", 0)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	seek, err := size("seek", 0)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}

	var in io.Reader = stdinOf(hc)
	if name, ok := op["if"]; ok {
		f, err := ddOpen(ctx, hc, name, os.O_RDONLY)
		if err != nil {
			return fail(hc, 1, "dd: failed to open '%s': %v", name, ddErr(err))
		}
		defer f.Close()
		in = f
	}
	var out io.Writer = hc.Stdout
	if name, ok := op["of"]; ok {
		flag := os.O_WRONLY | os.O_CREATE
		if !strings.Contains(op["conv"], "notrunc") && seek == 0 {
			flag |= os.O_TRUNC
		}
		f, err := ddOpen(ctx, hc, name, flag)
		if err != nil {
			return fail(hc, 1, "dd: failed to open '%s': %v", name, ddErr(err))
		}
		defer f.Close()
		if seek > 0 {
			if s, ok := f.(io.Seeker); ok {
				if _, err := s.Seek(seek*obs, io.SeekStart); err != nil {
					return fail(hc, 1, "dd: '%s': cannot seek: %v", name, ddErr(err))
				}
			}
		}
		out = f
	}
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, in, skip*ibs); err != nil && err != io.EOF {
			return fail(hc, 1, "dd: '%s': cannot skip: %v", op["if"], ddErr(err))
		}
	}

	start := time.Now()
	var fullIn, partIn, fullOut, partOut, total int64
	buf := make([]byte, ibs)
	code := 0
	for count < 0 || fullIn+partIn < count {
		if ctx.Err() != nil {
			code = 130
			break
		}
		n, rerr := io.ReadFull(in, buf)
		if n > 0 {
			if int64(n) == ibs {
				fullIn++
			} else {
				partIn++
			}
			// Output blocks of obs bytes.
			for off := 0; off < n; off += int(obs) {
				end := min(off+int(obs), n)
				w, werr := out.Write(buf[off:end])
				total += int64(w)
				if int64(end-off) == obs {
					fullOut++
				} else {
					partOut++
				}
				if werr != nil {
					fmt.Fprintf(hc.Stderr, "dd: error writing '%s': %v\n", op["of"], ddErr(werr))
					code = 1
					break
				}
			}
		}
		if code != 0 || rerr != nil {
			if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
				fmt.Fprintf(hc.Stderr, "dd: error reading '%s': %v\n", op["if"], ddErr(rerr))
				code = 1
			}
			break
		}
	}
	if op["status"] != "none" {
		fmt.Fprintf(hc.Stderr, "%d+%d records in\n%d+%d records out\n", fullIn, partIn, fullOut, partOut)
		if op["status"] != "noxfer" {
			secs := max(time.Since(start).Seconds(), 1e-6)
			fmt.Fprintf(hc.Stderr, "%d bytes%s copied, %.4g s, %s/s\n", total, ddHuman(total), secs, ddRate(float64(total)/secs))
		}
	}
	return exitCode(code)
}

func ddErr(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "No such file or directory"
	case errors.Is(err, syscall.ENOSPC):
		return "No space left on device"
	}
	return unwrap(err).Error()
}

// ddHuman is the " (52 MB, 50 MiB)" part of the summary of GNU dd.
func ddHuman(n int64) string {
	if n < 1000 {
		return ""
	}
	si, iec := ddRate(float64(n)), ""
	if n >= 1024 {
		units := []string{"KiB", "MiB", "GiB", "TiB"}
		v := float64(n) / 1024
		i := 0
		for v >= 1024 && i < len(units)-1 {
			v /= 1024
			i++
		}
		iec = ", " + ddNum(v) + " " + units[i]
	}
	return " (" + si + iec + ")"
}

func ddRate(v float64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	i := 0
	for v >= 1000 && i < len(units)-1 {
		v /= 1000
		i++
	}
	return ddNum(v) + " " + units[i]
}

// ddNum prints two significant digits like GNU dd: 2.0, 52, 500.
func ddNum(v float64) string {
	if v < 10 {
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// sigpipe turns a write to a pipe whose reader has ended into exit status
// 141, what SIGPIPE does to a process: cat /dev/zero | head -c 4 ends
// quietly instead of with an error of the shell.
func sigpipe(err error) error {
	if closedPipe(err) {
		return interp.ExitStatus(141)
	}
	return err
}

func closedPipe(err error) bool {
	return err != nil && (errors.Is(err, io.ErrClosedPipe) || strings.Contains(err.Error(), io.ErrClosedPipe.Error()))
}
