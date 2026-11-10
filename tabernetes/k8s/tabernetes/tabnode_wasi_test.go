package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestWasmPreopensVolumeLinks: a configMap volume in the layout of the
// atomic writer (key -> ..data/key, ..data -> ..<ts>) reaches a WebAssembly
// module as plain files, without the ".." plumbing.
func TestWasmPreopensVolumeLinks(t *testing.T) {
	tmp := t.TempDir()
	vol := filepath.Join(tmp, "vol")
	if _, err := writeAtomic(vol, map[string]volFile{
		"color":     {[]byte("blue"), 0o644},
		"dir/a.txt": {[]byte("nested"), 0o400},
	}, false); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(tmp, "rootfs")
	os.MkdirAll(filepath.Join(base, "etc"), 0o755)
	os.WriteFile(filepath.Join(base, "etc", "hostname"), []byte("p\n"), 0o644)
	root := &rootFS{base: base, mounts: []rootMount{
		{path: "/etc/sub/color", real: filepath.Join(vol, "color")}, // subPath
		{path: "/etc/cfg", real: vol, readOnly: true},
	}}

	list, err := wasmPreopens(root)
	if err != nil {
		t.Fatal(err)
	}
	type preopen struct {
		files map[string]string
		dirs  []string
	}
	got := map[string]preopen{}
	for i := 0; i < list.Length(); i++ {
		o := list.Index(i)
		p := preopen{files: map[string]string{}}
		fs := o.Get("files")
		for j := 0; j < fs.Length(); j++ {
			f := fs.Index(j)
			p.files[f.Index(0).String()] = string(jsBytes(f.Index(1)))
		}
		ds := o.Get("dirs")
		for j := 0; j < ds.Length(); j++ {
			p.dirs = append(p.dirs, ds.Index(j).String())
		}
		sort.Strings(p.dirs)
		got[o.Get("guest").String()] = p
	}

	cfg, ok := got["/etc/cfg"]
	if !ok {
		t.Fatalf("no preopen /etc/cfg: %v", got)
	}
	want := map[string]string{"color": "blue", "dir/a.txt": "nested"}
	if len(cfg.files) != len(want) {
		t.Errorf("files of /etc/cfg = %v, want %v", cfg.files, want)
	}
	for k, v := range want {
		if cfg.files[k] != v {
			t.Errorf("/etc/cfg/%s = %q, want %q", k, cfg.files[k], v)
		}
	}
	if strings.Join(cfg.dirs, ",") != "dir" {
		t.Errorf("dirs of /etc/cfg = %v, want [dir]", cfg.dirs)
	}
	for k := range cfg.files {
		if strings.Contains(k, "..") {
			t.Errorf("plumbing leaked into the module: %s", k)
		}
	}
	rootFiles := got["/"].files
	if rootFiles["etc/sub/color"] != "blue" || rootFiles["etc/hostname"] != "p\n" {
		t.Errorf("root preopen files = %v", rootFiles)
	}
}
