package main

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// sh runs script with the coreutils in a temporary directory and returns the
// combined output and the exit status. The tests run under node
// (go_js_wasm_exec), which lacks os.Pipe, so xargs is not covered here.
func sh(t *testing.T, script string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	var self interp.ExecHandlerFunc
	handler := func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		self = func(ctx context.Context, args []string) error {
			if u := coreutils[args[0]]; u != nil {
				return sigpipe(u(ctx, interp.HandlerCtx(ctx), args[1:], self))
			}
			return next(ctx, args)
		}
		return self
	}
	r, err := interp.New(interp.StdIO(strings.NewReader(""), &out, &out), interp.Dir(t.TempDir()), interp.ExecHandlers(handler))
	if err != nil {
		t.Fatal(err)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatal(err)
	}
	err = r.Run(context.Background(), f)
	code := 0
	if st, ok := interp.IsExitStatus(err); ok {
		code = int(st)
	} else if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return out.String(), code
}

func TestCoreutils(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		{"seq 3", "1\n2\n3\n"},
		{"seq 3 | tr -d '\\n'", "123"},
		{"echo Hello | tr '[:lower:]' '[:upper:]'", "HELLO\n"},
		{"echo abcdef | head -c 3", "abc"},
		{"echo abcdef | cut -c 2-4", "bcd\n"},
		{"seq 3 | wc -l", "3\n"},
		{"seq 10 | head -3", "1\n2\n3\n"},
		{"seq 10 | head -n 2", "1\n2\n"},
		{"seq 10 | tail -2", "9\n10\n"},
		{"printf 'b\\na\\nb\\n' | sort | uniq", "a\nb\n"},
		{"printf 'b\\na\\nb\\n' | sort | uniq -c", "      1 a\n      2 b\n"},
		{"printf 'Running\\nPending\\nRunning\\n' | grep -c Running", "2\n"},
		{"printf 'Running\\nPending\\n' | grep -v Running", "Pending\n"},
		{"echo a,b,c | cut -d, -f2", "b\n"},
		{"echo hello | tr a-z A-Z", "HELLO\n"},
		{"echo -n tabernetes | base64", "dGFiZXJuZXRlcw==\n"},
		{"echo dGFiZXJuZXRlcw== | base64 -d", "tabernetes"},
		{"echo hi | tee f >/dev/null; cat f", "hi\n"},
		{"mkdir -p a/b && touch a/b/c && ls a/b", "c\n"},
		{"touch x && rm x && ls", ""},
	} {
		t.Run(tc.script, func(t *testing.T) {
			got, code := sh(t, tc.script)
			if code != 0 || got != tc.want {
				t.Errorf("got %q (exit %d), want %q", got, code, tc.want)
			}
		})
	}
}

func TestCoreutilsErrors(t *testing.T) {
	for _, script := range []string{
		"cat missing",
		"head --nope",
		"wget",
		"wget ftp://example.com",
		"wget -O a -O",
		"wget web",
		"curl",
		"curl -s web",
		"curl --nope x",
		"nslookup web",
	} {
		if out, code := sh(t, script); code == 0 {
			t.Errorf("%s: exit 0, output %q", script, out)
		}
	}
}

