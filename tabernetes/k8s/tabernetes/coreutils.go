package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// A small set of coreutils for the shell, enough for kubectl pipelines like
// "kubectl get pods -A | grep -v Running | wc -l". They are deliberately
// simple: the common flags, no locale, no options nobody types.
type util func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error

var coreutils map[string]util

func init() {
	coreutils = map[string]util{
		"base64": base64Cmd, "cat": cat, "curl": curl, "cut": cut, "env": env, "free": free, "grep": grep,
		"head": head, "host": hostCmd, "ls": ls, "mkdir": mkdir, "nslookup": nslookup, "rm": rm, "seq": seq, "sleep": sleepCmd,
		"sort": sortCmd, "tail": tail, "tee": tee, "touch": touch, "tr": tr,
		"uniq": uniq, "vi": viCmd, "vim": viCmd, "wc": wc, "wget": wget, "xargs": xargs,
	}
}

// opts parses short flags. spec lists the flags, a trailing ":" marks a flag
// with a value: "n:v" knows -n <value> and -v. -n5, -vn5 and --long=value
// work. A bare -<number> is stored under "#" (head -5).
func opts(name string, args []string, spec string, long map[string]string) (map[string]string, []string, error) {
	o := map[string]string{}
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			args = args[1:]
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			break
		}
		args = args[1:]
		if strings.HasPrefix(a, "--") {
			k, v, hasV := strings.Cut(a[2:], "=")
			short, ok := long[k]
			if !ok {
				return nil, nil, fmt.Errorf("%s: unknown option --%s", name, k)
			}
			if strings.Contains(spec, short+":") && !hasV {
				if len(args) == 0 {
					return nil, nil, fmt.Errorf("%s: option --%s needs a value", name, k)
				}
				v, args = args[0], args[1:]
			}
			o[short] = v
			continue
		}
		if _, err := strconv.Atoi(a[1:]); err == nil && strings.Contains(spec, "#") {
			o["#"] = a[1:]
			continue
		}
		for i := 1; i < len(a); i++ {
			f := string(a[i])
			j := strings.Index(spec, f)
			if j < 0 || f == ":" {
				return nil, nil, fmt.Errorf("%s: unknown option -%s", name, f)
			}
			if j+1 < len(spec) && spec[j+1] == ':' {
				v := a[i+1:]
				if v == "" {
					if len(args) == 0 {
						return nil, nil, fmt.Errorf("%s: option -%s needs a value", name, f)
					}
					v, args = args[0], args[1:]
				}
				o[f] = v
				break
			}
			o[f] = ""
		}
	}
	return o, args, nil
}

func has(o map[string]string, k string) bool { _, ok := o[k]; return ok }

// abs makes p absolute. Inside a container of the tab node (tabnode_proc.go)
// the path goes through the root file system of the container.
func abs(ctx context.Context, hc interp.HandlerContext, p string) string {
	return realPath(ctx, hc.Dir, p)
}

func fail(hc interp.HandlerContext, code int, format string, a ...any) error {
	fmt.Fprintf(hc.Stderr, format+"\n", a...)
	return interp.ExitStatus(uint8(code))
}

// inputs calls fn for stdin or each named file ("-" is stdin).
func inputs(ctx context.Context, hc interp.HandlerContext, name string, files []string, fn func(file string, r io.Reader) error) error {
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	for _, f := range files {
		var r io.Reader = hc.Stdin
		if r == nil {
			r = strings.NewReader("")
		}
		if f == "/dev/null" {
			r = strings.NewReader("")
		} else if f != "-" {
			fh, err := openAs(ctx, hc, f, os.O_RDONLY, 0)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "%s: %s: %v\n", name, f, unwrap(err))
				code = 1
				continue
			}
			defer fh.Close()
			r = fh
		}
		if err := fn(f, r); err != nil {
			return err
		}
	}
	return exitCode(code)
}

func unwrap(err error) error {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err
	}
	return err
}

func lines(r io.Reader) []string {
	b, _ := io.ReadAll(r)
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" && len(b) == 0 {
		return nil
	}
	return strings.Split(s, "\n")
}

func count(o map[string]string, def int) (int, error) {
	v, ok := o["n"]
	if !ok {
		v, ok = o["#"]
	}
	if !ok {
		return def, nil
	}
	return strconv.Atoi(strings.TrimPrefix(v, "+"))
}

func cat(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	return inputs(ctx, hc, "cat", args, func(_ string, r io.Reader) error {
		_, err := io.Copy(hc.Stdout, r)
		return err
	})
}

