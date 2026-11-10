package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/itchyny/gojq"
	"go.yaml.in/yaml/v3"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

func init() {
	coreutils["jq"] = jqCmd
	coreutils["yq"] = yqCmd
}

// jq and yq run on the gojq engine. jq supports -r -j -c -n -s -R -e -S -a
// --tab --indent n -M -C --arg --argjson --slurpfile --rawfile --args
// --jsonargs -f, combined short flags, $ENV/env, input/inputs and $ARGS.
// Differences from jq 1.7: gojq keeps objects as Go maps, so keys always come
// out sorted (as with -S), and 1.0 prints as 1.
//
// yq is NOT mikefarah/yq (far too large for the binary): it takes the same
// expressions as jq (gojq syntax) over YAML documents, with the yq command line
// (eval/eval-all, -i, -o, -I, -r, -n, -e, -P, -p) plus strenv() and env(NAME).
// Mappings lose their order (keys come out sorted) when printed.

// All file access of jq and yq goes through these four helpers (a place for
// permission checks). p is the user's path.
func jqOpen(ctx context.Context, hc interp.HandlerContext, p string) (*os.File, error) {
	return openAs(ctx, hc, p, os.O_RDONLY, 0)
}

func jqStat(ctx context.Context, hc interp.HandlerContext, p string) (os.FileInfo, error) {
	return os.Stat(abs(ctx, hc, p))
}

func jqReadFile(ctx context.Context, hc interp.HandlerContext, p string) ([]byte, error) {
	return readFileAs(ctx, hc.Dir, p)
}

// jqWriteFile replaces the content of an existing file, keeping its mode.
func jqWriteFile(ctx context.Context, hc interp.HandlerContext, p string, data []byte) error {
	st, err := jqStat(ctx, hc, p)
	if err != nil {
		return err
	}
	return writeFileAs(ctx, hc, p, data, st.Mode().Perm())
}

func envList(hc interp.HandlerContext) []string {
	var out []string
	hc.Env.Each(func(name string, vr expand.Variable) bool {
		if vr.Exported && vr.Kind == expand.String {
			out = append(out, name+"="+vr.Str)
		}
		return true
	})
	return out
}

// normJSON converts decoded JSON (UseNumber) to the types gojq accepts.
func normJSON(v any) any {
	switch v := v.(type) {
	case json.Number:
		s := string(v)
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return int(i)
		}
		if !strings.ContainsAny(s, ".eE") {
			if b, ok := new(big.Int).SetString(s, 10); ok {
				return b
			}
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	case []any:
		for i := range v {
			v[i] = normJSON(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = normJSON(v[k])
		}
	}
	return v
}

// normYAML converts a value decoded by yaml.v3 to the types gojq accepts.
func normYAML(v any) any {
	switch v := v.(type) {
	case int64:
		return int(v)
	case uint64:
		if v > 1<<62 {
			return new(big.Int).SetUint64(v)
		}
		return int(v)
	case uint:
		return int(v)
	case float32:
		return float64(v)
	case time.Time:
		return v.Format(time.RFC3339)
	case []byte:
		return string(v)
	case []any:
		for i := range v {
			v[i] = normYAML(v[i])
		}
	case map[string]any:
		for k := range v {
			v[k] = normYAML(v[k])
		}
	case map[any]any:
		m := make(map[string]any, len(v))
		for k, x := range v {
			m[fmt.Sprint(k)] = normYAML(x)
		}
		return m
	}
	return v
}

// encodeJSON writes v like jq: indent "" is compact, keys are sorted.
func encodeJSON(b *bytes.Buffer, v any, indent string, level int) {
	nl := func(l int) {
		if indent != "" {
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(indent, l))
		}
	}
	switch v := v.(type) {
	case []any:
		if len(v) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteByte('[')
		for i, x := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(level + 1)
			encodeJSON(b, x, indent, level+1)
		}
		nl(level)
		b.WriteByte(']')
	case map[string]any:
		if len(v) == 0 {
			b.WriteString("{}")
			return
		}
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			nl(level + 1)
			kb, _ := gojq.Marshal(k)
			b.Write(kb)
			b.WriteByte(':')
			if indent != "" {
				b.WriteByte(' ')
			}
			encodeJSON(b, v[k], indent, level+1)
		}
		nl(level)
		b.WriteByte('}')
	default:
		sb, _ := gojq.Marshal(v)
		b.Write(sb)
	}
}

