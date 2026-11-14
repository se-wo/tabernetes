package main

import (
	"context"
	"net"
	"net/netip"
	"sync"
)

// The loopback table of this tab node (docs/wasi-sockets.md, section 8.11,
// decision 6). The tab node has no loopback for each pod: listenPod changes
// 127.0.0.0/8, localhost and ::1 to the pod IP. A row keeps the protocol, the
// pod IP and the port of such a listener. Each path that dials a pod IP
// examines the table before it connects: only the pod itself may connect, as
// on Linux. Each Go thread is its own tab node, so the table is a global of
// the package. W4.1 adds UDP rows (UD20, UD21).

// loopbackKey is a row of the loopback table and of the table of the ports
// in use (tabnode_dial.go): "tcp" (later "udp"), the pod IP and the port.
type loopbackKey struct {
	proto string
	ip    string
	port  int
}

func newLoopbackKey(proto, ip string, port int) loopbackKey {
	if a, err := netip.ParseAddr(ip); err == nil {
		ip = a.Unmap().String()
	}
	return loopbackKey{proto, ip, port}
}

var (
	loopbackMu sync.Mutex
	// loopbackRows maps a row to its owner. A release removes the row only
	// for its own owner.
	loopbackRows = map[loopbackKey]*int{}
)

// loopbackIP is the source of the kubelet probes, the lifecycle hooks and
// port-forward: they dial without a local address (section 4.3).
var loopbackIP = net.IPv4(127, 0, 0, 1)

// addLoopback adds a row (proto "tcp", the pod IP, the port) to the loopback
// table of this tab node (section 8.11). release removes it; a second call
// does nothing.
func addLoopback(proto, ip string, port int) (release func()) {
	k := newLoopbackKey(proto, ip, port)
	owner := new(int)
	loopbackMu.Lock()
	loopbackRows[k] = owner
	loopbackMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			loopbackMu.Lock()
			if loopbackRows[k] == owner {
				delete(loopbackRows, k)
			}
			loopbackMu.Unlock()
		})
	}
}

// loopbackRefused reports whether a dial from src to dst must get
// connection-refused: dst has a row, and src is not dst.IP.
func loopbackRefused(proto string, src net.IP, dst *net.TCPAddr) bool {
	if dst == nil || dst.IP == nil {
		return false
	}
	if src != nil && src.Equal(dst.IP) {
		return false
	}
	k := newLoopbackKey(proto, dst.IP.String(), dst.Port)
	loopbackMu.Lock()
	defer loopbackMu.Unlock()
	return loopbackRows[k] != nil
}

// isLoopbackHost reports whether host is a loopback host of listenPod:
// localhost, 127.0.0.0/8 or ::1.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

// literalTCPAddr gives the TCP address of addr if its host is an IP, else
// nil. It never resolves a name.
func literalTCPAddr(addr string) *net.TCPAddr {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil
	}
	return net.TCPAddrFromAddrPort(ap)
}

// dialFromNodeHost dials addr as the host of the node: the kubelet probes
// (tabnode_probe.go) and the lifecycle hooks (tabnode_lifecycle.go). They
// dial without a local address, so their source is 127.0.0.1, which is not
// the pod IP: a row of the loopback table refuses them (TC15, PX5).
func dialFromNodeHost(ctx context.Context, network, addr string) (net.Conn, error) {
	if dst := literalTCPAddr(addr); dst != nil && loopbackRefused("tcp", loopbackIP, dst) {
		return nil, refusedError(dst)
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}
