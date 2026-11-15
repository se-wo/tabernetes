package main

// The kernel part of the udp channel of the node agent (section 8.9 of
// docs/wasi-sockets.md, ND13 to ND18). udplink.go has the channel and the
// flow table.
//
//   - Pod here -> tab address: the route of the pod network takes the
//     datagram into the veth pair (podlink.go). On podLinkIn, the rules of
//     podLinkRules give it to the agent:
//   - with nft_socket, "socket transparent 1" sends a datagram to a tab
//     address that has an open socket (a tabPort) to that socket;
//   - else tproxy sends it to the TPROXY socket on port 15001, which reads
//     the original destination from IP_ORIGDSTADDR. tproxy finds only
//     connected UDP sockets, and a tabPort is not connected.
//     Both readers call fromPod(pod, tab address, datagram). The flow table
//     does not care which socket read it (ND18).
//   - Tab address -> pod here: fromPage writes the datagram with the tabPort
//     of the tab address: a UDP socket bound to the tab address with
//     IP_TRANSPARENT, owned by uid podLinkUID (ND16, ND17). The answers of
//     the tab side leave from the same socket, so the pod sees the tab
//     address as the source (ND15).
//
// A kernel error on a socket (for example an ICMP error) sends nothing to
// the page (channel rule 7): the agent logs the first error of each kind.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
)

func init() {
	openTabPort = openTransparentUDP
	startUDPNet = startTProxyUDP
}

// udpNetLink is the link that the readers of the sockets give the datagrams
// of the pods to. startUDPNet sets it.
var udpNetLink atomic.Pointer[udpLink]

// udpNetLogged has the kinds of socket errors that are in the log already.
var udpNetLogged sync.Map

// logUDPNetError logs the first error of each kind of where.
func logUDPNetError(where string, err error) {
	kind := err.Error()
	var errno syscall.Errno
	if errors.As(err, &errno) {
		kind = errno.Error()
	}
	if _, seen := udpNetLogged.LoadOrStore(where+": "+kind, true); !seen {
		log.Printf("udp channel: %s: %v (the first error of this kind; the page gets no error, channel rule 7)", where, err)
	}
}

// startTProxyUDP opens the TPROXY socket for datagrams of pods here to tab
// addresses and gives them to l until ctx ends.
func startTProxyUDP(ctx context.Context, l *udpLink) error {
	udpNetLink.Store(l)
	if out, err := exec.Command("nft", "list", "chain", "inet", "tabernetes_podlink", "prerouting").Output(); err == nil &&
		!strings.Contains(string(out), "socket transparent") {
		log.Printf("udp channel: no socket match, the host of a tab node in a Web Worker gets no UDP answers from pods of this node")
	}
	c, err := listenTProxyUDP(ctx, fmt.Sprintf("0.0.0.0:%d", tproxyPort))
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); c.Close() }()
	go readTProxyUDP(c, l.fromPod)
	return nil
}

// listenTProxyUDP opens a UDP socket that receives datagrams to addresses
// that are not its own, with their original destination.
func listenTProxyUDP(ctx context.Context, addr string) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			for _, o := range [][2]int{
				{syscall.SOL_IP, syscall.IP_TRANSPARENT},
				{syscall.SOL_IP, syscall.IP_RECVORIGDSTADDR},
				{syscall.SOL_SOCKET, syscall.SO_REUSEADDR},
			} {
				if err := syscall.SetsockoptInt(int(fd), o[0], o[1], 1); err != nil && serr == nil {
					serr = fmt.Errorf("setsockopt %d/%d: %w", o[0], o[1], err)
				}
			}
		}); err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("TPROXY socket: %w", err)
	}
	return pc.(*net.UDPConn), nil
}

// readTProxyUDP reads the datagrams of c and gives each with its original
// destination to fromPod, until c is closed.
func readTProxyUDP(c *net.UDPConn, fromPod func(src, dst netip.AddrPort, b []byte)) {
	buf := make([]byte, udpReadBuffer)
	oob := make([]byte, 128)
	for {
		n, oobn, _, src, err := c.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			logUDPNetError("TPROXY socket", err)
			continue
		}
		dst, err := parseOrigDst(oob[:oobn])
		if err != nil {
			logUDPNetError("TPROXY socket", err)
			continue
		}
		fromPod(src, dst, buf[:n])
	}
}

var errNoOrigDst = errors.New("no IP_ORIGDSTADDR in the control message")