func TestOpts(t *testing.T) {
	long := map[string]string{"lines": "n", "verbose": "v"}
	for _, tc := range []struct {
		args []string
		spec string
		want map[string]string
		rest []string
	}{
		{[]string{"-n5", "f"}, "n:v", map[string]string{"n": "5"}, []string{"f"}},
		{[]string{"-n", "5", "f"}, "n:v", map[string]string{"n": "5"}, []string{"f"}},
		{[]string{"-vn5"}, "n:v", map[string]string{"v": "", "n": "5"}, nil},
		{[]string{"--lines=7", "-"}, "n:v", map[string]string{"n": "7"}, []string{"-"}},
		{[]string{"--lines", "7"}, "n:v", map[string]string{"n": "7"}, nil},
		{[]string{"-5", "f"}, "n:#", map[string]string{"#": "5"}, []string{"f"}},
		{[]string{"--", "-v"}, "n:v", map[string]string{}, []string{"-v"}},
	} {
		o, rest, err := opts("t", tc.args, tc.spec, long)
		if err != nil || !maps.Equal(o, tc.want) || !slices.Equal(rest, tc.rest) {
			t.Errorf("opts(%q, %q) = %v, %q, %v; want %v, %q", tc.args, tc.spec, o, rest, err, tc.want, tc.rest)
		}
	}
	for _, args := range [][]string{{"-x"}, {"--nope"}, {"-n"}, {"--lines"}} {
		if _, _, err := opts("t", args, "n:v", long); err == nil {
			t.Errorf("opts(%q): no error", args)
		}
	}
}

func TestSplitWgetArgs(t *testing.T) {
	for _, tc := range []struct{ args, flags, urls []string }{
		{[]string{"https://jkroepke.de", "-v"}, []string{"-v"}, []string{"https://jkroepke.de"}},
		{[]string{"-qO-", "localhost:8080"}, []string{"-qO-"}, []string{"localhost:8080"}},
		{[]string{"localhost:8080", "-qO-"}, []string{"-qO-"}, []string{"localhost:8080"}},
		{[]string{"u", "-O", "-"}, []string{"-O", "-"}, []string{"u"}},
		{[]string{"-O", "x.md", "u"}, []string{"-O", "x.md"}, []string{"u"}},
		{[]string{"--", "-u"}, nil, []string{"-u"}},
		{[]string{"-qO", "/dev/null", "u"}, []string{"-qO", "/dev/null"}, []string{"u"}},
		{[]string{"-qT", "2", "-O-", "u"}, []string{"-qT", "2", "-O-"}, []string{"u"}},
	} {
		flags, urls := splitWgetArgs(tc.args)
		if !slices.Equal(flags, tc.flags) || !slices.Equal(urls, tc.urls) {
			t.Errorf("splitWgetArgs(%q) = %q, %q; want %q, %q", tc.args, flags, urls, tc.flags, tc.urls)
		}
	}
}

func TestWgetURL(t *testing.T) {
	for raw, want := range map[string]string{
		"localhost:8080":           "http://localhost:8080",
		"localhost/x":              "http://localhost/x",
		"127.0.0.1:80":             "http://127.0.0.1:80",
		"http://localhost:8080":    "http://localhost:8080",
		"jkroepke.de":              "https://jkroepke.de",
		"localhost.example.com:80": "https://localhost.example.com:80",
		"raw.githubusercontent.com/kubernetes/website/main/README.md": "https://raw.githubusercontent.com/kubernetes/website/main/README.md",
	} {
		u, err := wgetURL(raw)
		if err != nil || u.String() != want {
			t.Errorf("wgetURL(%q) = %v, %v; want %s", raw, u, err, want)
		}
	}
	for _, raw := range []string{"ftp://example.com", "https://"} {
		if u, err := wgetURL(raw); err == nil {
			t.Errorf("wgetURL(%q) = %v, want an error", raw, u)
		}
	}
}

