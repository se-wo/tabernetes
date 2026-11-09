package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBindSysfs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	spec := `{"ociVersion":"1.2.0","linux":{"resources":{"memory":{"limit":9223372036854771712}}},"mounts":[` +
		`{"destination":"/proc","type":"proc","source":"proc","options":["nosuid","noexec","nodev"]},` +
		`{"destination":"/sys","type":"sysfs","source":"sysfs","options":["nosuid","noexec","nodev","rw"]}]}`
	if err := os.WriteFile(path, []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := bindSysfs(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		`{"destination":"/sys","options":["rbind","ro","rro","nosuid","noexec","nodev"],"source":"/sys","type":"bind"}`,
		`{"destination":"/proc","options":["nosuid","noexec","nodev"],"source":"proc","type":"proc"}`,
		`"limit":9223372036854771712`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
}
