package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// date, watch, less, more, df, du, mount, umount, nc, telnet, ping, which.

func init() {
	for name, u := range map[string]util{
		"date": dateCmd, "watch": watchCmd, "less": lessCmd, "more": lessCmd, "df": dfCmd, "du": duCmd,
		"mount": mountCmd, "umount": umountCmd, "nc": ncCmd, "telnet": telnetCmd, "ping": pingCmd, "which": whichCmd,
	} {
		coreutils[name] = u
	}
}

// nodeHostname is the hostname of the pod, "tabernetes" in the terminal.
func nodeHostname(ctx context.Context) string {
	if pe := procOf(ctx); pe != nil && pe.root != nil {
		if b, err := os.ReadFile(pe.root.real("/etc/hostname")); err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
	}
	return "tabernetes"
}

// ---- date ----

var (
	dateLayouts = []string{
		time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04", "2006-01-02T15:04",
		"2006-01-02", "2006/01/02", "01/02/2006", "Mon Jan _2 15:04:05 MST 2006", time.RFC1123Z, time.RFC1123, "2 Jan 2006", "Jan 2 2006",
	}
	dateRel = regexp.MustCompile(`^([+-]?\d+)\s*(second|sec|minute|min|hour|day|week|month|year)s?(\s+ago)?$`)
)

// parseDate reads the argument of date -d: now, today, yesterday, tomorrow,
// @epoch, "N units [ago]", the layouts above and a bare time of day.
func parseDate(s string, loc *time.Location, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	switch strings.ToLower(s) {
	case "", "now":
		return now, nil
	case "today":
		return now, nil
	case "yesterday":
		return now.AddDate(0, 0, -1), nil
	case "tomorrow":
		return now.AddDate(0, 0, 1), nil
	}
	if e, ok := strings.CutPrefix(s, "@"); ok {
		f, err := strconv.ParseFloat(e, 64)
		if err != nil {
			return now, fmt.Errorf("invalid date '%s'", s)
		}
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)).In(loc), nil
	}
	if m := dateRel.FindStringSubmatch(strings.ToLower(s)); m != nil {
		n, _ := strconv.Atoi(m[1])
		if m[3] != "" {
			n = -n
		}
		switch m[2] {
		case "second", "sec":
			return now.Add(time.Duration(n) * time.Second), nil
		case "minute", "min":
			return now.Add(time.Duration(n) * time.Minute), nil
		case "hour":
			return now.Add(time.Duration(n) * time.Hour), nil
		case "day":
			return now.AddDate(0, 0, n), nil
		case "week":
			return now.AddDate(0, 0, 7*n), nil
		case "month":
			return now.AddDate(0, n, 0), nil
		}
		return now.AddDate(n, 0, 0), nil
	}
	for _, l := range dateLayouts {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return t.In(loc), nil
		}
	}
	for _, l := range []string{"15:04:05", "15:04"} {
		if t, err := time.ParseInLocation(l, s, loc); err == nil {
			return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc), nil
		}
	}
	return now, fmt.Errorf("invalid date '%s'", s)
}

