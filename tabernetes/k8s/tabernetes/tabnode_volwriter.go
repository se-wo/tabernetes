package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// volFile is one file of a volume: its bytes and its permission bits.
type volFile struct {
	data []byte
	mode os.FileMode
}

const volDataLink = "..data"

// writeAtomic makes dir hold payload the way the kubelet's atomic writer does
// (pkg/volume/util/atomic_writer.go): the files live in a timestamped
// directory, ..data points to it and every top-level name is a link through
// ..data. A refresh swaps ..data in one rename, so a reader never sees half
// of a new payload. It returns false when nothing had to change. keepOld
// keeps the previous timestamped directory, for subPath mounts that still
// point into it.
func writeAtomic(dir string, payload map[string]volFile, keepOld bool) (changed bool, err error) {
	tops := map[string]bool{}
	for p := range payload {
		if err := validVolPath(p); err != nil {
			return false, err
		}
		tops[strings.SplitN(p, "/", 2)[0]] = true
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	oldTS, _ := os.Readlink(filepath.Join(dir, volDataLink))
	if oldTS != "" && sameVolPayload(filepath.Join(dir, oldTS), payload) {
		return false, nil
	}

	// Named like the kubelet's: ..2006_01_02_15_04_05.<random>.
	tsDir, err := os.MkdirTemp(dir, time.Now().UTC().Format("..2006_01_02_15_04_05."))
	if err != nil {
		return false, err
	}
	ts := filepath.Base(tsDir)
	fail := func(err error) (bool, error) {
		os.RemoveAll(tsDir)
		return false, err
	}
	if err := os.Chmod(tsDir, 0o755); err != nil {
		return fail(err)
	}
	for p, f := range payload {
		real := filepath.Join(tsDir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(real), 0o755); err != nil {
			return fail(err)
		}
		if err := os.WriteFile(real, f.data, f.mode.Perm()); err != nil {
			return fail(err)
		}
		// Explicit, so the umask of the file system does not matter.
		if err := os.Chmod(real, f.mode.Perm()); err != nil {
			return fail(err)
		}
	}

	tmp := filepath.Join(dir, volDataLink+"_tmp")
	os.Remove(tmp)
	if err := os.Symlink(ts, tmp); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, volDataLink)); err != nil {
		os.Remove(tmp)
		return fail(err)
	}

	for top := range tops {
		l := filepath.Join(dir, top)
		if _, err := os.Lstat(l); err == nil {
			continue
		}
		if err := os.Symlink(path.Join(volDataLink, top), l); err != nil {
			return true, err
		}
	}
	// Names of the old payload that are gone. Only our own links: a user
	// file in the volume is not touched.
	if oldTS != "" {
		des, _ := os.ReadDir(dir)
		for _, de := range des {
			n := de.Name()
			if tops[n] || strings.HasPrefix(n, "..") || de.Type()&fs.ModeSymlink == 0 {
				continue
			}
			if t, err := os.Readlink(filepath.Join(dir, n)); err == nil && t == path.Join(volDataLink, n) {
				os.Remove(filepath.Join(dir, n))
			}
		}
		if !keepOld {
			os.RemoveAll(filepath.Join(dir, oldTS))
		}
	}
	return true, nil
}

func validVolPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "..") {
		return fmt.Errorf("invalid volume file path %q", p)
	}
	for _, e := range strings.Split(p, "/") {
		if e == ".." || e == "" {
			return fmt.Errorf("invalid volume file path %q", p)
		}
	}
	return nil
}

// sameVolPayload reports whether tsDir holds exactly payload: same files,
// bytes and modes.
func sameVolPayload(tsDir string, payload map[string]volFile) bool {
	n := 0
	ok := true
	filepath.WalkDir(tsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			ok = false
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(tsDir, p)
		want, found := payload[filepath.ToSlash(rel)]
		if !found {
			ok = false
			return fs.SkipAll
		}
		st, err := os.Stat(p)
		b, rerr := os.ReadFile(p)
		if err != nil || rerr != nil || st.Mode().Perm() != want.mode.Perm() || !bytes.Equal(b, want.data) {
			ok = false
			return fs.SkipAll
		}
		n++
		return nil
	})
	return ok && n == len(payload)
}
