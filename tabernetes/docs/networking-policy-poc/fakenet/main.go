// Throwaway proof of concept for docs/networking-policy.md: Go's fake
// network of js/wasm keeps the local address of a dial, so a listener sees
// the pod IP of the dialer. Run it with ../run.sh fakenet.
package main

import (
	"fmt"
	"net"
)

func main() {
	ln, err := net.Listen("tcp", "10.244.0.2:80")
	if err != nil {
		panic(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fmt.Println("accepted: remote", c.RemoteAddr(), "local", c.LocalAddr())
			c.Close()
		}
	}()
	// Plain dial, as dialCluster does today.
	c, err := net.Dial("tcp", "10.244.0.2:80")
	fmt.Println("plain dial:", err, c.LocalAddr())
	// Dial from the pod IP of the client.
	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("10.244.0.3")}}
	c, err = d.Dial("tcp", "10.244.0.2:80")
	fmt.Println("dial from pod IP:", err, c.LocalAddr())
	// Dial to an address nobody listens on.
	_, err = net.Dial("tcp", "10.244.0.9:80")
	fmt.Println("no listener:", err)
	var x [1]byte
	c.Read(x[:])
}