// strftime formats t like date +FORMAT. The flags - (no padding), _ (pad
// with spaces), 0 and ^ (upper case) work after the %.
func strftime(t time.Time, f string) string {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 >= len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		flag := byte(0)
		if strings.IndexByte("-_0^", f[i]) >= 0 && i+1 < len(f) {
			flag = f[i]
			i++
		}
		num := func(v, w int, pad byte) string {
			s := strconv.Itoa(v)
			switch flag {
			case '-':
				return s
			case '_':
				pad = ' '
			case '0':
				pad = '0'
			}
			for len(s) < w {
				s = string(pad) + s
			}
			return s
		}
		var s string
		h12 := t.Hour() % 12
		if h12 == 0 {
			h12 = 12
		}
		switch f[i] {
		case 'Y':
			s = strconv.Itoa(t.Year())
		case 'C':
			s = num(t.Year()/100, 2, '0')
		case 'y':
			s = num(t.Year()%100, 2, '0')
		case 'm':
			s = num(int(t.Month()), 2, '0')
		case 'd':
			s = num(t.Day(), 2, '0')
		case 'e':
			s = num(t.Day(), 2, ' ')
		case 'H':
			s = num(t.Hour(), 2, '0')
		case 'k':
			s = num(t.Hour(), 2, ' ')
		case 'I':
			s = num(h12, 2, '0')
		case 'l':
			s = num(h12, 2, ' ')
		case 'M':
			s = num(t.Minute(), 2, '0')
		case 'S':
			s = num(t.Second(), 2, '0')
		case 'j':
			s = num(t.YearDay(), 3, '0')
		case 'u':
			s = strconv.Itoa((int(t.Weekday())+6)%7 + 1)
		case 'w':
			s = strconv.Itoa(int(t.Weekday()))
		case 'V':
			_, w := t.ISOWeek()
			s = num(w, 2, '0')
		case 'G':
			y, _ := t.ISOWeek()
			s = strconv.Itoa(y)
		case 's':
			s = strconv.FormatInt(t.Unix(), 10)
		case 'N':
			s = fmt.Sprintf("%09d", t.Nanosecond())
		case 'a':
			s = t.Format("Mon")
		case 'A':
			s = t.Format("Monday")
		case 'b', 'h':
			s = t.Format("Jan")
		case 'B':
			s = t.Format("January")
		case 'p':
			s = t.Format("PM")
		case 'P':
			s = t.Format("pm")
		case 'Z':
			s = t.Format("MST")
		case 'z':
			s = t.Format("-0700")
		case 'T':
			s = t.Format("15:04:05")
		case 'F':
			s = t.Format("2006-01-02")
		case 'D', 'x':
			s = t.Format("01/02/06")
		case 'R':
			s = t.Format("15:04")
		case 'X':
			s = t.Format("15:04:05")
		case 'r':
			s = t.Format("03:04:05 PM")
		case 'c':
			s = t.Format("Mon Jan _2 15:04:05 2006")
		case 'n':
			s = "\n"
		case 't':
			s = "\t"
		case '%':
			s = "%"
		default:
			s = "%" + string(f[i])
		}
		if flag == '^' {
			s = strings.ToUpper(s)
		}
		b.WriteString(s)
	}
	return b.String()
}

