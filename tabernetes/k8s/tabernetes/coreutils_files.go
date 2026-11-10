package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"maps"
	"math/rand/v2"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// File tools: cp mv ln chmod chown chgrp stat find diff rmdir basename
// dirname realpath readlink mktemp and the md5/sha*sum family. In a
// container they check permissions like its user (tabnode_security.go).
func init() {
	maps.Copy(coreutils, map[string]util{
		"cp": cpCmd, "mv": mvCmd, "ln": lnCmd, "chmod": chmodCmd, "chown": chownCmd, "chgrp": chgrpCmd,
		"stat": statCmd, "find": findCmd, "diff": diffCmd, "rmdir": rmdirCmd, "basename": basenameCmd,
		"dirname": dirnameCmd, "realpath": realpathCmd, "readlink": readlinkCmd, "mktemp": mktempCmd,
		"md5sum": sumCmd("md5sum", md5.New), "sha1sum": sumCmd("sha1sum", sha1.New),
		"sha256sum": sumCmd("sha256sum", sha256.New), "sha512sum": sumCmd("sha512sum", sha512.New),
	})
}

// filesOpen is the one place where these tools open or create files (flag
// and perm as for os.OpenFile); p is the user's path.
func filesOpen(ctx context.Context, hc interp.HandlerContext, p string, flag int, perm os.FileMode) (*os.File, error) {
	return openAs(ctx, hc, p, flag, perm)
}

func filesRead(ctx context.Context, hc interp.HandlerContext, p string) ([]byte, error) {
	f, err := filesOpen(ctx, hc, p, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// fcErr is the C library text of an error: "No such file or directory".
func fcErr(err error) string {
	s := unwrap(err).Error()
	if le, ok := err.(*os.LinkError); ok {
		s = le.Err.Error()
	}
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func fcIsDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

// fcJoin joins a user directory and a name without doubling slashes.
func fcJoin(dir, name string) string {
	if strings.HasSuffix(dir, "/") {
		return dir + name
	}
	return dir + "/" + name
}

func fcBase(p string) string {
	if t := strings.TrimRight(p, "/"); t != "" {
		return path.Base(t)
	}
	return p
}

// fcPerm is the permission bits of m including setuid, setgid and sticky.
func fcPerm(m fs.FileMode) uint32 {
	p := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		p |= 0o4000
	}
	if m&fs.ModeSetgid != 0 {
		p |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		p |= 0o1000
	}
	return p
}

func fcFileMode(p uint32) fs.FileMode {
	m := fs.FileMode(p & 0o777)
	if p&0o4000 != 0 {
		m |= fs.ModeSetuid
	}
	if p&0o2000 != 0 {
		m |= fs.ModeSetgid
	}
	if p&0o1000 != 0 {
		m |= fs.ModeSticky
	}
	return m
}

// fcWalk calls fn for p (real path of the user path up) and everything below
// it, with the user's name of each entry. It does not follow symlinks.
func fcWalk(ctx context.Context, hc interp.HandlerContext, up string, fn func(real, user string, d fs.DirEntry)) error {
	root := abs(ctx, hc, up)
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		u := up
		if rel, _ := filepath.Rel(root, p); rel != "." {
			u = fcJoin(up, filepath.ToSlash(rel))
		}
		fn(p, u, d)
		return ctx.Err()
	})
}

// ---- cp, mv, ln ----

type fcCopier struct {
	ctx                                        context.Context
	hc                                         interp.HandlerContext
	name                                       string
	recursive, keep, force, noClobber, verbose bool
	noFollow                                   bool
	code                                       int
}

func (c *fcCopier) errf(format string, a ...any) {
	fmt.Fprintf(c.hc.Stderr, c.name+": "+format+"\n", a...)
	c.code = 1
}

