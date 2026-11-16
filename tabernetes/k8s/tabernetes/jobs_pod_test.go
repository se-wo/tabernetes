package main

import (
	"context"
	"io"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
)

// A background job of a container ends with the container: its TCP
// connection closes and its UDP port is free again. The terminal does not
// see the job.
func TestPodJobEndsWithContainer(t *testing.T) {
	withTabNode(t, nil)
	server, client := nextLoopbackIP(), nextLoopbackIP()
	ln, err := listenPod(podCtx(t, server), ":7500")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted, closed := make(chan struct{}), make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		close(accepted)
		io.Copy(io.Discard, c) // returns when the client closes
		close(closed)
	}()

	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	w.ip = client
	c := w.ctrs[0]
	c.env, c.dir, c.root = []string{"HOME=/root"}, "/", &rootFS{base: t.TempDir()}
	// sleep keeps stdin of nc open: only the stop closes the connection.
	c.argv = []string{"sh", "-c", "sleep 3600 | nc " + server + " 7500 & nc -u -l -p 7600 & sleep 3600"}
	proc, err := c.startNative(context.Background(), newLogBuf())
	if err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	c.proc = proc
	w.mu.Unlock()

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the job did not connect")
	}
	for i := 0; !portReserved("udp", client, 7600); i++ {
		if i == 500 {
			t.Fatal("the UDP job did not bind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	jobs.mu.Lock()
	n := len(jobs.list)
	jobs.mu.Unlock()
	if n != 0 {
		t.Errorf("the terminal lists %d jobs of the container", n)
	}

	// The pod is deleted: the stop (stopProc) ends with the kill.
	proc.kill("")
	select {
	case <-proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the main process did not end")
	}
	if !proc.jobs.wait(time.After(5 * time.Second)) {
		t.Error("the jobs did not end")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the TCP connection of the job stays open after the stop")
	}
	for i := 0; portReserved("udp", client, 7600); i++ {
		if i == 500 {
			t.Fatal("the UDP socket of the job stays open after the stop")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// When the main process ends, the jobs it left behind end, like the other
// processes of a container whose PID 1 ended.
func TestPodJobEndsWithMainProcess(t *testing.T) {
	withTabNode(t, nil)
	client := nextLoopbackIP()
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	w.ip = client
	c := w.ctrs[0]
	c.env, c.dir, c.root = []string{"HOME=/root"}, "/", &rootFS{base: t.TempDir()}
	c.argv = []string{"sh", "-c", "nc -u -l -p 7610 & sleep 1"}
	proc, err := c.startNative(context.Background(), newLogBuf())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; !portReserved("udp", client, 7610); i++ {
		if i == 500 {
			t.Fatal("the UDP job did not bind")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-proc.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the main process did not end")
	}
	// runOnce does this when the main process ended.
	proc.cancel()
	if !proc.jobs.wait(time.After(5 * time.Second)) {
		t.Fatal("the job did not end")
	}
	if portReserved("udp", client, 7610) {
		t.Error("the UDP socket of the job stays open")
	}
}
