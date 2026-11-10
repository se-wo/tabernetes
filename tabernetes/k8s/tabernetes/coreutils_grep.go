package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	coreutils["grep"] = grep
	coreutils["egrep"] = func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
		return grep(ctx, hc, append([]string{"-E"}, args...), exec)
	}
	coreutils["fgrep"] = func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
		return grep(ctx, hc, append([]string{"-F"}, args...), exec)
	}
}

// grep with the flags of BusyBox grep: -i -v -c -n -l -L -o -q -s -h -H -w
// -x -E -F -r/-R, -e and -f (patterns), -m max, -A/-B/-C context, --include
// and --exclude with -r, --color ignored.
func grep(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var pats []string
	var include, exclude []string
	var rest []string
	// -e may repeat and patterns may start with "-", so collect -e and -f
	// before opts sees the rest.
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			rest = append(rest, args[i:]...)
			i = len(args)
		case a == "-e" || a == "--regexp" || a == "-f" || a == "--file":
			if i+1 >= len(args) {
				return fail(hc, 2, "grep: option %s needs a value", a)
			}
			v := args[i+1]
			i++
			if a == "-f" || a == "--file" {
				b, err := os.ReadFile(abs(ctx, hc, v))
				if err != nil {
					return fail(hc, 2, "grep: %s: %v", v, unwrap(err))
				}
				pats = append(pats, strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")...)
				continue
			}
			pats = append(pats, v)
		case strings.HasPrefix(a, "--regexp="):
			pats = append(pats, a[len("--regexp="):])
		case strings.HasPrefix(a, "--include="):
			include = append(include, a[len("--include="):])
		case strings.HasPrefix(a, "--exclude="):
			exclude = append(exclude, a[len("--exclude="):])
		case a == "--color" || strings.HasPrefix(a, "--color=") || strings.HasPrefix(a, "--colour"):
		default:
			rest = append(rest, a)
		}
	}
	o, args, err := opts("grep", rest, "ivcnlLoqshHwxEFrRm:A:B:C:#", map[string]string{
		"ignore-case": "i", "invert-match": "v", "count": "c", "line-number": "n", "files-with-matches": "l",
		"files-without-match": "L", "only-matching": "o", "quiet": "q", "silent": "q", "no-messages": "s",
		"no-filename": "h", "with-filename": "H", "word-regexp": "w", "line-regexp": "x", "extended-regexp": "E",
		"fixed-strings": "F", "recursive": "r", "dereference-recursive": "R", "max-count": "m",
		"after-context": "A", "before-context": "B", "context": "C",
	})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(pats) == 0 {
		if len(args) == 0 {
			return fail(hc, 2, "usage: grep [-ivcnlLoqshHwxEFr] [-A n] [-B n] [-C n] [-e pattern]... pattern [file...]")
		}
		pats, args = strings.Split(args[0], "\n"), args[1:]
	}
	num := func(k string, def int) (int, error) {
		v, ok := o[k]
		if !ok {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("grep: invalid number '%s'", v)
		}
		return n, nil
	}
	ctxN, err := num("C", 0)
	if err == nil && has(o, "#") {
		ctxN, err = num("#", 0)
	}
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	after, err := num("A", ctxN)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	before, err := num("B", ctxN)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	maxCount, err := num("m", -1)
	if err != nil {
		return fail(hc, 2, "%v", err)
	}

	var alts []string
	for _, p := range pats {
		switch {
		case has(o, "F"):
			p = regexp.QuoteMeta(p)
		case !has(o, "E"):
			// basic regular expressions: \| \( \) \+ \? \{ \} are the special ones
			p = strings.NewReplacer(`\|`, `|`, `\(`, `(`, `\)`, `)`, `\+`, `+`, `\?`, `?`, `\{`, `{`, `\}`, `}`,
				`|`, `\|`, `(`, `\(`, `)`, `\)`, `+`, `\+`, `?`, `\?`, `{`, `\{`, `}`, `\}`).Replace(p)
		}
		alts = append(alts, "(?:"+p+")")
	}
	pat := strings.Join(alts, "|")
	switch {
	case has(o, "x"):
		pat = `^(?:` + pat + `)$`
	case has(o, "w"):
		pat = `\b(?:` + pat + `)\b`
	}
	if has(o, "i") {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return fail(hc, 2, "grep: %v", err)
	}

	recursive := has(o, "r") || has(o, "R")
	if recursive && len(args) == 0 {
		args = []string{"."}
	}
	// Files to search, with the name to print. Directories without -r
	// are skipped with a message, like grep does.
	type file struct{ name string }
	var files []file
	code := 0
	for _, a := range args {
		if a == "-" {
			files = append(files, file{a})
			continue
		}
		fi, err := os.Stat(abs(ctx, hc, a))
		if err != nil || !fi.IsDir() {
			files = append(files, file{a})
			continue
		}
		if !recursive {
			if !has(o, "s") {
				fmt.Fprintf(hc.Stderr, "grep: %s: Is a directory\n", a)
			}
			code = 2
			continue
		}
		root := abs(ctx, hc, a)
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			base := path.Base(p)
			if len(include) > 0 && !grepGlob(include, base) || grepGlob(exclude, base) {
				return nil
			}
			files = append(files, file{path.Join(a, strings.TrimPrefix(p, root))})
			return nil
		})
	}
	if len(files) == 0 && len(args) == 0 {
		files = []file{{"-"}}
	}
	names := (len(files) > 1 || recursive) && !has(o, "h") || has(o, "H")

	found := false
	for _, f := range files {
		if ctx.Err() != nil {
			return interp.ExitStatus(130)
		}
		var r io.Reader = stdinOf(hc)
		label := f.name
		if f.name == "-" {
			label = "(standard input)"
		} else {
			fh, err := os.Open(abs(ctx, hc, f.name))
			if err != nil {
				if !has(o, "s") {
					fmt.Fprintf(hc.Stderr, "grep: %s: %v\n", f.name, unwrap(err))
				}
				code = 2
				continue
			}
			r = fh
			defer fh.Close()
		}
		n := grepFile(hc, re, o, r, label, names, before, after, maxCount)
		if n > 0 {
			found = true
		}
		switch {
		case has(o, "q"):
			if found {
				return nil
			}
		case has(o, "l"):
			if n > 0 {
				fmt.Fprintln(hc.Stdout, label)
			}
		case has(o, "L"):
			if n == 0 {
				fmt.Fprintln(hc.Stdout, label)
			}
		case has(o, "c"):
			if names {
				fmt.Fprintf(hc.Stdout, "%s:%d\n", label, n)
			} else {
				fmt.Fprintf(hc.Stdout, "%d\n", n)
			}
		}
	}
	if code != 0 && !(found && has(o, "q")) {
		return interp.ExitStatus(uint8(code))
	}
	if !found {
		return interp.ExitStatus(1)
	}
	return nil
}

