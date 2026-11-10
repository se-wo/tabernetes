package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"mvdan.cc/sh/v3/interp"
)

func init() { coreutils["sed"] = sedCmd }

// sed: a GNU sed subset. Options -n -e -f -i[SUFFIX] -E -r -s (--posix and
// friends are ignored). Addresses N $ /re/ \%re% /re/I first~step a,b a,+N
// a,~N 0,/re/ and !. Commands { } s y d D p P n N g G h H x a i c q Q = l :
// b t T r R w W z #. The regex is BRE (or ERE with -E) translated to RE2 with
// leftmost-longest matching; back-references in the pattern are not supported.

// Every file sed opens goes through these three helpers (one place for checks).
func sedReadFile(ctx context.Context, hc interp.HandlerContext, name string) ([]byte, error) {
	return os.ReadFile(abs(ctx, hc, name))
}

func sedWriteFile(ctx context.Context, hc interp.HandlerContext, name string, data []byte, mode os.FileMode) error {
	return os.WriteFile(abs(ctx, hc, name), data, mode)
}

func sedCreate(ctx context.Context, hc interp.HandlerContext, name string) (io.WriteCloser, error) {
	return os.Create(abs(ctx, hc, name))
}

type sedErr string

type sedAddr struct {
	kind    byte // n line, $ last, r regex, s first~step, 0 zero, + plus, ~ multiple
	n, step int
	re      *regexp.Regexp // nil with kind r: the last regex
}

type sedPart struct {
	kind byte // l literal, g group, c case op
	s    string
	n    int
}

type sedSink struct {
	w       io.Writer
	c       io.Closer
	missing bool
}

// put writes a line; nonl leaves the newline out (input had none).
func (s *sedSink) put(text string, nonl bool) {
	s.raw(text)
	if nonl {
		s.missing = true
	} else {
		io.WriteString(s.w, "\n")
	}
}

func (s *sedSink) raw(text string) {
	if s.missing {
		io.WriteString(s.w, "\n")
		s.missing = false
	}
	io.WriteString(s.w, text)
}

type sedCommand struct {
	a1, a2   *sedAddr
	neg      bool
	name     byte
	text     string // a i c text, label, file name
	re       *regexp.Regexp
	repl     []sedPart
	global   bool
	print    bool
	nth      int
	w        *sedSink
	n        int
	target   int
	ymap     map[rune]rune
	active   bool
	end      int
	rlines   []string
	rloaded  bool
	rindex   int
	hasLabel bool
}

type sedProg struct {
	cmds  []*sedCommand
	quiet bool
}

type sedParser struct {
	ctx    context.Context
	hc     interp.HandlerContext
	src    string
	pos    int
	ere    bool
	stdout *sedSink
	files  map[string]*sedSink
	prog   *sedProg
	labels map[string]int
	stack  []int
}

func (p *sedParser) die(format string, a ...any) {
	panic(sedErr(fmt.Sprintf("-e expression #1, char %d: ", p.pos) + fmt.Sprintf(format, a...)))
}

func (p *sedParser) peek() byte {
	if p.pos < len(p.src) {
		return p.src[p.pos]
	}
	return 0
}

