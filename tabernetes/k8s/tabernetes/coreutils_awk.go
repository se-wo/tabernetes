package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/benhoyt/goawk/interp"
	"github.com/benhoyt/goawk/parser"
	shinterp "mvdan.cc/sh/v3/interp"
)

func init() { coreutils["awk"] = awkCmd }

// awkFS gives goawk the files of the shell: names go through abs() (the root
// of the container inside a pod, the shell directory in the terminal) while
// FILENAME stays what the user typed. It also covers getline < file and
// print > file, so redirections inside the program are mapped as well.
type awkFS struct {
	ctx context.Context
	hc  shinterp.HandlerContext
}

func (f awkFS) Open(name string) (fs.File, error) {
	return os.Open(abs(f.ctx, f.hc, name))
}

func (f awkFS) Create(name string) (io.WriteCloser, error) {
	return os.OpenFile(abs(f.ctx, f.hc, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
}

func (f awkFS) Append(name string) (io.WriteCloser, error) {
	return os.OpenFile(abs(f.ctx, f.hc, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
}

// awkCmd is awk on github.com/benhoyt/goawk: -F fs, -v var=value, -f progfile,
// --version, then the program text and files or var=value operands. system()
// and pipes to or from commands are disabled (goawk has no hook for them).
func awkCmd(ctx context.Context, hc shinterp.HandlerContext, args []string, _ shinterp.ExecHandlerFunc) error {
	var vars, progs []string
	fsep, haveFS := "", false
	for len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "-" {
		a := args[0]
		args = args[1:]
		if a == "--" {
			break
		}
		if a == "--version" {
			fmt.Fprintln(hc.Stdout, "awk (goawk "+"v1.32.0)")
			return nil
		}
		if len(a) < 2 || !strings.Contains("Fvf", a[1:2]) {
			return fail(hc, 2, "awk: unknown option %s", a)
		}
		v := a[2:]
		if v == "" {
			if len(args) == 0 {
				return fail(hc, 2, "awk: option -%s needs a value", a[1:2])
			}
			v, args = args[0], args[1:]
		}
		switch a[1] {
		case 'F':
			fsep, haveFS = v, true
			if v == "t" {
				fsep = "\t"
			}
			fsep = strings.ReplaceAll(fsep, `\t`, "\t")
		case 'v':
			name, val, ok := strings.Cut(v, "=")
			if !ok {
				return fail(hc, 2, "awk: invalid -v argument %q, want var=value", v)
			}
			vars = append(vars, name, val)
		case 'f':
			progs = append(progs, v)
		}
	}
	var src strings.Builder
	if len(progs) == 0 {
		if len(args) == 0 {
			return fail(hc, 2, "usage: awk [-F fs] [-v var=value] [-f progfile | 'prog'] [file ...]")
		}
		src.WriteString(args[0])
		args = args[1:]
	}
	for _, p := range progs {
		b, err := os.ReadFile(abs(ctx, hc, p))
		if err != nil {
			return fail(hc, 2, "awk: %s: %v", p, unwrap(err))
		}
		src.Write(b)
		src.WriteByte('\n')
	}
	prog, err := parser.ParseProgram([]byte(src.String()), nil)
	if err != nil {
		return fail(hc, 2, "awk: %v", err)
	}
	if haveFS {
		vars = append([]string{"FS", fsep}, vars...)
	}
	environ := []string{}
	for name, vr := range hc.Env.Each {
		if vr.Exported && vr.IsSet() {
			environ = append(environ, name, vr.String())
		}
	}
	cfg := &interp.Config{
		Stdin: stdinOf(hc), Output: hc.Stdout, Error: hc.Stderr,
		Argv0: "awk", Args: args, Vars: vars, Environ: environ,
		NoExec: true, FileSystem: awkFS{ctx, hc},
	}
	ip, err := interp.New(prog)
	if err != nil {
		return fail(hc, 2, "awk: %v", err)
	}
	code, err := ip.ExecuteContext(ctx, cfg)
	if closedPipe(err) {
		return shinterp.ExitStatus(141)
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return shinterp.ExitStatus(130)
		}
		return fail(hc, 2, "awk: %v", err)
	}
	if code != 0 {
		return shinterp.ExitStatus(uint8(code))
	}
	return nil
}
