package main

// Nodes in Web Workers. "addnode" (web/src/nodes.js) starts the wasm module
// of the page once more in a Web Worker, without the control plane, as a
// node of its own: tab-worker-1, tab-worker-2, ... Each worker is a thread
// with its own memory, so the pods of different nodes run in parallel. Go's
// js/wasm port is single threaded, and real wasm threads need a cross-origin
// isolated page; workers need neither.
//
// A worker node talks to the control plane like a Docker node, through
// channels in place of WebRTC data channels (web/src/channel.js wraps a
// MessagePort): "api" from the worker to the apiserver, "kubelet:10250" from
// the apiserver to the kubelet in the worker. The pods of a worker listen in
// the in-process network of that worker; the service proxies reach them
// through the port-forward of the apiserver.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall/js"
	"time"
)

var workerSeq int // guarded by nodesMu

// prepareWorkerNode names a new worker node, gives it an IP, issues its
// credentials and listens on <ip>:10250 for the apiserver. open(label)
// returns a Promise of a channel into the worker. With gpu, the node offers
// the WebGPU adapter of its worker (addnode --gpu, tabnode_gpu.go).
func (c *cluster) prepareWorkerNode(open js.Value, gpu bool) (map[string]any, error) {
	nodesMu.Lock()
	n := nextNode
	nextNode++
	workerSeq++
	k := workerSeq
	nodesMu.Unlock()
	if n > 250 {
		return nil, errors.New("too many nodes")
	}
	name := fmt.Sprintf("tab-worker-%d", k)
	ip := fmt.Sprintf("10.0.0.%d", n)
	nc, err := c.issueNodeCreds(name, ip)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", ip+":10250")
	if err != nil {
		return nil, err
	}
	jn := &joinedNode{name: name, ip: ip, open: open, ln: ln}
	nodesMu.Lock()
	nodes[name] = jn
	nodesMu.Unlock()
	go jn.serve()
	return map[string]any{
		"name":        name,
		"ip":          ip,
		"ca":          string(nc.ca),
		"cert":        string(nc.cert),
		"key":         string(nc.key),
		"servingCert": string(nc.servingCert),
		"servingKey":  string(nc.servingKey),
		"adminToken":  nc.adminToken,
		"gpu":         gpu,
	}, nil
}

// removeWorkerNode stops forwarding to the kubelet of a worker node that
// is gone.
func removeWorkerNode(name string) {
	nodesMu.Lock()
	jn := nodes[name]
	delete(nodes, name)
	nodesMu.Unlock()
	if jn != nil {
		jn.ln.Close()
	}
}

// runWorkerNode is the life of the instance in a Web Worker: a node, no
// control plane. cfg is what prepareWorkerNode returned.
func runWorkerNode(cfg js.Value) error {
	tabNodeName = cfg.Get("name").String()
	tabNodeIP = cfg.Get("ip").String()
	gpuOffered = cfg.Get("gpu").Truthy()
	nc := nodeCreds{
		ca:          []byte(cfg.Get("ca").String()),
		cert:        []byte(cfg.Get("cert").String()),
		key:         []byte(cfg.Get("key").String()),
		servingCert: []byte(cfg.Get("servingCert").String()),
		servingKey:  []byte(cfg.Get("servingKey").String()),
		adminToken:  cfg.Get("adminToken").String(),
	}
	if err := os.MkdirAll(p("certs"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll("/root", 0o755); err != nil {
		return err
	}
	os.Setenv("HOME", "/root")
	if err := os.WriteFile(p("ca.crt"), nc.ca, 0o644); err != nil {
		return err
	}
	// The service proxy reaches pods of other nodes with this kubeconfig.
	writeKubeconfig("admin", nc.adminToken)

	// The apiserver of this worker is a tunnel to the one of the page: every
	// connection to 127.0.0.1:6443 gets a channel of its own.
	ln, err := net.Listen("tcp", "127.0.0.1:6443")
	if err != nil {
		return err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				dc, err := awaitPromise(ctx, js.Global().Call("tabernetesOpen", "api"))
				if err != nil {
					fmt.Fprintln(os.Stderr, "tabernetes: opening api channel:", err)
					conn.Close()
					return
				}
				splice(newDCConn(dc), conn)
			}()
		}
	}()
	return startTabNode(context.Background(), nc)
}
