package main

import "net/netip"

// The hooks between the UDP table and the udp channel (docs/wasi-sockets.md,
// section 8.9.1). A hook that is nil drops the datagram. The addresses are
// IPv4 addresses without the IPv4-in-IPv6 form (netip.Addr.Unmap).
var (
	// udpToNode sends a datagram of the UDP table of this tab node to a pod
	// or the node IP of another node. src is the source after the flow
	// table, dst the destination. It must not wait and must not keep b. It
	// reports false when it dropped the datagram. podlink_udp.go sets it;
	// tabnode_udp.go calls it (section 8.7, step 4).
	udpToNode func(src, dst netip.AddrPort, b []byte) bool

	// udpFromNode gives a datagram from another node to the UDP table of
	// this tab node. It must not wait. It can keep b. tabnode_udp.go sets
	// it; podlink_udp.go calls it.
	udpFromNode func(src, dst netip.AddrPort, b []byte)
)
