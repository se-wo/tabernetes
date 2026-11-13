// Proof of concept for phase 0, item 9 of ../../wasi-sockets.md: a UDP table
// next to the in-process network of Go for js/wasm. A TCP listener in the
// in-process network and a UDP socket of the table use the same port on the
// same IP. Run it with ../run.sh udptable.
package main

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// key of the UDP table: protocol, IP and port (UD1).
type key struct {
	proto string
	addr  netip.AddrPort
}

type datagram struct {
	from netip.AddrPort
	data []byte
}

type udpSocket struct {
	t     *table
	local netip.AddrPort
	queue chan datagram
	bytes int // in the queue (UD5)
	mu    sync.Mutex
}

type table struct {
	mu      sync.Mutex
	sockets map[key]*udpSocket
	next    map[netip.Addr]uint16 // ephemeral ports for each IP (UD4)
}

const queueLimit = 256 << 10

var errInUse = errors.New("address-in-use")

func newTable() *table {
	return &table{sockets: map[key]*udpSocket{}, next: map[netip.Addr]uint16{}}
}

// bind adds a socket. Port 0 takes the next ephemeral port of the IP, from
// 32768 (UD3, UD4).
func (t *table) bind(addr netip.AddrPort) (*udpSocket, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if addr.Port() == 0 {
		p := t.next[addr.Addr()]
		if p < 32768 {
			p = 32768
		}
		for t.sockets[key{"udp", netip.AddrPortFrom(addr.Addr(), p)}] != nil {
			p++
		}
		t.next[addr.Addr()] = p + 1
		addr = netip.AddrPortFrom(addr.Addr(), p)
	}
	k := key{"udp", addr}
	if t.sockets[k] != nil {
		return nil, errInUse
	}
	s := &udpSocket{t: t, local: addr, queue: make(chan datagram, 1024)}
	t.sockets[k] = s
	return s, nil
}

// send drops the datagram when no socket has the address or the queue has
// no space (UD6, UD8). The sender gets no error.
func (s *udpSocket) send(to netip.AddrPort, b []byte) {
	s.t.mu.Lock()
	d := s.t.sockets[key{"udp", to}]
	s.t.mu.Unlock()
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.bytes+len(b) > queueLimit {
		return
	}
	d.bytes += len(b)
	d.queue <- datagram{s.local, append([]byte(nil), b...)}
}

func (s *udpSocket) recv() datagram {
	d := <-s.queue
	s.mu.Lock()
	s.bytes -= len(d.data)
	s.mu.Unlock()
	return d
}

func main() {
	pod := netip.MustParseAddr("10.244.0.2")
	client := netip.MustParseAddr("10.244.0.3")

	// TCP on 10.244.0.2:53 in the in-process network.
	ln, err := net.Listen("tcp", "10.244.0.2:53")
	fmt.Println("tcp listen 10.244.0.2:53:", err)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			c.Write([]byte("tcp answer"))
			c.Close()
		}
	}()
	// UDP on the same IP and port in the table.
	t := newTable()
	srv, err := t.bind(netip.AddrPortFrom(pod, 53))
	fmt.Println("udp bind 10.244.0.2:53:", err)
	_, err = t.bind(netip.AddrPortFrom(pod, 53))
	fmt.Println("second udp bind 10.244.0.2:53:", err)
	// The in-process network itself: UDP on the TCP port fails (fact 11).
	_, err = net.ListenPacket("udp", "10.244.0.2:53")
	fmt.Println("udp in the in-process network on the same port:", err)

	cl, _ := t.bind(netip.AddrPortFrom(client, 0))
	cl2, _ := t.bind(netip.AddrPortFrom(client, 0))
	other, _ := t.bind(netip.AddrPortFrom(pod, 0))
	fmt.Println("ephemeral ports:", cl.local, cl2.local, other.local)
	cl.send(srv.local, []byte("udp query"))
	d := srv.recv()
	fmt.Printf("udp server got %q from %v\n", d.data, d.from)
	srv.send(d.from, []byte("udp answer"))
	fmt.Printf("udp client got %q\n", cl.recv().data)

	c, err := net.Dial("tcp", "10.244.0.2:53")
	if err != nil {
		panic(err)
	}
	b := make([]byte, 64)
	n, _ := c.Read(b)
	fmt.Printf("tcp client got %q\n", b[:n])

	// The queue limit: 1,000 datagrams of 1,024 bytes to a socket that does
	// not read.
	for i := 0; i < 1000; i++ {
		cl.send(other.local, make([]byte, 1024))
	}
	fmt.Println("queued datagrams after 1,000 sends:", len(other.queue))
}
