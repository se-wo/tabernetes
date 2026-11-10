package main

// The pod channel of a Docker node (docs/networking-policy.md, phase 4):
// connections between pods of this node and pods of the tab nodes, with the
// source pod as the source address, so NetworkPolicy can decide them.
//
//   - Tab pod -> pod here: the page opens a data channel "pod;from=<node>"
//     with the header "TABP/1 tcp <src> <dst>". The agent dials the pod from
//     the source IP of the header (IP_TRANSPARENT), as uid podLinkUID: the
//     rules of kube-network-policies accept every packet of uid 0 on the node,
//     so they decide this connection like any other packet.
//   - Pod here -> tab pod: an nftables TPROXY rule hands connections to pod
//     IPs that no route of flannel serves (the pod CIDRs of the tab nodes) to
//     a listener of the agent, with the original destination. The agent
//     opens "pod:*"; the page checks the source against this node and
//     routes by destination. The kernel has decided the egress of the pod by
//     then (kube-network-policies evaluates pod-to-host traffic in its input
//     chain, and TPROXY keeps the destination).
//
// Only the page opens channels to this agent, and the page checks the
// header of every channel it relays, so the agent trusts the header.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/vishvananda/netlink"
)

const (
	podLinkLabel     = "pod"
	podLinkVersion   = "TABP/1"
	podLinkNodeLabel = "tabernetes.io/pod-channel=v1"
	// The pod network of flannel (k8s/tabernetes/addons/flannel.yaml).
	podNetwork = "10.244.0.0/16"
	// Connections of pods here to tab pods arrive at this port.
	tproxyPort = 15001
	// The mark and routing table that deliver them to the agent.
	tproxyMark  = 0x7ab
	tproxyTable = 0x7ab
	// The agent dials pods for the tab as this user.
	podLinkUID = 65534
	// How long a dropped connection takes to fail: the SYN retries of Linux.
	synTimeout = 127 * time.Second
	// flannel writes the subnet of this node here.
	flannelSubnetEnv = "/run/flannel/subnet.env"
)

// podLinkRules sends connections of pods to tab pods to the agent: TCP to
// the pod network that flannel does not route (no cni0, no flannel.1), and
// the answers to the agent's transparent dials. The established socket wins
// over the listener in TPROXY's lookup.
var podLinkRules = fmt.Sprintf(`table inet tabernetes_podlink
delete table inet tabernetes_podlink
table inet tabernetes_podlink {
	chain prerouting {
		type filter hook prerouting priority mangle; policy accept;
		meta l4proto tcp ip daddr %s fib daddr oifname != { "cni0", "flannel.1", "lo" } tproxy ip to :%d meta mark set %#x accept
	}
}
`, podNetwork, tproxyPort, tproxyMark)

// setupPodLinkNet installs the TPROXY rule, its routing and the bridge
// sysctl that lets nftables see the packets between pods of this node.
func setupPodLinkNet() error {
	const bridge = "/proc/sys/net/bridge/bridge-nf-call-iptables"
	if err := os.WriteFile(bridge, []byte("1"), 0o644); err != nil {
		log.Printf("NetworkPolicy between pods of this node needs br_netfilter in the host kernel: %v", err)
	}
	nft := exec.Command("nft", "-f", "-")
	nft.Stdin = strings.NewReader(podLinkRules)
	if out, err := nft.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, out)
	}
	rule := netlink.NewRule()
	rule.Mark = tproxyMark
	rule.Table = tproxyTable
	if err := netlink.RuleAdd(rule); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("ip rule: %w", err)
	}
	lo, err := netlink.LinkByName("lo")
	if err != nil {
		return err
	}
	_, all, _ := net.ParseCIDR("0.0.0.0/0")
	route := &netlink.Route{LinkIndex: lo.Attrs().Index, Dst: all, Table: tproxyTable, Type: syscall.RTN_LOCAL, Scope: netlink.SCOPE_HOST}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("ip route: %w", err)
	}
	return nil
}