// copy copies the user path s to d.
func (c *fcCopier) copy(s, d string, top bool) {
	if c.ctx.Err() != nil {
		return
	}
	src, dst := abs(c.ctx, c.hc, s), abs(c.ctx, c.hc, d)
	var fi os.FileInfo
	var err error
	if c.noFollow || !top {
		fi, err = os.Lstat(src)
	} else {
		fi, err = os.Stat(src)
	}
	if err != nil {
		c.errf("cannot stat '%s': %s", s, fcErr(err))
		return
	}
	di, derr := os.Lstat(dst)
	if derr == nil {
		if os.SameFile(fi, di) {
			c.errf("'%s' and '%s' are the same file", s, d)
			return
		}
		if c.noClobber {
			return
		}
	}
	switch {
	case fi.IsDir():
		if !c.recursive {
			c.errf("-r not specified; omitting directory '%s'", s)
			return
		}
		if dst == src || strings.HasPrefix(dst, strings.TrimRight(src, "/")+"/") {
			c.errf("cannot copy a directory, '%s', into itself, '%s'", s, d)
			return
		}
		if derr == nil && !di.IsDir() {
			c.errf("cannot overwrite non-directory '%s' with directory '%s'", d, s)
			return
		}
		if c.verbose {
			fmt.Fprintf(c.hc.Stdout, "'%s' -> '%s'\n", s, d)
		}
		if derr != nil {
			err := createAs(c.ctx, c.hc, d)
			if err == nil {
				err = os.Mkdir(dst, 0o755)
			}
			if err != nil {
				c.errf("cannot create directory '%s': %s", d, fcErr(err))
				return
			}
			fsOwn(c.ctx, c.hc.Dir, d)
		}
		ents, err := readDirAs(c.ctx, c.hc, s)
		if err != nil {
			c.errf("cannot open directory '%s': %s", s, fcErr(err))
			return
		}
		for _, e := range ents {
			c.copy(fcJoin(s, e.Name()), fcJoin(d, e.Name()), false)
		}
		if derr != nil || c.keep {
			os.Chmod(dst, fcFileMode(fcPerm(fi.Mode())))
		}
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := os.Readlink(src)
		if err == nil {
			err = createAs(c.ctx, c.hc, d)
		}
		if err == nil {
			if derr == nil {
				os.Remove(dst)
			}
			if err = os.Symlink(t, dst); err == nil {
				fsOwn(c.ctx, c.hc.Dir, d)
			}
		}
		if err != nil {
			c.errf("cannot create symbolic link '%s': %s", d, fcErr(err))
			return
		}
		if c.verbose {
			fmt.Fprintf(c.hc.Stdout, "'%s' -> '%s'\n", s, d)
		}
		return
	case fi.Mode().IsRegular():
		if derr == nil && di.IsDir() {
			c.errf("cannot overwrite directory '%s' with non-directory", d)
			return
		}
		in, err := filesOpen(c.ctx, c.hc, s, os.O_RDONLY, 0)
		if err != nil {
			c.errf("cannot open '%s' for reading: %s", s, fcErr(err))
			return
		}
		defer in.Close()
		out, err := filesOpen(c.ctx, c.hc, d, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
		if err != nil && c.force && removeCheck(c.ctx, c.hc, d) == nil {
			os.Remove(dst)
			out, err = filesOpen(c.ctx, c.hc, d, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
		}
		if err != nil {
			c.errf("cannot create regular file '%s': %s", d, fcErr(err))
			return
		}
		_, err = io.Copy(out, in)
		out.Close()
		if err != nil {
			c.errf("error writing '%s': %s", d, fcErr(err))
			return
		}
		if derr != nil {
			os.Chmod(dst, fcFileMode(fcPerm(fi.Mode())))
		}
		if c.verbose {
			fmt.Fprintf(c.hc.Stdout, "'%s' -> '%s'\n", s, d)
		}
	default:
		c.errf("cannot copy '%s': not a regular file", s)
		return
	}
	if c.keep {
		os.Chmod(dst, fcFileMode(fcPerm(fi.Mode())))
		os.Chtimes(dst, fi.ModTime(), fi.ModTime())
	}
}

// fcPairs turns "src... dst" into (source, destination) user paths.
func fcPairs(ctx context.Context, hc interp.HandlerContext, name string, args []string) ([][2]string, error) {
	if len(args) < 2 {
		if len(args) == 1 {
			return nil, fail(hc, 1, "%s: missing destination file operand after '%s'", name, args[0])
		}
		return nil, fail(hc, 1, "%s: missing file operand", name)
	}
	dst, srcs := args[len(args)-1], args[:len(args)-1]
	var pairs [][2]string
	if fcIsDir(abs(ctx, hc, dst)) {
		for _, s := range srcs {
			pairs = append(pairs, [2]string{s, fcJoin(dst, fcBase(s))})
		}
		return pairs, nil
	}
	if len(srcs) > 1 {
		return nil, fail(hc, 1, "%s: target '%s' is not a directory", name, dst)
	}
	return [][2]string{{srcs[0], dst}}, nil
}

// cpCmd supports -r -R -a -p -f -n -v (and -i is ignored).
func cpCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("cp", args, "rRapfnvi", map[string]string{"recursive": "r", "archive": "a", "preserve": "p", "force": "f", "no-clobber": "n", "verbose": "v"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	pairs, err := fcPairs(ctx, hc, "cp", args)
	if err != nil {
		return err
	}
	c := &fcCopier{ctx: ctx, hc: hc, name: "cp", recursive: has(o, "r") || has(o, "R") || has(o, "a"),
		keep: has(o, "a") || has(o, "p"), force: has(o, "f"), noClobber: has(o, "n"), verbose: has(o, "v"), noFollow: has(o, "a")}
	for _, p := range pairs {
		c.copy(p[0], p[1], true)
	}
	return exitCode(c.code)
}

// mvCmd supports -f -n -v; it renames and falls back to copy and remove.
func mvCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("mv", args, "fnvi", map[string]string{"force": "f", "no-clobber": "n", "verbose": "v"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	pairs, err := fcPairs(ctx, hc, "mv", args)
	if err != nil {
		return err
	}
	code := 0
	for _, p := range pairs {
		s, d := p[0], p[1]
		sr, dr := abs(ctx, hc, s), abs(ctx, hc, d)
		si, err := os.Lstat(sr)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "mv: cannot stat '%s': %s\n", s, fcErr(err))
			code = 1
			continue
		}
		if di, err := os.Lstat(dr); err == nil {
			if has(o, "n") {
				continue
			}
			if os.SameFile(si, di) {
				fmt.Fprintf(hc.Stderr, "mv: '%s' and '%s' are the same file\n", s, d)
				code = 1
				continue
			}
		}
		if si.IsDir() && strings.HasPrefix(dr, strings.TrimRight(sr, "/")+"/") {
			fmt.Fprintf(hc.Stderr, "mv: cannot move '%s' to a subdirectory of itself, '%s'\n", s, d)
			code = 1
			continue
		}
		err = removeCheck(ctx, hc, s)
		if err == nil {
			err = createAs(ctx, hc, d)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "mv: cannot move '%s' to '%s': %s\n", s, d, fcErr(err))
			code = 1
			continue
		}
		if err := os.Rename(sr, dr); err != nil {
			c := &fcCopier{ctx: ctx, hc: hc, name: "mv", recursive: true, keep: true, noFollow: true}
			c.copy(s, d, true)
			if c.code == 0 {
				err = os.RemoveAll(sr)
			}
			if c.code != 0 || err != nil {
				code = 1
				continue
			}
		}
		if has(o, "v") {
			fmt.Fprintf(hc.Stdout, "renamed '%s' -> '%s'\n", s, d)
		}
	}
	return exitCode(code)
}

// lnCmd supports -s -f -n -v -T; hard links need a file system that has them.
func lnCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("ln", args, "sfnvT", map[string]string{"symbolic": "s", "force": "f", "no-dereference": "n", "verbose": "v"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(args) == 0 {
		return fail(hc, 1, "ln: missing file operand")
	}
	if len(args) == 1 {
		args = append(args, fcBase(args[0]))
	}
	dst, targets := args[len(args)-1], args[:len(args)-1]
	dr := abs(ctx, hc, dst)
	isDir := fcIsDir(dr) && !has(o, "T")
	if li, err := os.Lstat(dr); has(o, "n") && err == nil && li.Mode()&fs.ModeSymlink != 0 {
		isDir = false
	}
	if len(targets) > 1 && !isDir {
		return fail(hc, 1, "ln: target '%s' is not a directory", dst)
	}
	code := 0
	for _, t := range targets {
		l := dst
		if isDir {
			l = fcJoin(dst, fcBase(t))
		}
		lr := abs(ctx, hc, l)
		kind := "hard"
		if has(o, "s") {
			kind = "symbolic"
		}
		if _, err := os.Lstat(lr); err == nil {
			if !has(o, "f") {
				fmt.Fprintf(hc.Stderr, "ln: failed to create %s link '%s': File exists\n", kind, l)
				code = 1
				continue
			}
			if removeCheck(ctx, hc, l) == nil {
				os.Remove(lr)
			}
		}
		if err = createAs(ctx, hc, l); err != nil {
		} else if has(o, "s") {
			err = os.Symlink(t, lr)
		} else {
			err = os.Link(abs(ctx, hc, t), lr)
		}
		if err == nil && has(o, "s") {
			fsOwn(ctx, hc.Dir, l)
		}
		if err != nil {
			if kind == "hard" {
				fmt.Fprintf(hc.Stderr, "ln: failed to create hard link '%s' => '%s': %s\n", l, t, fcErr(err))
			} else {
				fmt.Fprintf(hc.Stderr, "ln: failed to create symbolic link '%s': %s\n", l, fcErr(err))
			}
			code = 1
			continue
		}
		if has(o, "v") {
			arrow := "=>"
			if kind == "symbolic" {
				arrow = "->"
			}
			fmt.Fprintf(hc.Stdout, "'%s' %s '%s'\n", l, arrow, t)
		}
	}
	return exitCode(code)
}

// ---- chmod, chown, chgrp ----

// fcSymMode applies a symbolic mode like u+x,go-w,a=r to cur.
func fcSymMode(spec string, cur uint32, dir bool) (uint32, bool) {
	for _, clause := range strings.Split(spec, ",") {
		i := 0
		var who uint32
		for i < len(clause) && strings.IndexByte("ugoa", clause[i]) >= 0 {
			who |= map[byte]uint32{'u': 1, 'g': 2, 'o': 4, 'a': 7}[clause[i]]
			i++
		}
		explicit := who != 0
		if !explicit {
			who = 7
		}
		if i >= len(clause) {
			return 0, false
		}
		for i < len(clause) {
			op := clause[i]
			if op != '+' && op != '-' && op != '=' {
				return 0, false
			}
			i++
			j := i
			for j < len(clause) && clause[j] != '+' && clause[j] != '-' && clause[j] != '=' {
				j++
			}
			var perm, special uint32
			for _, ch := range clause[i:j] {
				switch ch {
				case 'r':
					perm |= 4
				case 'w':
					perm |= 2
				case 'x':
					perm |= 1
				case 'X':
					if dir || cur&0o111 != 0 {
						perm |= 1
					}
				case 's':
					if who&1 != 0 {
						special |= 0o4000
					}
					if who&2 != 0 {
						special |= 0o2000
					}
				case 't':
					special |= 0o1000
				case 'u':
					perm |= cur >> 6 & 7
				case 'g':
					perm |= cur >> 3 & 7
				case 'o':
					perm |= cur & 7
				default:
					return 0, false
				}
			}
			i = j
			var class, clear uint32
			for n, shift := range []uint{6, 3, 0} {
				if who&(1<<n) != 0 {
					class |= perm << shift
					clear |= 7 << shift
				}
			}
			if !explicit && op != '-' {
				class &^= 0o022
			}
			if who&1 != 0 {
				clear |= 0o4000
			}
			if who&2 != 0 {
				clear |= 0o2000
			}
			bits := class | special
			switch op {
			case '+':
				cur |= bits
			case '-':
				cur &^= bits
			case '=':
				cur = cur&^clear | bits
			}
		}
	}
	return cur, true
}

// chmodCmd supports octal and symbolic modes, -R (and -v -c -f are accepted).
func chmodCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	rec, quiet := false, false
	for len(args) > 0 {
		a := args[0]
		if a == "--" {
			args = args[1:]
			break
		}
		if a == "--recursive" {
			rec = true
		} else if len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "Rvcf") == "" {
			rec = rec || strings.Contains(a, "R")
			quiet = quiet || strings.Contains(a, "f")
		} else {
			break
		}
		args = args[1:]
	}
	if len(args) < 2 {
		if len(args) == 1 {
			return fail(hc, 1, "chmod: missing operand after '%s'", args[0])
		}
		return fail(hc, 1, "chmod: missing operand")
	}
	spec, files := args[0], args[1:]
	octal, oerr := strconv.ParseUint(spec, 8, 32)
	if oerr == nil && octal > 0o7777 {
		oerr = fmt.Errorf("big")
	}
	if oerr != nil {
		if _, ok := fcSymMode(spec, 0, false); !ok {
			return fail(hc, 1, "chmod: invalid mode: '%s'", spec)
		}
	}
	code := 0
	apply := func(real, user string) {
		fi, err := os.Stat(real)
		if err == nil {
			err = attrCheck(ctx, hc, user, capFowner, -1, -1)
		}
		if err == nil {
			m := uint32(octal)
			if oerr != nil {
				m, _ = fcSymMode(spec, fcPerm(fi.Mode()), fi.IsDir())
			}
			err = os.Chmod(real, fcFileMode(m))
		}
		if err != nil && !quiet {
			fmt.Fprintf(hc.Stderr, "chmod: changing permissions of '%s': %s\n", user, fcErr(err))
			code = 1
		}
	}
	for _, f := range files {
		if _, err := os.Stat(abs(ctx, hc, f)); err != nil {
			if !quiet {
				fmt.Fprintf(hc.Stderr, "chmod: cannot access '%s': %s\n", f, fcErr(err))
				code = 1
			}
			continue
		}
		if !rec {
			apply(abs(ctx, hc, f), f)
			continue
		}
		fcWalk(ctx, hc, f, func(real, user string, d fs.DirEntry) {
			if d.Type()&fs.ModeSymlink == 0 || user == f {
				apply(real, user)
			}
		})
	}
	return exitCode(code)
}

