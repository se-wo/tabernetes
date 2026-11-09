package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
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
					msg := fmt.Sprintf("Container %s failed %s probe, will be restarted", c.spec.Name, strings.ToLower(kind))
					c.w.ctrEvent(c, v1.EventTypeNormal, "Killing", "%s", msg)
					proc.kill(msg)
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
		port, err := c.resolvePort(p.HTTPGet.Port)
		if err != nil {
			return err
		}
		host := p.HTTPGet.Host
		if host == "" {
			host = ip
		}
		scheme := strings.ToLower(string(p.HTTPGet.Scheme))
		if scheme == "" {
			scheme = "http"
		}
		path := p.HTTPGet.Path
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s://%s%s", scheme, net.JoinHostPort(host, strconv.Itoa(port)), path), nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "kube-probe/tabernetes")
		for _, h := range p.HTTPGet.HTTPHeaders {
			req.Header.Set(h.Name, h.Value)
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
		return nil // no gRPC health client in the tab; counts as healthy
	}
	return nil
}

var probeClient = &http.Client{
	Transport:     &http.Transport{DialContext: (&net.Dialer{}).DialContext, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// resolvePort turns a named port into its number.
func (c *ctr) resolvePort(p intstr.IntOrString) (int, error) {
	if p.Type == intstr.Int {
		return p.IntValue(), nil
	}
	for _, cp := range c.spec.Ports {
		if cp.Name == p.StrVal {
			return int(cp.ContainerPort), nil
		}
	}
	return 0, fmt.Errorf("port %q not found in container %s", p.StrVal, c.spec.Name)
}
