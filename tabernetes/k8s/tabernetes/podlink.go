package main

// The pod channel (docs/networking-policy.md): a connection from a pod, or
// the host of a node, to a pod on another node. The apiserver's port-forward
// would lose the source; this channel carries it.
//
// Labels:
//
//	pod:<node>       a node asks the page for a channel to the pod network of
//	                 <node> (from a Web Worker or a Docker node); "pod:*" lets
//	                 the page pick the node of the destination
//	pod;from=<node>  the channel arrives at <node>: the page names the node it
//	                 comes from, the sender cannot choose it
//
// Only the page opens channels into a node, so the receiving node trusts the
// name in the label. The page also checks the header of every channel it
// relays, so a Docker node, which has no informers, can trust it as well. The first line of the channel is the header
// "TABP/1 tcp <src ip:port> <dst ip:port>". The receiving node checks that
// the source belongs to the sending node (its node IP or pod CIDR), asks its
// own NetworkPolicy engine and answers "ok" or "refused". A denied
// connection gets no answer, like a dropped SYN; the sender times out.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall/js"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	podLinkLabel   = "pod"
	podLinkVersion = "TABP/1"
	// podLinkNodeLabel marks the nodes that take pod channels.
	podLinkNodeLabel = "tabernetes.io/pod-channel"
)

// podLinkNode is the node of a pod channel label, and whether it is one.
// "pod:<node>" asks for a route to <node>, "pod;from=<node>" comes from it.
func podLinkNode(label string) (node string, route, ok bool) {
	if n, ok := strings.CutPrefix(label, podLinkLabel+":"); ok && n != "" {
		return n, true, true
	}
	if n, ok := strings.CutPrefix(label, podLinkLabel+";from="); ok && n != "" {
		return n, false, true
	}
	return "", false, false
}

// attachPodLink handles a pod channel that arrived at this instance. from is
// the node the page says it comes from ("" in a Web Worker: channels there
// come from the page and name their node in the label).
func attachPodLink(conn io.ReadWriteCloser, label, from string) {
	node, route, _ := podLinkNode(label)
	if route {
		// Only the page routes, and only channels of nodes it knows.
		if from == "" {
			fmt.Fprintln(os.Stderr, "pod channel: route request without a sending node")
			conn.Close()
			return
		}
		line, err := readPodLinkLine(conn, 256)
		if err != nil {
			conn.Close()
			return
		}
		src, dstAddr, err := parsePodLinkHeader(line)
		if err != nil || !theTabNode.policy.nodeOwns(from, src.IP) {
			fmt.Fprintf(os.Stderr, "pod channel from %s: refused header %q\n", from, line)
			conn.Close()
			return
		}
		if node == "*" {
			node = theTabNode.policy.nodeOf(dstAddr.IP)
		}
		if node == tabNodeName {
			theTabNode.servePodLinkHeader(conn, from, line)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		dst, err := openNodeChannel(ctx, node, podLinkLabel+";from="+from)
		if err == nil {
			_, err = fmt.Fprintf(dst, "%s\n", line)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "pod channel %s -> %s (%s): %v\n", from, node, dstAddr, err)
			if dst != nil {
				dst.Close()
			}
			// Nobody can take it: like a host without route.
			fmt.Fprintf(conn, "refused\n")
			conn.Close()
			return
		}
		splice(conn, dst)
		return
	}
	if from != "" {
		// A sending node may not name itself.
		fmt.Fprintf(os.Stderr, "pod channel: %s sent a stamped channel\n", from)
		conn.Close()
		return
	}
	theTabNode.servePodLink(conn, node)
}

// openNodeChannel opens a channel with label to the node name: from the page
// directly, from a Web Worker through the page.
func openNodeChannel(ctx context.Context, name, label string) (io.ReadWriteCloser, error) {
	nodesMu.Lock()
	jn := nodes[name]
	nodesMu.Unlock()
	var p js.Value
	switch open := js.Global().Get("tabernetesOpen"); {
	case jn != nil:
		p = jn.open.Invoke(label)
	case open.Truthy() && strings.HasPrefix(label, podLinkLabel+":"):
		p = open.Invoke(label)
	default:
		return nil, fmt.Errorf("no channel to node %s", name)
	}
	dc, err := awaitPromise(ctx, p)
	if err != nil {
		return nil, err
	}
	return newDCConn(dc), nil
}