// fcDB reads /etc/passwd or /etc/group of the (container) file system: the
// fields of each line.
func fcDB(ctx context.Context, hc interp.HandlerContext, file string) [][]string {
	b, err := filesRead(ctx, hc, file)
	if err != nil {
		return nil
	}
	var rows [][]string
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Split(l, ":"); len(f) >= 3 && !strings.HasPrefix(l, "#") {
			rows = append(rows, f)
		}
	}
	return rows
}

// fcLookup maps a name or number to an id using /etc/passwd or /etc/group;
// gid is the primary group of a user.
func fcLookup(ctx context.Context, hc interp.HandlerContext, file, s string) (id, gid int, ok bool) {
	for _, r := range fcDB(ctx, hc, file) {
		if r[0] == s {
			id, _ = strconv.Atoi(r[2])
			gid = id
			if len(r) > 3 {
				gid, _ = strconv.Atoi(r[3])
			}
			return id, gid, true
		}
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n, n, true
	}
	return 0, 0, false
}

// fcName maps an id to a name or "UNKNOWN". The terminal has no
// /etc/passwd; its files belong to root there.
func fcName(rows [][]string, id int) string {
	for _, r := range rows {
		if r[2] == strconv.Itoa(id) {
			return r[0]
		}
	}
	if id == 0 && len(rows) == 0 {
		return "root"
	}
	return "UNKNOWN"
}

func chownCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	return fcChown(ctx, hc, "chown", args)
}

func chgrpCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	return fcChown(ctx, hc, "chgrp", args)
}

// fcChown implements chown (user[:group], user:, :group) and chgrp (group)
// with -R and -h; the ids are only stored.
func fcChown(ctx context.Context, hc interp.HandlerContext, name string, args []string) error {
	o, args, err := opts(name, args, "RhfvcHLP", map[string]string{"recursive": "R", "no-dereference": "h", "quiet": "f", "silent": "f", "verbose": "v"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(args) < 2 {
		return fail(hc, 1, "%s: missing operand", name)
	}
	spec, files := args[0], args[1:]
	uid, gid := -1, -1
	user, group := spec, ""
	if name == "chgrp" {
		user, group = "", spec
	} else if i := strings.IndexAny(spec, ":"); i >= 0 {
		user, group = spec[:i], spec[i+1:]
	} else if _, _, ok := fcLookup(ctx, hc, "/etc/passwd", spec); !ok && strings.Contains(spec, ".") {
		user, group, _ = strings.Cut(spec, ".")
	}
	if user != "" {
		id, pg, ok := fcLookup(ctx, hc, "/etc/passwd", user)
		if !ok {
			return fail(hc, 1, "%s: invalid user: '%s'", name, spec)
		}
		uid = id
		if strings.Contains(spec, ":") && group == "" {
			gid = pg
		}
	}
	if group != "" {
		id, _, ok := fcLookup(ctx, hc, "/etc/group", group)
		if !ok {
			return fail(hc, 1, "%s: invalid group: '%s'", name, spec)
		}
		gid = id
	}
	code := 0
	apply := func(real, user string, link bool) {
		err := attrCheck(ctx, hc, user, capChown, uid, gid)
		if err != nil {
		} else if link {
			err = os.Lchown(real, uid, gid)
		} else {
			err = os.Chown(real, uid, gid)
		}
		if err != nil && !has(o, "f") {
			fmt.Fprintf(hc.Stderr, "%s: changing ownership of '%s': %s\n", name, user, fcErr(err))
			code = 1
		}
	}
	for _, f := range files {
		if _, err := os.Lstat(abs(ctx, hc, f)); err != nil {
			if !has(o, "f") {
				fmt.Fprintf(hc.Stderr, "%s: cannot access '%s': %s\n", name, f, fcErr(err))
				code = 1
			}
			continue
		}
		if !has(o, "R") {
			apply(abs(ctx, hc, f), f, has(o, "h"))
			continue
		}
		fcWalk(ctx, hc, f, func(real, user string, d fs.DirEntry) {
			apply(real, user, d.Type()&fs.ModeSymlink != 0 && (user != f || has(o, "h")))
		})
	}
	return exitCode(code)
}

// ---- stat ----

type fcStatInfo struct {
	name, link                   string
	fi                           os.FileInfo
	mode                         uint32
	size, blocks, blksize, nlink int64
	ino, dev                     uint64
	uid, gid                     int
	atime, mtime, ctime          time.Time
}

func fcStatOf(name string, fi os.FileInfo) *fcStatInfo {
	s := &fcStatInfo{name: name, fi: fi, mode: fcPerm(fi.Mode()), size: fi.Size(), nlink: 1, blksize: 4096}
	s.mtime = fi.ModTime()
	s.atime, s.ctime = s.mtime, s.mtime
	s.blocks = (s.size + 4095) / 4096 * 8
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		s.uid, s.gid, s.ino, s.dev, s.nlink = int(st.Uid), int(st.Gid), st.Ino, uint64(st.Dev), int64(st.Nlink)
		s.atime = time.Unix(st.Atime, st.AtimeNsec)
		s.ctime = time.Unix(st.Ctime, st.CtimeNsec)
		if st.Blksize > 0 {
			s.blksize = int64(st.Blksize)
		}
		s.blocks = int64(st.Blocks)
	}
	return s
}

func fcModeString(m fs.FileMode) string {
	b := []byte("----------")
	switch {
	case m&fs.ModeDir != 0:
		b[0] = 'd'
	case m&fs.ModeSymlink != 0:
		b[0] = 'l'
	case m&fs.ModeCharDevice != 0:
		b[0] = 'c'
	case m&fs.ModeDevice != 0:
		b[0] = 'b'
	case m&fs.ModeNamedPipe != 0:
		b[0] = 'p'
	case m&fs.ModeSocket != 0:
		b[0] = 's'
	}
	for i, c := range "rwxrwxrwx" {
		if m&(1<<(8-i)) != 0 {
			b[i+1] = byte(c)
		}
	}
	set := func(bit fs.FileMode, i int, lo, up byte) {
		if m&bit != 0 {
			if b[i] == 'x' {
				b[i] = lo
			} else {
				b[i] = up
			}
		}
	}
	set(fs.ModeSetuid, 3, 's', 'S')
	set(fs.ModeSetgid, 6, 's', 'S')
	set(fs.ModeSticky, 9, 't', 'T')
	return string(b)
}

func (s *fcStatInfo) kind() string {
	m := s.fi.Mode()
	switch {
	case m.IsDir():
		return "directory"
	case m&fs.ModeSymlink != 0:
		return "symbolic link"
	case m&fs.ModeCharDevice != 0:
		return "character special file"
	case m&fs.ModeDevice != 0:
		return "block special file"
	case m&fs.ModeNamedPipe != 0:
		return "fifo"
	case m&fs.ModeSocket != 0:
		return "socket"
	case s.size == 0:
		return "regular empty file"
	}
	return "regular file"
}

func fcTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05.000000000 -0700") }

// field returns the value of one % directive.
func (s *fcStatInfo) field(c byte, users, groups [][]string) (string, bool) {
	switch c {
	case 'n':
		return s.name, true
	case 'N':
		if s.link != "" {
			return "'" + s.name + "' -> '" + s.link + "'", true
		}
		return "'" + s.name + "'", true
	case 's':
		return strconv.FormatInt(s.size, 10), true
	case 'a':
		return strconv.FormatUint(uint64(s.mode), 8), true
	case 'A':
		return fcModeString(s.fi.Mode()), true
	case 'f':
		return strconv.FormatUint(uint64(s.mode), 16), true
	case 'u':
		return strconv.Itoa(s.uid), true
	case 'U':
		return fcName(users, s.uid), true
	case 'g':
		return strconv.Itoa(s.gid), true
	case 'G':
		return fcName(groups, s.gid), true
	case 'F':
		return s.kind(), true
	case 'y':
		return fcTime(s.mtime), true
	case 'Y':
		return strconv.FormatInt(s.mtime.Unix(), 10), true
	case 'x':
		return fcTime(s.atime), true
	case 'X':
		return strconv.FormatInt(s.atime.Unix(), 10), true
	case 'z':
		return fcTime(s.ctime), true
	case 'Z':
		return strconv.FormatInt(s.ctime.Unix(), 10), true
	case 'w':
		return "-", true
	case 'W':
		return "0", true
	case 'h':
		return strconv.FormatInt(s.nlink, 10), true
	case 'i':
		return strconv.FormatUint(s.ino, 10), true
	case 'b':
		return strconv.FormatInt(s.blocks, 10), true
	case 'B':
		return "512", true
	case 'o':
		return strconv.FormatInt(s.blksize, 10), true
	case 'd':
		return strconv.FormatUint(s.dev, 10), true
	case 'D':
		return strconv.FormatUint(s.dev, 16), true
	case '%':
		return "%", true
	}
	return "", false
}

func (s *fcStatInfo) format(f string, escapes bool, users, groups [][]string) string {
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		c := f[i]
		if c == '\\' && escapes && i+1 < len(f) {
			i++
			switch f[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte(7)
			case 'b':
				b.WriteByte(8)
			case 'f':
				b.WriteByte(12)
			case 'v':
				b.WriteByte(11)
			case '0', '1', '2', '3', '4', '5', '6', '7':
				j := i
				for j < len(f) && j < i+3 && f[j] >= '0' && f[j] <= '7' {
					j++
				}
				n, _ := strconv.ParseUint(f[i:j], 8, 8)
				b.WriteByte(byte(n))
				i = j - 1
			default:
				b.WriteByte(f[i])
			}
			continue
		}
		if c != '%' || i+1 >= len(f) {
			b.WriteByte(c)
			continue
		}
		j := i + 1
		for j < len(f) && (f[j] == '-' || f[j] >= '0' && f[j] <= '9') {
			j++
		}
		if j >= len(f) {
			b.WriteString(f[i:])
			break
		}
		v, ok := s.field(f[j], users, groups)
		if !ok {
			b.WriteString(f[i : j+1])
		} else {
			b.WriteString(fmt.Sprintf("%"+f[i+1:j]+"s", v))
		}
		i = j
	}
	return b.String()
}