// servePodLinks starts the pod channel: the TPROXY listener for connections
// to tab pods. Channels from the page arrive in onDataChannel.
func (t *tunnel) servePodLinks(ctx context.Context) {
	if err := setupPodLinkNet(); err != nil {
		log.Printf("pod channel: pods of this node cannot reach tab pods: %v", err)
		return
	}
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error { return setTransparent(c) }}
	l, err := lc.Listen(ctx, "tcp4", fmt.Sprintf("0.0.0.0:%d", tproxyPort))
	if err != nil {
		log.Printf("pod channel: %v", err)
		return
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go t.forwardToTab(c.(*net.TCPConn))
	}
}

// setTransparent lets a socket take connections to addresses that are not
// its own (TPROXY).
func setTransparent(c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_IP, syscall.IP_TRANSPARENT, 1)
	}); err != nil {
		return err
	}
	return serr
}

// forwardToTab carries a connection of a pod here to a tab pod. Its
// destination is the original one (TPROXY).
func (t *tunnel) forwardToTab(c *net.TCPConn) {
	src, dst := c.RemoteAddr().(*net.TCPAddr), c.LocalAddr().(*net.TCPAddr)
	ordered := true
	dc, err := t.pc.CreateDataChannel(podLinkLabel+":*", &webrtc.DataChannelInit{Ordered: &ordered})
	if err != nil {
		log.Printf("pod channel %s -> %s: %v", src, dst, err)
		c.Close()
		return
	}
	dc.OnOpen(func() {
		raw, err := dc.Detach()
		if err != nil {
			c.Close()
			return
		}
		if _, err := fmt.Fprintf(raw, "%s tcp %s %s\n", podLinkVersion, src, dst); err != nil {
			raw.Close()
			c.Close()
			return
		}
		answer := make(chan string, 1)
		go func() {
			line, _ := readPodLinkLine(raw, 64)
			answer <- line
		}()
		select {
		case line := <-answer:
			if line == "ok" {
				splice(c, raw)
				return
			}
			// Refused: a reset, like a port without listener.
			c.SetLinger(0)
		case <-time.After(synTimeout):
			// Dropped by the policy of the tab node. The pod saw the
			// connection open already (TPROXY answers the SYN); it sees it
			// end now.
		}
		raw.Close()
		c.Close()
	})
}

// servePodLink serves a pod channel from the page: a connection from a tab
// pod (or a tab node) to a pod of this node.
func servePodLink(raw io.ReadWriteCloser) {
	line, err := readPodLinkLine(raw, 256)
	if err != nil {
		raw.Close()
		return
	}
	src, dst, err := parsePodLinkHeader(line)
	if err != nil {
		log.Printf("pod channel: %v", err)
		raw.Close()
		return
	}
	if subnet, err := flannelSubnet(); err != nil || !subnet.Contains(dst.IP) {
		fmt.Fprintf(raw, "refused\n")
		raw.Close()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), synTimeout)
	defer cancel()
	c, err := dialTransparent(ctx, src.IP, dst)
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		// Dropped (by NetworkPolicy): no answer, the tab side times out.
		raw.Close()
		return
	case err != nil:
		fmt.Fprintf(raw, "refused\n")
		raw.Close()
		return
	}
	if _, err := fmt.Fprintf(raw, "ok\n"); err != nil {
		c.Close()
		raw.Close()
		return
	}
	splice(c, raw)
}

