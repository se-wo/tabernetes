package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// probe runs the startup, liveness and readiness probes of a running
// container until ctx ends (the container stops).
func (c *ctr) probe(ctx context.Context, proc *process) {
	spec := c.spec
	if spec.StartupProbe != nil {
		if !c.runProbe(ctx, proc, "Startup", spec.StartupProbe, func(ok bool) bool {
			if ok {
				c.w.mu.Lock()
				c.startupOK = true
				c.ready = spec.ReadinessProbe == nil
				c.markStartedLocked()
				c.w.mu.Unlock()
				c.w.markDirty()
			}
			return !ok // stop after the first success
		}) {
			return
		}
	}
	if spec.LivenessProbe != nil {
		go c.runProbe(ctx, proc, "Liveness", spec.LivenessProbe, func(bool) bool { return true })
	}
	if spec.ReadinessProbe != nil {
		c.runProbe(ctx, proc, "Readiness", spec.ReadinessProbe, func(ok bool) bool {
			c.w.mu.Lock()
			changed := c.ready != ok
			c.ready = ok
			c.w.mu.Unlock()
			if changed {
				c.w.markDirty()
			}
			return true
		})
	}
}

// runProbe probes until ctx ends or result returns false. A probe that
// crosses its failure threshold kills the container (startup and liveness)
// or reports not ready (readiness). It returns false when ctx ended.
func (c *ctr) runProbe(ctx context.Context, proc *process, kind string, p *v1.Probe, result func(bool) bool) bool {
	period := time.Duration(max(p.PeriodSeconds, 1)) * time.Second
	timeout := time.Duration(max(p.TimeoutSeconds, 1)) * time.Second
	failureThreshold := max(p.FailureThreshold, 1)
	successThreshold := max(p.SuccessThreshold, 1)
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(p.InitialDelaySeconds) * time.Second):
	}
	fails, successes := int32(0), int32(0)
	for {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		err := c.probeOnce(pctx, p)
		cancel()
		if ctx.Err() != nil {
			return false
		}
		if err == nil {
			fails = 0
			successes++
			if successes >= successThreshold || kind != "Readiness" {
				if !result(true) {
					return true
				}
			}
		} else {
			successes = 0
			fails++
			c.w.ctrEvent(c, v1.EventTypeWarning, "Unhealthy", "%s probe failed: %v", kind, err)
			if fails >= failureThreshold {
				switch kind {
				case "Readiness":
					result(false)
				default:
					// Like the kubelet: the preStop hook runs, within the
					// grace period of the probe or the pod.
					msg := fmt.Sprintf("Container %s failed %s probe", c.spec.Name, strings.ToLower(kind))
					if c.sidecar || c.w.restartPolicy() != v1.RestartPolicyNever {
						msg += ", will be restarted"
					}
					pod, _ := c.w.podInfo()
					c.stopProc(proc, time.Now().Add(killGrace(pod, p)), msg, false)
					return false
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(period):
		}
	}
}

func (c *ctr) probeOnce(ctx context.Context, p *v1.Probe) error {
	_, ip := c.w.podInfo()
	switch {
	case p.HTTPGet != nil:
		req, err := c.httpGetRequest(ctx, p.HTTPGet, ip, "probe")
		if err != nil {
			return err
		}
		resp, err := probeClient.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 10<<10))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			return fmt.Errorf("HTTP probe failed with statuscode: %d", resp.StatusCode)
		}
		return nil
	case p.TCPSocket != nil:
		port, err := c.resolvePort(p.TCPSocket.Port)
		if err != nil {
			return err
		}
		host := p.TCPSocket.Host
		if host == "" {
			host = ip
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			return err
		}
		conn.Close()
		return nil
	case p.Exec != nil:
		c.w.mu.Lock()
		env, dir, root, wasm := c.env, c.dir, c.root, c.wasm
		c.w.mu.Unlock()
		if wasm {
			return nil // a WebAssembly module has no exec
		}
		var out strings.Builder
		pe := &procEnv{w: c.w, c: c, root: root, ip: ip, cpu: newThrottle(c.spec.Resources)}
		if code := runArgv(ctx, pe, p.Exec.Command, env, dir, strings.NewReader(""), &out, &out); code != 0 {
			return fmt.Errorf("command %q exited with %d: %s", strings.Join(p.Exec.Command, " "), code, strings.TrimSpace(out.String()))
		}
		return nil
	case p.GRPC != nil:
		service := ""
		if p.GRPC.Service != nil {
			service = *p.GRPC.Service
		}
		timeout := time.Second
		if dl, ok := ctx.Deadline(); ok {
			timeout = time.Until(dl)
		}
		return grpcProbe(ip, service, int(p.GRPC.Port), timeout)
	}
	return nil
}

var probeClient = &http.Client{
	Transport:     &http.Transport{DialContext: (&net.Dialer{}).DialContext, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// httpGetRequest is the request of an httpGet probe or hook, like
// NewRequestForHTTPGetAction of the kubelet: the host is the pod IP unless
// set, the port may be a name of the container, and the headers may set
// Host, User-Agent and Accept.
func (c *ctr) httpGetRequest(ctx context.Context, a *v1.HTTPGetAction, ip, purpose string) (*http.Request, error) {
	port, err := c.resolvePort(a.Port)
	if err != nil {
		return nil, err
	}
	host := a.Host
	if host == "" {
		host = ip
	}
	scheme := strings.ToLower(string(a.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	u, err := url.Parse(a.Path)
	if err != nil {
		u = &url.URL{Path: a.Path}
	}
	u.Scheme = scheme
	u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	for _, hh := range a.HTTPHeaders {
		h.Add(hh.Name, hh.Value)
	}
	if _, ok := h["User-Agent"]; !ok {
		h.Set("User-Agent", "kube-"+purpose+"/tabernetes")
	}
	if _, ok := h["Accept"]; !ok {
		h.Set("Accept", "*/*")
	} else if h.Get("Accept") == "" {
		h.Del("Accept")
	}
	req.Header = h
	req.Host = h.Get("Host")
	return req, nil
}

// resolvePort turns a named port into its number, like the kubelet: a name
// that is no port of the container may still be a number.
func (c *ctr) resolvePort(p intstr.IntOrString) (int, error) {
	port := p.IntValue()
	if p.Type == intstr.String {
		port = -1
		for _, cp := range c.spec.Ports {
			if cp.Name == p.StrVal {
				port = int(cp.ContainerPort)
				break
			}
		}
		if port < 0 {
			n, err := strconv.Atoi(p.StrVal)
			if err != nil {
				return 0, fmt.Errorf("port %q not found in container %s", p.StrVal, c.spec.Name)
			}
			port = n
		}
	}
	if port <= 0 || port >= 65536 {
		return 0, fmt.Errorf("invalid port number: %v", port)
	}
	return port, nil
}