func grep(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("grep", args, "ivcnEFoqwe:", map[string]string{"ignore-case": "i", "invert-match": "v", "count": "c", "regexp": "e"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	pat, ok := o["e"]
	if !ok {
		if len(args) == 0 {
			return fail(hc, 2, "usage: grep [-ivcnoqwFE] pattern [file...]")
		}
		pat, args = args[0], args[1:]
	}
	if has(o, "F") {
		pat = regexp.QuoteMeta(pat)
	} else if !has(o, "E") {
		// basic regular expressions: \| \( \) \+ are the special ones
		pat = strings.NewReplacer(`\|`, `|`, `\(`, `(`, `\)`, `)`, `\+`, `+`, `\?`, `?`, `|`, `\|`, `(`, `\(`, `)`, `\)`, `+`, `\+`, `?`, `\?`).Replace(pat)
	}
	if has(o, "w") {
		pat = `\b(?:` + pat + `)\b`
	}
	if has(o, "i") {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return fail(hc, 2, "grep: %v", err)
	}
	found := false
	err = inputs(ctx, hc, "grep", args, func(file string, r io.Reader) error {
		prefix := ""
		if len(args) > 1 {
			prefix = file + ":"
		}
		n := 0
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 16<<20)
		for i := 1; sc.Scan(); i++ {
			l := sc.Text()
			if re.MatchString(l) == has(o, "v") {
				continue
			}
			found, n = true, n+1
			switch {
			case has(o, "q"), has(o, "c"):
			case has(o, "o") && !has(o, "v"):
				for _, m := range re.FindAllString(l, -1) {
					fmt.Fprintln(hc.Stdout, prefix+m)
				}
			case has(o, "n"):
				fmt.Fprintf(hc.Stdout, "%s%d:%s\n", prefix, i, l)
			default:
				fmt.Fprintln(hc.Stdout, prefix+l)
			}
		}
		if has(o, "c") {
			fmt.Fprintf(hc.Stdout, "%s%d\n", prefix, n)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return interp.ExitStatus(1)
	}
	return nil
}

func head(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("head", args, "n:#", map[string]string{"lines": "n"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	n, err := count(o, 10)
	if err != nil {
		return fail(hc, 2, "head: invalid number of lines")
	}
	return inputs(ctx, hc, "head", args, func(_ string, r io.Reader) error {
		sc := bufio.NewScanner(r)
		sc.Buffer(nil, 16<<20)
		for i := 0; i < n && sc.Scan(); i++ {
			fmt.Fprintln(hc.Stdout, sc.Text())
		}
		// keep draining, so the writer of a pipe does not block forever
		io.Copy(io.Discard, r)
		return nil
	})
}

func tail(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("tail", args, "n:#", map[string]string{"lines": "n"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	n, err := count(o, 10)
	if err != nil {
		return fail(hc, 2, "tail: invalid number of lines")
	}
	from := strings.HasPrefix(o["n"], "+")
	return inputs(ctx, hc, "tail", args, func(_ string, r io.Reader) error {
		l := lines(r)
		switch {
		case from:
			l = l[min(len(l), max(n-1, 0)):]
		case len(l) > n:
			l = l[len(l)-n:]
		}
		for _, s := range l {
			fmt.Fprintln(hc.Stdout, s)
		}
		return nil
	})
}

func wc(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("wc", args, "lwc", map[string]string{"lines": "l", "words": "w", "bytes": "c"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(o) == 0 {
		o = map[string]string{"l": "", "w": "", "c": ""}
	}
	return inputs(ctx, hc, "wc", args, func(file string, r io.Reader) error {
		b, _ := io.ReadAll(r)
		var f []string
		if has(o, "l") {
			f = append(f, strconv.Itoa(bytes.Count(b, []byte("\n"))))
		}
		if has(o, "w") {
			f = append(f, strconv.Itoa(len(strings.Fields(string(b)))))
		}
		if has(o, "c") {
			f = append(f, strconv.Itoa(len(b)))
		}
		if file != "-" {
			f = append(f, file)
		}
		fmt.Fprintln(hc.Stdout, strings.Join(f, " "))
		return nil
	})
}

func sortCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("sort", args, "rnuk:t:", map[string]string{"reverse": "r", "numeric-sort": "n", "unique": "u", "key": "k", "field-separator": "t"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	key := func(s string) string { return s }
	if k, ok := o["k"]; ok {
		n, err := strconv.Atoi(strings.SplitN(k, ",", 2)[0])
		if err != nil || n < 1 {
			return fail(hc, 2, "sort: invalid key %q", k)
		}
		key = func(s string) string {
			var f []string
			if t, ok := o["t"]; ok {
				f = strings.Split(s, t)
			} else {
				f = strings.Fields(s)
			}
			if n > len(f) {
				return ""
			}
			return strings.Join(f[n-1:], " ")
		}
	}
	var all []string
	inputs(ctx, hc, "sort", args, func(_ string, r io.Reader) error { all = append(all, lines(r)...); return nil })
	less := func(a, b string) bool { return key(a) < key(b) }
	if has(o, "n") {
		num := func(s string) float64 {
			f, _ := strconv.ParseFloat(strings.Fields(key(s) + " 0")[0], 64)
			return f
		}
		less = func(a, b string) bool { return num(a) < num(b) }
	}
	sort.SliceStable(all, func(i, j int) bool {
		if has(o, "r") {
			return less(all[j], all[i])
		}
		return less(all[i], all[j])
	})
	if has(o, "u") {
		all = slices.CompactFunc(all, func(a, b string) bool { return key(a) == key(b) })
	}
	for _, l := range all {
		fmt.Fprintln(hc.Stdout, l)
	}
	return nil
}

func uniq(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("uniq", args, "cd", map[string]string{"count": "c", "repeated": "d"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	return inputs(ctx, hc, "uniq", args, func(_ string, r io.Reader) error {
		l := lines(r)
		for i := 0; i < len(l); {
			j := i
			for j < len(l) && l[j] == l[i] {
				j++
			}
			switch {
			case has(o, "d") && j-i < 2:
			case has(o, "c"):
				fmt.Fprintf(hc.Stdout, "%7d %s\n", j-i, l[i])
			default:
				fmt.Fprintln(hc.Stdout, l[i])
			}
			i = j
		}
		return nil
	})
}

func cut(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("cut", args, "d:f:", map[string]string{"delimiter": "d", "fields": "f"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if !has(o, "f") {
		return fail(hc, 2, "usage: cut -f list [-d delim] [file...]")
	}
	d := "\t"
	if v, ok := o["d"]; ok {
		d = v
	}
	var fields [][2]int
	for _, part := range strings.Split(o["f"], ",") {
		lo, hi, isRange := strings.Cut(part, "-")
		a, _ := strconv.Atoi(lo)
		b := a
		if isRange {
			b = 1 << 30
			if hi != "" {
				b, _ = strconv.Atoi(hi)
			}
			if lo == "" {
				a = 1
			}
		}
		if a < 1 || b < a {
			return fail(hc, 2, "cut: invalid field list %q", o["f"])
		}
		fields = append(fields, [2]int{a, b})
	}
	return inputs(ctx, hc, "cut", args, func(_ string, r io.Reader) error {
		for _, l := range lines(r) {
			f := strings.Split(l, d)
			if len(f) == 1 {
				fmt.Fprintln(hc.Stdout, l)
				continue
			}
			var out []string
			for i := range f {
				for _, rg := range fields {
					if i+1 >= rg[0] && i+1 <= rg[1] {
						out = append(out, f[i])
						break
					}
				}
			}
			fmt.Fprintln(hc.Stdout, strings.Join(out, d))
		}
		return nil
	})
}

func tr(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("tr", args, "d", nil)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	set := func(s string) []rune {
		var out []rune
		r := []rune(s)
		for i := 0; i < len(r); i++ {
			if i+2 < len(r) && r[i+1] == '-' {
				for c := r[i]; c <= r[i+2]; c++ {
					out = append(out, c)
				}
				i += 2
				continue
			}
			out = append(out, r[i])
		}
		return out
	}
	if (has(o, "d") && len(args) != 1) || (!has(o, "d") && len(args) != 2) {
		return fail(hc, 2, "usage: tr set1 set2 | tr -d set1")
	}
	from := set(args[0])
	var to []rune
	if !has(o, "d") {
		to = set(args[1])
	}
	b, _ := io.ReadAll(hc.Stdin)
	out := strings.Map(func(c rune) rune {
		i := slices.Index(from, c)
		switch {
		case i < 0:
			return c
		case has(o, "d") || len(to) == 0:
			return -1
		case i < len(to):
			return to[i]
		default:
			return to[len(to)-1]
		}
	}, string(b))
	io.WriteString(hc.Stdout, out)
	return nil
}

func base64Cmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("base64", args, "dw:", map[string]string{"decode": "d", "wrap": "w"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	return inputs(ctx, hc, "base64", args, func(_ string, r io.Reader) error {
		b, _ := io.ReadAll(r)
		if has(o, "d") {
			s := strings.Join(strings.Fields(string(b)), "")
			out, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				out, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
			}
			if err != nil {
				return fail(hc, 1, "base64: invalid input")
			}
			hc.Stdout.Write(out)
			return nil
		}
		s := base64.StdEncoding.EncodeToString(b)
		w := 76
		if v, ok := o["w"]; ok {
			w, _ = strconv.Atoi(v)
		}
		for w > 0 && len(s) > w {
			fmt.Fprintln(hc.Stdout, s[:w])
			s = s[w:]
		}
		fmt.Fprintln(hc.Stdout, s)
		return nil
	})
}

func ls(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("ls", args, "la1Ad", nil)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(args) == 0 {
		args = []string{"."}
	}
	code := 0
	for n, a := range args {
		fi, err := os.Lstat(abs(ctx, hc, a))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "ls: %s: %v\n", a, unwrap(err))
			code = 2
			continue
		}
		var entries []fs.FileInfo
		if fi.IsDir() && !has(o, "d") {
			des, err := readDirAs(ctx, hc, a)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "ls: can't open '%s': %v\n", a, unwrap(err))
				code = 2
				continue
			}
			for _, de := range des {
				if strings.HasPrefix(de.Name(), ".") && !has(o, "a") && !has(o, "A") {
					continue
				}
				if info, err := de.Info(); err == nil {
					entries = append(entries, info)
				}
			}
			if len(args) > 1 {
				if n > 0 {
					fmt.Fprintln(hc.Stdout)
				}
				fmt.Fprintf(hc.Stdout, "%s:\n", a)
			}
		} else {
			entries = []fs.FileInfo{fi}
		}
		for _, e := range entries {
			name := e.Name()
			if !fi.IsDir() || has(o, "d") {
				name = a
			}
			if e.IsDir() {
				name = "\x1b[1;34m" + name + "\x1b[0m"
			}
			if has(o, "l") {
				fmt.Fprintf(hc.Stdout, "%s 1 %s %8d %s %s\n", lsMode(e.Mode()), lsOwner(ctx, e), e.Size(), e.ModTime().Format("Jan _2 15:04"), name)
			} else {
				fmt.Fprintln(hc.Stdout, name)
			}
		}
	}
	return exitCode(code)
}

func mkdir(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("mkdir", args, "p", map[string]string{"parents": "p"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	for _, a := range args {
		if err := mkdirAs(ctx, hc, a, has(o, "p")); err != nil {
			return fail(hc, 1, "mkdir: %s: %v", a, unwrap(err))
		}
	}
	return nil
}

func rm(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("rm", args, "rfR", map[string]string{"recursive": "r", "force": "f"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	code := 0
	for _, a := range args {
		p := abs(ctx, hc, a)
		if _, err := os.Lstat(p); err != nil {
			if !has(o, "f") {
				fmt.Fprintf(hc.Stderr, "rm: %s: %v\n", a, unwrap(err))
				code = 1
			}
			continue
		}
		if err := removeAs(ctx, hc, a, has(o, "r") || has(o, "R")); err != nil {
			fmt.Fprintf(hc.Stderr, "rm: %s: %v\n", a, unwrap(err))
			code = 1
		}
	}
	return exitCode(code)
}

func touch(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	for _, a := range args {
		f, err := openAs(ctx, hc, a, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return fail(hc, 1, "touch: %s: %v", a, unwrap(err))
		}
		f.Close()
	}
	return nil
}

func tee(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("tee", args, "a", map[string]string{"append": "a"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if has(o, "a") {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	ws := []io.Writer{hc.Stdout}
	for _, a := range args {
		f, err := openAs(ctx, hc, a, flag, 0o644)
		if err != nil {
			return fail(hc, 1, "tee: %s: %v", a, unwrap(err))
		}
		defer f.Close()
		ws = append(ws, f)
	}
	_, err = io.Copy(io.MultiWriter(ws...), hc.Stdin)
	return err
}

func seq(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	n := make([]int, len(args))
	for i, a := range args {
		v, err := strconv.Atoi(a)
		if err != nil {
			return fail(hc, 1, "seq: invalid number %q", a)
		}
		n[i] = v
	}
	first, step, last := 1, 1, 0
	switch len(n) {
	case 1:
		last = n[0]
	case 2:
		first, last = n[0], n[1]
	case 3:
		first, step, last = n[0], n[1], n[2]
	default:
		return fail(hc, 1, "usage: seq [first [step]] last")
	}
	if step == 0 {
		return fail(hc, 1, "seq: step must not be 0")
	}
	for i := first; (step > 0 && i <= last) || (step < 0 && i >= last); i += step {
		fmt.Fprintln(hc.Stdout, i)
	}
	return nil
}

func sleepCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if len(args) != 1 {
		return fail(hc, 1, "usage: sleep seconds")
	}
	d, err := time.ParseDuration(args[0])
	if err != nil {
		f, err2 := strconv.ParseFloat(args[0], 64)
		if err2 != nil {
			return fail(hc, 1, "sleep: invalid time %q", args[0])
		}
		d = time.Duration(f * float64(time.Second))
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return interp.ExitStatus(130)
	}
}

func env(_ context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var out []string
	hc.Env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			out = append(out, name+"="+vr.Str)
		}
		return true
	})
	sort.Strings(out)
	for _, l := range out {
		fmt.Fprintln(hc.Stdout, l)
	}
	return nil
}

// xargs runs a command with the words of stdin as extra arguments, through
// the shell, so "kubectl get pods -o name | xargs kubectl delete" works.
func xargs(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	o, args, err := opts("xargs", args, "n:I:", nil)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(args) == 0 {
		args = []string{"echo"}
	}
	b, _ := io.ReadAll(hc.Stdin)
	var words []string
	if repl, ok := o["I"]; ok {
		for _, l := range lines(bytes.NewReader(b)) {
			cmd := make([]string, len(args))
			for i, a := range args {
				cmd[i] = strings.ReplaceAll(a, repl, l)
			}
			if err := exec(ctx, cmd); err != nil {
				return err
			}
		}
		return nil
	}
	words = strings.Fields(string(b))
	n := len(words)
	if v, ok := o["n"]; ok {
		n, _ = strconv.Atoi(v)
	}
	if n < 1 {
		n = max(len(words), 1)
	}
	for i := 0; i < len(words) || i == 0; i += n {
		chunk := words[i:min(i+n, len(words))]
		if err := exec(ctx, append(slices.Clone(args), chunk...)); err != nil {
			return err
		}
		if len(words) == 0 {
			break
		}
	}
	return nil
}

// wget downloads with the fetch() of the browser (net/http on js/wasm), so
// only servers that send CORS headers work, raw.githubusercontent.com does.
// Requests to the cluster stay in the wasm: localhost (kubectl port-forward),
// names and cluster IPs of services, pod and node IPs (tabnode_dial.go).
func wget(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	flags, urls := splitWgetArgs(args)
	o, _, err := opts("wget", flags, "qvsO:T:", map[string]string{
		"quiet": "q", "verbose": "v", "output-document": "O", "timeout": "T", "spider": "s",
	})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(urls) == 0 {
		return fail(hc, 2, "usage: wget [-q] [-v] [-O file] [-T seconds] [--spider] url...")
	}
	out, toFile := o["O"], has(o, "O")
	if toFile && out != "-" && len(urls) > 1 {
		return fail(hc, 2, "wget: -O with several urls is not supported")
	}
	if t := o["T"]; t != "" {
		secs, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return fail(hc, 2, "wget: invalid timeout %q", t)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs*float64(time.Second)))
		defer cancel()
	}
	for _, raw := range urls {
		u, err := cmdURL(ctx, raw)
		if err != nil {
			return fail(hc, 1, "wget: %v", err)
		}
		name := out
		if !toFile {
			if name = path.Base(u.Path); name == "/" || name == "." || name == "" {
				name = "index.html"
			}
		}
		if has(o, "s") {
			name = ""
		}
		if err := download(ctx, hc, u.String(), name, has(o, "q")); err != nil {
			return err
		}
	}
	return nil
}

// splitWgetArgs separates options from urls. Options may follow the urls, as
// with GNU wget: wget url -O x.
func splitWgetArgs(args []string) (flags, urls []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return flags, append(urls, args[i+1:]...)
		case a == "-O" || a == "--output-document" || a == "-T" || a == "--timeout":
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			// -qO file: the value of the last flag of a group follows.
			if !strings.HasPrefix(a, "--") && strings.ContainsAny(a[len(a)-1:], "OT") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			urls = append(urls, a)
		}
	}
	return flags, urls
}

// wgetURL is cmdURL outside of any pod.
func wgetURL(raw string) (*url.URL, error) { return cmdURL(context.Background(), raw) }

// cmdURL parses a url of the command line. Without a scheme it is http for
// the cluster (localhost, services, cluster and pod IPs) and https for
// everything else: the page itself is https, the browser blocks plain http.
func cmdURL(ctx context.Context, raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		scheme := "https://"
		h, _, _ := strings.Cut(raw, "/")
		h, _, _ = strings.Cut(h, "?")
		if host := urlHost(h); host == "localhost" || inCluster(ctx, host) {
			scheme = "http://"
		}
		raw = scheme + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s: invalid url", raw)
	}
	return u, nil
}

// urlHost strips the port of host[:port].
func urlHost(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// noSuchHost reports names that only the cluster DNS could know but does
// not: single labels like "web" and names under cluster.local. Those fail
// right away instead of going to fetch().
func noSuchHost(ctx context.Context, host string) bool {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if h == "localhost" || net.ParseIP(h) != nil || inCluster(ctx, h) {
		return false
	}
	return !strings.Contains(h, ".") || strings.HasSuffix(h, ".cluster.local") || strings.HasSuffix(h, ".svc")
}

// No keep-alive: a pooled connection would keep answering for a server that
// has stopped since.
var localClient = &http.Client{Transport: &http.Transport{DialContext: dialLocal, DisableKeepAlives: true}}

// dialLocal dials in the in-process network of the wasm. It has no resolver,
// so localhost becomes 127.0.0.1.
func dialLocal(ctx context.Context, network, addr string) (net.Conn, error) {
	if host, port, err := net.SplitHostPort(addr); err == nil && host == "localhost" {
		addr = net.JoinHostPort("127.0.0.1", port)
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// cmdTransport sends each request where it belongs: into the in-process
// network for the cluster, to fetch() for the rest. A redirect may cross
// from one to the other.
type cmdTransport struct {
	tlsConfig *tls.Config // for the cluster only, fetch() has its own
}

func (t cmdTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c := httpClientFor(req.Context(), req.URL.Hostname())
	rt := c.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	if c != http.DefaultClient && t.tlsConfig != nil {
		if tr, ok := rt.(*http.Transport); ok {
			tr = tr.Clone()
			tr.TLSClientConfig = t.tlsConfig
			rt = tr
		}
	}
	return rt.RoundTrip(req)
}

func download(ctx context.Context, hc interp.HandlerContext, u, name string, quiet bool) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fail(hc, 1, "wget: %v", err)
	}
	if noSuchHost(ctx, req.URL.Hostname()) {
		return fail(hc, 4, "wget: bad address '%s'", req.URL.Host)
	}
	cluster := httpClientFor(ctx, req.URL.Hostname()) != http.DefaultClient
	resp, err := (&http.Client{Transport: cmdTransport{}}).Do(req)
	if err != nil && cluster {
		if ctx.Err() == context.DeadlineExceeded {
			return fail(hc, 4, "wget: download timed out")
		}
		return fail(hc, 4, "wget: can't connect to remote host (%s): %v", req.URL.Host, connError(err))
	}
	if err != nil {
		// fetch() reports a CORS block as a plain network error.
		return fail(hc, 4, "wget: %s: %v (the server must allow CORS requests)", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fail(hc, 8, "wget: server returned error: %s %s", resp.Proto, resp.Status)
	}
	if name == "" { // --spider
		if !quiet {
			fmt.Fprintln(hc.Stderr, "remote file exists")
		}
		return nil
	}
	w, err := createOutput(ctx, hc, name)
	if err != nil {
		return fail(hc, 1, "wget: %s: %v", name, unwrap(err))
	}
	defer w.Close()
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return fail(hc, 4, "wget: %s: %v", u, err)
	}
	if !quiet && name != "-" {
		fmt.Fprintf(hc.Stderr, "%s saved [%d bytes]\n", name, n)
	}
	return nil
}

// createOutput opens the output file of a download. "-" is stdout, and the
// devices exist in the terminal, too.
func createOutput(ctx context.Context, hc interp.HandlerContext, name string) (io.WriteCloser, error) {
	switch name {
	case "-", "/dev/stdout":
		return nopCloser{hc.Stdout}, nil
	case "/dev/stderr":
		return nopCloser{hc.Stderr}, nil
	case "/dev/null":
		return nopCloser{io.Discard}, nil
	}
	return openAs(ctx, hc, name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// refused reports a refused connection. The in-process network flattens
// the error into a string.
func refused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "connection refused")
}

// connError shortens the error of a failed dial in the in-process network to
// what a socket would say.
func connError(err error) string {
	switch {
	case refused(err):
		return "Connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "Connection reset by peer"
	}
	return unwrap(err).Error()
}

// curlArgs is a parsed curl command line.
type curlArgs struct {
	silent, showErr, fail, include, head, location, verbose, insecure, get bool
	outputs                                                                []string // -o file, or "" for -O, in the order of the urls
	method, writeOut, user, cacert                                         string
	data                                                                   []string
	dataSet                                                                bool
	headers                                                                []string
	timeout                                                                time.Duration
	urls                                                                   []string
}

// curlLong maps the long options to short ones; "" are accepted and ignored.
var curlLong = map[string]string{
	"silent": "s", "show-error": "S", "fail": "f", "include": "i", "head": "I", "location": "L",
	"verbose": "v", "insecure": "k", "output": "o", "remote-name": "O", "request": "X", "data": "d",
	"data-ascii": "d", "data-raw": "d+", "data-binary": "d+", "header": "H", "max-time": "m",
	"write-out": "w", "user": "u", "user-agent": "A", "get": "G", "url": "url", "cacert": "cacert",
	"connect-timeout": "connect-timeout", "compressed": "", "no-progress-meter": "", "progress-bar": "", "http1.1": "",
}

// curlValue lists the options that take a value.
var curlValue = map[string]bool{
	"o": true, "X": true, "d": true, "d+": true, "H": true, "m": true, "w": true, "u": true, "A": true,
	"url": true, "cacert": true, "connect-timeout": true, "e": true,
}

func parseCurl(ctx context.Context, hc interp.HandlerContext, args []string) (*curlArgs, error) {
	c := &curlArgs{}
	set := func(k, v string) error {
		switch k {
		case "s":
			c.silent = true
		case "S":
			c.showErr = true
		case "f":
			c.fail = true
		case "i":
			c.include = true
		case "I":
			c.head = true
		case "L":
			c.location = true
		case "v":
			c.verbose = true
		case "k":
			c.insecure = true
		case "G":
			c.get = true
		case "o":
			c.outputs = append(c.outputs, v)
		case "O":
			c.outputs = append(c.outputs, "")
		case "X":
			c.method = v
		case "d", "d+":
			if rest, ok := strings.CutPrefix(v, "@"); ok {
				var b []byte
				var err error
				if rest == "-" {
					b, err = io.ReadAll(hc.Stdin)
				} else {
					b, err = os.ReadFile(abs(ctx, hc, rest))
				}
				if err != nil {
					return fmt.Errorf("Failed to open %s", rest)
				}
				v = string(b)
				if k == "d" { // -d @file drops the line breaks, --data-binary keeps them
					v = strings.NewReplacer("\r", "", "\n", "").Replace(v)
				}
			}
			c.data = append(c.data, v)
			c.dataSet = true
		case "H":
			c.headers = append(c.headers, v)
		case "m":
			secs, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return fmt.Errorf("expected a proper numerical parameter: %s", v)
			}
			c.timeout = time.Duration(secs * float64(time.Second))
		case "w":
			c.writeOut = v
		case "u":
			c.user = v
		case "A":
			c.headers = append(c.headers, "User-Agent: "+v)
		case "e":
			c.headers = append(c.headers, "Referer: "+v)
		case "url":
			c.urls = append(c.urls, v)
		case "cacert":
			c.cacert = v
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			c.urls = append(c.urls, args[i+1:]...)
			return c, nil
		case strings.HasPrefix(a, "--"):
			name, v, hasV := strings.Cut(a[2:], "=")
			k, ok := curlLong[name]
			if !ok {
				return nil, fmt.Errorf("option --%s: is unknown", name)
			}
			if curlValue[k] && !hasV {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("option --%s: requires parameter", name)
				}
				i++
				v = args[i]
			}
			if err := set(k, v); err != nil {
				return nil, err
			}
		case strings.HasPrefix(a, "-") && a != "-":
			for j := 1; j < len(a); j++ {
				k := string(a[j])
				if !strings.Contains("sSfiILvkGoOXdHmwuAe", k) {
					return nil, fmt.Errorf("option -%s: is unknown", k)
				}
				if curlValue[k] {
					v := a[j+1:]
					if v == "" {
						if i+1 >= len(args) {
							return nil, fmt.Errorf("option -%s: requires parameter", k)
						}
						i++
						v = args[i]
					}
					if err := set(k, v); err != nil {
						return nil, err
					}
					break
				}
				if err := set(k, ""); err != nil {
					return nil, err
				}
			}
		default:
			c.urls = append(c.urls, a)
		}
	}
	return c, nil
}

// curl is the curl of the tutorials: -sSfiILvk, -o/-O, -X, -d, -H, -m, -w.
// Like wget it stays in the wasm for the cluster and uses fetch() otherwise,
// where the browser decides about redirects, TLS and CORS.
func curl(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	c, err := parseCurl(ctx, hc, args)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "curl: %v\ncurl: try 'curl --help' for more information\n", err)
		return interp.ExitStatus(2)
	}
	if len(c.urls) == 0 {
		fmt.Fprintln(hc.Stderr, "curl: no URL specified\ncurl: try 'curl --help' for more information")
		return interp.ExitStatus(2)
	}
	tr := cmdTransport{}
	if c.insecure || c.cacert != "" {
		tr.tlsConfig = &tls.Config{InsecureSkipVerify: c.insecure}
		if c.cacert != "" {
			pem, err := os.ReadFile(abs(ctx, hc, c.cacert))
			if err != nil {
				return c.fail77(hc, err)
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tr.tlsConfig.RootCAs = pool
		}
	}
	code := 0
	for i, raw := range c.urls {
		out := "-"
		if i < len(c.outputs) {
			out = c.outputs[i]
		}
		if st := c.do(ctx, hc, tr, raw, out); st != 0 {
			code = st
		}
	}
	return exitCode(code)
}

func (c *curlArgs) fail77(hc interp.HandlerContext, err error) error {
	c.errorf(hc, 77, "error setting certificate file: %v", unwrap(err))
	return interp.ExitStatus(77)
}

// errorf prints a curl error unless -s without -S, and returns its code.
func (c *curlArgs) errorf(hc interp.HandlerContext, code int, format string, a ...any) int {
	if !c.silent || c.showErr {
		fmt.Fprintf(hc.Stderr, "curl: (%d) %s\n", code, fmt.Sprintf(format, a...))
	}
	return code
}

// do runs one url and returns the exit code of curl for it.
func (c *curlArgs) do(ctx context.Context, hc interp.HandlerContext, tr cmdTransport, raw, out string) int {
	start := time.Now()
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	u, err := cmdURL(ctx, raw)
	if err != nil {
		return c.errorf(hc, 3, "URL rejected: Malformed input to a URL function")
	}
	method := http.MethodGet
	var body io.Reader
	data := strings.Join(c.data, "&")
	switch {
	case c.head:
		method = http.MethodHead
	case c.dataSet && c.get:
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += data
	case c.dataSet:
		method = http.MethodPost
		body = strings.NewReader(data)
	}
	if c.method != "" {
		method = c.method
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return c.errorf(hc, 3, "%v", err)
	}
	req.Header.Set("User-Agent", "curl/8.11.1")
	req.Header.Set("Accept", "*/*")
	if c.dataSet && !c.get {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, h := range c.headers {
		k, v, _ := strings.Cut(h, ":")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch {
		case v == "" && strings.HasSuffix(h, ";"): // -H 'X;' sends an empty header
			req.Header.Set(strings.TrimSuffix(k, ";"), "")
		case v == "": // -H 'X:' removes it
			req.Header.Del(k)
		case strings.EqualFold(k, "Host"):
			req.Host = v
		default:
			req.Header.Add(k, v)
		}
	}
	if c.user != "" {
		user, pass, _ := strings.Cut(c.user, ":")
		req.SetBasicAuth(user, pass)
	}
	host := req.URL.Hostname()
	if noSuchHost(ctx, host) {
		return c.errorf(hc, 6, "Could not resolve host: %s", host)
	}
	cluster := httpClientFor(ctx, host) != http.DefaultClient
	redirects := 0
	client := &http.Client{Transport: tr, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if !c.location {
			return http.ErrUseLastResponse
		}
		if len(via) >= 50 {
			return errors.New("Maximum (50) redirects followed")
		}
		redirects++
		return nil
	}}
	if c.verbose {
		port := req.URL.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[req.URL.Scheme]
		}
		if cluster {
			fmt.Fprintf(hc.Stderr, "*   Trying %s:%s...\n", clusterHost(ctx, host), port)
		} else {
			fmt.Fprintf(hc.Stderr, "* Using fetch() of the browser for %s\n", host)
		}
		fmt.Fprintf(hc.Stderr, "> %s %s HTTP/1.1\n> Host: %s\n", method, req.URL.RequestURI(), req.URL.Host)
		for _, k := range slices.Sorted(maps.Keys(req.Header)) {
			for _, v := range req.Header[k] {
				fmt.Fprintf(hc.Stderr, "> %s: %s\n", k, v)
			}
		}
		fmt.Fprintln(hc.Stderr, ">")
	}
	resp, err := client.Do(req)
	if err != nil {
		port := req.URL.Port()
		if port == "" {
			port = map[string]string{"http": "80", "https": "443"}[req.URL.Scheme]
		}
		ms := time.Since(start).Milliseconds()
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			return c.errorf(hc, 28, "Operation timed out after %d milliseconds with 0 bytes received", ms)
		case refused(err):
			return c.errorf(hc, 7, "Failed to connect to %s port %s after %d ms: Couldn't connect to server", host, port, ms)
		case errors.Is(err, io.EOF) || errors.Is(err, syscall.ECONNRESET):
			return c.errorf(hc, 52, "Empty reply from server")
		case strings.Contains(err.Error(), "certificate"):
			return c.errorf(hc, 60, "SSL certificate problem: %v", unwrap(err))
		case !cluster:
			return c.errorf(hc, 7, "%s: %v (the server must allow CORS requests)", host, unwrap(err))
		}
		return c.errorf(hc, 56, "%v", unwrap(err))
	}
	defer resp.Body.Close()
	proto := resp.Proto
	if proto == "" {
		proto = "HTTP/1.1"
	}
	status := fmt.Sprintf("%s %s", proto, resp.Status)
	var head strings.Builder
	fmt.Fprintf(&head, "%s\r\n", status)
	for _, k := range slices.Sorted(maps.Keys(resp.Header)) {
		for _, v := range resp.Header[k] {
			fmt.Fprintf(&head, "%s: %s\r\n", k, v)
		}
	}
	head.WriteString("\r\n")
	if c.verbose {
		for _, l := range strings.Split(strings.TrimRight(head.String(), "\r\n"), "\r\n") {
			fmt.Fprintf(hc.Stderr, "< %s\n", l)
		}
		fmt.Fprintln(hc.Stderr, "<")
	}
	if c.fail && resp.StatusCode >= 400 {
		return c.errorf(hc, 22, "The requested URL returned error: %d", resp.StatusCode)
	}
	w := hc.Stdout
	if out != "-" {
		name := out
		if name == "" { // -O
			if name = path.Base(u.Path); name == "/" || name == "." {
				return c.errorf(hc, 23, "Remote file name has no length")
			}
		}
		f, err := createOutput(ctx, hc, name)
		if err != nil {
			return c.errorf(hc, 23, "Failure writing output to destination: %v", unwrap(err))
		}
		defer f.Close()
		w = f
	}
	if c.include || c.head {
		io.WriteString(w, head.String())
	}
	var n int64
	if !c.head {
		n, err = io.Copy(w, resp.Body)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				return c.errorf(hc, 28, "Operation timed out after %d milliseconds with %d bytes received", time.Since(start).Milliseconds(), n)
			}
			return c.errorf(hc, 56, "Failure when receiving data from the peer")
		}
	}
	if c.writeOut != "" {
		io.WriteString(hc.Stdout, curlWriteOut(c.writeOut, map[string]string{
			"http_code":     fmt.Sprintf("%03d", resp.StatusCode),
			"response_code": strconv.Itoa(resp.StatusCode),
			"url_effective": resp.Request.URL.String(),
			"content_type":  resp.Header.Get("Content-Type"),
			"size_download": strconv.FormatInt(n, 10),
			"time_total":    fmt.Sprintf("%.6f", time.Since(start).Seconds()),
			"num_redirects": strconv.Itoa(redirects),
			"method":        method,
			"remote_ip":     clusterHost(ctx, resp.Request.URL.Hostname()),
			"scheme":        resp.Request.URL.Scheme,
		}))
	}
	return 0
}

// curlWriteOut expands -w: %{variable}, \n, \t, \r and %%.
func curlWriteOut(format string, vars map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		switch {
		case format[i] == '\\' && i+1 < len(format):
			switch format[i+1] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteString(format[i : i+2])
			}
			i++
		case strings.HasPrefix(format[i:], "%%"):
			b.WriteByte('%')
			i++
		case strings.HasPrefix(format[i:], "%{"):
			end := strings.IndexByte(format[i:], '}')
			if end < 0 {
				b.WriteString(format[i:])
				return b.String()
			}
			b.WriteString(vars[format[i+2:i+end]])
			i += end
		default:
			b.WriteByte(format[i])
		}
	}
	return b.String()
}