func fcDevNum(dev uint64) (uint64, uint64) {
	return (dev>>8)&0xfff | (dev>>32)&^0xfff, dev&0xff | (dev>>12)&^0xff
}

// statCmd supports -L, -c/--format and --printf with the common directives;
// the default output is GNU's.
func statCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("stat", args, "Lc:p:", map[string]string{"dereference": "L", "format": "c", "printf": "p"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(args) == 0 {
		return fail(hc, 1, "stat: missing operand")
	}
	users, groups := fcDB(ctx, hc, "/etc/passwd"), fcDB(ctx, hc, "/etc/group")
	code := 0
	for _, a := range args {
		var fi os.FileInfo
		var err error
		if has(o, "L") {
			fi, err = os.Stat(abs(ctx, hc, a))
		} else {
			fi, err = os.Lstat(abs(ctx, hc, a))
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "stat: cannot statx '%s': %s\n", a, fcErr(err))
			code = 1
			continue
		}
		s := fcStatOf(a, fi)
		if fi.Mode()&fs.ModeSymlink != 0 {
			s.link, _ = os.Readlink(abs(ctx, hc, a))
		}
		if f, ok := o["p"]; ok {
			fmt.Fprint(hc.Stdout, s.format(f, true, users, groups))
		} else if f, ok := o["c"]; ok {
			fmt.Fprintln(hc.Stdout, s.format(f, false, users, groups))
		} else {
			name := a
			if s.link != "" {
				name += " -> " + s.link
			}
			maj, min := fcDevNum(s.dev)
			fmt.Fprintf(hc.Stdout, "  File: %s\n  Size: %-10d\tBlocks: %-10d IO Block: %-6d %s\n", name, s.size, s.blocks, s.blksize, s.kind())
			fmt.Fprintf(hc.Stdout, "Device: %d,%d\tInode: %-10d  Links: %-5d\n", maj, min, s.ino, s.nlink)
			fmt.Fprintf(hc.Stdout, "Access: (%04o/%s)  Uid: (%5d/%8s)   Gid: (%5d/%8s)\n", s.mode, fcModeString(fi.Mode()), s.uid, fcName(users, s.uid), s.gid, fcName(groups, s.gid))
			fmt.Fprintf(hc.Stdout, "Access: %s\nModify: %s\nChange: %s\n Birth: -\n", fcTime(s.atime), fcTime(s.mtime), fcTime(s.ctime))
		}
	}
	return exitCode(code)
}

// ---- find ----

type fcEnt struct {
	user, real string
	fi         os.FileInfo
}

type fcBatch struct {
	argv  []string
	paths []string
}

type fcFind struct {
	ctx        context.Context
	hc         interp.HandlerContext
	exec       interp.ExecHandlerFunc
	now        time.Time
	toks       []string
	pos        int
	err        string
	maxDepth   int
	minDepth   int
	depthFirst bool
	action     bool
	prune      bool
	code       int
	batches    []*fcBatch
	expr       func(*fcEnt) bool
}

