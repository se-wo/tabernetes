package main

// Marks of the boot for docs/boot-time.md. They are off unless the page
// sets globalThis.tabernetesBootMarks (terminal.js does that for
// ?bootmarks in the URL). Then each mark is a performance.mark("go:<name>")
// of the page, with the Go thread's busy time and the GC CPU time so far in
// its detail. Off, a mark costs one load of a bool.

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"syscall/js"
	"time"

	v1 "k8s.io/api/core/v1"
)

var bootMarksOn bool

func initBootMarks() {
	bootMarksOn = js.Global().Get("tabernetesBootMarks").Truthy()
}

func bootMark(name string) {
	if !bootMarksOn {
		return
	}
	detail := map[string]any{
		"gc":         float64(gcCPU()) / float64(time.Millisecond),
		"goroutines": runtime.NumGoroutine(),
	}
	if busy, ok := procBusy(); ok {
		detail["busy"] = float64(busy) / float64(time.Millisecond)
	}
	opts := js.Global().Get("Object").New()
	opts.Set("detail", detail)
	js.Global().Get("performance").Call("mark", "go:"+name, opts)
}

// markedPods are the pods of the tab node that got their marks.
var (
	markedPodsMu sync.Mutex
	markedPods   = map[string]bool{}
)

// bootMarkPod marks the first time the tab node sees a pod and the first
// time it sees it Ready.
func bootMarkPod(pod *v1.Pod) {
	if !bootMarksOn {
		return
	}
	key := pod.Namespace + "/" + pod.Name
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == v1.PodReady && c.Status == v1.ConditionTrue {
			ready = true
		}
	}
	markedPodsMu.Lock()
	seen, wasReady, hadIP := markedPods["seen:"+key], markedPods["ready:"+key], markedPods["ip:"+key]
	markedPods["seen:"+key] = true
	if ready {
		markedPods["ready:"+key] = true
	}
	if pod.Status.PodIP != "" {
		markedPods["ip:"+key] = true
	}
	markedPodsMu.Unlock()
	if !seen {
		bootMark("pod-seen:" + key)
	}
	if pod.Status.PodIP != "" && !hadIP {
		// The IP goes into the name: the measurement script reads it there.
		bootMark("pod-ip:" + key + "=" + pod.Status.PodIP)
	}
	if ready && !wasReady {
		bootMark("pod-ready:" + key)
	}
}

// markReadyz marks the first answer of kube-apiserver to /readyz?verbose
// and each check of it the first time it passes. waitReady polls every
// 100 ms, so a mark is up to that much (plus the request) late.
func markReadyz(answered *bool, passed map[string]bool, body []byte) {
	if !*answered {
		*answered = true
		bootMark("apiserver-serving")
	}
	for _, l := range strings.Split(string(body), "\n") {
		check, ok := strings.CutPrefix(l, "[+]")
		if !ok {
			continue
		}
		check, _, _ = strings.Cut(check, " ")
		if !passed[check] {
			passed[check] = true
			bootMark("readyz:" + check)
		}
	}
}

// waitComponentReady polls the health endpoint of a component every 50 ms
// until it answers 200 and marks "<name>-healthy". Only with marks on: the
// polls are requests of their own.
func waitComponentReady(name, url string) {
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		// The serving certificates of the controller-manager and the
		// scheduler are self-signed (--cert-dir).
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext:     (&net.Dialer{}).DialContext,
	}}
	for deadline := time.Now().Add(bootTimeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		resp, err := hc.Get(url)
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			bootMark(name + "-healthy")
			return
		}
	}
}

var markedOnce sync.Map

// bootMarkOnce marks name the first time only.
func bootMarkOnce(name string) {
	if _, dup := markedOnce.LoadOrStore(name, true); !dup {
		bootMark(name)
	}
}
