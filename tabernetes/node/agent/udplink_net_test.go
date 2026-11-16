package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// cmsg builds one control message as the kernel writes it.
func cmsg(level, typ int32, data []byte) []byte {
	b := make([]byte, syscall.CmsgSpace(len(data)))
	h := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = level, typ
	h.SetLen(syscall.CmsgLen(len(data)))
	copy(b[syscall.CmsgLen(0):], data)
	return b
}

// sockaddrIn is a struct sockaddr_in: the family in host order (amd64 and
// arm64: little-endian), the port big-endian.
func sockaddrIn(family uint16, a netip.AddrPort) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[0:2], family)
	binary.BigEndian.PutUint16(b[2:4], a.Port())
	a4 := a.Addr().As4()
	copy(b[4:8], a4[:])
	return b
}

// The original destination of a datagram of the TPROXY socket (ND14).
func TestParseOrigDst(t *testing.T) {
	tab := netip.MustParseAddrPort("10.244.0.7:53")
	orig := cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, sockaddrIn(syscall.AF_INET, tab))
	other := cmsg(syscall.SOL_IP, syscall.IP_TTL, []byte{64, 0, 0, 0})
	for _, oob := range [][]byte{orig, append(other, orig...)} {
		got, err := parseOrigDst(oob)
		if err != nil || got != tab {
			t.Errorf("parseOrigDst = %v, %v, want %v", got, err, tab)
		}
	}
	for name, oob := range map[string][]byte{
		"none":       nil,
		"other only": other,
		"short":      cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, []byte{2, 0, 0, 53}),
		"IPv6":       cmsg(syscall.SOL_IP, syscall.IP_ORIGDSTADDR, sockaddrIn(syscall.AF_INET6, tab)),
		"truncated":  orig[:len(orig)-12],
	} {
		if got, err := parseOrigDst(oob); err == nil {
			t.Errorf("%s: parseOrigDst = %v, want an error", name, got)
		}
	}
}

// The UDP rules of the pod channel (ND13), with and without nft_socket.
func TestPodLinkRulesUDP(t *testing.T) {
	const (
		tcpTProxy = `iifname "tabpods1" meta nfproto ipv4 meta l4proto tcp tproxy ip to :15001 meta mark set 0x7ab accept`
		udpTProxy = `iifname "tabpods1" meta nfproto ipv4 meta l4proto udp tproxy ip to :15001 meta mark set 0x7ab accept`
		tcpSocket = `meta l4proto tcp socket transparent 1 meta mark set 0x7ab accept`
		udpSocket = `meta l4proto udp socket transparent 1 meta mark set 0x7ab accept`
		drop      = `iifname "tabpods1" drop`
	)
	for _, c := range []struct {
		socketRule bool
		want       []string
	}{
		{true, []string{tcpSocket, udpSocket, tcpTProxy, udpTProxy, drop}},
		{false, []string{tcpTProxy, udpTProxy, drop}},
	} {
		var got []string
		inChain := false
		for _, line := range strings.Split(podLinkRules(c.socketRule), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "chain prerouting"):
				inChain = true
			case line == "}":
				inChain = false
			case inChain && !strings.HasPrefix(line, "type "):
				got = append(got, line)
			}
		}
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("podLinkRules(%v), chain prerouting:\n%s\nwant:\n%s", c.socketRule, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
}

// socketAs makes UDP sockets for the tab addresses and TCP sockets for
// dialTransparent.
func TestSocketAsType(t *testing.T) {
	for _, typ := range []int{syscall.SOCK_DGRAM, syscall.SOCK_STREAM} {
		fd, err := socketAs(os.Geteuid(), typ)
		if err != nil {
			t.Fatalf("socketAs(%d, %d): %v", os.Geteuid(), typ, err)
		}
		got, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
		syscall.Close(fd)
		if err != nil || got != typ {
			t.Errorf("socketAs(_, %d): SO_TYPE %d, %v", typ, got, err)
		}
	}
}

// A tab address must be IPv4 with a port (IPv4 only, as dialTransparent).
func TestBindTransparentUDPIPv4(t *testing.T) {
	for _, a := range []string{"[fd00::7]:53", "10.244.0.7:0"} {
		if p, err := bindTransparentUDP(netip.MustParseAddrPort(a)); err == nil {
			p.Close()
			t.Errorf("bindTransparentUDP(%s) did not fail", a)
		}
	}
}

// udpPodScript is a pod: a UDP socket on 10.244.1.5:5353 that sends and
// receives on command. "send <ip> <port> <size>" sends; "recv" prints
// "<ip> <port> <size>" of the next datagram, or "timeout".
const udpPodScript = `
import socket, sys
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 1 << 20)
s.bind(('10.244.1.5', 5353))
s.settimeout(3)
print('ready', flush=True)
for line in sys.stdin:
    f = line.split()
    if f[0] == 'send':
        s.sendto(b'p' * int(f[3]), (f[1], int(f[2])))
        print('sent', flush=True)
    else:
        try:
            b, a = s.recvfrom(65536)
            print(a[0], a[1], len(b), flush=True)
        except socket.timeout:
            print('timeout', flush=True)
`

type udpTestPod struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Scanner
}

func (p *udpTestPod) do(cmd string) string {
	p.t.Helper()
	fmt.Fprintln(p.in, cmd)
	if !p.out.Scan() {
		p.t.Fatalf("pod: %q: no answer", cmd)
	}
	return p.out.Text()
}

