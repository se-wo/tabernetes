package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The network of pods on the tab node is the in-process network of the wasm
// (Go's fake network for js): a socket is known by its exact ip:port. A pod
// listens on its pod IP, the apiserver and other pods dial that. There is no
// loopback per pod: localhost of a pod is its pod IP. The loopback table
// (tabnode_loopback.go) keeps the listeners on a loopback host for the pod
// itself.

// errPortInUse is the error of reservePort and of listenPod for an address
// that a listener has (wasi: address-in-use).
var errPortInUse = errors.New("address in use")

// The table of the ports in use of this tab node (docs/wasi-sockets.md,
// section 8.3.2, rule 3). Binds of components (tabnode_wasi_tcp.go),
// listeners of listenPod and the local ports of dialClusterFrom use it. The
// in-process network does not examine the local port of a dial, and it gives
// port 0 a counter from 1 (fact 11), so this table keeps the ports. The key
// has the protocol: W4.1 adds UDP.
var (
	portsMu sync.Mutex
	ports   = map[loopbackKey]*portUse{}
)

// portUse is one IP and port in the table: the number of reservations (binds
// and local ports of connections) and whether a listener has it.
type portUse struct {
	reserved int
	listener bool
}

// reservePort reserves a port of ip (the pod IP) for proto ("tcp") in the
// table of the ports in use (section 8.3.2, rule 3). Port 0 gives the first
// free port from 32768 to 60999 (TC9). For a port that is not 0, it gives
// errPortInUse if a listener has ip and port. Two reservations without a
// listener do not conflict (SO_REUSEADDR, rule 3). release frees the port.
func reservePort(proto, ip string, port int) (got int, release func(), err error) {
	portsMu.Lock()
	defer portsMu.Unlock()
	if port == 0 {
		for p := proxyEphemeralFirst; p <= proxyEphemeralLast; p++ { // TC9: the range of Linux
			if ports[newLoopbackKey(proto, ip, p)] == nil {
				port = p
				break
			}
		}
		if port == 0 {
			return 0, nil, errPortInUse
		}
	}
	k := newLoopbackKey(proto, ip, port)
	u := ports[k]
	if u != nil && u.listener {
		return 0, nil, errPortInUse
	}
	if u == nil {
		u = &portUse{}
		ports[k] = u
	}
	u.reserved++
	return port, onceFunc(func() { dropPortUse(k, func(u *portUse) { u.reserved-- }) }), nil
}

// portReserved reports whether the table has a reservation or a listener
// for ip and port. W3.1b uses it if W3.3a finds that runwasi refuses a
// second bind without a listener.
func portReserved(proto, ip string, port int) bool {
	portsMu.Lock()
	defer portsMu.Unlock()
	return ports[newLoopbackKey(proto, ip, port)] != nil
}

// claimListener notes a listener on ip and port in the table of the ports in
// use. It gives errPortInUse if another listener has them.
func claimListener(proto, ip string, port int) (release func(), err error) {
	k := newLoopbackKey(proto, ip, port)
	portsMu.Lock()
	defer portsMu.Unlock()
	u := ports[k]
	if u != nil && u.listener {
		return nil, errPortInUse
	}
	if u == nil {
		u = &portUse{}
		ports[k] = u
	}
	u.listener = true
	return onceFunc(func() { dropPortUse(k, func(u *portUse) { u.listener = false }) }), nil
}

func dropPortUse(k loopbackKey, change func(*portUse)) {
	portsMu.Lock()
	defer portsMu.Unlock()
	if u := ports[k]; u != nil {
		change(u)
		if u.reserved <= 0 && !u.listener {
			delete(ports, k)
		}
	}
}

// onceFunc is sync.OnceFunc for a release: a second call does nothing.
func onceFunc(f func()) func() { return sync.OnceFunc(f) }