// grepFile prints the matching lines of r with their context and returns
// the number of matching lines.
func grepFile(hc interp.HandlerContext, re *regexp.Regexp, o map[string]string, r io.Reader, label string, names bool, before, after, maxCount int) int {
	quiet := has(o, "q") || has(o, "l") || has(o, "L") || has(o, "c")
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 16<<20)
	type line struct {
		n int
		s string
	}
	var buf []line     // up to "before" lines before the next match
	last, left := 0, 0 // the last printed line, context lines still to print after a match
	n := 0
	print := func(l line, sep string) {
		if last > 0 && l.n > last+1 && (before > 0 || after > 0) {
			fmt.Fprintln(hc.Stdout, "--")
		}
		last = l.n
		var p strings.Builder
		if names {
			p.WriteString(label + sep)
		}
		if has(o, "n") {
			p.WriteString(strconv.Itoa(l.n) + sep)
		}
		fmt.Fprintln(hc.Stdout, p.String()+l.s)
	}
	for i := 1; sc.Scan(); i++ {
		s := sc.Text()
		if re.MatchString(s) == has(o, "v") {
			if quiet {
				continue
			}
			if left > 0 {
				print(line{i, s}, "-")
				left--
			} else if before > 0 {
				buf = append(buf, line{i, s})
				if len(buf) > before {
					buf = buf[1:]
				}
			}
			continue
		}
		if maxCount >= 0 && n >= maxCount {
			break
		}
		n++
		if quiet {
			if has(o, "q") || has(o, "l") {
				break
			}
			continue
		}
		for _, b := range buf {
			print(b, "-")
		}
		buf = buf[:0]
		if has(o, "o") && !has(o, "v") {
			for _, m := range re.FindAllString(s, -1) {
				if m != "" {
					print(line{i, m}, ":")
				}
			}
		} else {
			print(line{i, s}, ":")
		}
		left = after
	}
	return n
}

func grepGlob(globs []string, name string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}