// clusterNamespace is the namespace whose services short names find: the
// one of the pod, default in the terminal.
func clusterNamespace(ctx context.Context) string {
	if pe := procOf(ctx); pe != nil {
		return pe.w.namespace
	}
	return metav1.NamespaceDefault
}

// dnsLookup answers a name from the service proxy.
func dnsLookup(ctx context.Context, name string) (a dnsAnswer, found bool) {
	if theTabNode == nil || theTabNode.net == nil {
		return a, false
	}
	return theTabNode.net.lookup(name, clusterNamespace(ctx))
}

// nslookup answers like the nslookup of busybox, from the cluster DNS of
// the service proxy (tabnode_net.go): there is no CoreDNS on the tab node.
func nslookup(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var names []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return fail(hc, 1, "usage: nslookup [-type=a] host [dns server]")
	}
	server := "10.96.0.10"
	if theTabNode != nil && theTabNode.net != nil {
		server = theTabNode.net.dnsServer()
	}
	fmt.Fprintf(hc.Stdout, "Server:\t\t%s\nAddress:\t%s:53\n\n", server, server)
	name := names[0]
	if ip := net.ParseIP(name); ip != nil {
		if theTabNode != nil && theTabNode.net != nil {
			if fqdn, ok := theTabNode.net.reverse(name); ok {
				fmt.Fprintf(hc.Stdout, "%s\tname = %s\n\n", reverseName(ip), fqdn)
				return nil
			}
		}
		fmt.Fprintf(hc.Stdout, "** server can't find %s: NXDOMAIN\n\n", reverseName(ip))
		return interp.ExitStatus(1)
	}
	a, ok := dnsLookup(ctx, name)
	if !ok {
		fqdn := name
		if !strings.Contains(name, ".") {
			fqdn = name + "." + clusterNamespace(ctx) + ".svc.cluster.local"
		}
		fmt.Fprintf(hc.Stdout, "** server can't find %s: NXDOMAIN\n\n", strings.TrimSuffix(fqdn, "."))
		return interp.ExitStatus(1)
	}
	if a.cname != "" {
		fmt.Fprintf(hc.Stdout, "%s\tcanonical name = %s\n", a.name, a.cname)
	}
	for _, ip := range a.ips {
		n := a.name
		if a.cname != "" {
			n = a.cname
		}
		fmt.Fprintf(hc.Stdout, "Name:\t%s\nAddress: %s\n", n, ip)
	}
	fmt.Fprintln(hc.Stdout)
	return nil
}