func (p *sedParser) skip(set string) {
	for p.pos < len(p.src) && strings.IndexByte(set, p.src[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *sedParser) num() int {
	s := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	n, _ := strconv.Atoi(p.src[s:p.pos])
	return n
}

// delim reads up to the unescaped delimiter d (consumed). \d becomes d.
func (p *sedParser) delim(d byte, what string) string {
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '\\' && p.pos+1 < len(p.src) {
			if p.src[p.pos+1] == d {
				b.WriteByte(d)
			} else {
				b.WriteByte(c)
				b.WriteByte(p.src[p.pos+1])
			}
			p.pos += 2
			continue
		}
		p.pos++
		if c == d {
			return b.String()
		}
		b.WriteByte(c)
	}
	p.die("unterminated %s", what)
	return ""
}

func (p *sedParser) regex(pat string, icase, multi bool) *regexp.Regexp {
	if pat == "" {
		return nil
	}
	t, err := sedTranslate(pat, p.ere)
	if err != nil {
		p.die("%v", err)
	}
	fl := "s"
	if icase {
		fl += "i"
	}
	if multi {
		fl += "m"
	}
	re, err := regexp.Compile("(?" + fl + ")" + t)
	if err != nil {
		p.die("invalid regular expression: %v", err)
	}
	re.Longest()
	return re
}

func (p *sedParser) addr(second bool) *sedAddr {
	c := p.peek()
	switch {
	case c >= '0' && c <= '9':
		n := p.num()
		if !second && p.peek() == '~' {
			p.pos++
			return &sedAddr{kind: 's', n: n, step: p.num()}
		}
		if n == 0 && !second {
			return &sedAddr{kind: '0'}
		}
		return &sedAddr{kind: 'n', n: n}
	case c == '$':
		p.pos++
		return &sedAddr{kind: '$'}
	case c == '/' || c == '\\':
		d := byte('/')
		if c == '\\' {
			p.pos++
			d = p.peek()
		}
		p.pos++
		pat := p.delim(d, "address regex")
		var ic, ml bool
		for p.peek() == 'I' || p.peek() == 'M' {
			ic = ic || p.peek() == 'I'
			ml = ml || p.peek() == 'M'
			p.pos++
		}
		return &sedAddr{kind: 'r', re: p.regex(pat, ic, ml)}
	case second && (c == '+' || c == '~'):
		p.pos++
		return &sedAddr{kind: c, n: p.num()}
	}
	return nil
}

// endCmd checks that a command ends here.
func (p *sedParser) endCmd() {
	p.skip(" \t")
	switch p.peek() {
	case 0, ';', '\n', '}', '#':
		return
	}
	p.die("extra characters after command")
}

func (p *sedParser) rest() string {
	s := p.pos
	for p.pos < len(p.src) && p.src[p.pos] != '\n' {
		p.pos++
	}
	return p.src[s:p.pos]
}

func (p *sedParser) label() string {
	p.skip(" \t")
	s := p.pos
	for p.pos < len(p.src) && !strings.ContainsRune(";\n} \t", rune(p.src[p.pos])) {
		p.pos++
	}
	return p.src[s:p.pos]
}

func (p *sedParser) sink(name string) *sedSink {
	switch name {
	case "":
		p.die("missing filename in r/R/w/W commands")
	case "/dev/stdout":
		return p.stdout
	case "/dev/stderr":
		return &sedSink{w: p.hc.Stderr}
	}
	if s := p.files[name]; s != nil {
		return s
	}
	f, err := sedCreate(p.ctx, p.hc, name)
	if err != nil {
		p.die("couldn't open file %s: %v", name, unwrap(err))
	}
	s := &sedSink{w: f, c: f}
	p.files[name] = s
	return s
}

// text reads the argument of a, i and c.
func (p *sedParser) text() string {
	p.skip(" \t")
	if p.peek() == '\\' {
		p.pos++
		save := p.pos
		p.skip(" \t")
		if p.peek() == '\n' {
			p.pos++
		} else {
			p.pos = save
		}
	} else if p.peek() == 0 || p.peek() == '\n' {
		p.die("expected \\ after `a', `c' or `i'")
	}
	var b strings.Builder
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		p.pos++
		if c == '\n' {
			break
		}
		if c == '\\' && p.pos < len(p.src) {
			c = p.src[p.pos]
			p.pos++
			if c == 't' {
				c = '\t'
			}
		}
		b.WriteByte(c)
	}
	return b.String() + "\n"
}

func sedParse(ctx context.Context, hc interp.HandlerContext, src string, ere, quiet bool, stdout *sedSink, files map[string]*sedSink) (prog *sedProg, err error) {
	defer func() {
		if r := recover(); r != nil {
			e, ok := r.(sedErr)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("%s", string(e))
		}
	}()
	p := &sedParser{ctx: ctx, hc: hc, src: src, ere: ere, stdout: stdout, files: files, prog: &sedProg{quiet: quiet}, labels: map[string]int{}}
	if src == "#n" || strings.HasPrefix(src, "#n\n") {
		p.prog.quiet = true
	}
	p.parse()
	return p.prog, nil
}

