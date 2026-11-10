package main

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/interp"
)

// tar, gzip, gunzip and zcat. They live in a file of their own and join the
// coreutils map from an init(): Go runs the init functions of a package in the
// order of the file names, so coreutils_tar.go comes after coreutils.go, which
// assigns the map.
func init() {
	coreutils["tar"] = tarCmd
	coreutils["gzip"] = gzipCmd
	coreutils["gunzip"] = func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
		return gzipCmd(ctx, hc, append([]string{"-d"}, args...), exec)
	}
	coreutils["zcat"] = func(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
		return gzipCmd(ctx, hc, append([]string{"-dc"}, args...), exec)
	}
}

// errText is an error the way GNU tools print it: "No such file or directory".
func errText(err error) string {
	s := unwrap(err).Error()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "No such file or directory"
	case errors.Is(err, fs.ErrPermission):
		return "Permission denied"
	case errors.Is(err, fs.ErrExist):
		return "File exists"
	}
	if s != "" {
		return strings.ToUpper(s[:1]) + s[1:]
	}
	return s
}

type tarOpts struct {
	mode     byte // 'c', 'x' or 't'
	file     string
	hasFile  bool
	dirs     []string // -C values in order of appearance
	gz       bool
	verbose  bool
	noMtime  bool
	toStdout bool
	strip    int
	exclude  []string
	// members: the operands, each with the -C directory that was in effect.
	members []tarMember
}

type tarMember struct{ dir, name string }

func parseTar(args []string) (*tarOpts, error) {
	o := &tarOpts{}
	setMode := func(m byte) error {
		if o.mode != 0 && o.mode != m {
			return errors.New("You may not specify more than one '-Acdtrux', '--delete' or '--test-label' option")
		}
		o.mode = m
		return nil
	}
	cur := ""
	next := func(opt string, i *int) (string, error) {
		*i++
		if *i >= len(args) {
			return "", fmt.Errorf("option requires an argument -- '%s'", opt)
		}
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			for _, m := range args[i+1:] {
				o.members = append(o.members, tarMember{cur, m})
			}
			i = len(args)
		case strings.HasPrefix(a, "--"):
			k, v, hasV := strings.Cut(a[2:], "=")
			needV := func() error {
				if hasV {
					return nil
				}
				var err error
				v, err = next(k, &i)
				return err
			}
			var err error
			switch k {
			case "create":
				err = setMode('c')
			case "extract", "get":
				err = setMode('x')
			case "list":
				err = setMode('t')
			case "file":
				if err = needV(); err == nil {
					o.file, o.hasFile = v, true
				}
			case "directory":
				if err = needV(); err == nil {
					cur = v
					o.dirs = append(o.dirs, v)
				}
			case "gzip", "gunzip", "ungzip":
				o.gz = true
			case "verbose":
				o.verbose = true
			case "touch":
				o.noMtime = true
			case "to-stdout":
				o.toStdout = true
			case "strip-components":
				if err = needV(); err == nil {
					o.strip, err = strconv.Atoi(v)
					if err != nil || o.strip < 0 {
						err = fmt.Errorf("%s: Invalid number", v)
					}
				}
			case "exclude":
				if err = needV(); err == nil {
					o.exclude = append(o.exclude, v)
				}
			case "same-permissions", "preserve-permissions", "no-same-owner", "same-owner", "numeric-owner", "keep-old-files", "overwrite", "no-same-permissions":
			default:
				err = fmt.Errorf("unrecognized option '--%s'", k)
			}
			if err != nil {
				return nil, err
			}
		case a == "-" || !strings.HasPrefix(a, "-") && (i > 0 || a == ""):
			o.members = append(o.members, tarMember{cur, a})
		default:
			// A flag cluster, with or without the leading dash (the first
			// argument may be "xmf", like in "tar xmf - -C dir").
			cl := strings.TrimPrefix(a, "-")
			for j := 0; j < len(cl); j++ {
				var err error
				switch c := cl[j]; c {
				case 'c', 'x', 't':
					err = setMode(c)
				case 'z':
					o.gz = true
				case 'v':
					o.verbose = true
				case 'm':
					o.noMtime = true
				case 'O':
					o.toStdout = true
				case 'p', 'o', 'k', 'h', 'P', 'S', 'a':
				case 'f', 'C':
					v := cl[j+1:]
					if v == "" {
						v, err = next(string(c), &i)
					}
					j = len(cl)
					if err == nil {
						if c == 'f' {
							o.file, o.hasFile = v, true
						} else {
							cur = v
							o.dirs = append(o.dirs, v)
						}
					}
				default:
					err = fmt.Errorf("invalid option -- '%c'", c)
				}
				if err != nil {
					return nil, err
				}
			}
		}
	}
	if o.mode == 0 {
		return nil, errors.New("You must specify one of the '-Acdtrux', '--delete' or '--test-label' options")
	}
	return o, nil
}