// parseOrigDst gives the original destination from the control messages of
// a datagram (IP_ORIGDSTADDR: a struct sockaddr_in).
func parseOrigDst(oob []byte) (netip.AddrPort, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return netip.AddrPort{}, err
	}
	for _, m := range msgs {
		if m.Header.Level != syscall.SOL_IP || m.Header.Type != syscall.IP_ORIGDSTADDR {
			continue
		}
		// sa_family (host order), sin_port (big-endian), sin_addr.
		if len(m.Data) < 8 {
			return netip.AddrPort{}, errNoOrigDst
		}
		if family := uint16(m.Data[0]) | uint16(m.Data[1])<<8; family != syscall.AF_INET {
			return netip.AddrPort{}, fmt.Errorf("IP_ORIGDSTADDR of family %d", family)
		}
		return netip.AddrPortFrom(netip.AddrFrom4([4]byte(m.Data[4:8])), binary.BigEndian.Uint16(m.Data[2:4])), nil
	}
	return netip.AddrPort{}, errNoOrigDst
}

// udpTransparentPort is the socket of one tab address: bound to it with
// IP_TRANSPARENT, not connected, owned by uid podLinkUID.
type udpTransparentPort struct {
	tab netip.AddrPort
	c   *net.UDPConn
	rc  syscall.RawConn
}

// openTransparentUDP opens the socket of tab and starts its reader, which
// gives the datagrams of pods to the link of startUDPNet.
func openTransparentUDP(tab netip.AddrPort) (tabPort, error) {
	p, err := bindTransparentUDP(tab)
	if err != nil {
		logUDPNetError("socket of "+tab.Addr().String(), err)
		return nil, err
	}
	go p.read(func(src, dst netip.AddrPort, b []byte) {
		if l := udpNetLink.Load(); l != nil {
			l.fromPod(src, dst, b)
		}
	})
	return p, nil
}

// bindTransparentUDP makes the socket of tab. IPv4 only, as dialTransparent.
func bindTransparentUDP(tab netip.AddrPort) (*udpTransparentPort, error) {
	a := tab.Addr().Unmap()
	if !a.Is4() || tab.Port() == 0 {
		return nil, fmt.Errorf("tab address %s: IPv4 with a port only", tab)
	}
	tab = netip.AddrPortFrom(a, tab.Port())
	fd, err := socketAs(podLinkUID, syscall.SOCK_DGRAM)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*udpTransparentPort, error) {
		syscall.Close(fd)
		return nil, err
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_IP, syscall.IP_TRANSPARENT, 1); err != nil {
		return fail(fmt.Errorf("IP_TRANSPARENT: %w", err))
	}
	// The TPROXY socket has 0.0.0.0:15001: a tab address with port 15001
	// can still bind.
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return fail(fmt.Errorf("SO_REUSEADDR: %w", err))
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: a.As4(), Port: int(tab.Port())}); err != nil {
		return fail(fmt.Errorf("bind %s: %w", tab, err))
	}
	if err := syscall.SetNonblock(fd, true); err != nil {
		return fail(err)
	}
	f := os.NewFile(uintptr(fd), "udplink")
	defer f.Close()
	pc, err := net.FilePacketConn(f)
	if err != nil {
		return nil, err
	}
	c := pc.(*net.UDPConn)
	rc, err := c.SyscallConn()
	if err != nil {
		c.Close()
		return nil, err
	}
	return &udpTransparentPort{tab: tab, c: c, rc: rc}, nil
}

// WriteTo sends b from the tab address to the pod dst. It does not wait: a
// full send buffer is an error (a counted drop of the caller).
func (p *udpTransparentPort) WriteTo(b []byte, dst netip.AddrPort) error {
	d := dst.Addr().Unmap()
	if !d.Is4() {
		return errUDPNotIPv4
	}
	to := &syscall.SockaddrInet4{Addr: d.As4(), Port: int(dst.Port())}
	var serr error
	if err := p.rc.Write(func(fd uintptr) bool {
		serr = syscall.Sendto(int(fd), b, 0, to)
		return true
	}); err != nil {
		return err
	}
	if serr != nil {
		logUDPNetError("socket of "+p.tab.Addr().String(), serr)
		return serr
	}
	return nil
}

func (p *udpTransparentPort) Close() error { return p.c.Close() }

// read gives each datagram that arrives at the socket to fromPod, with the
// tab address as the destination, until the socket is closed.
func (p *udpTransparentPort) read(fromPod func(src, dst netip.AddrPort, b []byte)) {
	buf := make([]byte, udpReadBuffer)
	for {
		n, src, err := p.c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// For example ECONNREFUSED of an earlier datagram.
			logUDPNetError("socket of "+p.tab.Addr().String(), err)
			continue
		}
		fromPod(src, p.tab, buf[:n])
	}
}