// listenPod listens for a program of a container. "", 0.0.0.0 and :: become
// the pod IP. A loopback host (127.0.0.0/8, localhost, ::1) also becomes the
// pod IP, and the listener gets a row in the loopback table: only the pod
// itself can connect (section 8.11, TC2, PX5). This applies to each program
// of a pod, as on Linux. Inside a pod, the listener is in the table of the
// ports in use until it closes. Outside a pod (the terminal), the address is
// used as it is.
func listenPod(ctx context.Context, addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	pe := procOf(ctx)
	loopback := false
	if pe != nil {
		switch {
		case host == "", host == "0.0.0.0", host == "::", host == "[::]":
			host = pe.ip
		case isLoopbackHost(host):
			host = pe.ip
			loopback = true
		}
	}
	addr = net.JoinHostPort(host, port)
	if pe != nil {
		if p, err := strconv.Atoi(port); err == nil && p != 0 && portHasListener("tcp", host, p) {
			return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: literalTCPAddr(addr), Err: errPortInUse}
		}
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: literalTCPAddr(addr), Err: errPortInUse}
		}
		return nil, err
	}
	ln := &podListener{Listener: l}
	if pe != nil {
		a, _ := l.Addr().(*net.TCPAddr)
		if a != nil {
			release, err := claimListener("tcp", host, a.Port)
			if err != nil {
				l.Close()
				return nil, &net.OpError{Op: "listen", Net: "tcp", Addr: a, Err: err}
			}
			ln.releases = append(ln.releases, release)
			if loopback && pe.ip != tabNodeIP {
				// A hostNetwork pod has the node IP: the host of the node
				// (the terminal, the kubelet) reaches its 127.0.0.1 on
				// Linux. It gets no row.
				ln.releases = append(ln.releases, addLoopback("tcp", host, a.Port))
			}
		}
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	if pe != nil && pe.ip != tabNodeIP && theTabNode != nil {
		// NetworkPolicy for ingress (tabnode_netpol.go). hostNetwork pods
		// share the node IP; no policy selects them.
		return &policyListener{Listener: ln, n: theTabNode}, nil
	}
	return ln, nil
}

// portHasListener reports whether a listener has ip and port in the table of
// the ports in use.
func portHasListener(proto, ip string, port int) bool {
	portsMu.Lock()
	defer portsMu.Unlock()
	u := ports[newLoopbackKey(proto, ip, port)]
	return u != nil && u.listener
}

// podListener is a listener of listenPod. Close removes its rows from the
// table of the ports in use and from the loopback table.
type podListener struct {
	net.Listener
	releases []func()
}

func (l *podListener) Close() error {
	err := l.Listener.Close()
	for _, r := range l.releases {
		r()
	}
	return err
}

// dialCluster dials from a pod or the terminal into the cluster: localhost
// is the pod (or 127.0.0.1 in the terminal), names of services resolve to
// their cluster IP, which the service proxy (tabnode_net.go) serves. A pod
// dials from its pod IP, and NetworkPolicy decides before the connection
// exists (tabnode_netpol.go). The terminal, the kubelet and hostNetwork pods
// are the host of the node: no policy. Each path examines the loopback table
// (section 8.11).
func dialCluster(ctx context.Context, network, addr string) (net.Conn, error) {
	addr, err := clusterAddr(ctx, addr)
	if err != nil {
		return nil, err
	}
	var src *net.TCPAddr
	if pe := procOf(ctx); pe != nil && pe.ip != "" {
		src = &net.TCPAddr{IP: net.ParseIP(pe.ip)}
	}
	return dialClusterSrc(ctx, src, addr)
}

// dialClusterFrom dials addr as dialCluster does, from the pod of ctx, with
// the local port localPort of the pod IP. localPort 0 reserves an ephemeral
// port with reservePort and releases it when the connection closes.
func dialClusterFrom(ctx context.Context, addr string, localPort int) (net.Conn, error) {
	addr, err := clusterAddr(ctx, addr)
	if err != nil {
		return nil, err
	}
	pe := procOf(ctx)
	if pe == nil || pe.ip == "" {
		return dialClusterSrc(ctx, nil, addr)
	}
	release := func() {}
	if localPort == 0 {
		localPort, release, err = reservePort("tcp", pe.ip, 0)
		if err != nil {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Addr: literalTCPAddr(addr), Err: err}
		}
	}
	c, err := dialClusterSrc(ctx, &net.TCPAddr{IP: net.ParseIP(pe.ip), Port: localPort}, addr)
	if err != nil {
		release()
		return nil, err
	}
	return &portConn{Conn: c, release: release}, nil
}

// clusterAddr resolves the host of addr as a pod would (clusterHost).
func clusterAddr(ctx context.Context, addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(clusterHost(ctx, host), port), nil
}

// dialClusterSrc dials addr, after clusterHost, from src: the pod IP (with a
// local port or 0), or nil for the terminal.
func dialClusterSrc(ctx context.Context, src *net.TCPAddr, addr string) (net.Conn, error) {
	if pe := procOf(ctx); pe != nil && pe.ip != "" && pe.ip != tabNodeIP && theTabNode != nil && src != nil {
		return theTabNode.dialFromPod(ctx, src, addr)
	}
	// The host of the node: the terminal and hostNetwork pods. Its source is
	// not the pod IP of a row (section 8.11, TC15).
	var from net.IP
	if src != nil {
		from = src.IP
	}
	if dst := literalTCPAddr(addr); dst != nil && loopbackRefused("tcp", from, dst) {
		return nil, refusedError(dst)
	}
	d := &net.Dialer{}
	if src != nil && src.Port != 0 {
		d.LocalAddr = src
	}
	return d.DialContext(ctx, "tcp", addr)
}