func TestParseCurl(t *testing.T) {
	c, err := parseCurl(context.Background(), interp.HandlerContext{}, []string{
		"-sSfL", "-XPUT", "-H", "A: b", "--data", "x=1", "-d", "y=2", "-o", "out", "web:8080", "-m", "1.5", "-w", "%{http_code}", "--compressed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !c.silent || !c.showErr || !c.fail || !c.location || c.method != "PUT" || !slices.Equal(c.headers, []string{"A: b"}) ||
		!slices.Equal(c.data, []string{"x=1", "y=2"}) || !slices.Equal(c.outputs, []string{"out"}) ||
		!slices.Equal(c.urls, []string{"web:8080"}) || c.timeout != 1500*time.Millisecond || c.writeOut != "%{http_code}" {
		t.Errorf("parseCurl: %+v", c)
	}
	for _, args := range [][]string{{"-Z"}, {"--nope"}, {"-o"}, {"-m", "x", "u"}} {
		if _, err := parseCurl(context.Background(), interp.HandlerContext{}, args); err == nil {
			t.Errorf("parseCurl(%q): no error", args)
		}
	}
}

func TestCurlWriteOut(t *testing.T) {
	got := curlWriteOut(`code=%{http_code}\n%%{x}%{nope}\t.`, map[string]string{"http_code": "200"})
	if want := "code=200\n%{x}\t."; got != want {
		t.Errorf("curlWriteOut = %q, want %q", got, want)
	}
}

func TestClusterURLs(t *testing.T) {
	// Without a tab node only localhost and cluster IPs are known.
	for raw, want := range map[string]string{
		"10.96.0.20":         "http://10.96.0.20",
		"10.96.0.20:8080/x":  "http://10.96.0.20:8080/x",
		"10.244.0.5?a=b":     "http://10.244.0.5?a=b",
		"[::1]:80":           "http://[::1]:80",
		"example.com/10.96.": "https://example.com/10.96.",
	} {
		u, err := cmdURL(context.Background(), raw)
		if err != nil || u.String() != want {
			t.Errorf("cmdURL(%q) = %v, %v; want %s", raw, u, err, want)
		}
	}
	for host, want := range map[string]bool{"web": true, "web.default.svc": true, "x.cluster.local": true, "example.com": false, "localhost": false, "10.96.0.1": false} {
		if got := noSuchHost(context.Background(), host); got != want {
			t.Errorf("noSuchHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLsSymlinks(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644)
	os.Symlink("f", filepath.Join(dir, "l"))
	out, code := sh(t, "ls -l "+dir)
	if code != 0 || !strings.Contains("\n"+out, "\nlrwx") || !strings.Contains(out, " l -> f") {
		t.Errorf("ls -l: %q (exit %d)", out, code)
	}
	out, _ = sh(t, "ls -lL "+dir)
	if strings.Contains(out, "->") || strings.Contains(out, "lrwx") {
		t.Errorf("ls -lL: %q", out)
	}
}

// lsFixture is a directory with a (3 bytes), b (empty), .h, d/x, and l -> a.
func lsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a"), []byte("abc"), 0o644)
	os.WriteFile(filepath.Join(dir, "b"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, ".h"), nil, 0o644)
	os.Mkdir(filepath.Join(dir, "d"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "x"), nil, 0o644)
	os.Symlink("a", filepath.Join(dir, "l"))
	return dir
}

func TestLsFlags(t *testing.T) {
	dir := lsFixture(t)
	// Sorting needs sizes and times: x is 10 bytes 2h old, y 30 bytes 1h old,
	// z 20 bytes 3h old.
	sorted := t.TempDir()
	for n, c := range map[string]struct {
		size int
		age  time.Duration
	}{"x": {10, 2 * time.Hour}, "y": {30, time.Hour}, "z": {20, 3 * time.Hour}} {
		p := filepath.Join(sorted, n)
		os.WriteFile(p, make([]byte, c.size), 0o644)
		os.Chtimes(p, time.Now().Add(-c.age), time.Now().Add(-c.age))
	}
	blue := func(s string) string { return "\x1b[1;34m" + s + "\x1b[m" }
	for _, tc := range []struct{ name, script, want string }{
		{"plain", "ls --color=never", "a\nb\nd\nl\n"},
		{"default is plain without a terminal", "ls", "a\nb\nd\nl\n"},
		{"pipe", "ls | cat", "a\nb\nd\nl\n"},
		{"command substitution", "x=$(ls); echo \"$x\"", "a\nb\nd\nl\n"},
		{"redirect", "ls > ../out.txt; cat ../out.txt", "a\nb\nd\nl\n"},
		{"for loop", "for f in $(ls -d d); do echo $f; done", "d\n"},
		{"--color=always in a pipe", "ls --color=always -d d | cat", blue("d") + "\n"},
		{"--color in a pipe", "ls --color -d d | cat", blue("d") + "\n"},
		{"-1", "ls -1 --color=never", "a\nb\nd\nl\n"},
		{"-a", "ls -a --color=never", ".\n..\n.h\na\nb\nd\nl\n"},
		{"-A", "ls -A --color=never", ".h\na\nb\nd\nl\n"},
		{"-d dir", "ls -d --color=never d", "d\n"},
		{"-d dot", "ls -d --color=never .", ".\n"},
		{"-R", "ls -R --color=never", ".:\na\nb\nd\nl\n\n./d:\nx\n"},
		{"-R operand", "ls -R --color=never d", "d:\nx\n"},
		{"-R hidden", "ls -aR --color=never d", "d:\n.\n..\nx\n"},
		{"two operands", "ls --color=never d a", "a\n\nd:\nx\n"},
		{"-r", "ls -r --color=never", "l\nd\nb\na\n"},
		{"-L", "ls -L --color=never", "a\nb\nd\nl\n"},
		{"--color=always", "ls --color=always d a", "\x1b[0;0ma\x1b[m\n\nd:\n\x1b[0;0mx\x1b[m\n"},
		{"--color bare", "ls --color a", "\x1b[0;0ma\x1b[m\n"},
		{"--color=always symlink", "ls --color=always l", "\x1b[1;36ml\x1b[m\n"},
		{"--color=never dir", "ls --color=never -d d", "d\n"},
		{"--color=auto in a pipe", "ls --color=auto -d d | cat", "d\n"},
		{"-S", "cd " + sorted + " && ls -S", "y\nz\nx\n"},
		{"-t", "cd " + sorted + " && ls -t", "y\nx\nz\n"},
		{"-rt", "cd " + sorted + " && ls -rt", "z\nx\ny\n"},
		{"-S beats -t", "cd " + sorted + " && ls -tS", "y\nz\nx\n"},
		{"-rS", "cd " + sorted + " && ls -rS", "x\nz\ny\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := "cd " + dir + " && " + tc.script
			if strings.HasPrefix(tc.script, "cd ") {
				script = tc.script
			}
			out, code := sh(t, script)
			if out != tc.want || code != 0 {
				t.Errorf("%s: got %q (exit %d), want %q", tc.script, out, code, tc.want)
			}
		})
	}
	// -i: the inode comes first, 7 wide.
	out, _ := sh(t, "cd "+dir+" && ls -i --color=never a")
	if !regexp.MustCompile(`^ *[0-9]+ a\n$`).MatchString(out) || len(out) < 10 {
		t.Errorf("ls -i: %q", out)
	}
	out, _ = sh(t, "cd "+dir+" && ls -li --color=never a")
	if !regexp.MustCompile(`^ *[0-9]+ -rw-r--r-- +1 root +root +3 \w{3} [ 0-9]\d [0-9:]{5} a\n$`).MatchString(out) {
		t.Errorf("ls -li: %q", out)
	}
}

func TestLsLong(t *testing.T) {
	dir := lsFixture(t)
	big := t.TempDir()
	for n, size := range map[string]int{"big": 1043000, "k": 1024, "k1": 1023} {
		os.WriteFile(filepath.Join(big, n), make([]byte, size), 0o644)
	}
	date := `\w{3} [ 0-9]\d [0-9:]{5}`
	for _, tc := range []struct{ name, script, re string }{
		{"-l file", "ls -l a", `^-rw-r--r-- {4}1 root {5}root {13}3 ` + date + ` a\n$`},
		{"-l dir", "ls -l --color=never", `^total [0-9]+\n-rw-r--r-- {4}1 root {5}root {13}3 ` + date + ` a\n-rw-r--r-- {4}1 root {5}root {13}0 ` + date + ` b\ndrwxr-xr-x {4}[0-9]+ root {5}root +[0-9]+ ` + date + ` d\nlrwxrwxrwx {4}1 root {5}root {13}1 ` + date + ` l -> a\n$`},
		{"-ln", "ls -ln a", `^-rw-r--r-- {4}1 0 {8}0 {8}[ ]{8}3 ` + date + ` a\n$`},
		{"-nl", "ls -nl a", `^-rw-r--r-- {4}1 0 {8}0 {8}[ ]{8}3 ` + date + ` a\n$`},
		{"-n alone is long", "ls -n a", `^-rw-r--r-- {4}1 0 {8}0 {8}[ ]{8}3 ` + date + ` a\n$`},
		{"-n symlink", "ls -n l", `^lrwxrwxrwx {4}1 0 {8}0 {8}[ ]{8}1 ` + date + ` l -> a\n$`},
		{"-lan", "ls -lan --color=never", `^total [0-9]+\ndrwx.* \.\n.* \.\.\n.* \.h\n.* a\n`},
		{"-lL", "ls -lL --color=never", `^total [0-9]+\n(-rw-r--r--.*\n)+drwx.* d\n-rw-r--r-- .* l\n$`},
		{"-ld", "ls -ld --color=never d", `^drwxr-xr-x {4}[0-9]+ root {5}root +[0-9]+ ` + date + ` d\n$`},
		{"-l on a symlink target colored", "ls -l --color=always l", ` \x1b\[1;36ml\x1b\[m -> \x1b\[0;0ma\x1b\[m\n$`},
		{"-lh", "cd " + big + " && ls -lh", `^total [0-9]+[A-Z]? *\n-rw-r--r-- {4}1 root {5}root {5}1018\.6K ` + date + ` big\n.* {6}1\.0K ` + date + ` k\n.* {5}1023 ` + date + ` k1\n$`},
		{"-lhS", "cd " + big + " && ls -lhS", `1018\.6K .* big\n.*1\.0K .* k\n.*1023 .* k1\n$`},
		{"-ltr", "ls -ltr --color=never", `^total [0-9]+\n(.*\n){4}$`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.script
			if !strings.HasPrefix(s, "cd ") {
				s = "cd " + dir + " && " + s
			}
			out, code := sh(t, s)
			if code != 0 || !regexp.MustCompile(tc.re).MatchString(out) {
				t.Errorf("%s: %q (exit %d) does not match %s", tc.script, out, code, tc.re)
			}
		})
	}
}

func TestLsTotal(t *testing.T) {
	dir := lsFixture(t)
	var want int64
	for _, n := range []string{"a", "b", "d", "l"} {
		fi, _ := os.Lstat(filepath.Join(dir, n))
		want += int64(fi.Sys().(*syscall.Stat_t).Blocks)
	}
	out, _ := sh(t, "cd "+dir+" && ls -l")
	if first, _, _ := strings.Cut(out, "\n"); first != "total "+strconv.FormatInt(want/2, 10) {
		t.Errorf("total line %q, want total %d", first, want/2)
	}
	for _, s := range []string{"ls -l a", "ls -ld d", "ls -l a l", "ls -l nope"} {
		if out, _ := sh(t, "cd "+dir+" && "+s); strings.Contains(out, "total") {
			t.Errorf("%s has a total line: %q", s, out)
		}
	}
	if out, _ := sh(t, "cd "+dir+" && ls -lR d"); !strings.HasPrefix(out, "d:\ntotal ") {
		t.Errorf("ls -lR: %q", out)
	}
	if out, _ := sh(t, "cd "+dir+" && ls -lh --color=never d"); !regexp.MustCompile(`^total [0-9]+[KM]? *\n`).MatchString(out) {
		t.Errorf("ls -lh total: %q", out)
	}
}

func TestLsErrors(t *testing.T) {
	dir := lsFixture(t)
	out, code := sh(t, "cd "+dir+" && ls -Z")
	if code != 1 || !strings.HasPrefix(out, "ls: invalid option -- 'Z'\nBusyBox v1.38.0 (") ||
		!strings.Contains(out, " multi-call binary.\n\nUsage: ls [-1AaCxdLHRFplinshrSXvctu] [-w WIDTH] [FILE]...\n\nList directory contents\n") {
		t.Errorf("ls -Z: %q (exit %d)", out, code)
	}
	out, code = sh(t, "cd "+dir+" && ls -lZ a")
	if code != 1 || !strings.HasPrefix(out, "ls: invalid option -- 'Z'\n") || strings.Contains(out, "rw-") {
		t.Errorf("ls -lZ: %q (exit %d)", out, code)
	}
	// -x is real BusyBox but not here: it must still be an error.
	if out, code = sh(t, "ls -x"); code != 1 || !strings.HasPrefix(out, "ls: invalid option -- 'x'\n") {
		t.Errorf("ls -x: %q (exit %d)", out, code)
	}
	if out, code = sh(t, "ls --nope"); code != 1 || !strings.HasPrefix(out, "ls: unrecognized option '--nope'\nBusyBox v1.38.0") {
		t.Errorf("ls --nope: %q (exit %d)", out, code)
	}
	if out, code = sh(t, "cd "+dir+" && ls nope a"); code != 1 || out != "ls: nope: No such file or directory\na\n" {
		t.Errorf("ls nope a: %q (exit %d)", out, code)
	}
}

func TestLsHuman(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0", 3: "3", 1000: "1000", 1023: "1023", 1024: "1.0K", 1025: "1.0K", 1536: "1.5K",
		10240: "10.0K", 10445: "10.2K", 102400: "100.0K", 1043000: "1018.6K", 1048575: "1024.0K",
		1048576: "1.0M", 1126400: "1.1M", 10485760: "10.0M", 1073741823: "1024.0M",
		1073741824: "1.0G", 5000000000: "4.7G", 1099511627776: "1.0T",
	} {
		if got := lsHuman(n); got != want {
			t.Errorf("lsHuman(%d) = %q, want %q", n, got, want)
		}
	}
	for kb, want := range map[int64]string{0: "0      ", 4: "4K     ", 980: "980K   ", 1536: "2M     ", 19532: "19M    ", 2192: "2M     "} {
		if got := lsTotal(kb, true); got != want {
			t.Errorf("lsTotal(%d) = %q, want %q", kb, got, want)
		}
	}
	if got := lsTotal(2192, false); got != "2192" {
		t.Errorf("lsTotal = %q", got)
	}
}

func TestLsDate(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		t    time.Time
		re   string
	}{
		{"recent", now.Add(-time.Hour), `^\w{3} [ 0-9]\d [0-9]{2}:[0-9]{2}$`},
		{"five months", now.AddDate(0, -5, 0), `^\w{3} [ 0-9]\d [0-9]{2}:[0-9]{2}$`},
		{"old", now.AddDate(0, -7, 0), `^\w{3} [ 0-9]\d  [0-9]{4}$`},
		{"future", now.Add(24 * time.Hour), `^\w{3} [ 0-9]\d  [0-9]{4}$`},
	} {
		if got := lsDate(tc.t); !regexp.MustCompile(tc.re).MatchString(got) {
			t.Errorf("%s: %q", tc.name, got)
		}
	}
	if got := lsDate(time.Date(2020, 1, 2, 3, 4, 5, 0, time.Local)); got != "Jan  2  2020" {
		t.Errorf("lsDate = %q", got)
	}
}