func (o *tarOpts) excluded(name string) bool {
	for _, p := range o.exclude {
		parts := strings.Split(strings.Trim(name, "/"), "/")
		for i := range parts {
			if ok, _ := path.Match(p, strings.Join(parts[:i+1], "/")); ok {
				return true
			}
			if ok, _ := path.Match(p, parts[i]); ok {
				return true
			}
		}
	}
	return false
}

// virt makes dir an absolute path in the file system the command sees.
func virt(hc interp.HandlerContext, dir string) string {
	if path.IsAbs(dir) {
		return path.Clean(dir)
	}
	return path.Join(hc.Dir, dir)
}

func tarCmd(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	o, err := parseTar(args)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "tar: %v\n", err)
		fmt.Fprintln(hc.Stderr, "Try 'tar --help' or 'tar --usage' for more information.")
		return interp.ExitStatus(2)
	}
	failed := false
	switch o.mode {
	case 'c':
		failed, err = tarCreate(ctx, hc, o)
	default:
		failed, err = tarRead(ctx, hc, o)
	}
	if err != nil {
		fmt.Fprintf(hc.Stderr, "tar: %v\n", err)
		failed = true
	}
	if failed {
		fmt.Fprintln(hc.Stderr, "tar: Exiting with failure status due to previous errors")
		return interp.ExitStatus(2)
	}
	return nil
}