// date prints the time: +FORMAT, -u, -d STR, -I[FMT], -R and -r FILE.
func dateCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	args = slices.Clone(args)
	for i, a := range args {
		if strings.HasPrefix(a, "-I") && len(a) > 2 {
			args[i] = "--iso-8601=" + a[2:]
		}
	}
	o, rest, err := opts("date", args, "uId:r:R", map[string]string{"utc": "u", "universal": "u", "date": "d", "iso-8601": "I", "rfc-email": "R", "reference": "r"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	loc := time.Local
	if has(o, "u") {
		loc = time.UTC
	}
	t := time.Now().In(loc)
	if f, ok := o["r"]; ok {
		fi, err := os.Stat(abs(ctx, hc, f))
		if err != nil {
			return fail(hc, 1, "date: %s: %v", f, unwrap(err))
		}
		t = fi.ModTime().In(loc)
	} else if s, ok := o["d"]; ok {
		if t, err = parseDate(s, loc, t); err != nil {
			return fail(hc, 1, "date: %v", err)
		}
	}
	format := "Mon Jan _2 15:04:05 MST 2006"
	switch {
	case len(rest) > 0 && strings.HasPrefix(rest[0], "+"):
		fmt.Fprintln(hc.Stdout, strftime(t, rest[0][1:]))
		return nil
	case has(o, "R"):
		format = "Mon, 02 Jan 2006 15:04:05 -0700"
	case has(o, "I"):
		switch o["I"] {
		case "", "date":
			format = "2006-01-02"
		case "hours":
			format = "2006-01-02T15-07:00"
		case "minutes":
			format = "2006-01-02T15:04-07:00"
		case "seconds":
			format = "2006-01-02T15:04:05-07:00"
		case "ns":
			format = "2006-01-02T15:04:05,000000000-07:00"
		default:
			return fail(hc, 1, "date: invalid argument '%s' for '--iso-8601'", o["I"])
		}
	case len(rest) > 0:
		return fail(hc, 1, "date: invalid date '%s'", rest[0])
	}
	fmt.Fprintln(hc.Stdout, t.Format(format))
	return nil
}

// ---- watch ----

// watch runs a command again and again and redraws the screen, like procps:
// -n secs (fractions ok), -t no title; -d, -e (stop when the command fails)
// and -x are accepted. The command goes through a shell of its own, so its
// output can be captured and shell syntax works.
func watchCmd(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	o, rest, err := opts("watch", args, "n:tdexpb", map[string]string{"interval": "n", "no-title": "t", "differences": "d", "errexit": "e", "exec": "x"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(rest) == 0 {
		return fail(hc, 1, "watch: no command given")
	}
	secs := 2.0
	if v, ok := o["n"]; ok {
		if secs, err = strconv.ParseFloat(v, 64); err != nil {
			return fail(hc, 1, "watch: failed to parse number: %s", v)
		}
		secs = max(secs, 0.1)
	}
	src := strings.Join(rest, " ")
	if len(rest) > 1 {
		var q []string
		for _, a := range rest {
			s, err := syntax.Quote(a, syntax.LangBash)
			if err != nil {
				return fail(hc, 1, "watch: %v", err)
			}
			q = append(q, s)
		}
		src = strings.Join(q, " ")
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(src), "")
	if err != nil {
		return fail(hc, 1, "watch: %v", err)
	}
	host := nodeHostname(ctx)
	for ctx.Err() == nil {
		var buf bytes.Buffer
		r, err := interp.New(interp.StdIO(nil, &buf, &buf), interp.Dir(hc.Dir), interp.Env(hc.Env),
			interp.ExecHandlers(func(interp.ExecHandlerFunc) interp.ExecHandlerFunc { return exec }))
		if err != nil {
			return fail(hc, 1, "watch: %v", err)
		}
		runErr := r.Run(ctx, file)
		if ctx.Err() != nil {
			return nil
		}
		var out strings.Builder
		out.WriteString("\x1b[H\x1b[2J")
		if !has(o, "t") {
			left := fmt.Sprintf("Every %.1fs: %s", secs, strings.Join(rest, " "))
			right := host + ": " + time.Now().Format("Mon Jan _2 15:04:05 2006")
			out.WriteString(left + strings.Repeat(" ", max(80-len(left)-len(right), 1)) + right + "\n\n")
		}
		out.Write(buf.Bytes())
		io.WriteString(hc.Stdout, out.String())
		if st, ok := interp.IsExitStatus(runErr); ok && has(o, "e") && st != 0 {
			return exitCode(int(st))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(secs * float64(time.Second))):
		}
	}
	return nil
}

// ---- less, more ----

// less and more are no pagers: a pager needs raw mode on the terminal, which
// a pipeline in the shell of a tab does not have. They copy files or stdin to
// stdout, like less -F does for input that fits on a screen. -N (less) numbers
// the lines, the other flags are accepted and ignored.
func lessCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	number := false
	var files []string
	for _, a := range args {
		switch {
		case a == "-" || !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+"):
			files = append(files, a)
		case strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "N"):
			number = true
		case a == "--LINE-NUMBERS":
			number = true
		}
	}
	n := 0
	return inputs(ctx, hc, "less", files, func(_ string, r io.Reader) error {
		if !number {
			_, err := io.Copy(hc.Stdout, r)
			return err
		}
		for _, l := range lines(r) {
			n++
			fmt.Fprintf(hc.Stdout, "%6d  %s\n", n, l)
		}
		return nil
	})
}

// ---- mounts, df ----

// fsRow is one mounted file system of the container (or the terminal).
type fsRow struct {
	src, typ, mount, real string
	ro                    bool
	total                 int64 // 1K blocks
}

const (
	diskKB = 64 << 20 // the disk of the node: 64G
	tmpKB  = 4 << 20  // a tmpfs: the 4G of memory of the wasm
)

