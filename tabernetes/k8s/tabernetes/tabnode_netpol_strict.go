package main

// Strict mode, like --strict-mode of kube-network-policies (its default): a
// change of policy also applies to connections that are open already. On a
// real node the dataplane flushes their conntrack entries and the next packet
// is decided again. Here every connection that the policy allowed is tracked,
// and each change of policies, pods or namespaces decides them again; a
// connection that is denied now is closed.

import (
	"context"
	"net"
	"time"

	"k8s.io/client-go/tools/cache"
)

// liveConn is an open connection that the policy allowed.
type liveConn struct {
	src, dst *net.TCPAddr
	c        net.Conn
}

// recheckDelay gathers the changes of a burst (an apply of several
// policies, the status updates of a starting pod) into one recheck.
const recheckDelay = 200 * time.Millisecond

// watchChanges makes every change of the informers of the engine decide the
// open connections again.
func (p *netPolicy) watchChanges(ctx context.Context, informers ...cache.SharedIndexInformer) {
	kick := func() {
		select {
		case p.kick <- struct{}{}:
		default:
		}
	}
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { kick() },
		UpdateFunc: func(any, any) { kick() },
		DeleteFunc: func(any) { kick() },
	}
	for _, inf := range informers {
		inf.AddEventHandler(h)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.kick:
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(recheckDelay):
			}
			p.recheck(ctx)
		}
	}()
}

// recheck closes the open connections that the policy denies now.
func (p *netPolicy) recheck(ctx context.Context) {
	p.liveMu.Lock()
	conns := make([]*liveConn, 0, len(p.live))
	for lc := range p.live {
		conns = append(conns, lc)
	}
	p.liveMu.Unlock()
	for _, lc := range conns {
		if !p.allowed(ctx, lc.src, lc.dst) {
			p.liveMu.Lock()
			delete(p.live, lc)
			p.liveMu.Unlock()
			lc.c.Close()
		}
	}
}

// track keeps c, a connection from src to dst that the policy allowed, until
// it is closed.
func (p *netPolicy) track(c net.Conn, src, dst *net.TCPAddr) net.Conn {
	if p == nil {
		return c
	}
	lc := &liveConn{src: src, dst: dst, c: c}
	p.liveMu.Lock()
	p.live[lc] = struct{}{}
	p.liveMu.Unlock()
	return &trackedConn{Conn: c, p: p, lc: lc}
}

// trackedConn leaves the open connections when it is closed.
type trackedConn struct {
	net.Conn
	p  *netPolicy
	lc *liveConn
}

func (c *trackedConn) Close() error {
	c.p.liveMu.Lock()
	delete(c.p.live, c.lc)
	c.p.liveMu.Unlock()
	return c.Conn.Close()
}

func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}
