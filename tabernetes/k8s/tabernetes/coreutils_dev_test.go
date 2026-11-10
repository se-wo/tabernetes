package main

import (
	"strings"
	"testing"
)

func TestDevAndDd(t *testing.T) {
	for _, tc := range []struct {
		script, want string
		code         int
	}{
		{"head -c 1K /dev/zero | wc -c", "1024\n", 0},
		{"head -c 3 /dev/urandom | wc -c", "3\n", 0},
		{"cat /dev/zero | head -c 4 | wc -c", "4\n", 0},
		{"seq 100000 | head -1", "1\n", 0},
		{"seq 100000 | grep -m1 5", "5\n", 0},
		{"head -c 2M /dev/zero > f; stat -c %s f", "2097152\n", 0},
		{"head -c 1KB /dev/zero | wc -c", "1000\n", 0},
		{"head -c 1x /dev/zero", "head: invalid number of bytes: '1x'\n", 2},
		{"dd if=/dev/zero of=f bs=1M count=3 status=none; stat -c %s f", "3145728\n", 0},
		{"dd if=/dev/urandom bs=10 count=1 status=none | wc -c", "10\n", 0},
		{"printf abcdef > f; dd if=f bs=2 skip=1 count=2 status=none", "cdef", 0},
		{"printf abcdef > f; printf XY | dd of=f bs=1 seek=2 conv=notrunc status=none; cat f", "abXYef", 0},
		{"printf abcde | dd of=f bs=2 status=noxfer; cat f", "2+1 records in\n2+1 records out\nabcde", 0},
		{"dd if=/dev/zero of=/dev/full bs=1 count=1 status=none", "dd: error writing '/dev/full': No space left on device\n", 1},
		{"dd if=missing of=f status=none", "dd: failed to open 'missing': No such file or directory\n", 1},
		{"dd foo", "dd: unrecognized operand 'foo'\n", 1},
	} {
		got, code := sh(t, tc.script)
		if got != tc.want || code != tc.code {
			t.Errorf("%s:\ngot  %q (%d)\nwant %q (%d)", tc.script, got, code, tc.want, tc.code)
		}
	}
	got, _ := sh(t, "dd if=/dev/zero of=f bs=1K count=2")
	if !strings.HasPrefix(got, "2+0 records in\n2+0 records out\n2048 bytes (2.0 kB, 2.0 KiB) copied, ") {
		t.Errorf("dd summary: %q", got)
	}
}
