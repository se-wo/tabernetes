package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func TestMisc(t *testing.T) {
	for _, tc := range []struct {
		script, want string
		code         int
	}{
		{"date -u -d '2026-11-10 08:45:00' '+%Y-%m-%d %H:%M:%S %Z %a %b %e %j %u %p %I'", "2026-11-10 08:45:00 UTC Tue Nov 10 314 2 AM 08\n", 0},
		{"date -u -d @0 +%F-%T-%s", "1970-01-01-00:00:00-0\n", 0},
		{"date -u -d 2026-11-10T08:00:00Z", "Tue Nov 10 08:00:00 UTC 2026\n", 0},
		{"date -u -d '2026-11-03 01:02:03' '+%e|%-d|%y|%%|%D|%R'", " 3|3|26|%|11/03/26|01:02\n", 0},
		{"date -u -d '2026-11-10 08:45:00' -I", "2026-11-10\n", 0},
		{"date -u -d '2026-11-10 08:45:00' -Iseconds", "2026-11-10T08:45:00+00:00\n", 0},
		{"date -u -d '2026-11-10 08:45:00' -R", "Tue, 10 Nov 2026 08:45:00 +0000\n", 0},
		{"date -u -d '2 days ago' +%F >/dev/null; date -d nonsense", "date: invalid date 'nonsense'\n", 1},
		{"printf 'a\\nb\\n' | less", "a\nb\n", 0},
		{"printf 'a\\nb\\n' | more -R", "a\nb\n", 0},
		{"printf 'a\\nb\\n' | less -N", "     1  a\n     2  b\n", 0},
		{"printf 'x\\n' > f; less f; du -a f", "x\n1\tf\n", 0},
		{"mount", "tabfs on / type tmpfs (rw)\n", 0},
		{"mount -t tmpfs a b", "mount: permission denied (are you root?)\n", 32},
		{"umount /x", "umount: permission denied (are you root?)\n", 32},
		{"which cat curl kubectl k", "/usr/bin/cat\n/usr/bin/curl\n/usr/bin/kubectl\n/usr/bin/k\n", 0},
		{"which cat nosuchtool", "/usr/bin/cat\n", 1},
		{"nc -u host 53", "nc: UDP is not supported\n", 1},
	} {
		got, code := sh(t, tc.script)
		if got != tc.want || code != tc.code {
			t.Errorf("%s:\n got %q (%d)\nwant %q (%d)", tc.script, got, code, tc.want, tc.code)
		}
	}
}

func TestMiscDuDf(t *testing.T) {
	got, code := sh(t, "mkdir -p d/s; printf '%2049s' x > d/a; printf 'x' > d/s/b; du d; du -s d; du -a -c d; du -d 0 -k d; du -h d/a; du nosuch")
	if code != 1 {
		t.Errorf("code %d", code)
	}
	want := "1\td/s\n4\td\n4\td\n3\td/a\n1\td/s/b\n1\td/s\n4\td\n4\ttotal\n4\td\n3.0K\td/a\ndu: cannot access 'nosuch': No such file or directory\n"
	if got != want {
		t.Errorf("du:\n got %q\nwant %q", got, want)
	}
	out, code := sh(t, "df")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.HasPrefix(lines[0], "Filesystem") || !strings.HasSuffix(lines[0], "Use% Mounted on") ||
		!strings.HasPrefix(lines[1], "tabfs") || !strings.HasSuffix(lines[1], " /") || !strings.Contains(lines[1], "67108864") {
		t.Errorf("df: %q (%d)", out, code)
	}
	out, _ = sh(t, "df -hT /")
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "Type") || !strings.Contains(lines[1], "64G") {
		t.Errorf("df -hT: %q", out)
	}
	if out, code := sh(t, "df /nosuch"); code != 1 || !strings.Contains(out, "No such file") {
		t.Errorf("df nosuch: %q %d", out, code)
	}
}

func TestMiscHuman(t *testing.T) {
	for kb, want := range map[int64]string{0: "0", 5: "5.0K", 1024: "1.0M", 1536: "1.5M", 67108864: "64G", 4194304: "4.0G"} {
		if got := human(kb); got != want {
			t.Errorf("human(%d) = %s, want %s", kb, got, want)
		}
	}
}

func TestMiscWatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	exec := func(ctx context.Context, args []string) error {
		interp.HandlerCtx(ctx).Stdout.Write([]byte("fake " + strings.Join(args, " ") + "\n"))
		return nil
	}
	hc := interp.HandlerContext{Stdout: &out, Stderr: &out, Dir: "/", Env: nil}
	if err := watchCmd(ctx, hc, []string{"-n", "0.2", "fakecmd", "a b"}, exec); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if n := strings.Count(s, "\x1b[H\x1b[2J"); n < 2 {
		t.Errorf("rounds: %d in %q", n, s)
	}
	if !strings.Contains(s, "Every 0.2s: fakecmd a b") || !strings.Contains(s, "fake fakecmd a b\n") {
		t.Errorf("output %q", s)
	}
}

func listener(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello\n"))
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return ln, port
}

func TestMiscNet(t *testing.T) {
	_, port := listener(t)
	out, code := sh(t, "nc -zv 127.0.0.1 "+port)
	if code != 0 || out != "127.0.0.1 (127.0.0.1:"+port+") open\n" {
		t.Errorf("nc -z: %q %d", out, code)
	}
	out, code = sh(t, "nc -w 2 127.0.0.1 "+port)
	if code != 0 || out != "hello\n" {
		t.Errorf("nc: %q %d", out, code)
	}
	out, code = sh(t, "telnet 127.0.0.1 "+port)
	if code != 0 || out != "Connected to 127.0.0.1.\nhello\nConnection closed by foreign host.\n" {
		t.Errorf("telnet: %q %d", out, code)
	}
	out, code = sh(t, "ping -c 2 -i 0.05 127.0.0.1")
	if code != 0 || !strings.Contains(out, "PING 127.0.0.1 (127.0.0.1): 56 data bytes\n") ||
		!strings.Contains(out, "64 bytes from 127.0.0.1: seq=1 ttl=64 time=") || !strings.Contains(out, "2 packets transmitted, 2 packets received, 0% packet loss\nround-trip min/avg/max = ") {
		t.Errorf("ping: %q %d", out, code)
	}
	// Nobody listens on 9999 (the fake network hands out ports from 1).
	out, code = sh(t, "nc -z 127.0.0.1 9999")
	if code != 1 || out != "nc: connect to 127.0.0.1 port 9999 (tcp) failed: Connection refused\n" {
		t.Errorf("nc refused: %q %d", out, code)
	}
	out, code = sh(t, "ping -c 1 10.96.0.50")
	if code != 1 || !strings.Contains(out, "1 packets transmitted, 0 packets received, 100% packet loss") {
		t.Errorf("ping service ip: %q %d", out, code)
	}
	out, code = sh(t, "ping -c 1 no-such-host.invalid")
	if code != 1 || out != "ping: bad address 'no-such-host.invalid'\n" {
		t.Errorf("ping bad: %q %d", out, code)
	}
}