// host answers like the host command of bind.
func hostCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var names []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return fail(hc, 1, "Usage: host [-t type] name [server]")
	}
	name := names[0]
	if ip := net.ParseIP(name); ip != nil {
		if theTabNode != nil && theTabNode.net != nil {
			if fqdn, ok := theTabNode.net.reverse(name); ok {
				fmt.Fprintf(hc.Stdout, "%s domain name pointer %s.\n", reverseName(ip), fqdn)
				return nil
			}
		}
		fmt.Fprintf(hc.Stdout, "Host %s not found: 3(NXDOMAIN)\n", reverseName(ip))
		return interp.ExitStatus(1)
	}
	a, ok := dnsLookup(ctx, name)
	if !ok {
		fmt.Fprintf(hc.Stdout, "Host %s not found: 3(NXDOMAIN)\n", name)
		return interp.ExitStatus(1)
	}
	n := a.name
	if a.cname != "" {
		fmt.Fprintf(hc.Stdout, "%s is an alias for %s.\n", a.name, a.cname)
		n = a.cname
	}
	for _, ip := range a.ips {
		fmt.Fprintf(hc.Stdout, "%s has address %s\n", n, ip)
	}
	return nil
}

// reverseName is the in-addr.arpa name of an IPv4 address.
func reverseName(ip net.IP) string {
	v4 := ip.To4()
	if v4 == nil {
		return ip.String()
	}
	return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa", v4[3], v4[2], v4[1], v4[0])
}
