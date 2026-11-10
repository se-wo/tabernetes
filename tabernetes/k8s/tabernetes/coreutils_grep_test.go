package main

import (
	"context"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func TestGrep(t *testing.T) {
	const in = `printf 'a\nb\nmatch1\nc\nd\ne\nmatch2\nf\n'`
	for _, tc := range []struct {
		script, want string
		code         int
	}{
		{in + " | grep -A1 match", "match1\nc\n--\nmatch2\nf\n", 0},
		{in + " | grep -B1 match1", "b\nmatch1\n", 0},
		{in + " | grep -C1 -n match", "2-b\n3:match1\n4-c\n--\n6-e\n7:match2\n8-f\n", 0},
		{in + " | grep -A3 match1", "match1\nc\nd\ne\n", 0},
		{in + " | grep -A4 match1", "match1\nc\nd\ne\nmatch2\n", 0},
		{in + " | grep -m1 match", "match1\n", 0},
		{in + " | grep -x c", "c\n", 0},
		{in + " | grep -e ^a -e ^b", "a\nb\n", 0},
		{in + " | grep -E 'match(1|2)' | grep -c .", "2\n", 0},
		{in + " | grep -q match && echo yes", "yes\n", 0},
		{in + " | grep nothing", "", 1},
		{"echo 'x-y' | grep -e -y", "x-y\n", 0},
		{"echo 'foo bar' | grep -o 'o*'", "oo\n", 0},
		{"echo 'Foo' | grep -i foo", "Foo\n", 0},
		{"echo hi > a.txt; echo ho > b.txt; grep -l h a.txt b.txt", "a.txt\nb.txt\n", 0},
		{"echo hi > a.txt; echo ho > b.txt; grep -L i a.txt b.txt", "b.txt\n", 0},
		{"echo hi > a.txt; echo ho > b.txt; grep -c h a.txt b.txt", "a.txt:1\nb.txt:1\n", 0},
		{"echo hi > a.txt; grep -H hi a.txt", "a.txt:hi\n", 0},
		{"mkdir -p d/e; echo key=1 > d/e/f.conf; echo key=2 > d/g.txt; grep -r key d | sort", "d/e/f.conf:key=1\nd/g.txt:key=2\n", 0},
		{"mkdir -p d; echo key=1 > d/f.conf; echo key=2 > d/g.txt; grep -rh --include='*.conf' key d", "key=1\n", 0},
		{"mkdir d; grep -s x d", "", 2},
		{"grep x missing.txt", "grep: missing.txt: No such file or directory\n", 2},
		{"echo 'a b' | egrep 'a|z'", "a b\n", 0},
		{"echo 'a|b' | fgrep 'a|b'", "a|b\n", 0},
	} {
		got, code := sh(t, tc.script)
		if got != tc.want || code != tc.code {
			t.Errorf("%s:\ngot  %q (%d)\nwant %q (%d)", tc.script, got, code, tc.want, tc.code)
		}
	}
}

func TestSleepArgs(t *testing.T) {
	for _, tc := range []struct {
		script string
		code   int
	}{
		{"sleep 0.01", 0},
		{"sleep 0.01s 0.0001m", 0},
		{"sleep 10ms", 0},
		{"sleep 1x", 1},
		{"sleep", 1},
	} {
		if _, code := sh(t, tc.script); code != tc.code {
			t.Errorf("%s: exit %d, want %d", tc.script, code, tc.code)
		}
	}
	// 1d and infinity sleep until the context ends.
	for _, a := range [][]string{{"1d"}, {"infinity"}, {"1h", "30m"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		start := time.Now()
		err := sleepCmd(ctx, interp.HandlerContext{}, a, nil)
		cancel()
		if st, ok := interp.IsExitStatus(err); !ok || st != 130 || time.Since(start) > 5*time.Second {
			t.Errorf("sleep %s: %v", a, err)
		}
	}
}