// ls -n shows the numbers of the files on disk, which follow runAsUser and
// fsGroup; ls -l shows names from /etc/passwd and /etc/group when they exist.
func TestLsNumericIDs(t *testing.T) {
	pe, _ := secContainer(t, "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), FSGroup: ptr.To[int64](2000)}, nil)
	w := pe.w
	w.dir = t.TempDir()
	w.pod.Spec.Volumes = []v1.Volume{{Name: "data", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}}
	vol, _ := w.volumeDir("data")
	os.MkdirAll(vol, 0o755)
	pe.root.mounts = []rootMount{{path: "/data", real: vol}}
	if err := os.Chown(vol, os.Getuid(), 2000); err != nil {
		t.Skipf("chown not permitted here: %v", err)
	}
	w.chownVolumes()
	if got, code := runIn(t, pe, "echo x > /data/f; ln -s f /data/s"); code != 0 {
		t.Fatalf("exit %d: %s", code, got)
	}
	for _, tc := range []struct{ script, want string }{
		{"ls -ln /data/f", "-rw-r--r--    1 1000     2000             2 "},
		{"ls -nl /data/f", "-rw-r--r--    1 1000     2000             2 "},
		{"ls -ln /data/s", "lrwxrwxrwx    1 1000     2000             1 "},
		{"ls -l /data/f", "-rw-r--r--    1 1000     2000             2 "},
	} {
		if got, _ := runIn(t, pe, tc.script); !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: %q, want prefix %q", tc.script, got, tc.want)
		}
	}
	os.WriteFile(pe.root.real("/etc/passwd"), []byte("app:x:1000:1000::/:/bin/sh\n"), 0o644)
	os.WriteFile(pe.root.real("/etc/group"), []byte("web:x:2000:\n"), 0o644)
	if got, _ := runIn(t, pe, "ls -l /data/f"); !strings.HasPrefix(got, "-rw-r--r--    1 app      web              2 ") {
		t.Errorf("ls -l with names: %q", got)
	}
	if got, _ := runIn(t, pe, "ls -ln /data/f"); !strings.HasPrefix(got, "-rw-r--r--    1 1000     2000             2 ") {
		t.Errorf("ls -ln with names: %q", got)
	}
	// The terminal is root.
	if got, _ := sh(t, "touch f && ls -n f"); !strings.HasPrefix(got, "-rw-r--r--    1 0        0                0 ") {
		t.Errorf("ls -n in the terminal: %q", got)
	}
}