// fcGlob compiles a shell pattern (* ? [..]) to an anchored regexp.
func fcGlob(pat string, ci bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("(?s)^")
	if ci {
		b.WriteString("(?i)")
	}
	for i := 0; i < len(pat); i++ {
		switch c := pat[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteByte('.')
		case '[':
			j := i + 1
			if j < len(pat) && (pat[j] == '!' || pat[j] == '^') {
				j++
			}
			if j < len(pat) && pat[j] == ']' {
				j++
			}
			for j < len(pat) && pat[j] != ']' {
				j++
			}
			if j >= len(pat) {
				b.WriteString(`\[`)
				continue
			}
			class := pat[i+1 : j]
			if class[0] == '!' {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i = j
		case '\\':
			if i+1 < len(pat) {
				i++
			}
			b.WriteString(regexp.QuoteMeta(pat[i : i+1]))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return regexp.MustCompile("^$")
	}
	return re
}

// fcCmp parses [+-]N into a comparison.
func fcCmp(s string) (func(int64) bool, bool) {
	op := byte('=')
	if s != "" && (s[0] == '+' || s[0] == '-') {
		op, s = s[0], s[1:]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, false
	}
	return func(v int64) bool {
		switch op {
		case '+':
			return v > n
		case '-':
			return v < n
		}
		return v == n
	}, true
}

func (f *fcFind) peek() string {
	if f.pos < len(f.toks) {
		return f.toks[f.pos]
	}
	return ""
}

func (f *fcFind) bad(format string, a ...any) func(*fcEnt) bool {
	if f.err == "" {
		f.err = fmt.Sprintf(format, a...)
	}
	return func(*fcEnt) bool { return false }
}

func (f *fcFind) arg(pred string) (string, bool) {
	if f.pos >= len(f.toks) {
		f.bad("missing argument to `%s'", pred)
		return "", false
	}
	f.pos++
	return f.toks[f.pos-1], true
}

func (f *fcFind) orExpr() func(*fcEnt) bool { return f.orRest(f.andExpr()) }

func (f *fcFind) orRest(l func(*fcEnt) bool) func(*fcEnt) bool {
	for f.err == "" && (f.peek() == "-o" || f.peek() == "-or") {
		f.pos++
		r, l0 := f.andExpr(), l
		l = func(e *fcEnt) bool { return l0(e) || r(e) }
	}
	return l
}

func (f *fcFind) andExpr() func(*fcEnt) bool {
	l := f.notExpr()
	for f.err == "" && f.pos < len(f.toks) {
		t := f.peek()
		if t == "-o" || t == "-or" || t == ")" {
			break
		}
		if t == "-a" || t == "-and" {
			f.pos++
		}
		r, l0 := f.notExpr(), l
		l = func(e *fcEnt) bool { return l0(e) && r(e) }
	}
	return l
}

func (f *fcFind) notExpr() func(*fcEnt) bool {
	if t := f.peek(); t == "!" || t == "-not" {
		f.pos++
		x := f.notExpr()
		return func(e *fcEnt) bool { return !x(e) }
	}
	return f.primary()
}

func (f *fcFind) primary() func(*fcEnt) bool {
	if f.pos >= len(f.toks) {
		return f.bad("invalid expression; expected an expression")
	}
	t := f.toks[f.pos]
	f.pos++
	switch t {
	case "(":
		x := f.orExpr()
		if f.peek() != ")" {
			return f.bad("invalid expression; I was expecting to find a ')' somewhere but did not see one.")
		}
		f.pos++
		return x
	case "-true":
		return func(*fcEnt) bool { return true }
	case "-false":
		return func(*fcEnt) bool { return false }
	case "-prune":
		return func(*fcEnt) bool { f.prune = true; return true }
	case "-depth":
		f.depthFirst = true
		return func(*fcEnt) bool { return true }
	case "-print":
		f.action = true
		return func(e *fcEnt) bool { fmt.Fprintln(f.hc.Stdout, e.user); return true }
	case "-print0":
		f.action = true
		return func(e *fcEnt) bool { fmt.Fprint(f.hc.Stdout, e.user+"\x00"); return true }
	case "-delete":
		f.action, f.depthFirst = true, true
		return func(e *fcEnt) bool {
			err := removeCheck(f.ctx, f.hc, e.user)
			if err == nil {
				err = os.Remove(e.real)
			}
			if err != nil {
				fmt.Fprintf(f.hc.Stderr, "find: cannot delete '%s': %s\n", e.user, fcErr(err))
				f.code = 1
				return false
			}
			return true
		}
	case "-empty":
		return func(e *fcEnt) bool {
			if e.fi.IsDir() {
				ents, err := os.ReadDir(e.real)
				return err == nil && len(ents) == 0
			}
			return e.fi.Mode().IsRegular() && e.fi.Size() == 0
		}
	case "-exec":
		f.action = true
		var cmd []string
		for f.pos < len(f.toks) && f.toks[f.pos] != ";" && !(f.toks[f.pos] == "+" && len(cmd) > 0 && cmd[len(cmd)-1] == "{}") {
			cmd = append(cmd, f.toks[f.pos])
			f.pos++
		}
		if f.pos >= len(f.toks) || len(cmd) == 0 {
			return f.bad("missing argument to `-exec'")
		}
		plus := f.toks[f.pos] == "+"
		f.pos++
		if plus {
			b := &fcBatch{argv: cmd[:len(cmd)-1]}
			f.batches = append(f.batches, b)
			return func(e *fcEnt) bool { b.paths = append(b.paths, e.user); return true }
		}
		return func(e *fcEnt) bool {
			argv := make([]string, len(cmd))
			for i, a := range cmd {
				argv[i] = strings.ReplaceAll(a, "{}", e.user)
			}
			return f.run(argv)
		}
	}
	// predicates with one argument
	switch t {
	case "-name", "-iname", "-path", "-ipath", "-wholename", "-type", "-size", "-mtime", "-mmin", "-atime", "-ctime", "-newer", "-perm", "-maxdepth", "-mindepth":
	default:
		return f.bad("unknown predicate `%s'", t)
	}
	a, ok := f.arg(t)
	if !ok {
		return func(*fcEnt) bool { return false }
	}
	switch t {
	case "-name", "-iname":
		re := fcGlob(a, t == "-iname")
		return func(e *fcEnt) bool { return re.MatchString(path.Base(e.user)) }
	case "-path", "-ipath", "-wholename":
		re := fcGlob(a, t == "-ipath")
		return func(e *fcEnt) bool { return re.MatchString(e.user) }
	case "-type":
		return func(e *fcEnt) bool {
			m := e.fi.Mode()
			switch a {
			case "f":
				return m.IsRegular()
			case "d":
				return m.IsDir()
			case "l":
				return m&fs.ModeSymlink != 0
			case "p":
				return m&fs.ModeNamedPipe != 0
			case "s":
				return m&fs.ModeSocket != 0
			case "c":
				return m&fs.ModeCharDevice != 0
			case "b":
				return m&fs.ModeDevice != 0 && m&fs.ModeCharDevice == 0
			}
			return false
		}
	case "-size":
		unit := int64(512)
		num := a
		if n := len(a); n > 0 && strings.IndexByte("cwbkMG", a[n-1]) >= 0 {
			unit = map[byte]int64{'c': 1, 'w': 2, 'b': 512, 'k': 1 << 10, 'M': 1 << 20, 'G': 1 << 30}[a[n-1]]
			num = a[:n-1]
		}
		cmp, ok := fcCmp(num)
		if !ok {
			return f.bad("invalid argument `%s' to `-size'", a)
		}
		return func(e *fcEnt) bool { return cmp((e.fi.Size() + unit - 1) / unit) }
	case "-mtime", "-mmin", "-atime", "-ctime":
		cmp, ok := fcCmp(a)
		if !ok {
			return f.bad("invalid argument `%s' to `%s'", a, t)
		}
		unit := 24 * time.Hour
		if t == "-mmin" {
			unit = time.Minute
		}
		return func(e *fcEnt) bool {
			s := fcStatOf("", e.fi)
			tm := s.mtime
			if t == "-atime" {
				tm = s.atime
			} else if t == "-ctime" {
				tm = s.ctime
			}
			return cmp(int64(f.now.Sub(tm) / unit))
		}
	case "-newer":
		fi, err := os.Stat(abs(f.ctx, f.hc, a))
		if err != nil {
			return f.bad("'%s': %s", a, fcErr(err))
		}
		return func(e *fcEnt) bool { return e.fi.ModTime().After(fi.ModTime()) }
	case "-perm":
		mode := a
		kind := byte('=')
		if mode != "" && (mode[0] == '-' || mode[0] == '/') {
			kind, mode = mode[0], mode[1:]
		}
		n, err := strconv.ParseUint(mode, 8, 32)
		if err != nil {
			return f.bad("invalid mode `%s'", a)
		}
		return func(e *fcEnt) bool {
			p := uint64(fcPerm(e.fi.Mode()))
			switch kind {
			case '-':
				return p&n == n
			case '/':
				return p&n != 0
			}
			return p == n
		}
	default: // -maxdepth, -mindepth
		n, err := strconv.Atoi(a)
		if err != nil || n < 0 {
			return f.bad("Expected a positive decimal integer argument to `%s', but got `%s'", t, a)
		}
		if t == "-maxdepth" {
			f.maxDepth = n
		} else {
			f.minDepth = n
		}
		return func(*fcEnt) bool { return true }
	}
}

// run executes a command through the shell's exec handler.
func (f *fcFind) run(argv []string) bool {
	err := f.exec(f.ctx, argv)
	if err == nil {
		return true
	}
	if _, ok := interp.IsExitStatus(err); !ok {
		fmt.Fprintf(f.hc.Stderr, "find: '%s': %v\n", argv[0], err)
	}
	return false
}

func (f *fcFind) walk(user, real string, depth int) {
	if f.ctx.Err() != nil {
		return
	}
	fi, err := os.Lstat(real)
	if err != nil {
		fmt.Fprintf(f.hc.Stderr, "find: '%s': %s\n", user, fcErr(err))
		f.code = 1
		return
	}
	e := &fcEnt{user, real, fi}
	eval := depth >= f.minDepth
	f.prune = false
	if eval && !f.depthFirst {
		f.expr(e)
	}
	if fi.IsDir() && depth < f.maxDepth && !f.prune {
		ents, err := os.ReadDir(real)
		if err != nil {
			fmt.Fprintf(f.hc.Stderr, "find: '%s': %s\n", user, fcErr(err))
			f.code = 1
		}
		for _, d := range ents {
			f.walk(fcJoin(user, d.Name()), filepath.Join(real, d.Name()), depth+1)
		}
	}
	if eval && f.depthFirst {
		f.expr(e)
	}
}

// findCmd supports -name -iname -path -ipath -type -maxdepth -mindepth -size
// -mtime -mmin -atime -ctime -newer -perm -empty -prune -depth -print -print0
// -delete -exec ... ; and + with -not ! -a -o and parentheses.
func findCmd(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	var paths []string
	for len(args) > 0 && (args[0] == "-H" || args[0] == "-L" || args[0] == "-P") {
		args = args[1:]
	}
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") && args[0] != "(" && args[0] != "!" {
		paths = append(paths, args[0])
		args = args[1:]
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	f := &fcFind{ctx: ctx, hc: hc, exec: exec, now: time.Now(), toks: args, maxDepth: 1 << 30}
	f.expr = func(*fcEnt) bool { return true }
	if len(args) > 0 {
		f.expr = f.orExpr()
		if f.err == "" && f.pos < len(f.toks) {
			f.bad("unexpected extra predicate `%s'", f.toks[f.pos])
		}
	}
	if f.err != "" {
		return fail(hc, 1, "find: %s", f.err)
	}
	if !f.action {
		inner := f.expr
		f.expr = func(e *fcEnt) bool {
			if inner(e) {
				fmt.Fprintln(hc.Stdout, e.user)
				return true
			}
			return false
		}
	}
	for _, p := range paths {
		f.walk(p, abs(ctx, hc, p), 0)
	}
	for _, b := range f.batches {
		if len(b.paths) > 0 && !f.run(append(slices.Clone(b.argv), b.paths...)) {
			f.code = 1
		}
	}
	return exitCode(f.code)
}

// ---- diff ----

type fcLine struct {
	s    string
	nonl bool
}

func fcSplit(b []byte) []fcLine {
	var out []fcLine
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			out = append(out, fcLine{string(b), true})
			break
		}
		out = append(out, fcLine{string(b[:i]), false})
		b = b[i+1:]
	}
	return out
}

type fcBlock struct{ as, ae, bs, be int }

// fcMyers returns the edit script of a to b: '=', '-' or '+' per step.
func fcMyers(a, b []fcLine) []byte {
	n, m := len(a), len(b)
	pre := 0
	for pre < n && pre < m && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < n-pre && suf < m-pre && a[n-1-suf] == b[m-1-suf] {
		suf++
	}
	core := fcMyersCore(a[pre:n-suf], b[pre:m-suf])
	ops := bytes.Repeat([]byte{'='}, pre)
	ops = append(ops, core...)
	return append(ops, bytes.Repeat([]byte{'='}, suf)...)
}

func fcMyersCore(a, b []fcLine) []byte {
	n, m := len(a), len(b)
	if n == 0 {
		return bytes.Repeat([]byte{'+'}, m)
	}
	if m == 0 {
		return bytes.Repeat([]byte{'-'}, n)
	}
	max := n + m
	off := max + 1
	v := make([]int, 2*max+3)
	type snap struct {
		lo int
		v  []int
	}
	var trace []snap
	get := func(s snap, k int) int { return s.v[off+k-s.lo] }
	for d := 0; d <= max; d++ {
		lo := off - d - 1
		trace = append(trace, snap{lo, slices.Clone(v[lo : off+d+2])})
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				var ops []byte
				for dd := d; dd >= 0; dd-- {
					s := trace[dd]
					kk := x - y
					var pk int
					if kk == -dd || (kk != dd && get(s, kk-1) < get(s, kk+1)) {
						pk = kk + 1
					} else {
						pk = kk - 1
					}
					px := get(s, pk)
					py := px - pk
					if dd == 0 {
						px, py = 0, 0
					}
					for x > px && y > py {
						ops = append(ops, '=')
						x--
						y--
					}
					if dd > 0 {
						if x == px {
							ops = append(ops, '+')
							y--
						} else {
							ops = append(ops, '-')
							x--
						}
					}
				}
				slices.Reverse(ops)
				return ops
			}
		}
	}
	return nil
}