// mountRows lists the file systems a command sees: in a pod the root and one
// per volume mount (the type comes from the volume), in the terminal tabfs.
func mountRows(ctx context.Context) []fsRow {
	pe := procOf(ctx)
	if pe == nil || pe.root == nil || pe.c == nil {
		return []fsRow{{"tabfs", "tmpfs", "/", "/", false, diskKB}}
	}
	rows := []fsRow{{"overlay", "overlay", "/", pe.root.base, false, diskKB}}
	pod, _ := pe.w.podInfo()
	for _, m := range pe.c.spec.VolumeMounts {
		row := fsRow{"/dev/vda1", "ext4", path.Clean("/" + m.MountPath), pe.root.real(m.MountPath), m.ReadOnly, diskKB}
		for _, v := range pod.Spec.Volumes {
			if v.Name != m.Name {
				continue
			}
			switch {
			case v.HostPath != nil:
				row.src, row.typ = "tabfs", "tabfs"
			case v.EmptyDir != nil && v.EmptyDir.Medium == v1.StorageMediumMemory,
				v.ConfigMap != nil, v.Secret != nil, v.Projected != nil, v.DownwardAPI != nil:
				row.src, row.typ, row.total = "tmpfs", "tmpfs", tmpKB
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// kbSize is the size of a tree in 1K blocks, a block per started 1024 bytes
// of each file. It stops after 3000 entries (the terminal's root is the whole
// in-memory file system) and does not enter /proc, /sys and /dev.
func kbSize(real string) int64 {
	var sum int64
	n := 0
	filepath.WalkDir(real, func(p string, d os.DirEntry, err error) error {
		if n++; n > 3000 {
			return filepath.SkipAll
		}
		if err != nil {
			return nil
		}
		if d.IsDir() && (p == "/proc" || p == "/sys" || p == "/dev") {
			return filepath.SkipDir
		}
		if fi, err := d.Info(); err == nil && fi.Mode().IsRegular() {
			sum += (fi.Size() + 1023) / 1024
		}
		return nil
	})
	return sum
}

// human prints 1K blocks like df -h: one decimal below 10, rounded up.
func human(kb int64) string {
	if kb == 0 {
		return "0"
	}
	v := float64(kb)
	for _, u := range "KMGT" {
		if v < 1024 || u == 'T' {
			if v < 10 {
				return fmt.Sprintf("%.1f%c", float64(int64(v*10+0.999999))/10, u)
			}
			return fmt.Sprintf("%d%c", int64(v+0.999999), u)
		}
		v /= 1024
	}
	return ""
}

// df lists the file systems: -h, -k, -T, -P and operands (the file system
// that holds each file). The sizes are made up: a 64G disk, 4G tmpfs.
func dfCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, files, err := opts("df", args, "hkTPmiHa", map[string]string{"human-readable": "h", "print-type": "T", "portability": "P"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	rows := mountRows(ctx)
	if len(files) > 0 {
		var sel []fsRow
		for _, f := range files {
			if _, err := os.Stat(abs(ctx, hc, f)); err != nil {
				return fail(hc, 1, "df: %s: %v", f, unwrap(err))
			}
			p := path.Clean(path.Join(hc.Dir, f))
			if path.IsAbs(f) {
				p = path.Clean(f)
			}
			best := rows[0]
			for _, r := range rows {
				if (p == r.mount || strings.HasPrefix(p, strings.TrimSuffix(r.mount, "/")+"/")) && len(r.mount) >= len(best.mount) {
					best = r
				}
			}
			sel = append(sel, best)
		}
		rows = sel
	}
	head := []string{"Filesystem"}
	if has(o, "T") {
		head = append(head, "Type")
	}
	switch {
	case has(o, "h") || has(o, "H"):
		head = append(head, "Size", "Used", "Avail", "Use%")
	case has(o, "P"):
		head = append(head, "1024-blocks", "Used", "Available", "Capacity")
	case has(o, "m"):
		head = append(head, "1M-blocks", "Used", "Available", "Use%")
	default:
		head = append(head, "1K-blocks", "Used", "Available", "Use%")
	}
	head = append(head, "Mounted on")
	table := [][]string{head}
	for _, r := range rows {
		used := kbSize(r.real)
		avail := max(r.total-used, 0)
		cell := func(kb int64) string {
			switch {
			case has(o, "h") || has(o, "H"):
				return human(kb)
			case has(o, "m"):
				return strconv.FormatInt((kb+1023)/1024, 10)
			}
			return strconv.FormatInt(kb, 10)
		}
		row := []string{r.src}
		if has(o, "T") {
			row = append(row, r.typ)
		}
		row = append(row, cell(r.total), cell(used), cell(avail), fmt.Sprintf("%d%%", (used*100+r.total-1)/r.total), r.mount)
		table = append(table, row)
	}
	// Filesystem and Type are left aligned, the numbers right, Mounted on
	// is the last column.
	w := make([]int, len(head))
	for _, row := range table {
		for i, c := range row {
			w[i] = max(w[i], len(c))
		}
	}
	nameCols := 1
	if has(o, "T") {
		nameCols = 2
	}
	for _, row := range table {
		var b strings.Builder
		for i, c := range row {
			switch {
			case i == len(row)-1:
				b.WriteString(c)
			case i < nameCols:
				b.WriteString(c + strings.Repeat(" ", w[i]-len(c)+1))
			default:
				b.WriteString(strings.Repeat(" ", w[i]-len(c)) + c + " ")
			}
		}
		fmt.Fprintln(hc.Stdout, b.String())
	}
	return nil
}

// mount lists the mounts, like /proc/mounts. Mounting and unmounting need
// root on the host, as in any container: they fail the way they do there.
func mountCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	return mountList(ctx, hc, args, "mount")
}

func umountCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	return fail(hc, 32, "umount: permission denied (are you root?)")
}

func mountList(ctx context.Context, hc interp.HandlerContext, args []string, name string) error {
	if len(args) > 0 {
		return fail(hc, 32, "%s: permission denied (are you root?)", name)
	}
	pe := procOf(ctx)
	for _, r := range mountRows(ctx) {
		mode := "rw"
		if r.ro {
			mode = "ro"
		}
		opt := mode + ",relatime"
		if pe == nil || pe.root == nil {
			opt = mode
		}
		fmt.Fprintf(hc.Stdout, "%s on %s type %s (%s)\n", r.src, r.mount, r.typ, opt)
	}
	return nil
}

// ---- du ----

// du sums 1K blocks (a block per started 1024 bytes of every file): -s, -h,
// -a, -c, -k, -m, -d N / --max-depth=N. Directories themselves count 0.
func duCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, files, err := opts("du", args, "shackmd:", map[string]string{"summarize": "s", "human-readable": "h", "all": "a", "total": "c", "max-depth": "d"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	maxDepth := -1
	if has(o, "s") {
		maxDepth = 0
	}
	if v, ok := o["d"]; ok {
		if maxDepth, err = strconv.Atoi(v); err != nil {
			return fail(hc, 1, "du: invalid maximum depth '%s'", v)
		}
	}
	if len(files) == 0 {
		files = []string{"."}
	}
	show := func(kb int64) string {
		switch {
		case has(o, "h"):
			return human(kb)
		case has(o, "m"):
			return strconv.FormatInt((kb+1023)/1024, 10)
		}
		return strconv.FormatInt(kb, 10)
	}
	code := 0
	var total int64
	var walk func(real, user string, depth int) int64
	walk = func(real, user string, depth int) int64 {
		fi, err := os.Lstat(real)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "du: cannot access '%s': %v\n", user, unwrap(err))
			code = 1
			return 0
		}
		var sum int64
		if !fi.IsDir() {
			if fi.Mode().IsRegular() {
				sum = (fi.Size() + 1023) / 1024
			}
			if (has(o, "a") || depth == 0) && (maxDepth < 0 || depth <= maxDepth) {
				fmt.Fprintf(hc.Stdout, "%s\t%s\n", show(sum), user)
			}
			return sum
		}
		ents, _ := os.ReadDir(real)
		for _, e := range ents {
			sum += walk(filepath.Join(real, e.Name()), strings.TrimSuffix(user, "/")+"/"+e.Name(), depth+1)
		}
		if maxDepth < 0 || depth <= maxDepth {
			fmt.Fprintf(hc.Stdout, "%s\t%s\n", show(sum), user)
		}
		return sum
	}
	for _, f := range files {
		total += walk(abs(ctx, hc, f), f, 0)
	}
	if has(o, "c") {
		fmt.Fprintf(hc.Stdout, "%s\ttotal\n", show(total))
	}
	return exitCode(code)
}

// ---- network: nc, telnet, ping ----

// resolveName finds the IP of a host the way a pod does: IP literals,
// localhost, the cluster DNS with the search path of the pod.
func resolveName(ctx context.Context, host string) (string, bool) {
	if net.ParseIP(host) != nil {
		return host, true
	}
	if host != "localhost" {
		if a, ok := dnsLookup(ctx, host); ok && len(a.ips) > 0 {
			return a.ips[0], true
		}
	}
	h := clusterHost(ctx, host)
	return h, net.ParseIP(h) != nil
}

var services = map[string]int{"ftp": 21, "ssh": 22, "telnet": 23, "smtp": 25, "domain": 53, "dns": 53, "http": 80, "pop3": 110, "https": 443}

func parsePort(s string) (int, bool) {
	if p, ok := services[s]; ok {
		return p, true
	}
	p, err := strconv.Atoi(s)
	return p, err == nil && p > 0 && p < 65536
}

// dialHost resolves host and dials it from the pod (NetworkPolicy applies).
func dialHost(ctx context.Context, host string, port int, timeout time.Duration) (net.Conn, string, error) {
	ip, ok := resolveName(ctx, host)
	if !ok {
		return nil, "", fmt.Errorf("bad address '%s'", host)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c, err := dialCluster(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(port)))
	return c, ip, err
}

func dialMessage(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) || strings.Contains(err.Error(), "timed out") {
		return "Connection timed out"
	}
	return connError(err)
}

// pipeConn copies stdin to the connection and the connection to stdout until
// the remote closes it or ctx is done. The end of stdin half-closes. A read
// of stdin that never returns stays behind, there is no way to interrupt it.
func pipeConn(ctx context.Context, hc interp.HandlerContext, conn net.Conn) {
	done := make(chan struct{})
	go func() {
		io.Copy(hc.Stdout, conn)
		close(done)
	}()
	if hc.Stdin != nil {
		go func() {
			io.Copy(conn, hc.Stdin)
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
		}()
	}
	select {
	case <-done:
	case <-ctx.Done():
	}
	conn.Close()
	<-done
}

// nc connects stdin and stdout to a TCP connection: nc [-v] [-z] [-w secs]
// host port, and nc -l [-p] port for one incoming connection. TCP only.
func ncCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, rest, err := opts("nc", args, "vzlukw:p:s:q:i:nd46CN", nil)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if has(o, "u") {
		return fail(hc, 1, "nc: UDP is not supported")
	}
	var timeout time.Duration
	if v, ok := o["w"]; ok {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fail(hc, 1, "nc: invalid timeout %s", v)
		}
		timeout = time.Duration(f * float64(time.Second))
	}
	if has(o, "l") {
		ps := o["p"]
		if len(rest) > 0 {
			ps = rest[len(rest)-1]
		}
		port, ok := parsePort(ps)
		if !ok {
			return fail(hc, 1, "nc: bad local port '%s'", ps)
		}
		ln, err := listenPod(ctx, ":"+strconv.Itoa(port))
		if err != nil {
			return fail(hc, 1, "nc: bind: %s", connError(err))
		}
		defer ln.Close()
		if timeout > 0 {
			defer time.AfterFunc(timeout, func() { ln.Close() }).Stop()
		}
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fail(hc, 1, "nc: accept: %s", connError(err))
		}
		pipeConn(ctx, hc, conn)
		return nil
	}
	if len(rest) != 2 {
		return fail(hc, 1, "usage: nc [-vz] [-w secs] host port | nc -l [-p] port")
	}
	host, ps := rest[0], rest[1]
	port, ok := parsePort(ps)
	if !ok {
		return fail(hc, 1, "nc: bad port '%s'", ps)
	}
	conn, ip, err := dialHost(ctx, host, port, timeout)
	if err != nil {
		if strings.HasPrefix(err.Error(), "bad address") {
			return fail(hc, 1, "nc: %v", err)
		}
		return fail(hc, 1, "nc: connect to %s port %d (tcp) failed: %s", host, port, dialMessage(err))
	}
	if has(o, "z") {
		conn.Close()
		if has(o, "v") {
			fmt.Fprintf(hc.Stdout, "%s (%s) open\n", host, net.JoinHostPort(ip, strconv.Itoa(port)))
		}
		return nil
	}
	pipeConn(ctx, hc, conn)
	return nil
}