func TestStdoutIsTTY(t *testing.T) {
	ctx := context.Background()
	var buf bytes.Buffer
	for _, tc := range []struct {
		name string
		w    io.Writer
		want bool
	}{
		{"buffer", &buf, false},
		{"pipe", io.Discard, false},
		{"screen", holdWriter{tty}, true},
		{"screen of kubectl", ttyOut{}, true},
		{"screen in a context writer", ctxWriter{ctx, holdWriter{tty}}, true},
		{"pipe in a context writer", ctxWriter{ctx, &buf}, false},
	} {
		if got := stdoutIsTTY(ctx, tc.w); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// In a container the stdout of ls is a terminal only with tty: true.
func TestLsColorInContainer(t *testing.T) {
	blue := "\x1b[1;34m/tmp\x1b[m"
	pe, _ := secContainer(t, "busybox", nil, nil)
	for script, want := range map[string]bool{
		"ls -d /tmp":                      false,
		"ls -d /tmp | cat":                false,
		"ls --color=always -d /tmp | cat": true,
		"ls --color=always -d /tmp":       true,
	} {
		if got, _ := runIn(t, pe, script); strings.Contains(got, blue) != want {
			t.Errorf("no tty, %s: %q", script, got)
		}
	}
	in, keys := io.Pipe()
	defer keys.Close()
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pe.tty = newPty(ctx, in, &out, cancel)
	for script, want := range map[string]bool{
		"ls -d /tmp":                      true,
		"ls --color=auto -d /tmp":         true,
		"ls --color=never -d /tmp":        false,
		"ls -d /tmp | cat":                false,
		"x=$(ls -d /tmp); echo $x":        false,
		"ls --color=always -d /tmp | cat": true,
	} {
		before := len(out.String())
		runArgv(ctx, pe, []string{"sh", "-c", script}, []string{"PATH=/bin"}, "/", pe.tty, pe.tty, pe.tty)
		if got := out.String()[before:]; strings.Contains(got, blue) != want || !strings.Contains(got, "/tmp") {
			t.Errorf("tty, %s: %q", script, got)
		}
	}
}
