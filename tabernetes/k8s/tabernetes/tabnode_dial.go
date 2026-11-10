package main

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// The network of pods on the tab node is the in-process network of the wasm
// (Go's fake network for js): a socket is known by its exact ip:port. A pod
// listens on its pod IP, the apiserver and other pods dial that. There is no
// loopback per pod: localhost of a pod is its pod IP, which is close enough
// for sidecars that talk over localhost.

// listenPod listens for a program of a container. "", 0.0.0.0, :: and
// localhost become the pod IP. Outside a pod (the terminal), the address is
// used as it is.
func listenPod(ctx context.Context, addr string) (net.Listener, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	pe := procOf(ctx)
	if pe != nil {
		switch host {
		case "", "0.0.0.0", "::", "[::]", "localhost", "127.0.0.1", "::1":
			host = pe.ip
		}
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, err
	}
	context.AfterFunc(ctx, func() { ln.Close() })
	if pe != nil && pe.ip != tabNodeIP && theTabNode != nil {
		// NetworkPolicy for ingress (tabnode_netpol.go). hostNetwork pods
		// share the node IP; no policy selects them.
		ln = &policyListener{Listener: ln, n: theTabNode}
	}
	return ln, nil
}

// dialCluster dials from a pod or the terminal into the cluster: localhost
// is the pod (or 127.0.0.1 in the terminal), names of services resolve to
// their cluster IP, which the service proxy (tabnode_net.go) serves. A pod
// dials from its pod IP, and NetworkPolicy decides before the connection
// exists (tabnode_netpol.go). The terminal, the kubelet and hostNetwork pods
// are the host of the node: no policy.
func dialCluster(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	host = clusterHost(ctx, host)
	addr = net.JoinHostPort(host, port)
	if pe := procOf(ctx); pe != nil && pe.ip != "" && pe.ip != tabNodeIP && theTabNode != nil {
		return theTabNode.dialFromPod(ctx, pe.ip, addr)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

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