// telnet connects to host [port 23] and copies stdin and stdout; there is no
// option negotiation.
func telnetCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	_, rest, err := opts("telnet", args, "ladeK", nil)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(rest) == 0 || len(rest) > 2 {
		return fail(hc, 1, "usage: telnet host [port]")
	}
	port := 23
	if len(rest) == 2 {
		var ok bool
		if port, ok = parsePort(rest[1]); !ok {
			return fail(hc, 1, "telnet: %s: bad port", rest[1])
		}
	}
	conn, ip, err := dialHost(ctx, rest[0], port, 0)
	if err != nil {
		if strings.HasPrefix(err.Error(), "bad address") {
			return fail(hc, 1, "telnet: %v", err)
		}
		return fail(hc, 1, "telnet: can't connect to remote host (%s): %s", ip, dialMessage(err))
	}
	fmt.Fprintf(hc.Stdout, "Connected to %s.\n", rest[0])
	pipeConn(ctx, hc, conn)
	if ctx.Err() == nil {
		fmt.Fprintln(hc.Stdout, "Connection closed by foreign host.")
	}
	return nil
}

// ping has no ICMP to use in the wasm network, so it knocks with TCP: a host
// is alive when a connection to port 80 is accepted, or refused by a host that
// exists (a pod, the node, loopback). Cluster IPs of services never answer,
// as on a real cluster where kube-proxy has no ICMP for them.
// -c count, -W secs, -i secs, -q; it runs until ctx is done without -c.
func pingCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, rest, err := opts("ping", args, "c:W:w:i:s:qvn46", nil)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(rest) != 1 {
		return fail(hc, 1, "usage: ping [-c count] [-W secs] [-i secs] [-q] host")
	}
	count, wait, interval := 0, 2*time.Second, time.Second
	secs := func(k string, def time.Duration) time.Duration {
		if f, err := strconv.ParseFloat(o[k], 64); has(o, k) && err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
		return def
	}
	if v, ok := o["c"]; ok {
		if count, err = strconv.Atoi(v); err != nil || count < 1 {
			return fail(hc, 1, "ping: invalid count '%s'", v)
		}
	}
	wait, interval = secs("W", wait), secs("i", interval)
	host := rest[0]
	ip, ok := resolveName(ctx, host)
	if !ok {
		return fail(hc, 1, "ping: bad address '%s'", host)
	}
	_, svc, _ := net.ParseCIDR(serviceCIDR)
	parsed := net.ParseIP(ip)
	fmt.Fprintf(hc.Stdout, "PING %s (%s): 56 data bytes\n", host, ip)
	sent, got := 0, 0
	var minRTT, maxRTT, sum time.Duration
	for seq := 0; count == 0 || seq < count; seq++ {
		if seq > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(interval):
			}
		}
		if ctx.Err() != nil {
			break
		}
		sent++
		start := time.Now()
		if !svc.Contains(parsed) {
			dctx, cancel := context.WithTimeout(ctx, wait)
			c, err := dialCluster(dctx, "tcp", net.JoinHostPort(ip, "80"))
			cancel()
			if err == nil {
				c.Close()
			}
			if err == nil || (refused(err) && hostExists(parsed)) {
				rtt := time.Since(start)
				got++
				sum += rtt
				if got == 1 || rtt < minRTT {
					minRTT = rtt
				}
				maxRTT = max(maxRTT, rtt)
				if !has(o, "q") {
					fmt.Fprintf(hc.Stdout, "64 bytes from %s: seq=%d ttl=64 time=%.3f ms\n", ip, seq, float64(rtt)/1e6)
				}
			}
		}
	}
	fmt.Fprintf(hc.Stdout, "\n--- %s ping statistics ---\n%d packets transmitted, %d packets received, %d%% packet loss\n", host, sent, got, 100-100*got/max(sent, 1))
	if got > 0 {
		ms := func(d time.Duration) float64 { return float64(d) / 1e6 }
		fmt.Fprintf(hc.Stdout, "round-trip min/avg/max = %.3f/%.3f/%.3f ms\n", ms(minRTT), ms(sum)/float64(got), ms(maxRTT))
		return nil
	}
	return interp.ExitStatus(1)
}

// hostExists tells a refused connection of a host from the refusal the
// in-process network gives for an address nobody has.
func hostExists(ip net.IP) bool {
	if ip.IsLoopback() || ip.String() == tabNodeIP {
		return true
	}
	return theTabNode != nil && theTabNode.policy.podByIP(ip.String()) != nil
}

// ---- which ----

// which prints /usr/bin/<name> for the commands that exist in the shell:
// the coreutils, kubectl, and in a pod the tools, programs of the image and
// shells (/bin/sh). It exits 1 if one is missing.
func whichCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	_, names, err := opts("which", args, "a", nil)
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	pod := procOf(ctx) != nil
	code := 0
	for _, n := range names {
		switch {
		case pod && shells[n]:
			fmt.Fprintf(hc.Stdout, "/bin/%s\n", n)
		case coreutils[n] != nil, n == "kubectl", n == "k",
			pod && (podTools[n] != nil || imagePrograms[n] != nil):
			fmt.Fprintf(hc.Stdout, "/usr/bin/%s\n", n)
		default:
			code = 1
		}
	}
	return exitCode(code)
}
