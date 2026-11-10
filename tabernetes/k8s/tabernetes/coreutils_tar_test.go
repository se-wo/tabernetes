package main

import (
	"archive/tar"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

func TestTar(t *testing.T) {
	tree := "mkdir -p d/sub && echo one > d/a.txt && echo two > d/sub/b.txt && "
	for _, tc := range []struct{ script, want string }{
		{"echo hi > f && tar cf x.tar f && tar tf x.tar", "f\n"},
		{"echo hi > f && tar cf x.tar f && rm f && tar xf x.tar && cat f", "hi\n"},
		{"echo hi > f && tar cf - f | tar tf -", "f\n"},
		{"echo hi > f && tar -cf x.tar f && tar -tf x.tar", "f\n"},
		{tree + "tar cf x.tar d && tar tf x.tar", "d/\nd/a.txt\nd/sub/\nd/sub/b.txt\n"},
		{tree + "tar czf - d | tar tzf -", "d/\nd/a.txt\nd/sub/\nd/sub/b.txt\n"},
		{tree + "tar czf x.tgz d && tar tf x.tgz | wc -l", "4\n"},
		{tree + "tar cf - d | tar xmf - -C /tmp && cat /tmp/d/sub/b.txt", "two\n"},
		{tree + "mkdir out && tar cf - d | tar -xmf - -C out && cat out/d/a.txt", "one\n"},
		{tree + "tar cf x.tar d && mkdir out && tar xf x.tar -C out --strip-components=1 && cat out/sub/b.txt out/a.txt", "two\none\n"},
		{tree + "tar cf x.tar d && tar xOf x.tar d/a.txt", "one\n"},
		{tree + "tar cf x.tar -C d sub && tar tf x.tar", "sub/\nsub/b.txt\n"},
		{tree + "tar cvf x.tar d | head -1", "d/\n"},
		{tree + "tar cf x.tar d --exclude=sub && tar tf x.tar", "d/\nd/a.txt\n"},
		{"echo hi > f && tar cf x.tar f && tar tvf x.tar | grep -c -e '-rw-r--r-- '", "1\n"},
		{"echo hi > f && gzip f && ls && gunzip f.gz && cat f", "f.gz\nhi\n"},
		{"echo hi > f && gzip -k f && ls", "f\nf.gz\n"},
		{"echo hi | gzip | gunzip", "hi\n"},
		{"echo hi | gzip -c | zcat", "hi\n"},
		{"echo hi > f && gzip -c f > f.gz && zcat f.gz && gzip -dc f.gz && ls", "hi\nhi\nf\nf.gz\n"},
		{"echo hi > f && gzip -9 f && gzip -dk f.gz && cat f", "hi\n"},
	} {
		t.Run(tc.script, func(t *testing.T) {
			got, code := sh(t, tc.script)
			if code != 0 || got != tc.want {
				t.Errorf("got %q (exit %d), want %q", got, code, tc.want)
			}
		})
	}
}

func TestTarLeadingSlash(t *testing.T) {
	got, code := sh(t, "mkdir -p /tmp/tt && echo x > /tmp/tt/f && tar cf - /tmp/tt/f | tar tf -")
	if code != 0 || got != "tar: removing leading '/' from member names\ntmp/tt/f\n" {
		t.Errorf("got %q (exit %d)", got, code)
	}
}

func TestTarErrors(t *testing.T) {
	for _, tc := range []struct {
		script, msg string
		code        int
	}{
		{"tar cf x.tar missing", "tar: missing: Cannot stat: No such file or directory", 2},
		{"tar tf missing.tar", "tar: missing.tar: Cannot open: No such file or directory", 2},
		{"echo hi > f; tar tf f", "tar: This does not look like a tar archive", 2},
		{"tar f x", "tar: You must specify one of", 2},
		{"gzip missing", "gzip: missing: No such file or directory", 1},
		{"echo hi > f; gunzip f", "gzip: f: unknown suffix -- ignored", 2},
		{"echo hi > f; echo hi | gunzip", "gzip: stdin: not in gzip format", 1},
	} {
		t.Run(tc.script, func(t *testing.T) {
			got, code := sh(t, tc.script)
			if code != tc.code || !strings.Contains(got, tc.msg) {
				t.Errorf("got %q (exit %d), want %q (exit %d)", got, code, tc.msg, tc.code)
			}
		})
	}
}

// shRoot runs script as a command in a container whose root is a directory
// with a read-only and a writable volume mounted.
func shRoot(t *testing.T, script string) (string, int, string) {
	t.Helper()
	base := t.TempDir()
	for _, d := range []string{"rootfs/tmp/d", "ro", "rw/d"} {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"rootfs/tmp/d/f", "rw/d/f"} {
		if err := os.WriteFile(filepath.Join(base, f), []byte("hi\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	root := &rootFS{base: filepath.Join(base, "rootfs"), mounts: []rootMount{
		{path: "/ro", real: filepath.Join(base, "ro"), readOnly: true},
		{path: "/rw", real: filepath.Join(base, "rw")},
	}}
	ctx := withProc(context.Background(), &procEnv{root: root})
	var out bytes.Buffer
	var self interp.ExecHandlerFunc
	handler := func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		self = func(ctx context.Context, args []string) error {
			if u := coreutils[args[0]]; u != nil {
				return u(ctx, interp.HandlerCtx(ctx), args[1:], self)
			}
			return next(ctx, args)
		}
		return self
	}
	r, err := interp.New(interp.StdIO(strings.NewReader(""), &out, &out), interp.ExecHandlers(handler))
	if err != nil {
		t.Fatal(err)
	}
	r.Dir = "/"
	f, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	err = r.Run(ctx, f)
	if st, ok := interp.IsExitStatus(err); ok {
		code = int(st)
	} else if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return out.String(), code, base
}

func TestTarContainerRoot(t *testing.T) {
	// the volume mounts are found through the root file system
	got, code, base := shRoot(t, "tar cf - -C /rw d | tar xmf - -C /tmp/x")
	b, err := os.ReadFile(filepath.Join(base, "rootfs/tmp/x/d/f"))
	if code != 0 || err != nil || string(b) != "hi\n" {
		t.Errorf("got %q (exit %d) %q %v", got, code, b, err)
	}
	st, err := os.Stat(filepath.Join(base, "rootfs/tmp/x/d/f"))
	if err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("mode: %v %v", st, err)
	}
	got, code, base = shRoot(t, "tar cf - -C /tmp d | tar xmf - -C /rw/new")
	if _, err := os.Stat(filepath.Join(base, "rw/new/d/f")); code != 0 || err != nil {
		t.Errorf("got %q (exit %d) %v", got, code, err)
	}
}