// portConn is a connection of dialClusterFrom with an ephemeral port: Close
// releases the port in the table of the ports in use.
type portConn struct {
	net.Conn
	release func()
}

func (c *portConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}

// CloseWrite closes the send direction (TC13), if the connection can.
func (c *portConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// Unwrap gives the connection of the dial.
func (c *portConn) Unwrap() net.Conn { return c.Conn }

// clusterHost resolves host the way a pod would: localhost, service names
// and IPs.
func clusterHost(ctx context.Context, host string) string {
	pe := procOf(ctx)
	switch host {
	case "localhost", "127.0.0.1", "::1":
		if pe != nil {
			return pe.ip
		}
		return "127.0.0.1"
	}
	if net.ParseIP(host) != nil {
		return host
	}
	ns := "default"
	if pe != nil {
		ns = pe.w.namespace
	}
	if theTabNode != nil && theTabNode.net != nil {
		if ip, ok := theTabNode.net.resolve(host, ns); ok {
			return ip
		}
	}
	return host
}

// inCluster reports whether a request to host stays in the wasm: localhost,
// cluster IPs, pod IPs, node IPs and names of services. Everything else goes
// to the browser's fetch().
func inCluster(ctx context.Context, host string) bool {
	h := clusterHost(ctx, host)
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, cidr := range []string{serviceCIDR, clusterCIDR, "10.0.0.0/24"} {
		if _, n, _ := net.ParseCIDR(cidr); n.Contains(ip) {
			return true
		}
	}
	return false
}

// clusterClient is an HTTP client for requests that stay in the wasm.
var clusterClient = &http.Client{Transport: &http.Transport{DialContext: dialCluster, DisableKeepAlives: true}}

// httpClientFor picks the cluster client or fetch() for a URL host.
func httpClientFor(ctx context.Context, host string) *http.Client {
	if inCluster(ctx, strings.Trim(host, "[]")) {
		return clusterClient
	}
	return http.DefaultClient
}

// newComponentClient gives the cluster client for one client request of a
// proxy component (docs/wasi-sockets.md, CR1 to CR5, CR11). It is not
// clusterClient: wasmtime gives the component the response as the server
// sends it. So it does not follow redirects (CR3), it does not decompress
// (CR5), and it adds no headers other than Host (CR4: the caller sets
// Header["User-Agent"] to stop the default of Go). Each dial has a connect
// timeout (CR11).
//
// Each request gets a client of its own: DisableKeepAlives would add
// "Connection: close" (CR4), and a shared pool would give the connection of
// one pod to another pod. The caller calls CloseIdleConnections of the
// transport after the response body.
func newComponentClient() (*http.Client, *http.Transport) {
	tr := &http.Transport{DialContext: dialComponent, DisableCompression: true, MaxIdleConnsPerHost: 1}
	return &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, tr
}

// componentConnectTimeout is the connect timeout of wasmtime for a client
// request. A variable, so that tests can make it short.
var componentConnectTimeout = 600 * time.Second

// errConnectTimeout is the cause of a dial that componentConnectTimeout
// stopped.
var errConnectTimeout = errors.New("connect timeout")

// connectError is a dial of newComponentClient that failed. timeout is true
// only when componentConnectTimeout stopped the dial: wasmtime gives
// connection-timeout for this and connection-refused for each other connect
// error, also for a SYN that NetworkPolicy drops (CR8).
type connectError struct {
	err     error
	timeout bool
}

func (e *connectError) Error() string { return e.err.Error() }
func (e *connectError) Unwrap() error { return e.err }

// dialComponent dials with dialCluster and the connect timeout of wasmtime.
func dialComponent(ctx context.Context, network, addr string) (net.Conn, error) {
	dctx, cancel := context.WithCancelCause(ctx)
	t := time.AfterFunc(componentConnectTimeout, func() { cancel(errConnectTimeout) })
	c, err := dialCluster(dctx, network, addr)
	t.Stop()
	if err != nil {
		timeout := errors.Is(context.Cause(dctx), errConnectTimeout)
		cancel(nil)
		return nil, &connectError{err: err, timeout: timeout}
	}
	// The connection can keep the dial context (a pod on another node):
	// release it with the request, not before.
	context.AfterFunc(ctx, func() { cancel(nil) })
	return c, nil
}
