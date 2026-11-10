package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func vf(s string, mode os.FileMode) volFile { return volFile{[]byte(s), mode} }

func readVol(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteAtomicLayout(t *testing.T) {
	dir := t.TempDir()
	changed, err := writeAtomic(dir, map[string]volFile{
		"a":       vf("1", 0o644),
		"sec":     vf("s", 0o400),
		"sub/b.t": vf("2", 0o644),
	}, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	ts, err := os.Readlink(filepath.Join(dir, "..data"))
	if err != nil || !strings.HasPrefix(ts, "..") || filepath.IsAbs(ts) {
		t.Fatalf("..data -> %q, %v", ts, err)
	}
	if l, _ := os.Readlink(filepath.Join(dir, "a")); l != "..data/a" {
		t.Fatalf("a -> %q", l)
	}
	if l, _ := os.Readlink(filepath.Join(dir, "sub")); l != "..data/sub" {
		t.Fatalf("sub -> %q", l)
	}
	if got := readVol(t, filepath.Join(dir, "a")); got != "1" {
		t.Fatalf("a = %q", got)
	}
	if got := readVol(t, filepath.Join(dir, "sub", "b.t")); got != "2" {
		t.Fatalf("sub/b.t = %q", got)
	}
	st, err := os.Stat(filepath.Join(dir, "sec"))
	if err != nil || st.Mode().Perm() != 0o400 {
		t.Fatalf("sec mode %v, %v", st, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, ts)); st == nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("ts dir %v", st)
	}
}

func TestWriteAtomicNoop(t *testing.T) {
	dir := t.TempDir()
	p := map[string]volFile{"a": vf("1", 0o644)}
	writeAtomic(dir, p, false)
	before, _ := os.Readlink(filepath.Join(dir, "..data"))
	changed, err := writeAtomic(dir, p, false)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if after, _ := os.Readlink(filepath.Join(dir, "..data")); after != before {
		t.Fatalf("..data moved: %s -> %s", before, after)
	}
	// A different mode alone is a change.
	if changed, _ := writeAtomic(dir, map[string]volFile{"a": vf("1", 0o600)}, false); !changed {
		t.Fatal("mode change not written")
	}
}

func TestWriteAtomicUpdate(t *testing.T) {
	dir := t.TempDir()
	writeAtomic(dir, map[string]volFile{"a": vf("1", 0o644), "gone": vf("x", 0o644)}, false)
	old, _ := os.Readlink(filepath.Join(dir, "..data"))
	time.Sleep(2 * time.Millisecond)
	changed, err := writeAtomic(dir, map[string]volFile{"a": vf("2", 0o644), "new": vf("n", 0o644)}, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if got := readVol(t, filepath.Join(dir, "a")); got != "2" {
		t.Fatalf("a = %q", got)
	}
	if got := readVol(t, filepath.Join(dir, "new")); got != "n" {
		t.Fatalf("new = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(dir, "gone")); err == nil {
		t.Fatal("link of a removed key is still there")
	}
	if _, err := os.Lstat(filepath.Join(dir, old)); err == nil {
		t.Fatal("old timestamp dir not removed")
	}
	cur, _ := os.Readlink(filepath.Join(dir, "..data"))
	if cur == old {
		t.Fatal("..data did not move")
	}
}

func TestWriteAtomicKeepOldAndUserFiles(t *testing.T) {
	dir := t.TempDir()
	writeAtomic(dir, map[string]volFile{"a": vf("1", 0o644)}, true)
	old, _ := os.Readlink(filepath.Join(dir, "..data"))
	os.WriteFile(filepath.Join(dir, "user"), []byte("u"), 0o644)
	writeAtomic(dir, map[string]volFile{"b": vf("2", 0o644)}, true)
	if got := readVol(t, filepath.Join(dir, old, "a")); got != "1" {
		t.Fatalf("old dir lost: %q", got)
	}
	if got := readVol(t, filepath.Join(dir, "user")); got != "u" {
		t.Fatal("user file touched")
	}
}

func TestWriteAtomicEmpty(t *testing.T) {
	dir := t.TempDir()
	writeAtomic(dir, map[string]volFile{"a": vf("1", 0o644)}, false)
	changed, err := writeAtomic(dir, nil, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "a")); err == nil {
		t.Fatal("a still there")
	}
	ts, _ := os.Readlink(filepath.Join(dir, "..data"))
	des, err := os.ReadDir(filepath.Join(dir, ts))
	if err != nil || len(des) != 0 {
		t.Fatalf("%v %v", des, err)
	}
	if changed, _ := writeAtomic(dir, nil, false); changed {
		t.Fatal("empty payload written twice")
	}
}

func TestWriteAtomicBadPaths(t *testing.T) {
	for _, p := range []string{"/abs", "../x", "a/../b", "..hidden", "a/..", ""} {
		dir := t.TempDir()
		if _, err := writeAtomic(dir, map[string]volFile{p: vf("x", 0o644)}, false); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestVolumeModes(t *testing.T) {
	w := &podWorker{}
	items := []v1.KeyToPath{{Key: "k", Path: "p", Mode: ptr.To[int32](0o400)}, {Key: "k2", Path: "q"}}
	files, err := keyFiles(map[string][]byte{"k": []byte("1"), "k2": []byte("2")}, items, "configmap", "c", volumeMode(ptr.To[int32](0o600)))
	if err != nil {
		t.Fatal(err)
	}
	if files["p"].mode != 0o400 || files["q"].mode != 0o600 {
		t.Fatalf("%v", files)
	}
	files, _ = keyFiles(map[string][]byte{"k": nil}, nil, "secret", "s", volumeMode(nil))
	if files["k"].mode != 0o644 {
		t.Fatalf("default %v", files["k"].mode)
	}
	pod := &v1.Pod{}
	pod.Name = "p"
	dw, err := downwardFiles(pod, "", []v1.DownwardAPIVolumeFile{
		{Path: "n", FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		{Path: "m", FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}, Mode: ptr.To[int32](0o444)},
	}, 0o640)
	if err != nil || dw["n"].mode != 0o640 || dw["m"].mode != 0o444 {
		t.Fatalf("%v %v", dw, err)
	}
	// Projected: the volume defaultMode applies to every source.
	v := v1.Volume{Name: "v", VolumeSource: v1.VolumeSource{Projected: &v1.ProjectedVolumeSource{
		DefaultMode: ptr.To[int32](0o440),
		Sources: []v1.VolumeProjection{
			{DownwardAPI: &v1.DownwardAPIProjection{Items: []v1.DownwardAPIVolumeFile{
				{Path: "n", FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}},
				{Path: "m", FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"}, Mode: ptr.To[int32](0o400)},
			}}},
		}}}}
	pl, err := w.volumePayload(t.Context(), pod, "", v)
	if err != nil || pl["n"].mode != 0o440 || pl["m"].mode != 0o400 {
		t.Fatalf("%v %v", pl, err)
	}
}

func TestVolumeHasSubPath(t *testing.T) {
	pod := &v1.Pod{Spec: v1.PodSpec{Containers: []v1.Container{{VolumeMounts: []v1.VolumeMount{{Name: "a"}, {Name: "b", SubPath: "x"}}}}}}
	if volumeHasSubPath(pod, "a") || !volumeHasSubPath(pod, "b") {
		t.Fatal("subPath detection")
	}
}

func TestTokenNeedsRenewal(t *testing.T) {
	iat := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	exp := iat.Add(time.Hour)
	for _, c := range []struct {
		now  time.Time
		want bool
	}{
		{iat, false},
		{iat.Add(47 * time.Minute), false},
		{iat.Add(49 * time.Minute), true},
		{exp.Add(time.Minute), true},
	} {
		if got := tokenNeedsRenewal(iat, exp, c.now); got != c.want {
			t.Errorf("now=%v got %v", c.now.Sub(iat), got)
		}
	}
	// Long-lived token: renewed after 24h although 80 % is not reached.
	long := iat.Add(1000 * time.Hour)
	if tokenNeedsRenewal(iat, long, iat.Add(23*time.Hour)) || !tokenNeedsRenewal(iat, long, iat.Add(25*time.Hour)) {
		t.Fatal("24h rule")
	}
}

func TestLsMode(t *testing.T) {
	if got := lsMode(os.ModeSymlink | 0o777); got != "lrwxrwxrwx" {
		t.Fatal(got)
	}
	if got := lsMode(0o644); got != "-rw-r--r--" {
		t.Fatal(got)
	}
}