func fcBlocks(ops []byte) []fcBlock {
	var bl []fcBlock
	ai, bi := 0, 0
	for i := 0; i < len(ops); {
		if ops[i] == '=' {
			ai++
			bi++
			i++
			continue
		}
		blk := fcBlock{as: ai, bs: bi}
		for ; i < len(ops) && ops[i] != '='; i++ {
			if ops[i] == '-' {
				ai++
			} else {
				bi++
			}
		}
		blk.ae, blk.be = ai, bi
		bl = append(bl, blk)
	}
	return bl
}

func fcPut(w io.Writer, prefix string, l fcLine) {
	fmt.Fprintf(w, "%s%s\n", prefix, l.s)
	if l.nonl {
		fmt.Fprintln(w, `\ No newline at end of file`)
	}
}

func fcNR(s, e int) string {
	if e-s == 1 {
		return strconv.Itoa(s + 1)
	}
	return fmt.Sprintf("%d,%d", s+1, e)
}

func fcUR(s, l int) string {
	switch l {
	case 0:
		return fmt.Sprintf("%d,0", s)
	case 1:
		return strconv.Itoa(s + 1)
	}
	return fmt.Sprintf("%d,%d", s+1, l)
}

type fcDiff struct {
	ctx            context.Context
	hc             interp.HandlerContext
	unified        bool
	n              int
	brief, rec, nf bool
	opts           string
	header         string
	code           int
	stdin          []byte
	stdinRead      bool
}

func (c *fcDiff) trouble(format string, a ...any) {
	fmt.Fprintf(c.hc.Stderr, "diff: "+format+"\n", a...)
	c.code = max(c.code, 2)
}

func (c *fcDiff) out(format string, a ...any) {
	if c.header != "" {
		fmt.Fprintln(c.hc.Stdout, c.header)
		c.header = ""
	}
	fmt.Fprintf(c.hc.Stdout, format, a...)
}

func (c *fcDiff) read(name string) ([]byte, time.Time, error) {
	if name == "-" {
		if !c.stdinRead {
			c.stdinRead = true
			if c.hc.Stdin != nil {
				c.stdin, _ = io.ReadAll(c.hc.Stdin)
			}
		}
		return c.stdin, time.Now(), nil
	}
	fi, err := os.Stat(abs(c.ctx, c.hc, name))
	if err != nil {
		return nil, time.Time{}, err
	}
	b, err := filesRead(c.ctx, c.hc, name)
	return b, fi.ModTime(), err
}