// TestUDPLinkNetns checks the UDP rules, the TPROXY socket and the sockets
// of the tab addresses in a network namespace like the one of a rootless
// node (ND13 to ND18). It runs with TestPodLinkNetns and the same gate and
// command, see there.
func TestUDPLinkNetns(t *testing.T) {
	if os.Getenv("TABERNETES_NETNS_TEST") == "" {
		t.Skip("set TABERNETES_NETNS_TEST=1 and run in unshare, see TestPodLinkNetns")
	}
	netnsNode(t)
	if err := setupPodLinkNet(); err != nil {
		t.Fatal(err)
	}
	pod := netip.MustParseAddrPort("10.244.1.5:5353")
	tab := netip.MustParseAddrPort("10.244.0.7:53")
	for _, socketRule := range []bool{false, true} {
		t.Run(fmt.Sprintf("socketRule=%v", socketRule), func(t *testing.T) {
			if err := nft(podLinkRules(socketRule)); err != nil {
				t.Fatal(err)
			}
			waitUDPSocketsClosed(t)
			l, ch := newTestUDPLink(t)
			ctx, cancel := context.WithCancel(context.Background())
			swept := make(chan struct{})
			go func() { l.sweepLoop(ctx); close(swept) }()
			defer func() { cancel(); <-swept }()
			if err := startTProxyUDP(ctx, l); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("ip", "netns", "exec", "pod", "python3", "-I", "-c", udpPodScript)
			in, _ := cmd.StdinPipe()
			out, _ := cmd.StdoutPipe()
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Wait()
			defer in.Close()
			p := &udpTestPod{t: t, in: in, out: bufio.NewScanner(out)}
			if !p.out.Scan() || p.out.Text() != "ready" {
				t.Fatal("pod did not start")
			}

			// fromPod gets the datagram with the source and the original
			// destination.
			fromPod := func(what string, src, dst netip.AddrPort, size int) {
				t.Helper()
				m, err := parseUDPMessage(ch.waitWrite(t))
				if err != nil || m.src != src || m.dst != dst || len(m.payload) != size {
					t.Errorf("%s: the page got %v -> %v, %d bytes (%v), want %v -> %v, %d bytes", what, m.src, m.dst, len(m.payload), err, src, dst, size)
				}
			}
			toPod := func(what string, src netip.AddrPort, size int) {
				t.Helper()
				m, err := appendUDPDatagram(nil, src, pod, make([]byte, size))
				if err != nil {
					t.Fatal(err)
				}
				l.fromPage(m)
				if got, want := p.do("recv"), fmt.Sprintf("%s %d %d", src.Addr(), src.Port(), size); got != want {
					t.Errorf("%s: the pod got %q, want %q", what, got, want)
				}
			}

			// ND13, ND14: the first datagram, with the original destination.
			p.do("send 10.244.0.7 53 100")
			fromPod("ND13/ND14 first datagram", pod, tab, 100)
			// ND15: the answer leaves from the socket of the tab address.
			toPod("ND15 answer", tab, 200)
			// ND18: the next datagram of the pod reaches fromPod again.
			p.do("send 10.244.0.7 53 101")
			fromPod("ND18 next datagram", pod, tab, 101)
			// ND17: the socket of the tab address has the owner 65534.
			if uid, ok := udpSocketUID(t, tab); !ok || uid != podLinkUID {
				t.Errorf("ND17: the socket of %s has uid %d (found %v), want %d", tab, uid, ok, podLinkUID)
			}
			// ND16: a tab address sends first; the answer of the pod arrives.
			tab2 := netip.MustParseAddrPort("10.244.0.8:7000")
			toPod("ND16 tab sends first", tab2, 50)
			p.do("send 10.244.0.8 7000 51")
			fromPod("ND16 answer of the pod", pod, tab2, 51)
			// The largest datagram, in both directions.
			p.do("send 10.244.0.7 53 65507")
			fromPod("65,507 bytes pod -> tab", pod, tab, udpMaxDatagram)
			toPod("65,507 bytes tab -> pod", tab, udpMaxDatagram)
			// A source outside the pod network (the node IP of a tab node in
			// a Web Worker) gets its answer only through the socket rule.
			if socketRule {
				host := netip.MustParseAddrPort("10.0.0.1:9000")
				toPod("node IP sends", host, 10)
				p.do("send 10.0.0.1 9000 11")
				fromPod("answer to the node IP", pod, host, 11)
			}
			if t.Failed() {
				t.Logf("drops: %v", l.drops.count)
			}
		})
	}
}

// udpSocketUID gives the owner of the UDP socket bound to a, from
// /proc/net/udp.
func udpSocketUID(t *testing.T, a netip.AddrPort) (int, bool) {
	t.Helper()
	b, err := os.ReadFile("/proc/net/udp")
	if err != nil {
		t.Fatal(err)
	}
	a4 := a.Addr().As4()
	// The address in the byte order of the host (little-endian), the port big.
	local := fmt.Sprintf("%08X:%04X", binary.LittleEndian.Uint32(a4[:]), a.Port())
	for _, line := range strings.Split(string(b), "\n")[1:] {
		f := strings.Fields(line)
		if len(f) > 7 && f[1] == local {
			var uid int
			fmt.Sscan(f[7], &uid)
			return uid, true
		}
	}
	return 0, false
}

// waitUDPSocketsClosed waits until the TPROXY socket of an earlier subtest
// is closed: it closes after its context ends.
func waitUDPSocketsClosed(t *testing.T) {
	t.Helper()
	port := fmt.Sprintf(":%04X ", tproxyPort)
	for i := 0; i < 100; i++ {
		b, _ := os.ReadFile("/proc/net/udp")
		if !strings.Contains(string(b), port) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the TPROXY socket of the earlier subtest is still open")
}