// dialPodLink connects src to dst, a pod on the node node, over a pod channel.
// It waits for the answer of the node: refused, ok, or none (dropped).
func dialPodLink(ctx context.Context, node string, src, dst *net.TCPAddr) (net.Conn, error) {
	label := podLinkLabel + ";from=" + tabNodeName // from the page
	nodesMu.Lock()
	_, direct := nodes[node]
	nodesMu.Unlock()
	if !direct {
		label = podLinkLabel + ":" + node // from a Web Worker, through the page
	}
	c, err := openNodeChannel(ctx, node, label)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(c, "%s tcp %s %s\n", podLinkVersion, src, dst); err != nil {
		c.Close()
		return nil, err
	}
	answer := make(chan string, 1)
	go func() {
		line, _ := readPodLinkLine(c, 64)
		answer <- line
	}()
	select {
	case line := <-answer:
		switch line {
		case "ok":
			return &streamConn{ReadWriteCloser: c, local: src, remote: dst}, nil
		case "refused":
			c.Close()
			return nil, refusedError(dst)
		}
		c.Close()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: dst, Err: fmt.Errorf("pod channel to %s: unexpected answer %q", node, line)}
	case <-ctx.Done():
		c.Close()
		return nil, dropped(ctx, dst)
	case <-time.After(synTimeout):
		c.Close()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: dst, Err: os.ErrDeadlineExceeded}
	}
}

// readPodLinkLine reads up to "\n", byte by byte: the rest of the stream belongs
// to the connection.
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

// servePodLink serves a pod channel from the node from.
func (n *tabNode) servePodLink(conn io.ReadWriteCloser, from string) {
	if n == nil {
		conn.Close()
		return
	}
	line, err := readPodLinkLine(conn, 256)
	if err != nil {
		conn.Close()
		return
	}
	n.servePodLinkHeader(conn, from, line)
}

// servePodLinkHeader serves a pod channel whose header line was read.
func (n *tabNode) servePodLinkHeader(conn io.ReadWriteCloser, from, line string) {
	src, dst, err := parsePodLinkHeader(line)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pod channel from %s: %v\n", from, err)
		conn.Close()
		return
	}
	if !n.policy.nodeOwns(from, src.IP) {
		fmt.Fprintf(os.Stderr, "pod channel from %s: source %s is not on that node\n", from, src.IP)
		conn.Close()
		return
	}
	if !n.isTabPodIP(dst.IP.String()) {
		fmt.Fprintf(conn, "refused\n")
		conn.Close()
		return
	}
	ctx := context.Background()
	if !n.policy.allowed(ctx, src, dst) {
		// Dropped: no answer. The sender times out and closes; free the
		// channel after the time a SYN would take at most.
		go func() {
			time.Sleep(synTimeout)
			conn.Close()
		}()
		return
	}
	n.policy.admit(src.IP, dst)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	c, err := (&net.Dialer{LocalAddr: &net.TCPAddr{IP: src.IP}}).DialContext(ctx, "tcp", dst.String())
	if err != nil {
		n.policy.consume(src.IP, dst)
		fmt.Fprintf(conn, "refused\n")
		conn.Close()
		return
	}
	if _, err := fmt.Fprintf(conn, "ok\n"); err != nil {
		c.Close()
		conn.Close()
		return
	}
	go pipeConns(conn, c)
}

// podLinkTarget is the node of the pod with IP ip if that node takes pod
// channels.
func (n *tabNode) podLinkTarget(node string) bool {
	if n == nil || n.policy == nil || node == "" || node == tabNodeName {
		return false
	}
	nd, err := n.policy.nodes.Get(node)
	return err == nil && nd.Labels[podLinkNodeLabel] != ""
}

// nodeOf is the node whose pod CIDR contains ip, "" if none.
func (p *netPolicy) nodeOf(ip net.IP) string {
	if p == nil {
		return ""
	}
	list, err := p.nodes.List(labels.Everything())
	if err != nil {
		return ""
	}
	for _, nd := range list {
		for _, c := range nodePodCIDRs(nd) {
			if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
				return nd.Name
			}
		}
	}
	return ""
}

func nodePodCIDRs(nd *v1.Node) []string {
	if len(nd.Spec.PodCIDRs) == 0 && nd.Spec.PodCIDR != "" {
		return []string{nd.Spec.PodCIDR}
	}
	return nd.Spec.PodCIDRs
}

// nodeOwns reports whether ip is the node IP or in the pod CIDR of node name.
func (p *netPolicy) nodeOwns(name string, ip net.IP) bool {
	if p == nil {
		return false
	}
	nd, err := p.nodes.Get(name)
	if err != nil {
		return false
	}
	for _, a := range nd.Status.Addresses {
		if a.Type == v1.NodeInternalIP && net.ParseIP(a.Address).Equal(ip) {
			return true
		}
	}
	for _, c := range nodePodCIDRs(nd) {
		if _, n, err := net.ParseCIDR(c); err == nil && n.Contains(ip) {
			return true
		}
	}
	return false
}