func (c *fcDiff) files(ua, ub string, missingA, missingB bool) {
	var a, b []byte
	var ta, tb time.Time
	var err error
	if !missingA {
		if a, ta, err = c.read(ua); err != nil {
			c.trouble("%s: %s", ua, fcErr(err))
			return
		}
	}
	if !missingB {
		if b, tb, err = c.read(ub); err != nil {
			c.trouble("%s: %s", ub, fcErr(err))
			return
		}
	}
	if bytes.Equal(a, b) {
		return
	}
	c.code = max(c.code, 1)
	if bytes.IndexByte(a, 0) >= 0 || bytes.IndexByte(b, 0) >= 0 {
		c.out("Binary files %s and %s differ\n", ua, ub)
		return
	}
	if c.brief {
		c.out("Files %s and %s differ\n", ua, ub)
		return
	}
	la, lb := fcSplit(a), fcSplit(b)
	blocks := fcBlocks(fcMyers(la, lb))
	w := c.hc.Stdout
	if !c.unified {
		c.out("")
		for _, k := range blocks {
			ra, rb := fcNR(k.as, k.ae), fcNR(k.bs, k.be)
			switch {
			case k.as == k.ae:
				fmt.Fprintf(w, "%da%s\n", k.as, rb)
			case k.bs == k.be:
				fmt.Fprintf(w, "%sd%d\n", ra, k.bs)
			default:
				fmt.Fprintf(w, "%sc%s\n", ra, rb)
			}
			for _, l := range la[k.as:k.ae] {
				fcPut(w, "< ", l)
			}
			if k.as != k.ae && k.bs != k.be {
				fmt.Fprintln(w, "---")
			}
			for _, l := range lb[k.bs:k.be] {
				fcPut(w, "> ", l)
			}
		}
		return
	}
	c.out("--- %s\t%s\n+++ %s\t%s\n", ua, fcTime(ta), ub, fcTime(tb))
	for i := 0; i < len(blocks); {
		j := i
		for j+1 < len(blocks) && blocks[j+1].as-blocks[j].ae <= 2*c.n {
			j++
		}
		as := max(blocks[i].as-c.n, 0)
		ae := min(blocks[j].ae+c.n, len(la))
		bs := blocks[i].bs - (blocks[i].as - as)
		be := blocks[j].be + (ae - blocks[j].ae)
		fmt.Fprintf(w, "@@ -%s +%s @@\n", fcUR(as, ae-as), fcUR(bs, be-bs))
		pos := as
		for _, k := range blocks[i : j+1] {
			for _, l := range la[pos:k.as] {
				fcPut(w, " ", l)
			}
			for _, l := range la[k.as:k.ae] {
				fcPut(w, "-", l)
			}
			for _, l := range lb[k.bs:k.be] {
				fcPut(w, "+", l)
			}
			pos = k.ae
		}
		for _, l := range la[pos:ae] {
			fcPut(w, " ", l)
		}
		i = j + 1
	}
}

func (c *fcDiff) paths(ua, ub string, entry bool) {
	if c.ctx.Err() != nil {
		return
	}
	var ia, ib os.FileInfo
	var ea, eb error
	if ua != "-" {
		ia, ea = os.Stat(abs(c.ctx, c.hc, ua))
	}
	if ub != "-" {
		ib, eb = os.Stat(abs(c.ctx, c.hc, ub))
	}
	if ea != nil && !(c.nf && entry) {
		c.trouble("%s: %s", ua, fcErr(ea))
		return
	}
	if eb != nil && !(c.nf && entry) {
		c.trouble("%s: %s", ub, fcErr(eb))
		return
	}
	dirA, dirB := ia != nil && ia.IsDir(), ib != nil && ib.IsDir()
	switch {
	case dirA && dirB:
		c.dirs(ua, ub)
	case dirA && ib != nil:
		c.paths(fcJoin(ua, fcBase(ub)), ub, false)
	case dirB && ia != nil:
		c.paths(ua, fcJoin(ub, fcBase(ua)), false)
	case dirA != dirB && entry && (ia == nil || ib == nil):
		c.files(ua, ub, ia == nil, ib == nil)
	case dirA != dirB && entry:
		kind := func(fi os.FileInfo) string {
			if fi.IsDir() {
				return "directory"
			}
			return "regular file"
		}
		c.out("File %s is a %s while file %s is a %s\n", ua, kind(ia), ub, kind(ib))
		c.code = max(c.code, 1)
	default:
		c.files(ua, ub, false, false)
	}
}

func (c *fcDiff) dirs(ua, ub string) {
	names := map[string]bool{}
	for _, d := range []string{ua, ub} {
		ents, _ := os.ReadDir(abs(c.ctx, c.hc, d))
		for _, e := range ents {
			names[e.Name()] = true
		}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		pa, pb := fcJoin(ua, n), fcJoin(ub, n)
		ia, ea := os.Lstat(abs(c.ctx, c.hc, pa))
		ib, eb := os.Lstat(abs(c.ctx, c.hc, pb))
		if (ea != nil || eb != nil) && !c.nf {
			dir := strings.TrimRight(ua, "/")
			if ea != nil {
				dir = strings.TrimRight(ub, "/")
			}
			fmt.Fprintf(c.hc.Stdout, "Only in %s: %s\n", dir, n)
			c.code = max(c.code, 1)
			continue
		}
		if ea == nil && eb == nil && ia.IsDir() && ib.IsDir() && !c.rec {
			fmt.Fprintf(c.hc.Stdout, "Common subdirectories: %s and %s\n", pa, pb)
			continue
		}
		if !c.brief {
			c.header = strings.TrimSpace("diff " + c.opts + " " + pa + " " + pb)
		}
		c.paths(pa, pb, true)
		c.header = ""
	}
}

// diffCmd supports the normal format, -u/-U n, -q, -r, -N; "-" is stdin.
// Exit 0 for equal, 1 for different, 2 for trouble.
func diffCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	var optArgs []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") && a != "-" {
			optArgs = append(optArgs, a)
		}
	}
	norm := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--unified" {
			a = "-u"
		} else if strings.HasPrefix(a, "--unified=") {
			a = "-U" + strings.TrimPrefix(a, "--unified=")
		}
		norm = append(norm, a)
	}
	o, files, err := opts("diff", norm, "uU:qrNcbBiw", map[string]string{"brief": "q", "recursive": "r", "new-file": "N"})
	if err != nil {
		return fail(hc, 2, "%v", err)
	}
	if len(files) != 2 {
		return fail(hc, 2, "diff: usage: diff [OPTION]... FILE1 FILE2")
	}
	c := &fcDiff{ctx: ctx, hc: hc, n: 3, brief: has(o, "q"), rec: has(o, "r"), nf: has(o, "N"), opts: strings.Join(optArgs, " ")}
	if v, ok := o["U"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fail(hc, 2, "diff: invalid context length '%s'", v)
		}
		c.n, c.unified = n, true
	}
	c.unified = c.unified || has(o, "u")
	if files[0] == "-" && files[1] == "-" {
		return fail(hc, 2, "diff: cannot compare '-' to '-'")
	}
	c.paths(files[0], files[1], false)
	return exitCode(c.code)
}

// ---- small tools ----

// rmdirCmd supports -p.
func rmdirCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("rmdir", args, "pv", map[string]string{"parents": "p", "verbose": "v"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(args) == 0 {
		return fail(hc, 1, "rmdir: missing operand")
	}
	code := 0
	remove := func(p string) bool {
		r := abs(ctx, hc, p)
		fi, err := os.Lstat(r)
		if err == nil && !fi.IsDir() {
			err = syscall.ENOTDIR
		} else if err = removeCheck(ctx, hc, p); err == nil {
			err = os.Remove(r)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "rmdir: failed to remove '%s': %s\n", p, fcErr(err))
			code = 1
			return false
		}
		if has(o, "v") {
			fmt.Fprintf(hc.Stdout, "rmdir: removing directory, '%s'\n", p)
		}
		return true
	}
	for _, a := range args {
		if !remove(a) || !has(o, "p") {
			continue
		}
		for d := path.Dir(strings.TrimRight(a, "/")); d != "." && d != "/" && remove(d); d = path.Dir(d) {
		}
	}
	return exitCode(code)
}

// basenameCmd supports basename name [suffix], -a and -s suffix.
func basenameCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("basename", args, "as:", map[string]string{"multiple": "a", "suffix": "s"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(args) == 0 {
		return fail(hc, 1, "basename: missing operand")
	}
	suffix := o["s"]
	if !has(o, "a") && !has(o, "s") && len(args) == 2 {
		args, suffix = args[:1], args[1]
	} else if !has(o, "a") && !has(o, "s") && len(args) > 2 {
		return fail(hc, 1, "basename: extra operand '%s'", args[2])
	}
	for _, a := range args {
		b := fcBase(a)
		if suffix != "" && b != suffix {
			b = strings.TrimSuffix(b, suffix)
		}
		fmt.Fprintln(hc.Stdout, b)
	}
	return nil
}

func dirnameCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	if len(args) == 0 {
		return fail(hc, 1, "dirname: missing operand")
	}
	for _, a := range args {
		t := strings.TrimRight(a, "/")
		switch {
		case a == "":
			t = "."
		case t == "":
			t = "/"
		default:
			t = path.Dir(t)
		}
		fmt.Fprintln(hc.Stdout, t)
	}
	return nil
}