func tarCreate(ctx context.Context, hc interp.HandlerContext, o *tarOpts) (bool, error) {
	if len(o.members) == 0 {
		return false, errors.New("Cowardly refusing to create an empty archive\nTry 'tar --help' or 'tar --usage' for more information.")
	}
	var out io.Writer = hc.Stdout
	archive := ""
	if o.hasFile && o.file != "-" {
		archive = abs(ctx, hc, o.file)
		f, err := tarFS{ctx, hc}.Create(virt(hc, o.file), 0o666)
		if err != nil {
			return false, fmt.Errorf("%s: Cannot open: %s", o.file, errText(err))
		}
		defer f.Close()
		out = f
	}
	bw := bufio.NewWriter(out)
	var gw *gzip.Writer
	var w io.Writer = bw
	if o.gz {
		gw = gzip.NewWriter(bw)
		w = gw
	}
	tw := tar.NewWriter(w)
	failed, warned := false, false

	var add func(real, name string) error
	add = func(real, name string) error {
		if o.excluded(name) {
			return nil
		}
		fi, err := os.Lstat(real)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "tar: %s: Cannot stat: %s\n", name, errText(err))
			failed = true
			return nil
		}
		if archive != "" && real == archive {
			fmt.Fprintf(hc.Stderr, "tar: %s: file is the archive; not dumped\n", name)
			return nil
		}
		link := ""
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(real); err != nil {
				fmt.Fprintf(hc.Stderr, "tar: %s: Cannot readlink: %s\n", name, errText(err))
				failed = true
				return nil
			}
		}
		if !fi.IsDir() && !fi.Mode().IsRegular() && link == "" {
			fmt.Fprintf(hc.Stderr, "tar: %s: Unknown file type; file ignored\n", name)
			failed = true
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if o.verbose {
			fmt.Fprintln(hc.Stdout, hdr.Name)
		}
		switch {
		case fi.IsDir():
			ents, err := os.ReadDir(real)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "tar: %s: Cannot open: %s\n", name, errText(err))
				failed = true
				return nil
			}
			sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
			for _, e := range ents {
				if err := add(path.Join(real, e.Name()), path.Join(name, e.Name())); err != nil {
					return err
				}
			}
		case fi.Mode().IsRegular():
			f, err := os.Open(real)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "tar: %s: Cannot open: %s\n", name, errText(err))
				failed = true
				// The header is out already: pad with zeros.
				_, err = io.CopyN(tw, zeros{}, hdr.Size)
				return err
			}
			defer f.Close()
			n, err := io.Copy(tw, io.LimitReader(f, hdr.Size))
			if err != nil {
				return err
			}
			if n < hdr.Size {
				fmt.Fprintf(hc.Stderr, "tar: %s: File shrank by %d bytes; padding with zeros\n", name, hdr.Size-n)
				failed = true
				_, err = io.CopyN(tw, zeros{}, hdr.Size-n)
				return err
			}
		}
		return nil
	}

	for _, m := range o.members {
		name := m.name
		stripped := false
		for strings.HasPrefix(name, "/") {
			name, stripped = name[1:], true
		}
		if stripped && !warned {
			warned = true
			fmt.Fprintln(hc.Stderr, "tar: removing leading '/' from member names")
		}
		if name == "" {
			name = "."
		}
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			name = "."
		}
		base := virt(hc, m.dir)
		// The path of the member goes through the root file system as a whole,
		// so that volume mounts below the directory are found.
		real := abs(ctx, hc, path.Join(base, m.name))
		if path.IsAbs(m.name) {
			real = abs(ctx, hc, m.name)
		}
		if err := add(real, name); err != nil {
			return failed, err
		}
	}
	if err := tw.Close(); err != nil {
		return failed, err
	}
	if gw != nil {
		if err := gw.Close(); err != nil {
			return failed, err
		}
	}
	return failed, bw.Flush()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func tarRead(ctx context.Context, hc interp.HandlerContext, o *tarOpts) (bool, error) {
	var in io.Reader = hc.Stdin
	if in == nil {
		in = strings.NewReader("")
	}
	if o.hasFile && o.file != "-" {
		f, err := os.Open(abs(ctx, hc, o.file))
		if err != nil {
			return false, fmt.Errorf("%s: Cannot open: %s", o.file, errText(err))
		}
		defer f.Close()
		in = f
	}
	br := bufio.NewReader(in)
	// Like GNU tar, read gzip data whether -z was given or not.
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gr, err := gzip.NewReader(br)
		if err != nil {
			return false, err
		}
		in = gr
	} else if o.gz {
		return false, errors.New("This does not look like a tar archive")
	} else {
		in = br
	}
	tr := tar.NewReader(in)

	dest := virt(hc, "")
	if len(o.dirs) > 0 {
		for _, d := range o.dirs {
			dest = path.Join(dest, d)
			if path.IsAbs(d) {
				dest = path.Clean(d)
			}
		}
	}
	failed := false
	type dirTime struct {
		real string
		t    time.Time
	}
	var dirTimes []dirTime
	matches := func(name string) bool {
		if len(o.members) == 0 {
			return true
		}
		n := strings.Trim(name, "/")
		for _, m := range o.members {
			mm := strings.Trim(m.name, "/")
			if n == mm || strings.HasPrefix(n, mm+"/") {
				return true
			}
		}
		return false
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, tar.ErrHeader) || errors.Is(err, io.ErrUnexpectedEOF) {
				return failed, errors.New("This does not look like a tar archive")
			}
			return failed, err
		}
		name := strings.TrimLeft(hdr.Name, "/")
		if !matches(name) || o.excluded(name) {
			continue
		}
		if o.strip > 0 {
			parts := strings.Split(strings.Trim(name, "/"), "/")
			if len(parts) <= o.strip {
				continue
			}
			name = strings.Join(parts[o.strip:], "/")
		}
		clean := path.Clean(name)
		if clean == "." && hdr.Typeflag != tar.TypeDir {
			continue
		}
		escapes := false
		for _, p := range strings.Split(name, "/") {
			if p == ".." {
				escapes = true
			}
		}
		if o.mode == 't' {
			if o.verbose {
				fmt.Fprintf(hc.Stdout, "%s %d/%d %8d %s %s\n", tarModeString(hdr), hdr.Uid, hdr.Gid, hdr.Size,
					hdr.ModTime.Format("2006-01-02 15:04"), name+tarLinkSuffix(hdr))
			} else {
				fmt.Fprintln(hc.Stdout, name)
			}
			continue
		}
		if escapes {
			fmt.Fprintf(hc.Stderr, "tar: %s: Member name contains '..'\n", hdr.Name)
			failed = true
			continue
		}
		if o.verbose {
			if o.toStdout {
				fmt.Fprintln(hc.Stderr, name)
			} else {
				fmt.Fprintln(hc.Stdout, name)
			}
		}
		if o.toStdout {
			if hdr.Typeflag == tar.TypeReg {
				if _, err := io.Copy(hc.Stdout, tr); err != nil {
					return failed, err
				}
			}
			continue
		}
		if err := tarExtractOne(ctx, hc, o, tr, hdr, path.Join(dest, clean), dest); err != nil {
			fmt.Fprintf(hc.Stderr, "tar: %s: Cannot %s\n", name, err)
			failed = true
		} else if hdr.Typeflag == tar.TypeDir && !o.noMtime {
			dirTimes = append(dirTimes, dirTime{abs(ctx, hc, path.Join(dest, clean)), hdr.ModTime})
		}
	}
	for i := len(dirTimes) - 1; i >= 0; i-- {
		os.Chtimes(dirTimes[i].real, dirTimes[i].t, dirTimes[i].t)
	}
	return failed, nil
}