// countReader counts the newlines read, for "(at <stdin>:N)".
type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += bytes.Count(p[:n], []byte{'\n'})
	return n, err
}

// jsonIter yields the JSON values of a stream; a decode error is returned as
// an error value, then the iteration ends.
type jsonIter struct {
	dec  *json.Decoder
	done bool
}

func newJSONIter(r io.Reader) *jsonIter {
	d := json.NewDecoder(r)
	d.UseNumber()
	return &jsonIter{dec: d}
}

func (j *jsonIter) Next() (any, bool) {
	if j.done {
		return nil, false
	}
	var v any
	if err := j.dec.Decode(&v); err != nil {
		j.done = true
		if err == io.EOF {
			return nil, false
		}
		return err, true
	}
	return normJSON(v), true
}

// lineIter yields the lines of r as strings (jq -R).
type lineIter struct {
	r    *bufio.Reader
	done bool
}

func (l *lineIter) Next() (any, bool) {
	if l.done {
		return nil, false
	}
	s, err := l.r.ReadString('\n')
	if err != nil {
		l.done = true
		if s == "" {
			return nil, false
		}
	}
	return strings.TrimSuffix(s, "\n"), true
}

func collect(it gojq.Iter) ([]any, error) {
	out := []any{}
	for {
		v, ok := it.Next()
		if !ok {
			return out, nil
		}
		if err, ok := v.(error); ok {
			return nil, err
		}
		out = append(out, v)
	}
}

func jqCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var (
		filter, from           string
		haveFilter             bool
		files                  []string
		raw, join, null        bool
		slurp, rawIn, exitStat bool
		ascii                  bool
		indent                 = "  "
		mode                   string
		positional             []any
		named                  = map[string]any{}
		names                  []string
		vals                   []any
	)
	need := func(i, n int, flag string) error {
		if i+n >= len(args) {
			return fail(hc, 2, "jq: error: %s takes %d parameters", flag, n)
		}
		return nil
	}
	readJSONFile := func(flag, name, file string, rawText bool) (any, error) {
		b, err := jqReadFile(ctx, hc, file)
		if err != nil {
			return nil, fail(hc, 2, "jq: error: Bad JSON in %s %s %s: Could not open %s: %v", flag, name, file, file, unwrap(err))
		}
		if rawText {
			return string(b), nil
		}
		v, err := collect(newJSONIter(bytes.NewReader(b)))
		if err != nil {
			return nil, fail(hc, 2, "jq: error: Bad JSON in %s %s %s: %v", flag, name, file, err)
		}
		return v, nil
	}
	setVar := func(n string, v any) {
		names = append(names, "$"+n)
		vals = append(vals, v)
		named[n] = v
	}
	rest := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if rest || a == "-" || !strings.HasPrefix(a, "-") {
			switch {
			case !haveFilter && from == "":
				filter, haveFilter = a, true
			case mode == "args":
				positional = append(positional, a)
			case mode == "jsonargs":
				v, err := collect(newJSONIter(strings.NewReader(a)))
				if err != nil || len(v) != 1 {
					return fail(hc, 2, "jq: Invalid JSON text passed to --jsonargs")
				}
				positional = append(positional, v[0])
			default:
				files = append(files, a)
			}
			continue
		}
		if a == "--" {
			rest = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			switch a[2:] {
			case "raw-output":
				raw = true
			case "join-output":
				raw, join = true, true
			case "compact-output":
				indent = ""
			case "null-input":
				null = true
			case "slurp":
				slurp = true
			case "raw-input":
				rawIn = true
			case "exit-status":
				exitStat = true
			case "sort-keys", "monochrome-output", "color-output", "unbuffered":
			case "ascii-output":
				ascii = true
			case "tab":
				indent = "\t"
			case "indent":
				if err := need(i, 1, "--indent"); err != nil {
					return err
				}
				n, err := strconv.Atoi(args[i+1])
				if err != nil || n < 0 || n > 7 {
					return fail(hc, 2, "jq: Cannot indent more than 7 characters")
				}
				indent = strings.Repeat(" ", n)
				i++
			case "arg", "argjson", "slurpfile", "rawfile":
				flag := a
				if err := need(i, 2, flag); err != nil {
					return err
				}
				n, v := args[i+1], args[i+2]
				i += 2
				switch flag {
				case "--arg":
					setVar(n, v)
				case "--argjson":
					j, err := collect(newJSONIter(strings.NewReader(v)))
					if err != nil || len(j) != 1 {
						return fail(hc, 2, "jq: Invalid JSON text passed to --argjson")
					}
					setVar(n, j[0])
				default:
					x, err := readJSONFile(flag, n, v, flag == "--rawfile")
					if err != nil {
						return err
					}
					setVar(n, x)
				}
			case "args":
				mode = "args"
			case "jsonargs":
				mode = "jsonargs"
			case "from-file":
				if err := need(i, 1, "--from-file"); err != nil {
					return err
				}
				from = args[i+1]
				i++
			default:
				return fail(hc, 2, "jq: Unknown option: %s", a)
			}
			continue
		}
		for _, c := range a[1:] {
			switch c {
			case 'r':
				raw = true
			case 'j':
				raw, join = true, true
			case 'c':
				indent = ""
			case 'n':
				null = true
			case 's':
				slurp = true
			case 'R':
				rawIn = true
			case 'e':
				exitStat = true
			case 'S', 'M', 'C':
			case 'a':
				ascii = true
			case 'f':
				if err := need(i, 1, "-f"); err != nil {
					return err
				}
				from = args[i+1]
				i++
			default:
				return fail(hc, 2, "jq: Unknown option: %s", a)
			}
		}
	}
	if from != "" {
		b, err := jqReadFile(ctx, hc, from)
		if err != nil {
			return fail(hc, 2, "jq: error: Could not open %s: %v", from, unwrap(err))
		}
		if haveFilter { // jq: the first operand is then a file
			files = slices.Insert(files, 0, filter)
		}
		filter = string(b)
	} else if !haveFilter {
		filter = "."
	}
	if positional == nil {
		positional = []any{}
	}
	setVar("ARGS", nil)
	vals[len(vals)-1] = map[string]any{"positional": positional, "named": named}
	delete(named, "ARGS")

	q, err := gojq.Parse(filter)
	if err != nil {
		return jqCompileError(hc, err, filter)
	}

	// the input stream: stdin or the files, one after the other
	ret := 0
	var src io.Reader = hc.Stdin
	if src == nil {
		src = strings.NewReader("")
	}
	if len(files) > 0 {
		var rs []io.Reader
		for _, f := range files {
			fh, err := jqOpen(ctx, hc, f)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "jq: error: Could not open %s: %v\n", f, unwrap(err))
				ret = 2
				continue
			}
			defer fh.Close()
			rs = append(rs, fh, strings.NewReader("\n"))
		}
		src = io.MultiReader(rs...)
	}
	cr := &countReader{r: src}
	var in gojq.Iter
	if rawIn {
		in = &lineIter{r: bufio.NewReader(cr)}
		if slurp {
			b, _ := io.ReadAll(cr)
			in = gojq.NewIter[any](string(b))
		}
	} else {
		in = newJSONIter(cr)
		if slurp {
			all, err := collect(in)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "jq: error (at <stdin>:%d): %v\n", cr.n, err)
				return interp.ExitStatus(2)
			}
			in = gojq.NewIter[any](all)
		}
	}
	// "input"/"inputs" read from the same stream as the main loop
	code, err := compileJQ(q, hc, names, nil, in)
	if err != nil {
		return jqCompileError(hc, err, filter)
	}

	var last any
	var any_ bool
	run := func(v any) bool {
		it := code.RunWithContext(ctx, v, vals...)
		for {
			x, ok := it.Next()
			if !ok {
				return true
			}
			if err, ok := x.(error); ok {
				var he *gojq.HaltError
				if errors.As(err, &he) {
					if he.Value() == nil {
						ret = he.ExitCode()
						return false
					}
					if s, ok := he.Value().(string); ok {
						fmt.Fprint(hc.Stderr, s)
					} else {
						var b bytes.Buffer
						encodeJSON(&b, he.Value(), "", 0)
						fmt.Fprintln(hc.Stderr, b.String())
					}
					ret = he.ExitCode()
					return false
				}
				fmt.Fprintf(hc.Stderr, "jq: error (at <stdin>:%d): %v\n", cr.n, err)
				ret = 5
				return true
			}
			any_, last = true, x
			var b bytes.Buffer
			if s, ok := x.(string); ok && raw {
				b.WriteString(s)
			} else {
				encodeJSON(&b, x, indent, 0)
			}
			out := b.Bytes()
			if ascii {
				out = asciiOnly(out)
			}
			hc.Stdout.Write(out)
			if !join {
				fmt.Fprintln(hc.Stdout)
			}
		}
	}
	if null {
		run(nil)
	} else {
		for ctx.Err() == nil {
			v, ok := in.Next()
			if !ok {
				break
			}
			if err, ok := v.(error); ok {
				fmt.Fprintf(hc.Stderr, "jq: error (at <stdin>:%d): %v\n", cr.n, err)
				return interp.ExitStatus(2)
			}
			if !run(v) {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return interp.ExitStatus(130)
	}
	if ret == 0 && exitStat {
		if !any_ {
			ret = 4
		} else if last == nil || last == false {
			ret = 1
		}
	}
	return exitCode(ret)
}

func jqCompileError(hc interp.HandlerContext, err error, filter string) error {
	fmt.Fprintf(hc.Stderr, "jq: error: %v at <top-level>, line 1:\n%s\njq: 1 compile error\n", err, filter)
	return interp.ExitStatus(3)
}

func asciiOnly(b []byte) []byte {
	var o bytes.Buffer
	for _, r := range string(b) {
		switch {
		case r < 0x80:
			o.WriteRune(r)
		case r >= 0x10000:
			r -= 0x10000
			fmt.Fprintf(&o, "\\u%04x\\u%04x", 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&o, "\\u%04x", r)
		}
	}
	return o.Bytes()
}

func compileJQ(q *gojq.Query, hc interp.HandlerContext, names []string, extra []gojq.CompilerOption, in gojq.Iter) (*gojq.Code, error) {
	o := append([]gojq.CompilerOption{
		gojq.WithVariables(names),
		gojq.WithEnvironLoader(func() []string { return envList(hc) }),
	}, extra...)
	if in != nil {
		o = append(o, gojq.WithInputIter(in))
	}
	return gojq.Compile(q, o...)
}

var yqEnvArg = regexp.MustCompile(`\b(strenv|env)\(\s*([A-Za-z_][A-Za-z0-9_]*)\s*\)`)

// yqCmd: see the comment at the top. eval-all (ea) is the same as eval: every
// document of every file is evaluated on its own and the results are printed in
// order, because gojq has no way to merge several documents into one `.`.
func yqCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var (
		pos                     []string
		inplace, null, exitStat bool
		unwrapStr               = true
		outFmt                  = "yaml"
		indent                  = 2
		rest                    bool
		sub                     string
	)
	value := func(i *int, a string, k int) (string, error) {
		if j := strings.IndexByte(a, '='); j >= 0 {
			return a[j+1:], nil
		}
		if len(a) > k {
			return strings.TrimPrefix(a[k:], "="), nil
		}
		if *i+1 >= len(args) {
			return "", fmt.Errorf("flag needs an argument: %s", a)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if rest || a == "-" || !strings.HasPrefix(a, "-") {
			if len(pos) == 0 && sub == "" && slices.Contains([]string{"eval", "e", "eval-all", "ea"}, a) {
				sub = a
				continue
			}
			pos = append(pos, a)
			continue
		}
		if a == "--" {
			rest = true
			continue
		}
		var err error
		if strings.HasPrefix(a, "--") {
			k, _, _ := strings.Cut(a[2:], "=")
			switch k {
			case "inplace":
				inplace = true
			case "output-format":
				outFmt, err = value(&i, a, 0)
			case "input-format":
				_, err = value(&i, a, 0)
			case "indent":
				var s string
				if s, err = value(&i, a, 0); err == nil {
					indent, err = strconv.Atoi(s)
				}
			case "prettyPrint", "colors", "no-colors", "no-doc":
			case "unwrapScalar":
				unwrapStr = !strings.Contains(a, "=false")
			case "null-input":
				null = true
			case "exit-status":
				exitStat = true
			default:
				err = fmt.Errorf("unknown flag: %s", a)
			}
		} else {
			switch a[1] {
			case 'i':
				inplace = true
			case 'o':
				outFmt, err = value(&i, a, 2)
			case 'p':
				_, err = value(&i, a, 2)
			case 'I':
				var s string
				if s, err = value(&i, a, 2); err == nil {
					indent, err = strconv.Atoi(s)
				}
			case 'r':
				unwrapStr = !strings.Contains(a, "=false")
			case 'n':
				null = true
			case 'e':
				exitStat = true
			case 'P', 'M', 'C':
			default:
				err = fmt.Errorf("unknown shorthand flag: %s", a)
			}
		}
		if err != nil {
			return fail(hc, 1, "Error: %v", err)
		}
	}
	asJSON := false
	switch outFmt {
	case "yaml", "y":
	case "json", "j":
		asJSON = true
	default:
		return fail(hc, 1, "Error: unknown output format %q (yaml or json)", outFmt)
	}
	expr := "."
	var files []string
	if len(pos) > 0 {
		expr, files = pos[0], pos[1:]
		if len(pos) == 1 && !null {
			if st, err := jqStat(ctx, hc, pos[0]); err == nil && !st.IsDir() {
				expr, files = ".", pos
			}
		}
	}
	if inplace && (len(files) == 0 || files[0] == "-") {
		return fail(hc, 1, "Error: write in place flag specified, but no files were given")
	}
	// yq writes strenv(NAME) and env(NAME) with a bare name
	expr = yqEnvArg.ReplaceAllString(expr, `$1("$2")`)
	q, err := gojq.Parse(expr)
	var code *gojq.Code
	if err == nil {
		extra := []gojq.CompilerOption{
			gojq.WithFunction("strenv", 1, 1, func(_ any, a []any) any {
				n, _ := a[0].(string)
				v, ok := hc.Env.Get(n).Str, hc.Env.Get(n).IsSet()
				if !ok {
					return fmt.Errorf("variable %s not set", n)
				}
				return v
			}),
			gojq.WithFunction("env", 1, 1, func(_ any, a []any) any {
				n, _ := a[0].(string)
				vr := hc.Env.Get(n)
				if !vr.IsSet() {
					return fmt.Errorf("variable %s not set", n)
				}
				var v any
				if err := yaml.Unmarshal([]byte(vr.Str), &v); err != nil {
					return err
				}
				return normYAML(v)
			}),
		}
		code, err = compileJQ(q, hc, nil, extra, nil)
	}
	if err != nil {
		return fail(hc, 1, "Error: bad expression, please check expression syntax: %v", err)
	}

	ret, n := 0, 0 // n counts the results printed on the shared stdout
	var last any
	any_ := false
	// process evaluates expr on each document of r and prints to w.
	process := func(r io.Reader, w io.Writer, multi bool) error {
		var docs []any
		if null {
			docs = []any{nil}
		} else {
			dec := yaml.NewDecoder(r)
			for {
				var d any
				if err := dec.Decode(&d); err != nil {
					if err == io.EOF {
						break
					}
					return err
				}
				docs = append(docs, normYAML(d))
			}
		}
		for _, d := range docs {
			it := code.RunWithContext(ctx, d)
			for {
				x, ok := it.Next()
				if !ok {
					break
				}
				if err, ok := x.(error); ok {
					return err
				}
				any_, last = true, x
				_, isMap := x.(map[string]any)
				_, isArr := x.([]any)
				if n > 0 && !asJSON && (multi || len(docs) > 1 || isMap || isArr) {
					fmt.Fprintln(w, "---")
				}
				n++
				if err := printYQ(w, x, asJSON, unwrapStr, indent); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var err2 error
	switch {
	case inplace:
		for _, f := range files {
			b, err := jqReadFile(ctx, hc, f)
			if err == nil {
				var out bytes.Buffer
				n = 0
				if err = process(bytes.NewReader(b), &out, false); err == nil {
					err = jqWriteFile(ctx, hc, f, out.Bytes())
				}
			}
			if err != nil {
				err2 = fmt.Errorf("%s: %v", f, unwrap(err))
				break
			}
		}
	case null || len(files) == 0:
		var r io.Reader = hc.Stdin
		if r == nil {
			r = strings.NewReader("")
		}
		err2 = process(r, hc.Stdout, false)
	default:
		for _, f := range files {
			var r io.Reader = hc.Stdin
			if r == nil {
				r = strings.NewReader("")
			}
			if f != "-" {
				fh, err := jqOpen(ctx, hc, f)
				if err != nil {
					err2 = fmt.Errorf("%s: %v", f, unwrap(err))
					break
				}
				defer fh.Close()
				r = fh
			}
			if err2 = process(r, hc.Stdout, len(files) > 1); err2 != nil {
				break
			}
		}
	}
	if err2 != nil {
		if ctx.Err() != nil {
			return interp.ExitStatus(130)
		}
		return fail(hc, 1, "Error: %v", err2)
	}
	if exitStat && (!any_ || last == nil || last == false) {
		ret = 1
	}
	return exitCode(ret)
}

// printYQ prints one result as a YAML document or as JSON.
func printYQ(w io.Writer, v any, asJSON, unwrapStr bool, indent int) error {
	if asJSON {
		var b bytes.Buffer
		encodeJSON(&b, v, strings.Repeat(" ", indent), 0)
		b.WriteByte('\n')
		_, err := w.Write(b.Bytes())
		return err
	}
	if s, ok := v.(string); ok && unwrapStr {
		_, err := fmt.Fprintln(w, s)
		return err
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(indent)
	if err := enc.Encode(yamlValue(v)); err != nil {
		return err
	}
	return enc.Close()
}

// yamlValue makes big integers encodable.
func yamlValue(v any) any {
	switch v := v.(type) {
	case *big.Int:
		if v.IsUint64() {
			return v.Uint64()
		}
		f, _ := new(big.Float).SetInt(v).Float64()
		return f
	case []any:
		o := make([]any, len(v))
		for i, x := range v {
			o[i] = yamlValue(x)
		}
		return o
	case map[string]any:
		o := make(map[string]any, len(v))
		for k, x := range v {
			o[k] = yamlValue(x)
		}
		return o
	}
	return v
}