// fcCanon resolves p to a user path. mode 0: the last component may be
// missing, 1: everything must exist, 2: nothing needs to exist.
func fcCanon(ctx context.Context, hc interp.HandlerContext, p string, mode int) (string, error) {
	real := path.Clean(abs(ctx, hc, p))
	root := strings.TrimSuffix(abs(ctx, hc, "/"), "/")
	user := func(r string) string {
		if root != "" && (r == root || strings.HasPrefix(r, root+"/")) {
			r = strings.TrimPrefix(r, root)
		}
		if r == "" {
			return "/"
		}
		return r
	}
	r, err := filepath.EvalSymlinks(real)
	if err == nil {
		return user(r), nil
	}
	if mode == 1 {
		return "", err
	}
	if pr, err2 := filepath.EvalSymlinks(filepath.Dir(real)); err2 == nil {
		return user(filepath.Join(pr, filepath.Base(real))), nil
	}
	if mode == 2 {
		return user(real), nil
	}
	return "", err
}

// realpathCmd supports -e, -m and -s.
func realpathCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("realpath", args, "ems", map[string]string{"canonicalize-existing": "e", "canonicalize-missing": "m", "no-symlinks": "s"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(args) == 0 {
		return fail(hc, 1, "realpath: missing operand")
	}
	mode := 0
	if has(o, "e") {
		mode = 1
	} else if has(o, "m") {
		mode = 2
	}
	code := 0
	for _, a := range args {
		var r string
		var err error
		if has(o, "s") {
			r = path.Clean(path.Join(hc.Dir, a))
			if path.IsAbs(a) {
				r = path.Clean(a)
			}
		} else {
			r, err = fcCanon(ctx, hc, a, mode)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "realpath: %s: %s\n", a, fcErr(err))
			code = 1
			continue
		}
		fmt.Fprintln(hc.Stdout, r)
	}
	return exitCode(code)
}

// readlinkCmd supports -f, -e, -m and -n.
func readlinkCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("readlink", args, "femnqs", map[string]string{"canonicalize": "f", "canonicalize-existing": "e", "canonicalize-missing": "m", "no-newline": "n"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	if len(args) == 0 {
		return fail(hc, 1, "readlink: missing operand")
	}
	code := 0
	for _, a := range args {
		var r string
		var err error
		switch {
		case has(o, "e"):
			r, err = fcCanon(ctx, hc, a, 1)
		case has(o, "f"):
			r, err = fcCanon(ctx, hc, a, 0)
		case has(o, "m"):
			r, err = fcCanon(ctx, hc, a, 2)
		default:
			r, err = os.Readlink(abs(ctx, hc, a))
		}
		if err != nil {
			code = 1
			if has(o, "f") || has(o, "e") {
				continue
			}
			continue
		}
		if has(o, "n") {
			fmt.Fprint(hc.Stdout, r)
		} else {
			fmt.Fprintln(hc.Stdout, r)
		}
	}
	return exitCode(code)
}

// mktempCmd supports -d, -u, -q, -p dir and a template ending in XXX...
func mktempCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("mktemp", args, "duqp:", map[string]string{"directory": "d", "dry-run": "u", "quiet": "q", "tmpdir": "p"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	tmpl := "tmp.XXXXXXXXXX"
	dir := ""
	if len(args) > 0 {
		tmpl = args[0]
	} else if !has(o, "p") {
		dir = "/tmp"
	}
	if v, ok := o["p"]; ok {
		dir = v
		if dir == "" {
			dir = "/tmp"
		}
	}
	if dir != "" {
		tmpl = fcJoin(dir, tmpl)
	}
	i := strings.LastIndexByte(tmpl, 'X')
	n := 0
	for i-n >= 0 && tmpl[i-n] == 'X' {
		n++
	}
	if i < 0 || n < 3 {
		return fail(hc, 1, "mktemp: too few X's in template '%s'", tmpl)
	}
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	for try := 0; try < 100; try++ {
		name := []byte(tmpl)
		for j := i - n + 1; j <= i; j++ {
			name[j] = chars[rand.IntN(len(chars))]
		}
		p := string(name)
		switch {
		case has(o, "u"):
			if _, err := os.Lstat(abs(ctx, hc, p)); err == nil {
				continue
			}
		case has(o, "d"):
			err := createAs(ctx, hc, p)
			if err == nil {
				if err = os.Mkdir(abs(ctx, hc, p), 0o700); err == nil {
					fsOwn(ctx, hc.Dir, p)
				}
			}
			if err != nil {
				if os.IsExist(err) {
					continue
				}
				return fail(hc, 1, "mktemp: failed to create directory via template '%s': %s", tmpl, fcErr(err))
			}
		default:
			f, err := filesOpen(ctx, hc, p, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				if os.IsExist(err) {
					continue
				}
				return fail(hc, 1, "mktemp: failed to create file via template '%s': %s", tmpl, fcErr(err))
			}
			f.Close()
		}
		fmt.Fprintln(hc.Stdout, p)
		return nil
	}
	return fail(hc, 1, "mktemp: failed to create file via template '%s': File exists", tmpl)
}

var fcSumLine = regexp.MustCompile(`^\\?([0-9a-fA-F]+) [ *](.*)$`)

// sumCmd makes md5sum, sha1sum, sha256sum and sha512sum: -c checks a list
// (with -q and --status), -b and -t are accepted.
func sumCmd(name string, newHash func() hash.Hash) util {
	return func(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
		o, args, err := opts(name, args, "cbtqw", map[string]string{"check": "c", "binary": "b", "text": "t", "quiet": "q", "status": "s", "warn": "w"})
		if err != nil {
			return fail(hc, 1, "%v", err)
		}
		if len(args) == 0 {
			args = []string{"-"}
		}
		sum := func(file string) (string, error) {
			h := newHash()
			if file == "-" {
				if hc.Stdin != nil {
					io.Copy(h, hc.Stdin)
				}
			} else {
				f, err := filesOpen(ctx, hc, file, os.O_RDONLY, 0)
				if err != nil {
					return "", err
				}
				defer f.Close()
				if st, err := f.Stat(); err == nil && st.IsDir() {
					return "", fmt.Errorf("Is a directory")
				}
				if _, err := io.Copy(h, f); err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("%x", h.Sum(nil)), nil
		}
		code := 0
		if !has(o, "c") {
			for _, a := range args {
				s, err := sum(a)
				if err != nil {
					fmt.Fprintf(hc.Stderr, "%s: %s: %s\n", name, a, fcErr(err))
					code = 1
					continue
				}
				fmt.Fprintf(hc.Stdout, "%s  %s\n", s, a)
			}
			return exitCode(code)
		}
		bad, unread, malformed := 0, 0, 0
		for _, list := range args {
			var data []byte
			if list == "-" {
				if hc.Stdin != nil {
					data, _ = io.ReadAll(hc.Stdin)
				}
			} else if data, err = filesRead(ctx, hc, list); err != nil {
				fmt.Fprintf(hc.Stderr, "%s: %s: %s\n", name, list, fcErr(err))
				code = 1
				continue
			}
			for _, l := range strings.Split(string(data), "\n") {
				if l == "" {
					continue
				}
				m := fcSumLine.FindStringSubmatch(l)
				if m == nil {
					malformed++
					continue
				}
				s, err := sum(m[2])
				switch {
				case err != nil:
					unread++
					if !has(o, "s") {
						fmt.Fprintf(hc.Stdout, "%s: FAILED open or read\n", m[2])
					}
				case strings.EqualFold(s, m[1]):
					if !has(o, "q") && !has(o, "s") {
						fmt.Fprintf(hc.Stdout, "%s: OK\n", m[2])
					}
				default:
					bad++
					if !has(o, "s") {
						fmt.Fprintf(hc.Stdout, "%s: FAILED\n", m[2])
					}
				}
			}
		}
		if !has(o, "s") {
			if malformed > 0 {
				fmt.Fprintf(hc.Stderr, "%s: WARNING: %d line(s) are improperly formatted\n", name, malformed)
			}
			if unread > 0 {
				fmt.Fprintf(hc.Stderr, "%s: WARNING: %d listed file(s) could not be read\n", name, unread)
			}
			if bad > 0 {
				fmt.Fprintf(hc.Stderr, "%s: WARNING: %d computed checksum(s) did NOT match\n", name, bad)
			}
		}
		if bad > 0 || unread > 0 || malformed > 0 {
			code = 1
		}
		return exitCode(code)
	}
}