// dialTransparent connects to dst from the address src, which is not an
// address of this node, as uid podLinkUID.
func dialTransparent(ctx context.Context, src net.IP, dst *net.TCPAddr) (net.Conn, error) {
	src4, dst4 := src.To4(), dst.IP.To4()
	if src4 == nil || dst4 == nil {
		return nil, errors.New("IPv4 only")
	}
	fd, err := socketAs(podLinkUID)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Conn, error) {
		syscall.Close(fd)
		return nil, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); err != nil {
		return fail(fmt.Errorf("IP_TRANSPARENT: %w", err))
	}
	from := &syscall.SockaddrInet4{}
	copy(from.Addr[:], src4)
	if err := syscall.Bind(fd, from); err != nil {
		return fail(fmt.Errorf("bind %s: %w", src, err))
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		return fail(err)
	}
	to := &syscall.SockaddrInet4{Port: dst.Port}
	copy(to.Addr[:], dst4)
	err = syscall.Connect(fd, to)
	pending := errors.Is(err, syscall.EINPROGRESS)
	if err != nil && !pending {
		return fail(err)
	}
	f := os.NewFile(uintptr(fd), "podlink")
	defer f.Close()
	if d, ok := ctx.Deadline(); ok {
		f.SetWriteDeadline(d)
	}
	stop := context.AfterFunc(ctx, func() { f.SetWriteDeadline(time.Unix(1, 0)) })
	defer stop()
	rc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var soerr int
	if err := rc.Write(func(fd uintptr) bool {
		if pending {
			pending = false
			return false // wait until the socket is writable
		}
		soerr, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
		return true
	}); err != nil {
		return nil, err
	}
	if soerr != 0 {
		return nil, os.NewSyscallError("connect", syscall.Errno(soerr))
	}
	f.SetWriteDeadline(time.Time{})
	return net.FileConn(f)
}

// socketAs creates a TCP socket owned by uid: the kernel takes the owner of
// a socket from the file system uid of the thread that creates it. The
// thread changes its fsuid and back; a thread that cannot go back ends.
func socketAs(uid int) (int, error) {
	type result struct {
		fd  int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		syscall.RawSyscall(syscall.SYS_SETFSUID, uintptr(uid), 0, 0)
		var fd int
		var err error
		// setfsuid fails silently, for example for a uid outside the
		// user namespace: check it took.
		if cur, _, _ := syscall.RawSyscall(syscall.SYS_SETFSUID, ^uintptr(0), 0, 0); int(cur) != uid {
			fd, err = -1, fmt.Errorf("cannot create sockets as uid %d", uid)
		} else {
			fd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		}
		syscall.RawSyscall(syscall.SYS_SETFSUID, 0, 0, 0)
		// setfsuid returns the previous fsuid: -1 asks without changing it.
		if cur, _, _ := syscall.RawSyscall(syscall.SYS_SETFSUID, ^uintptr(0), 0, 0); cur == 0 {
			runtime.UnlockOSThread()
		}
		ch <- result{fd, err}
	}()
	r := <-ch
	return r.fd, r.err
}

// flannelSubnet is the pod subnet of this node.
func flannelSubnet() (*net.IPNet, error) {
	f, err := os.Open(flannelSubnetEnv)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseFlannelSubnet(f)
}

func parseFlannelSubnet(r io.Reader) (*net.IPNet, error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "FLANNEL_SUBNET="); ok {
			_, n, err := net.ParseCIDR(strings.TrimSpace(v))
			return n, err
		}
	}
	return nil, errors.New("no FLANNEL_SUBNET")
}

// readPodLinkLine reads up to "\n", byte by byte: the rest of the stream
// belongs to the connection.
func readPodLinkLine(r io.Reader, max int) (string, error) {
	var b []byte
	one := make([]byte, 1)
	for len(b) < max {
		if _, err := io.ReadFull(r, one); err != nil {
			return string(b), err
		}
		if one[0] == '\n' {
			return string(b), nil
		}
		b = append(b, one[0])
	}
	return string(b), errors.New("line too long")
}

// parsePodLinkHeader parses "TABP/1 tcp <src> <dst>".
func parsePodLinkHeader(line string) (src, dst *net.TCPAddr, err error) {
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != podLinkVersion || f[1] != "tcp" {
		return nil, nil, fmt.Errorf("bad header %q", line)
	}
	if src, err = net.ResolveTCPAddr("tcp", f[2]); err != nil || src.IP == nil {
		return nil, nil, fmt.Errorf("bad source in %q", line)
	}
	if dst, err = net.ResolveTCPAddr("tcp", f[3]); err != nil || dst.IP == nil {
		return nil, nil, fmt.Errorf("bad destination in %q", line)
	}
	return src, dst, nil
}
