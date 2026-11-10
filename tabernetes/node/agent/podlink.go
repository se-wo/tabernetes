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
//   - Pod here -> tab pod: the pod network that no route of flannel serves
//     (the pod CIDRs of the tab nodes) is routed into a veth pair of the
//     node. On its other end an nftables TPROXY rule hands the connection,
//     with the original destination, to a listener of the agent. The agent
//     opens "pod:*"; the page checks the source against this node and
//     routes by destination. The kernel has decided the egress of the pod by
//     then: the packet passed the postrouting chain of kube-network-policies
//     on its way into the veth pair, with the tab pod as its destination.
//
// Why a veth pair: TPROXY gives the packet its socket in prerouting. A
// packet of a pod enters through the bridge cni0, and with
// bridge-nf-call-iptables (which NetworkPolicy between pods of this node
// needs) prerouting runs inside the bridge, before the IP layer takes the
// packet and drops that socket again. Behind the veth pair prerouting runs
// in the IP layer.
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
	"net/netip"
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
	// The veth pair that carries connections of pods here to tab pods: the
	// pod network is routed into podLinkOut and arrives at podLinkIn.
	podLinkOut = "tabpods0"
	podLinkIn  = "tabpods1"
	// The next hop of that route. Nothing has this address: a permanent
	// neighbour entry names podLinkIn.
	podLinkGateway = "169.254.77.1"
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

// podLinkRules hands what arrives at podLinkIn to the agent: TCP goes to the
// TPROXY listener (an established socket wins over the listener in TPROXY's
// lookup), everything else is dropped, because only TCP has a way into the
// tab and the route would send it around again.
//
// These packets have passed the node once already, on their way into the
// veth pair, where conntrack and the DNAT of kube-proxy saw them; a cluster
// IP is the pod IP of its endpoint by now. They are not tracked a second
// time: after DNAT the second entry would collide with the first.
//
// With socketRule, the first rule also delivers the answers to the agent's
// transparent dials: from a tab pod they would take the veth pair as well,
// from the node IP of a tab node in a Web Worker (host traffic of that
// node) nothing else brings them back. It needs nft_socket in the kernel.
const podLinkRulesFormat = `table inet tabernetes_podlink
delete table inet tabernetes_podlink
table inet tabernetes_podlink {
	chain raw {
		type filter hook prerouting priority raw; policy accept;
		iifname "%[1]s" notrack
	}
	chain prerouting {
		type filter hook prerouting priority mangle; policy accept;
%[4]s		iifname "%[1]s" meta nfproto ipv4 meta l4proto tcp tproxy ip to :%[2]d meta mark set %#[3]x accept
		iifname "%[1]s" drop
	}
}
`

func podLinkRules(socketRule bool) string {
	rule := ""
	if socketRule {
		rule = fmt.Sprintf("\t\tmeta l4proto tcp socket transparent 1 meta mark set %#x accept\n", tproxyMark)
	}
	return fmt.Sprintf(podLinkRulesFormat, podLinkIn, tproxyPort, tproxyMark, rule)
}