func (p *sedParser) parse() {
	for {
		p.skip(" \t\n;")
		if p.pos >= len(p.src) {
			break
		}
		if p.peek() == '#' {
			p.rest()
			continue
		}
		c := &sedCommand{nth: 1}
		c.a1 = p.addr(false)
		if c.a1 != nil {
			p.skip(" \t")
			if p.peek() == ',' {
				p.pos++
				p.skip(" \t")
				if c.a2 = p.addr(true); c.a2 == nil {
					p.die("unexpected `,'")
				}
			}
			if c.a1.kind == '0' && (c.a2 == nil || c.a2.kind != 'r') {
				p.die("invalid usage of line address 0")
			}
		}
		p.skip(" \t")
		for p.peek() == '!' {
			c.neg = true
			p.pos++
			p.skip(" \t")
		}
		if p.pos >= len(p.src) {
			p.die("missing command")
		}
		c.name = p.src[p.pos]
		p.pos++
		switch c.name {
		case '{':
			p.stack = append(p.stack, len(p.prog.cmds))
		case '}':
			if len(p.stack) == 0 {
				p.die("unexpected `}'")
			}
			p.prog.cmds[p.stack[len(p.stack)-1]].target = len(p.prog.cmds)
			p.stack = p.stack[:len(p.stack)-1]
			p.endCmd()
		case 'd', 'D', 'p', 'P', 'n', 'N', 'g', 'G', 'h', 'H', 'x', 'z', '=':
			p.endCmd()
		case 'l', 'q', 'Q', 'L':
			p.skip(" \t")
			c.n = p.num()
			p.endCmd()
		case 'a', 'i', 'c':
			c.text = p.text()
		case ':':
			if c.a1 != nil {
				p.die(": doesn't want any addresses")
			}
			c.text = p.label()
			if c.text == "" {
				p.die("\":\" lacks a label")
			}
			p.labels[c.text] = len(p.prog.cmds)
		case 'b', 't', 'T':
			c.text = p.label()
			c.hasLabel = c.text != ""
		case 'r', 'R', 'w', 'W':
			p.skip(" \t")
			c.text = p.rest()
			if c.name == 'w' || c.name == 'W' {
				c.w = p.sink(c.text)
			} else if c.text == "" {
				p.die("missing filename in r/R/w/W commands")
			}
		case 'y':
			p.pos++
			d := p.src[p.pos-1]
			from := sedYStr(p.delim(d, "`y' command"))
			to := sedYStr(p.delim(d, "`y' command"))
			if len([]rune(from)) != len([]rune(to)) {
				p.die("strings for `y' command are different lengths")
			}
			c.ymap = map[rune]rune{}
			tr := []rune(to)
			for i, r := range []rune(from) {
				c.ymap[r] = tr[i]
			}
			p.endCmd()
		case 's':
			p.parseS(c)
		default:
			p.die("unknown command: `%c'", c.name)
		}
		p.prog.cmds = append(p.prog.cmds, c)
	}
	if len(p.stack) > 0 {
		p.die("unmatched `{'")
	}
	for _, c := range p.prog.cmds {
		if (c.name == 'b' || c.name == 't' || c.name == 'T') && c.hasLabel {
			t, ok := p.labels[c.text]
			if !ok {
				p.die("can't find label for jump to `%s'", c.text)
			}
			c.target = t
		} else if c.name == 'b' || c.name == 't' || c.name == 'T' {
			c.target = -1
		}
	}
}