func TestTarReadOnlyVolume(t *testing.T) {
	got, code, base := shRoot(t, "tar cf - -C /tmp d | tar xmf - -C /ro")
	if code != 2 || !strings.Contains(got, "tar: d/f: Cannot open: Read-only file system") {
		t.Errorf("got %q (exit %d)", got, code)
	}
	if _, err := os.Stat(filepath.Join(base, "ro/d/f")); err == nil {
		t.Error("file written into the read-only volume")
	}
	// the exit status of tar itself
	got, code, _ = shRoot(t, "tar cf /tmp/x.tar -C /tmp d; tar xmf /tmp/x.tar -C /ro")
	if code != 2 || !strings.Contains(got, "Cannot open: Read-only file system") {
		t.Errorf("got %q (exit %d)", got, code)
	}
	got, code, _ = shRoot(t, "tar cf /ro/x.tar -C /tmp d")
	if code != 2 || !strings.Contains(got, "Read-only file system") {
		t.Errorf("got %q (exit %d)", got, code)
	}
}

// A shell with a fixed directory and stdin, for archives built by Go.
func shIn(t *testing.T, dir, stdin, script string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	var self interp.ExecHandlerFunc
	handler := func(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
		self = func(ctx context.Context, args []string) error {
			if u := coreutils[args[0]]; u != nil {
				return u(ctx, interp.HandlerCtx(ctx), args[1:], self)
			}
			return next(ctx, args)
		}
		return self
	}
	r, err := interp.New(interp.StdIO(strings.NewReader(stdin), &out, &out), interp.Dir(dir), interp.ExecHandlers(handler))
	if err != nil {
		t.Fatal(err)
	}
	f, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatal(err)
	}
	code := 0
	err = r.Run(context.Background(), f)
	if st, ok := interp.IsExitStatus(err); ok {
		code = int(st)
	} else if err != nil {
		t.Fatalf("%s: %v", script, err)
	}
	return out.String(), code
}

func TestTarModes(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "d"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "d", "secret"), []byte("s"), 0o600)
	if _, code := shIn(t, dir, "", "tar cf x.tar d"); code != 0 {
		t.Fatal("create")
	}
	if out, _ := shIn(t, dir, "", "tar tvf x.tar"); !strings.Contains(out, "-rwxr-xr-x") || !strings.Contains(out, "-rw------- ") {
		t.Errorf("modes missing in listing: %q", out)
	}
	os.RemoveAll(filepath.Join(dir, "d"))
	if out, code := shIn(t, dir, "", "tar xf x.tar"); code != 0 {
		t.Fatalf("extract: %q", out)
	}
	for name, want := range map[string]os.FileMode{"run.sh": 0o755, "secret": 0o600} {
		st, err := os.Stat(filepath.Join(dir, "d", name))
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %v", name, st, err, want)
		}
	}
	// symlinks survive
	os.Symlink("run.sh", filepath.Join(dir, "d", "link"))
	shIn(t, dir, "", "tar cf y.tar d && rm -r d && tar xf y.tar")
	if l, err := os.Readlink(filepath.Join(dir, "d", "link")); err != nil || l != "run.sh" {
		t.Errorf("symlink: %q %v", l, err)
	}
}

func TestTarEscape(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, n := range []string{"../evil", "ok/../../evil2", "good"} {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
	}
	tw.Close()
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	os.MkdirAll(dest, 0o755)
	out, code := shIn(t, dest, buf.String(), "tar xf -")
	if code != 2 || !strings.Contains(out, "tar: ../evil: Member name contains '..'") {
		t.Errorf("got %q (exit %d)", out, code)
	}
	for _, n := range []string{"evil", "evil2"} {
		if _, err := os.Stat(filepath.Join(parent, n)); err == nil {
			t.Errorf("%s escaped", n)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "good")); err != nil {
		t.Errorf("good was not extracted: %v", err)
	}
}

func TestReadOnlyVolume(t *testing.T) {
	pod := &v1.Pod{Spec: v1.PodSpec{Volumes: []v1.Volume{
		{Name: "cm", VolumeSource: v1.VolumeSource{ConfigMap: &v1.ConfigMapVolumeSource{}}},
		{Name: "sec", VolumeSource: v1.VolumeSource{Secret: &v1.SecretVolumeSource{}}},
		{Name: "data", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
	}}}
	for name, want := range map[string]bool{"cm": true, "sec": true, "data": false, "none": false} {
		if got := readOnlyVolume(pod, name); got != want {
			t.Errorf("readOnlyVolume(%s) = %v, want %v", name, got, want)
		}
	}
}