// readOnlyAt reports whether virtual path p lies on a read-only volume mount
// of the container (readOnly: true, configMap, secret).
func readOnlyAt(ctx context.Context, hc interp.HandlerContext, p string) bool {
	if pe := procOf(ctx); pe != nil && pe.root != nil {
		return pe.root.readOnlyAt(p, hc.Dir)
	}
	return false
}

func tarModeString(h *tar.Header) string {
	m := fs.FileMode(h.Mode).Perm()
	t := "-"
	switch h.Typeflag {
	case tar.TypeDir:
		t = "d"
	case tar.TypeSymlink:
		t = "l"
	case tar.TypeLink:
		t = "h"
	}
	return t + m.String()[1:]
}

func tarLinkSuffix(h *tar.Header) string {
	switch h.Typeflag {
	case tar.TypeSymlink:
		return " -> " + h.Linkname
	case tar.TypeLink:
		return " link to " + h.Linkname
	}
	return ""
}

// tarFS is the one place where tar and gzip create things in the file system
// of the command. Every write goes through it, so that a check (read-only
// volumes today, permissions later) has a single place to live.
type tarFS struct {
	ctx context.Context
	hc  interp.HandlerContext
}

var errReadOnly = errors.New("Read-only file system")

// real maps the virtual path of the command to the file system of the tab and
// fails when the path may not be written.
func (f tarFS) real(virtual string) (string, error) {
	if readOnlyAt(f.ctx, f.hc, virtual) {
		return "", errReadOnly
	}
	return abs(f.ctx, f.hc, virtual), nil
}

