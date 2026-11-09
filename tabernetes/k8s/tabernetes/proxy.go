package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	v1 "k8s.io/api/core/v1"
)

// proxyClient sends the requests of the proxy command (web/src/proxy.js)
// into the in-process network: to localhost, where kubectl port-forward
// listens, or to a service, which the service proxy (tabnode_net.go) serves.
// No keep-alive: every request is a connection of its own, so the service
// proxy hands each to the next replica. Redirects go back to the browser
// tab, which keeps them inside the proxy.
var proxyClient = &http.Client{
	Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, _, _ := net.SplitHostPort(addr); host == "localhost" {
				return dialLocal(ctx, network, addr)
			}
			return dialCluster(ctx, network, addr)
		},
	},
	Timeout:       55 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

const proxyMaxBody = 64 << 20

// hopHeaders are not forwarded. Without Accept-Encoding the transport asks
// for gzip itself and unpacks it: a service worker response must not be
// compressed.
var hopHeaders = map[string]bool{
	"accept-encoding": true, "connection": true, "content-length": true, "host": true,
	"keep-alive": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
}

// proxyJS backs tabernetes.proxy(target, method, path, headers, body): it
// returns a Promise of {status, statusText, headers: [name, value][], body:
// Uint8Array}. target is a port of localhost or a service as
// proxyServiceJS returns it: "<name>.<namespace>:<port>".
func proxyJS(a []js.Value) js.Value {
	method, path := a[1].String(), a[2].String()
	isPort := a[0].Type() == js.TypeNumber
	hostport := a[0].String()
	if isPort {
		hostport = fmt.Sprintf("localhost:%d", a[0].Int())
	}
	header := http.Header{}
	for i := range a[3].Length() {
		kv := a[3].Index(i)
		if k := kv.Index(0).String(); !hopHeaders[strings.ToLower(k)] {
			header.Add(k, kv.Index(1).String())
		}
	}
	var body []byte
	if len(a) > 4 && a[4].Truthy() {
		body = make([]byte, a[4].Length())
		js.CopyBytesToGo(body, a[4])
	}
	return promise(func() (any, error) {
		req, err := http.NewRequest(method, "http://"+hostport+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header = header
		resp, err := proxyClient.Do(req)
		if err != nil && isPort {
			port := hostport[len("localhost:"):]
			return nil, fmt.Errorf("nothing answers on localhost:%s (kubectl port-forward ... %s:<pod port> &): %v", port, port, err)
		}
		if err != nil {
			return nil, fmt.Errorf("service %s: %v", hostport, err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, proxyMaxBody+1))
		if err != nil {
			return nil, err
		}
		if len(b) > proxyMaxBody {
			return nil, fmt.Errorf("response larger than %d MiB", proxyMaxBody>>20)
		}
		headers := []any{}
		for k, vs := range resp.Header {
			if hopHeaders[strings.ToLower(k)] {
				continue
			}
			for _, v := range vs {
				headers = append(headers, []any{k, v})
			}
		}
		arr := js.Global().Get("Uint8Array").New(len(b))
		js.CopyBytesToJS(arr, b)
		statusText, _ := strings.CutPrefix(resp.Status, fmt.Sprintf("%d ", resp.StatusCode))
		return map[string]any{
			"status":     resp.StatusCode,
			"statusText": statusText,
			"headers":    headers,
			"body":       arr,
		}, nil
	})
}

// proxyServiceJS backs tabernetes.proxyService(name, namespace, port): it
// checks that the service exists and returns a Promise of the target of
// tabernetes.proxy, "<name>.<namespace>:<port>". port is a number, the name
// of a port of the service or "" for its first port.
func proxyServiceJS(a []js.Value) js.Value {
	name, ns, port := a[0].String(), a[1].String(), a[2].String()
	return promise(func() (any, error) {
		if theTabNode == nil || theTabNode.net == nil || !theTabNode.net.synced.Load() {
			return nil, fmt.Errorf("the service proxy of the tab node is not running yet")
		}
		svc, err := theTabNode.net.services.Services(ns).Get(name)
		if err != nil {
			return nil, fmt.Errorf("service %q not found in namespace %q", name, ns)
		}
		if svc.Spec.Type == v1.ServiceTypeExternalName {
			return nil, fmt.Errorf("service %q is an ExternalName service", name)
		}
		if len(svc.Spec.Ports) == 0 {
			return nil, fmt.Errorf("service %q has no ports", name)
		}
		var p int32
		for _, sp := range svc.Spec.Ports {
			if port == "" || sp.Name == port || strconv.Itoa(int(sp.Port)) == port {
				p = sp.Port
				break
			}
		}
		if p == 0 {
			return nil, fmt.Errorf("service %q has no port %s", name, port)
		}
		return fmt.Sprintf("%s.%s:%d", name, ns, p), nil
	})
}