// setupPodLinkNet installs the veth pair with its route, the TPROXY rule
// with its routing, and the bridge sysctl that lets nftables see the packets
// between pods of this node.
func setupPodLinkNet() error {
	const bridge = "/proc/sys/net/bridge/bridge-nf-call-iptables"
	if err := os.WriteFile(bridge, []byte("1"), 0o644); err != nil {
		log.Printf("NetworkPolicy between pods of this node needs br_netfilter in the host kernel: %v", err)
	}
	if err := setupPodLinkRoute(); err != nil {
		return err
	}
	if err := nft(podLinkRules(true)); err != nil {
		// Without nft_socket: everything but host traffic of worker tab nodes.
		log.Printf("pod channel: no socket match (%v), the host of a tab node in a Web Worker cannot reach pods of this node", err)
		if err := nft(podLinkRules(false)); err != nil {
			return err
		}
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

// setupPodLinkRoute creates the veth pair and routes the pod network into
// it. The routes of flannel are more specific (a /24 per node with flannel,
// cni0 for this node), so the route takes what is left: the tab nodes.
func setupPodLinkRoute() error {
	out, err := netlink.LinkByName(podLinkOut)
	if err != nil {
		veth := &netlink.Veth{LinkAttrs: netlink.NewLinkAttrs(), PeerName: podLinkIn}
		veth.Name = podLinkOut
		if err := netlink.LinkAdd(veth); err != nil {
			return fmt.Errorf("veth %s: %w", podLinkOut, err)
		}
		if out, err = netlink.LinkByName(podLinkOut); err != nil {
			return err
		}
	}
	in, err := netlink.LinkByName(podLinkIn)
	if err != nil {
		return err
	}
	for _, l := range []netlink.Link{out, in} {
		if err := netlink.LinkSetUp(l); err != nil {
			return fmt.Errorf("%s up: %w", l.Attrs().Name, err)
		}
	}
	// The packets arrive at podLinkIn from a pod, whose route back is cni0:
	// no strict reverse path filter there. The kernel takes the higher of
	// "all" and the interface.
	const conf = "/proc/sys/net/ipv4/conf/"
	if err := os.WriteFile(conf+podLinkIn+"/rp_filter", []byte("0"), 0o644); err != nil {
		return fmt.Errorf("rp_filter of %s: %w", podLinkIn, err)
	}
	if b, err := os.ReadFile(conf + "all/rp_filter"); err == nil && strings.TrimSpace(string(b)) == "1" {
		if err := os.WriteFile(conf+"all/rp_filter", []byte("2"), 0o644); err != nil {
			return fmt.Errorf("rp_filter: %w", err)
		}
	}
	gw := net.ParseIP(podLinkGateway)
	neigh := &netlink.Neigh{LinkIndex: out.Attrs().Index, IP: gw, HardwareAddr: in.Attrs().HardwareAddr, State: netlink.NUD_PERMANENT}
	if err := netlink.NeighSet(neigh); err != nil {
		return fmt.Errorf("neighbour on %s: %w", podLinkOut, err)
	}
	_, pods, _ := net.ParseCIDR(podNetwork)
	route := &netlink.Route{LinkIndex: out.Attrs().Index, Dst: pods, Gw: gw, Flags: int(netlink.FLAG_ONLINK)}
	if err := netlink.RouteReplace(route); err != nil {
		return fmt.Errorf("route %s: %w", podNetwork, err)
	}
	return nil
}

// nft loads a ruleset.
func nft(rules string) error {
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("nft: %v: %s", err, out)
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
		detached, err := dc.Detach()
		if err != nil {
			c.Close()
			return
		}
		raw := newDCStream(detached)
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

// dcStream reads a detached data channel as a stream of bytes. A read of the
// channel takes one whole message and fails when the buffer is smaller, so
// the header line cannot be read byte by byte from it.
type dcStream struct {
	io.ReadWriteCloser
	buf, rest []byte
}

func newDCStream(raw io.ReadWriteCloser) *dcStream {
	// 64 KiB: the largest message of a data channel.
	return &dcStream{ReadWriteCloser: raw, buf: make([]byte, 64*1024)}
}

func (s *dcStream) Read(p []byte) (int, error) {
	for len(s.rest) == 0 {
		n, err := s.ReadWriteCloser.Read(s.buf)
		if n == 0 && err != nil {
			return 0, err
		}
		s.rest = s.buf[:n]
	}
	n := copy(p, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}

// servePodLink serves a pod channel from the page: a connection from a tab
// pod (or a tab node) to a pod of this node.
func servePodLink(detached io.ReadWriteCloser) {
	raw := newDCStream(detached)
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
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT):
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
	if err := syscall.Connect(fd, to); err != nil && !errors.Is(err, syscall.EINPROGRESS) {
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
	// Ask the socket every time, also before the first wait: the answer can
	// arrive before the poller watches the socket, and then no event follows.
	var soerr int
	if err := rc.Write(func(fd uintptr) bool {
		soerr, _ = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_ERROR)
		if soerr != 0 {
			return true
		}
		_, err := syscall.Getpeername(int(fd))
		return err == nil // else still connecting: wait until the socket is writable
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

// parsePodLinkHeader parses "TABP/1 tcp <src> <dst>". The addresses are IP
// literals: a name is refused, not resolved.
func parsePodLinkHeader(line string) (src, dst *net.TCPAddr, err error) {
	f := strings.Fields(line)
	if len(f) != 4 || f[0] != podLinkVersion || f[1] != "tcp" {
		return nil, nil, fmt.Errorf("bad header %q", line)
	}
	s, err := netip.ParseAddrPort(f[2])
	if err != nil {
		return nil, nil, fmt.Errorf("bad source in %q", line)
	}
	d, err := netip.ParseAddrPort(f[3])
	if err != nil || d.Port() == 0 {
		return nil, nil, fmt.Errorf("bad destination in %q", line)
	}
	return net.TCPAddrFromAddrPort(s), net.TCPAddrFromAddrPort(d), nil
}