func (f tarFS) Create(virtual string, perm fs.FileMode) (*os.File, error) {
	real, err := f.real(virtual)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(real, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}

func (f tarFS) MkdirAll(virtual string, perm fs.FileMode) error {
	real, err := f.real(virtual)
	if err != nil {
		return err
	}
	return os.MkdirAll(real, perm)
}

func (f tarFS) Symlink(target, virtual string) error {
	real, err := f.real(virtual)
	if err != nil {
		return err
	}
	return os.Symlink(target, real)
}

func (f tarFS) Link(oldVirtual, virtual string) error {
	real, err := f.real(virtual)
	if err != nil {
		return err
	}
	return os.Link(abs(f.ctx, f.hc, oldVirtual), real)
}

// Remove deletes an existing file or symlink that is about to be replaced.
func (f tarFS) Remove(virtual string) error {
	real, err := f.real(virtual)
	if err != nil {
		return err
	}
	return os.Remove(real)
}

// Attrs applies mode, owner and time of an archive header. The owner and the
// time are best effort: not every file system supports them.
func (f tarFS) Attrs(virtual string, h *tar.Header, mtime bool) error {
	real, err := f.real(virtual)
	if err != nil {
		return err
	}
	if h.Typeflag != tar.TypeSymlink {
		if err := os.Chmod(real, fs.FileMode(h.Mode).Perm()); err != nil {
			return err
		}
	}
	os.Lchown(real, h.Uid, h.Gid)
	if mtime && h.Typeflag != tar.TypeSymlink {
		os.Chtimes(real, h.ModTime, h.ModTime)
	}
	return nil
}

// tarExtractOne writes one member to the virtual path in the container.
// Errors read "open: ..." so that the caller prints "tar: name: Cannot open: ...".
func tarExtractOne(ctx context.Context, hc interp.HandlerContext, o *tarOpts, r io.Reader, h *tar.Header, virtual, dest string) error {
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeDir, tar.TypeSymlink, tar.TypeLink:
	default:
		return nil // devices, fifos, pax headers: ignored
	}
	f := tarFS{ctx, hc}
	real, err := f.real(virtual)
	if err != nil {
		return fmt.Errorf("open: %s", errText(err))
	}
	if err := f.MkdirAll(path.Dir(virtual), 0o755); err != nil {
		return fmt.Errorf("mkdir: %s", errText(err))
	}
	mode := fs.FileMode(h.Mode).Perm()
	if h.Typeflag == tar.TypeDir {
		if err := f.MkdirAll(virtual, 0o755); err != nil {
			return fmt.Errorf("mkdir: %s", errText(err))
		}
		h.Mode = int64(mode | 0o700)
		return f.Attrs(virtual, h, false)
	}
	if st, err := os.Lstat(real); err == nil {
		if st.IsDir() {
			return errors.New("open: Is a directory")
		}
		f.Remove(virtual)
	}
	switch h.Typeflag {
	case tar.TypeSymlink:
		if err := f.Symlink(h.Linkname, virtual); err != nil {
			return fmt.Errorf("create symlink: %s", errText(err))
		}
		return f.Attrs(virtual, h, false)
	case tar.TypeLink:
		target := path.Join(dest, path.Clean("/"+h.Linkname))
		if err := f.Link(target, virtual); err != nil {
			// no hard links in the in-memory file system: copy
			b, rerr := os.ReadFile(abs(ctx, hc, target))
			if rerr != nil {
				return fmt.Errorf("create hard link: %s", errText(rerr))
			}
			w, werr := f.Create(virtual, mode|0o200)
			if werr == nil {
				_, werr = w.Write(b)
				w.Close()
			}
			if werr != nil {
				return fmt.Errorf("open: %s", errText(werr))
			}
		}
		return f.Attrs(virtual, h, !o.noMtime)
	}
	w, err := f.Create(virtual, mode|0o200)
	if err != nil {
		return fmt.Errorf("open: %s", errText(err))
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return fmt.Errorf("write: %s", errText(err))
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("write: %s", errText(err))
	}
	if err := f.Attrs(virtual, h, !o.noMtime); err != nil {
		return fmt.Errorf("set mode: %s", errText(err))
	}
	return nil
}