func sedYStr(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			switch s[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(s[i])
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (p *sedParser) parseS(c *sedCommand) {
	if p.pos >= len(p.src) {
		p.die("unterminated `s' command")
	}
	d := p.src[p.pos]
	p.pos++
	pat := p.delim(d, "`s' command")
	rep := p.delim(d, "`s' command")
	var ic, ml bool
flags:
	for p.pos < len(p.src) {
		switch ch := p.src[p.pos]; {
		case ch == 'g':
			c.global = true
		case ch == 'p':
			c.print = true
		case ch == 'i' || ch == 'I':
			ic = true
		case ch == 'm' || ch == 'M':
			ml = true
		case ch == 'e':
		case ch >= '0' && ch <= '9':
			c.nth = p.num()
			if c.nth == 0 {
				p.die("number option to `s' command may not be zero")
			}
			continue
		case ch == 'w':
			p.pos++
			p.skip(" \t")
			c.w = p.sink(p.rest())
			break flags
		default:
			break flags
		}
		p.pos++
	}
	p.endCmd()
	c.re = p.regex(pat, ic, ml)
	c.repl = sedRepl(rep)
	if c.re != nil {
		for _, pt := range c.repl {
			if pt.kind == 'g' && pt.n > c.re.NumSubexp() {
				p.die("invalid reference \\%d on `s' command's RHS", pt.n)
			}
		}
	}
}

func sedRepl(s string) []sedPart {
	var parts []sedPart
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			parts = append(parts, sedPart{kind: 'l', s: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '&':
			flush()
			parts = append(parts, sedPart{kind: 'g'})
		case c == '\\' && i+1 < len(s):
			i++
			d := s[i]
			switch {
			case d >= '0' && d <= '9':
				flush()
				parts = append(parts, sedPart{kind: 'g', n: int(d - '0')})
			case d == 'n':
				lit.WriteByte('\n')
			case d == 't':
				lit.WriteByte('\t')
			case d == 'L' || d == 'U' || d == 'l' || d == 'u' || d == 'E':
				flush()
				parts = append(parts, sedPart{kind: 'c', s: string(d)})
			default:
				lit.WriteByte(d)
			}
		default:
			lit.WriteByte(c)
		}
	}
	flush()
	return parts
}

// sedTranslate turns a POSIX BRE (or ERE) into RE2 syntax.
func sedTranslate(p string, ere bool) (string, error) {
	var b strings.Builder
	atStart := true
	for i := 0; i < len(p); {
		c := p[i]
		switch {
		case c == '[':
			j, err := sedBracket(&b, p, i)
			if err != nil {
				return "", err
			}
			i, atStart = j, false
			continue
		case c == '\\':
			if i+1 >= len(p) {
				return "", fmt.Errorf("trailing backslash (\\)")
			}
			d := p[i+1]
			i += 2
			atStart = false
			switch {
			case !ere && d == '(':
				b.WriteByte('(')
				atStart = true
			case !ere && d == '|':
				b.WriteByte('|')
				atStart = true
			case !ere && (d == ')' || d == '{' || d == '}' || d == '+' || d == '?'):
				b.WriteByte(d)
			case d == 'n' || d == 't' || d == 'r' || d == 'f' || d == 'v' || d == 'w' || d == 'W' || d == 's' || d == 'S' || d == 'b' || d == 'B':
				b.WriteByte('\\')
				b.WriteByte(d)
			case d == '<' || d == '>':
				b.WriteString(`\b`)
			case d == '`':
				b.WriteString(`\A`)
			case d == '\'':
				b.WriteString(`\z`)
			case d >= '1' && d <= '9':
				return "", fmt.Errorf("back-references in the regex are not supported")
			default:
				b.WriteString(regexp.QuoteMeta(string(d)))
			}
			continue
		case c == '*':
			if atStart {
				b.WriteString(`\*`)
			} else {
				b.WriteByte('*')
			}
			atStart = false
		case c == '^':
			if ere || atStart {
				b.WriteByte('^')
			} else {
				b.WriteString(`\^`)
			}
		case c == '$':
			rest := p[i+1:]
			if ere || rest == "" || strings.HasPrefix(rest, `\)`) || strings.HasPrefix(rest, `\|`) {
				b.WriteByte('$')
			} else {
				b.WriteString(`\$`)
			}
			atStart = false
		case !ere && strings.IndexByte("(){}|+?", c) >= 0:
			b.WriteByte('\\')
			b.WriteByte(c)
			atStart = false
		case ere && (c == '(' || c == '|'):
			b.WriteByte(c)
			atStart = true
		default:
			b.WriteByte(c)
			atStart = false
		}
		i++
	}
	return b.String(), nil
}

func sedBracket(b *strings.Builder, p string, i int) (int, error) {
	b.WriteByte('[')
	i++
	if i < len(p) && p[i] == '^' {
		b.WriteByte('^')
		i++
	}
	for first := true; i < len(p); i, first = i+1, false {
		c := p[i]
		switch {
		case c == ']' && !first:
			b.WriteByte(']')
			return i + 1, nil
		case c == ']':
			b.WriteString(`\]`)
		case c == '[' && i+1 < len(p) && strings.IndexByte(":.=", p[i+1]) >= 0:
			end := strings.Index(p[i+2:], string(p[i+1])+"]")
			if end < 0 {
				return 0, fmt.Errorf("unterminated address regex")
			}
			if p[i+1] == ':' {
				b.WriteString(p[i : i+end+4])
			} else {
				b.WriteString(regexp.QuoteMeta(p[i+2 : i+2+end]))
			}
			i += end + 3
		case c == '[':
			b.WriteString(`\[`)
		case c == '\\':
			if i+1 < len(p) && (p[i+1] == 'n' || p[i+1] == 't') {
				b.WriteByte('\\')
				b.WriteByte(p[i+1])
				i++
			} else {
				b.WriteString(`\\`)
			}
		case c == '^':
			b.WriteString(`\^`)
		default:
			b.WriteByte(c)
		}
	}
	return 0, fmt.Errorf("unterminated address regex")
}

type sedLine struct {
	s    string
	nonl bool
}

type sedAppend struct {
	text string
	file string
}

type sedEngine struct {
	ctx      context.Context
	hc       interp.HandlerContext
	prog     *sedProg
	lines    []sedLine
	idx      int
	ps, hs   string
	nonl     bool
	lineNo   int
	out      *sedSink
	queue    []sedAppend
	last     *regexp.Regexp
	replaced bool
	quit     bool
	code     int
}

func (e *sedEngine) hasNext() bool { return e.idx < len(e.lines) }

func (e *sedEngine) read(appendMode bool) {
	l := e.lines[e.idx]
	e.idx++
	e.lineNo++
	if appendMode {
		e.ps += "\n" + l.s
	} else {
		e.ps = l.s
	}
	e.nonl = l.nonl
}

func (e *sedEngine) flush() {
	for _, a := range e.queue {
		if a.file == "" {
			e.out.raw(a.text)
		} else if b, err := sedReadFile(e.ctx, e.hc, a.file); err == nil && len(b) > 0 {
			e.out.raw(string(b))
			if b[len(b)-1] != '\n' {
				e.out.missing = true
			}
		}
	}
	e.queue = e.queue[:0]
}

func (e *sedEngine) regex(re *regexp.Regexp) *regexp.Regexp {
	if re == nil {
		if e.last == nil {
			panic(sedErr("no previous regular expression"))
		}
		return e.last
	}
	e.last = re
	return re
}

func (e *sedEngine) one(a *sedAddr) bool {
	switch a.kind {
	case 'n':
		return e.lineNo == a.n
	case '$':
		return !e.hasNext()
	case 'r':
		return e.regex(a.re).MatchString(e.ps)
	case 's':
		if a.step <= 0 {
			return e.lineNo == a.n
		}
		return e.lineNo >= a.n && (e.lineNo-a.n)%a.step == 0
	}
	return false
}

func (e *sedEngine) match(c *sedCommand) bool {
	if c.a1 == nil {
		return !c.neg
	}
	if c.a2 == nil {
		return e.one(c.a1) != c.neg
	}
	m := false
	if !c.active {
		if e.one(c.a1) {
			m = true
			switch c.a2.kind {
			case 'n':
				c.end = c.a2.n
			case '+':
				c.end = e.lineNo + c.a2.n
			case '~':
				c.end = e.lineNo
				if c.a2.n > 0 && e.lineNo%c.a2.n != 0 {
					c.end = e.lineNo + c.a2.n - e.lineNo%c.a2.n
				}
			default:
				c.end = 0
			}
			if c.a2.kind == 'r' || c.a2.kind == '$' {
				c.active = c.a2.kind == 'r' || e.hasNext()
			} else {
				c.active = c.end > e.lineNo
			}
		}
	} else {
		m = true
		switch c.a2.kind {
		case 'r', '$':
			if e.one(c.a2) {
				c.active = false
			}
		default:
			if e.lineNo >= c.end {
				c.active = false
			}
			m = e.lineNo <= c.end
		}
	}
	return m != c.neg
}

func sedExpand(b *strings.Builder, parts []sedPart, s string, m []int) {
	var mode, once byte
	add := func(t string) {
		if mode == 0 && once == 0 {
			b.WriteString(t)
			return
		}
		for _, r := range t {
			switch {
			case once == 'u':
				r, once = unicode.ToUpper(r), 0
			case once == 'l':
				r, once = unicode.ToLower(r), 0
			case mode == 'U':
				r = unicode.ToUpper(r)
			case mode == 'L':
				r = unicode.ToLower(r)
			}
			b.WriteRune(r)
		}
	}
	for _, pt := range parts {
		switch pt.kind {
		case 'l':
			add(pt.s)
		case 'g':
			if m[2*pt.n] >= 0 {
				add(s[m[2*pt.n]:m[2*pt.n+1]])
			}
		case 'c':
			switch pt.s {
			case "L", "U":
				mode = pt.s[0]
			case "E":
				mode, once = 0, 0
			default:
				once = pt.s[0]
			}
		}
	}
}

func (e *sedEngine) subst(c *sedCommand) bool {
	re := e.regex(c.re)
	ms := re.FindAllStringSubmatchIndex(e.ps, -1)
	var b strings.Builder
	pos, done := 0, false
	for k, m := range ms {
		if k+1 < c.nth {
			continue
		}
		b.WriteString(e.ps[pos:m[0]])
		sedExpand(&b, c.repl, e.ps, m)
		pos, done = m[1], true
		if !c.global {
			break
		}
	}
	if !done {
		return false
	}
	b.WriteString(e.ps[pos:])
	e.ps = b.String()
	return true
}

func sedList(s string) string {
	var b strings.Builder
	col := 0
	for _, r := range []byte(s) {
		var t string
		switch {
		case r == '\\':
			t = `\\`
		case r == '\n':
			t = `\n`
		case r == '\t':
			t = `\t`
		case r == '\r':
			t = `\r`
		case r == '\a':
			t = `\a`
		case r == '\b':
			t = `\b`
		case r == '\f':
			t = `\f`
		case r == '\v':
			t = `\v`
		case r < 32 || r >= 127:
			t = fmt.Sprintf(`\%03o`, r)
		default:
			t = string(r)
		}
		if col+len(t) > 69 {
			b.WriteString("\\\n")
			col = 0
		}
		b.WriteString(t)
		col += len(t)
	}
	b.WriteString("$\n")
	return b.String()
}

// exec runs the script on the pattern space. It reports whether the pattern
// space is printed at the end of the cycle and whether D restarts the cycle.
func (e *sedEngine) exec() (print, again bool) {
	cmds := e.prog.cmds
	for pc := 0; pc < len(cmds); pc++ {
		c := cmds[pc]
		if !e.match(c) {
			if c.name == '{' {
				pc = c.target
			}
			continue
		}
		switch c.name {
		case 's':
			if e.subst(c) {
				e.replaced = true
				if c.print {
					e.out.put(e.ps, e.nonl)
				}
				if c.w != nil {
					c.w.put(e.ps, e.nonl)
				}
			}
		case 'd':
			return false, false
		case 'D':
			i := strings.IndexByte(e.ps, '\n')
			if i < 0 {
				return false, false
			}
			e.ps = e.ps[i+1:]
			return false, true
		case 'p':
			e.out.put(e.ps, e.nonl)
		case 'P', 'W':
			first, nonl := e.ps, e.nonl
			if i := strings.IndexByte(e.ps, '\n'); i >= 0 {
				first, nonl = e.ps[:i], false
			}
			if c.name == 'P' {
				e.out.put(first, nonl)
			} else {
				c.w.put(first, nonl)
			}
		case 'n':
			if !e.hasNext() {
				e.quit = true
				return true, false
			}
			if !e.prog.quiet {
				e.out.put(e.ps, e.nonl)
			}
			e.flush()
			e.read(false)
		case 'N':
			if !e.hasNext() {
				e.quit = true
				return true, false
			}
			e.flush()
			e.read(true)
		case 'g':
			e.ps = e.hs
		case 'G':
			e.ps += "\n" + e.hs
		case 'h':
			e.hs = e.ps
		case 'H':
			e.hs += "\n" + e.ps
		case 'x':
			e.ps, e.hs = e.hs, e.ps
		case 'a':
			e.queue = append(e.queue, sedAppend{text: c.text})
		case 'r':
			e.queue = append(e.queue, sedAppend{file: c.text})
		case 'R':
			if !c.rloaded {
				c.rloaded = true
				if b, err := sedReadFile(e.ctx, e.hc, c.text); err == nil {
					c.rlines = lines(bytes.NewReader(b))
				}
			}
			if c.rindex < len(c.rlines) {
				e.queue = append(e.queue, sedAppend{text: c.rlines[c.rindex] + "\n"})
				c.rindex++
			}
		case 'i':
			e.out.raw(c.text)
		case 'c':
			if c.a2 == nil || !c.active {
				e.out.raw(c.text)
			}
			return false, false
		case 'y':
			e.ps = strings.Map(func(r rune) rune {
				if t, ok := c.ymap[r]; ok {
					return t
				}
				return r
			}, e.ps)
		case 'z':
			e.ps = ""
		case '=':
			e.out.raw(strconv.Itoa(e.lineNo) + "\n")
		case 'l':
			e.out.raw(sedList(e.ps))
		case 'w':
			c.w.put(e.ps, e.nonl)
		case 'q':
			e.quit, e.code = true, c.n
			return true, false
		case 'Q':
			e.quit, e.code = true, c.n
			return false, false
		case 'b', 't', 'T':
			jump := c.name == 'b'
			if c.name == 't' && e.replaced {
				jump, e.replaced = true, false
			} else if c.name == 'T' {
				jump = !e.replaced
				e.replaced = false
			}
			if jump {
				if e.ctx.Err() != nil {
					e.quit = true
					return false, false
				}
				if c.target < 0 {
					return true, false
				}
				pc = c.target
			}
		}
	}
	return true, false
}

func (e *sedEngine) run() {
	for _, c := range e.prog.cmds {
		c.active = c.a1 != nil && c.a1.kind == '0'
	}
	again := false
	for {
		if !again {
			if !e.hasNext() {
				break
			}
			e.read(false)
			e.replaced = false
		}
		var pr bool
		pr, again = e.exec()
		if pr && !e.prog.quiet {
			e.out.put(e.ps, e.nonl)
		}
		e.flush()
		if e.quit || e.ctx.Err() != nil {
			break
		}
	}
}

func sedSplit(data string, last bool) []sedLine {
	if data == "" {
		return nil
	}
	nonl := !strings.HasSuffix(data, "\n")
	ls := strings.Split(strings.TrimSuffix(data, "\n"), "\n")
	out := make([]sedLine, len(ls))
	for i, l := range ls {
		out[i].s = l
	}
	out[len(out)-1].nonl = nonl && last
	return out
}

func sedCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var scripts, files []string
	var quiet, inplace, ere, sep bool
	var suffix string
	haveScript := false
	needArg := func(i *int, v, name string) (string, bool) {
		if v != "" {
			return v, true
		}
		*i++
		if *i >= len(args) {
			fmt.Fprintf(hc.Stderr, "sed: option requires an argument -- '%s'\n", name)
			return "", false
		}
		return args[*i], true
	}
	addFile := func(name string) error {
		var b []byte
		var err error
		if name == "-" {
			if hc.Stdin != nil {
				b, err = io.ReadAll(hc.Stdin)
			}
		} else {
			b, err = sedReadFile(ctx, hc, name)
		}
		if err != nil {
			return fmt.Errorf("couldn't open file %s: %v", name, unwrap(err))
		}
		scripts = append(scripts, strings.TrimSuffix(string(b), "\n"))
		haveScript = true
		return nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			files = append(files, args[i+1:]...)
			break
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			files = append(files, a)
			continue
		}
		if strings.HasPrefix(a, "--") {
			k, v, hasV := strings.Cut(a[2:], "=")
			switch k {
			case "quiet", "silent":
				quiet = true
			case "expression", "file":
				if !hasV {
					v, hasV = needArg(&i, "", k)
					if !hasV {
						return interp.ExitStatus(1)
					}
				}
				if k == "expression" {
					scripts, haveScript = append(scripts, v), true
				} else if err := addFile(v); err != nil {
					return fail(hc, 1, "sed: %v", err)
				}
			case "in-place":
				inplace, suffix = true, v
			case "regexp-extended":
				ere = true
			case "separate":
				sep = true
			case "posix", "debug", "sandbox", "unbuffered", "follow-symlinks", "binary":
			case "version":
				fmt.Fprintln(hc.Stdout, "sed (tabernetes) 4.9")
				return nil
			default:
				return fail(hc, 1, "sed: unrecognized option '--%s'", k)
			}
			continue
		}
		for j := 1; j < len(a); j++ {
			switch a[j] {
			case 'n':
				quiet = true
			case 'E', 'r':
				ere = true
			case 's':
				sep = true
			case 'u', 'b':
			case 'i':
				inplace, suffix = true, a[j+1:]
				j = len(a)
			case 'e', 'f':
				opt := a[j]
				v, ok := needArg(&i, a[j+1:], string(opt))
				if !ok {
					return interp.ExitStatus(1)
				}
				j = len(a)
				if opt == 'e' {
					scripts, haveScript = append(scripts, v), true
				} else if err := addFile(v); err != nil {
					return fail(hc, 1, "sed: %v", err)
				}
			default:
				return fail(hc, 1, "sed: invalid option -- '%c'", a[j])
			}
		}
	}
	if !haveScript {
		if len(files) == 0 {
			return fail(hc, 1, "Usage: sed [OPTION]... {script-only-if-no-other-script} [input-file]...")
		}
		scripts, files = []string{files[0]}, files[1:]
	}
	stdout := &sedSink{w: hc.Stdout}
	wfiles := map[string]*sedSink{}
	defer func() {
		for _, s := range wfiles {
			s.c.Close()
		}
	}()
	prog, err := sedParse(ctx, hc, strings.Join(scripts, "\n"), ere, quiet, stdout, wfiles)
	if err != nil {
		return fail(hc, 1, "sed: %v", err)
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	code := 0
	load := func(names []string) []sedLine {
		var ls []sedLine
		for i, f := range names {
			var b []byte
			var err error
			if f == "-" {
				b = []byte{}
				if hc.Stdin != nil {
					b, err = io.ReadAll(hc.Stdin)
				}
			} else {
				b, err = sedReadFile(ctx, hc, f)
			}
			if err == nil && f != "-" {
				if st, serr := os.Stat(abs(ctx, hc, f)); serr == nil && st.IsDir() {
					fmt.Fprintf(hc.Stderr, "sed: couldn't edit %s: not a regular file\n", f)
					code = 4
					continue
				}
			}
			if err != nil {
				msg := unwrap(err).Error()
				fmt.Fprintf(hc.Stderr, "sed: can't read %s: %s%s\n", f, strings.ToUpper(msg[:1]), msg[1:])
				code = 2
				continue
			}
			ls = append(ls, sedSplit(string(b), i == len(names)-1)...)
		}
		return ls
	}
	runUnit := func(ls []sedLine, out *sedSink) *sedEngine {
		e := &sedEngine{ctx: ctx, hc: hc, prog: prog, lines: ls, out: out}
		func() {
			defer func() {
				if r := recover(); r != nil {
					m, ok := r.(sedErr)
					if !ok {
						panic(r)
					}
					fmt.Fprintf(hc.Stderr, "sed: %s\n", string(m))
					e.quit, e.code = true, 1
				}
			}()
			e.run()
		}()
		return e
	}
	switch {
	case inplace:
		for _, f := range files {
			if f == "-" {
				return fail(hc, 4, "sed: couldn't edit -: not a regular file")
			}
			orig, err := sedReadFile(ctx, hc, f)
			if err != nil {
				msg := unwrap(err).Error()
				fmt.Fprintf(hc.Stderr, "sed: can't read %s: %s%s\n", f, strings.ToUpper(msg[:1]), msg[1:])
				code = 2
				continue
			}
			mode := os.FileMode(0o644)
			if st, err := os.Stat(abs(ctx, hc, f)); err == nil {
				mode = st.Mode().Perm()
			}
			var buf bytes.Buffer
			e := runUnit(sedSplit(string(orig), true), &sedSink{w: &buf})
			if suffix != "" {
				dir, base := filepath.Split(f)
				name := base + suffix
				if strings.Contains(suffix, "*") {
					name = strings.ReplaceAll(suffix, "*", base)
				}
				if !strings.Contains(name, "/") {
					name = dir + name
				}
				if err := sedWriteFile(ctx, hc, name, orig, mode); err != nil {
					return fail(hc, 4, "sed: cannot rename %s: %v", f, unwrap(err))
				}
			}
			if err := sedWriteFile(ctx, hc, f, buf.Bytes(), mode); err != nil {
				return fail(hc, 4, "sed: couldn't open file %s: %v", f, unwrap(err))
			}
			if e.quit {
				if e.code != 0 {
					code = e.code
				}
				break
			}
		}
	case sep:
		for _, f := range files {
			e := runUnit(load([]string{f}), stdout)
			if e.quit {
				if e.code != 0 {
					code = e.code
				}
				break
			}
		}
	default:
		ls := load(files)
		if e := runUnit(ls, stdout); e.code != 0 {
			code = e.code
		}
	}
	return exitCode(code)
}