func gzipCmd(ctx context.Context, hc interp.HandlerContext, args []string, exec interp.ExecHandlerFunc) error {
	o, files, err := opts("gzip", args, "cdkfvqtl123456789", map[string]string{
		"stdout": "c", "to-stdout": "c", "decompress": "d", "uncompress": "d", "keep": "k", "force": "f",
		"quiet": "q", "verbose": "v", "best": "9", "fast": "1",
	})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	dec, toOut, keep, force := has(o, "d"), has(o, "c"), has(o, "k"), has(o, "f")
	var stdin io.Reader = hc.Stdin
	if stdin == nil {
		stdin = strings.NewReader("")
	}
	run := func(in io.Reader, out io.Writer, name string) error {
		if dec {
			gr, err := gzip.NewReader(in)
			if err != nil {
				if errors.Is(err, gzip.ErrHeader) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					return errors.New(name + ": not in gzip format")
				}
				return err
			}
			if _, err := io.Copy(out, gr); err != nil {
				if errors.Is(err, io.ErrUnexpectedEOF) {
					return errors.New(name + ": unexpected end of file")
				}
				return errors.New(name + ": invalid compressed data--format violated")
			}
			return nil
		}
		gw := gzip.NewWriter(out)
		if _, err := io.Copy(gw, in); err != nil {
			return err
		}
		return gw.Close()
	}
	if len(files) == 0 {
		if err := run(stdin, hc.Stdout, "stdin"); err != nil {
			return fail(hc, 1, "gzip: %v", err)
		}
		return nil
	}
	code := 0
	bump := func(c int) {
		if c > code {
			code = c
		}
	}
	for _, f := range files {
		if f == "-" {
			if err := run(stdin, hc.Stdout, "stdin"); err != nil {
				fmt.Fprintf(hc.Stderr, "gzip: %v\n", err)
				bump(1)
			}
			continue
		}
		in := abs(ctx, hc, f)
		st, err := os.Stat(in)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "gzip: %s: %s\n", f, errText(err))
			bump(1)
			continue
		}
		if st.IsDir() {
			fmt.Fprintf(hc.Stderr, "gzip: %s is a directory -- ignored\n", f)
			bump(2)
			continue
		}
		outName := ""
		if !toOut {
			if dec {
				switch {
				case strings.HasSuffix(f, ".gz"):
					outName = strings.TrimSuffix(f, ".gz")
				case strings.HasSuffix(f, ".tgz"):
					outName = strings.TrimSuffix(f, ".tgz") + ".tar"
				default:
					fmt.Fprintf(hc.Stderr, "gzip: %s: unknown suffix -- ignored\n", f)
					bump(2)
					continue
				}
			} else {
				if strings.HasSuffix(f, ".gz") && !force {
					fmt.Fprintf(hc.Stderr, "gzip: %s already has .gz suffix -- unchanged\n", f)
					bump(2)
					continue
				}
				outName = f + ".gz"
			}
			if _, err := os.Lstat(abs(ctx, hc, outName)); err == nil && !force {
				fmt.Fprintf(hc.Stderr, "gzip: %s already exists; not overwritten\n", outName)
				bump(2)
				continue
			}
		}
		fh, err := os.Open(in)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "gzip: %s: %s\n", f, errText(err))
			bump(1)
			continue
		}
		if toOut {
			err = run(fh, hc.Stdout, f)
			fh.Close()
			if err != nil {
				fmt.Fprintf(hc.Stderr, "gzip: %v\n", err)
				bump(1)
			}
			continue
		}
		outReal := abs(ctx, hc, outName)
		of, err := tarFS{ctx, hc}.Create(virt(hc, outName), st.Mode().Perm())
		if err != nil {
			fh.Close()
			fmt.Fprintf(hc.Stderr, "gzip: %s: %s\n", outName, errText(err))
			bump(1)
			continue
		}
		err = run(fh, of, f)
		fh.Close()
		if cerr := of.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			os.Remove(outReal)
			fmt.Fprintf(hc.Stderr, "gzip: %v\n", err)
			bump(1)
			continue
		}
		os.Chtimes(outReal, st.ModTime(), st.ModTime())
		if !keep {
			os.Remove(in)
		}
	}
	return exitCode(code)
}
