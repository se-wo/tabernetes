package main

// The kubelet API of the tab node on 10.0.0.1:10250: the apiserver comes here
// for kubectl logs, exec, attach and port-forward. The streaming protocols
// (SPDY, and websockets the apiserver may pass on) come from the library the
// container runtimes use (k8s.io/cri-streaming), so the wire format is the
// one of a real kubelet. A container is a shell in a goroutine, so exec runs
// a new shell in the container and attach follows the log of the container.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/cri-streaming/pkg/streaming/portforward"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
	utilexec "k8s.io/utils/exec"
)

const (
	// like the defaults of the CRI streaming server
	streamIdleTimeout     = 4 * time.Hour
	streamCreationTimeout = remotecommand.DefaultStreamCreationTimeout
)

// serve starts the kubelet API on 10.0.0.1:10250 (TLS, see servingTLS).
func (n *tabNode) serve(ctx context.Context) error {
	cfg, err := servingTLS(n.caPEM)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(tabNodeIP, strconv.Itoa(kubeletPort)))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /pods", n.authorized(n.getPods))
	mux.HandleFunc("GET /pods/", n.authorized(n.getPods))
	mux.HandleFunc("GET /containerLogs/{ns}/{pod}/{ctr}", n.authorized(n.getLogs))
	for _, verb := range []string{"exec", "attach"} {
		mux.HandleFunc("/"+verb+"/{ns}/{pod}/{ctr}", n.authorized(n.getStream(verb)))
		mux.HandleFunc("/"+verb+"/{ns}/{pod}/{uid}/{ctr}", n.authorized(n.getStream(verb)))
	}
	mux.HandleFunc("GET /stats/summary", n.statsAuthorized("stats", n.getSummary))
	mux.HandleFunc("GET /stats/summary/", n.statsAuthorized("stats", n.getSummary))
	mux.HandleFunc("GET /metrics/resource", n.statsAuthorized("metrics", n.getResourceMetrics))
	mux.HandleFunc("/portForward/{ns}/{pod}", n.authorized(n.getPortForward))
	mux.HandleFunc("/portForward/{ns}/{pod}/{uid}", n.authorized(n.getPortForward))

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	go func() {
		if err := srv.Serve(tls.NewListener(ln, cfg)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "tab node: kubelet server:", err)
		}
	}()
	return nil
}

// authorized lets only clients with a certificate of the cluster CA in (the
// apiserver has one). Pods can dial 10.0.0.1:10250 as well; without a client
// certificate they get /healthz only, like an anonymous client of a kubelet.
func (n *tabNode) authorized(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (n *tabNode) getPods(w http.ResponseWriter, r *http.Request) {
	list := v1.PodList{TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"}}
	for _, pw := range n.workerList() {
		pod, _ := pw.podInfo()
		list.Items = append(list.Items, *pod)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&list)
}

// lookup finds the pod worker of a request; a uid in the path must match.
func (n *tabNode) lookup(w http.ResponseWriter, r *http.Request) *podWorker {
	pw := n.worker(r.PathValue("ns"), r.PathValue("pod"))
	if pw == nil || (r.PathValue("uid") != "" && types.UID(r.PathValue("uid")) != pw.uid) {
		http.Error(w, fmt.Sprintf("pod %q does not exist", r.PathValue("pod")), http.StatusNotFound)
		return nil
	}
	return pw
}

// getLogs serves kubectl logs with the query parameters of PodLogOptions.
func (n *tabNode) getLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o := logOptions{tail: -1}
	var previous bool
	bad := func() { http.Error(w, `{"message": "Unable to decode query."}`, http.StatusBadRequest) }
	boolParam := func(name string, dst *bool) bool {
		if s := q.Get(name); s != "" {
			b, err := strconv.ParseBool(s)
			if err != nil {
				return false
			}
			*dst = b
		}
		return true
	}
	if !boolParam("follow", &o.follow) || !boolParam("timestamps", &o.timestamps) || !boolParam("previous", &previous) {
		bad()
		return
	}
	tail := q.Get("tailLines")
	if t := q.Get("tail"); t != "" && t != "all" { // the old name of tailLines
		tail = t
	}
	if tail != "" {
		t, err := strconv.Atoi(tail)
		if err != nil || t < 0 {
			bad()
			return
		}
		o.tail = t
	}
	if s := q.Get("sinceSeconds"); s != "" {
		secs, err := strconv.ParseInt(s, 10, 64)
		if err != nil || secs < 1 {
			bad()
			return
		}
		o.since = time.Now().Add(-time.Duration(secs) * time.Second)
	}
	if s := q.Get("sinceTime"); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			bad()
			return
		}
		o.since = t
	}
	if s := q.Get("limitBytes"); s != "" {
		lim, err := strconv.ParseInt(s, 10, 64)
		if err != nil || lim < 1 {
			bad()
			return
		}
		o.limitBytes = lim
	}
	if s := q.Get("stream"); s != "" && s != v1.LogStreamAll {
		http.Error(w, fmt.Sprintf("unable to return the given log stream: %q", s), http.StatusBadRequest)
		return
	}

	pw := n.lookup(w, r)
	if pw == nil {
		return
	}
	name := r.PathValue("ctr")
	c := pw.container(name)
	if c == nil {
		http.Error(w, fmt.Sprintf("container %q not found in pod %q", name, pw.name), http.StatusNotFound)
		return
	}
	logs, err := c.logsFor(previous)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	// The headers go out before the first line, so a follower of a quiet
	// container does not look like a hanging request.
	w.(http.Flusher).Flush()
	logs.copyTo(r.Context(), w, o)
}

// logsFor picks the log of a container the way the kubelet picks the
// container: previous is the run before the current one.
func (c *ctr) logsFor(previous bool) (*logBuf, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	pod := c.w.name
	st := c.state
	// c.last is the last run that ended (it may be the current state).
	ended := c.last.Terminated != nil
	switch {
	case previous:
		switch {
		case st.Running != nil, st.Terminated != nil:
			if c.prev != nil {
				return c.prev, nil
			}
		case st.Waiting != nil && ended:
			// in back-off: the run that just crashed is the previous one
			return c.logs, nil
		}
		return nil, fmt.Errorf("previous terminated container %q in pod %q not found", c.spec.Name, pod)
	case st.Running != nil, st.Terminated != nil:
		return c.logs, nil
	case st.Waiting != nil && ended:
		return c.logs, nil
	case st.Waiting != nil:
		return nil, fmt.Errorf("container %q in pod %q is waiting to start: %s", c.spec.Name, pod, st.Waiting.Reason)
	}
	return nil, fmt.Errorf("container %q in pod %q is waiting to start - no logs yet", c.spec.Name, pod)
}

// getStream serves exec and attach. Errors that come before the upgrade go
// back as plain HTTP errors, kubectl shows them after "unable to upgrade
// connection".
func (n *tabNode) getStream(verb string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts, err := remotecommand.NewOptions(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pw := n.lookup(w, r)
		if pw == nil {
			return
		}
		name := r.PathValue("ctr")
		c := pw.container(name)
		if c == nil {
			http.Error(w, fmt.Sprintf("container %q not found in pod %q", name, pw.name), http.StatusNotFound)
			return
		}
		pw.mu.Lock()
		running, wasm := c.state.Running != nil && c.proc != nil, c.wasm
		pw.mu.Unlock()
		if !running {
			http.Error(w, fmt.Sprintf("container %q in pod %q is not running", name, pw.name), http.StatusBadRequest)
			return
		}
		s := &tabStreamer{c: c}
		if verb == "exec" {
			if wasm {
				http.Error(w, fmt.Sprintf("container %q runs a WebAssembly module in a Web Worker of the browser: there is no shell to exec into", name), http.StatusBadRequest)
				return
			}
			cmd := r.URL.Query()["command"]
			if len(cmd) == 0 {
				http.Error(w, "no command given", http.StatusBadRequest)
				return
			}
			remotecommand.ServeExec(w, r, s, pw.name, string(pw.uid), name, cmd, opts,
				streamIdleTimeout, streamCreationTimeout, remotecommand.SupportedStreamingProtocols)
			return
		}
		remotecommand.ServeAttach(w, r, s, pw.name, string(pw.uid), name, opts,
			streamIdleTimeout, streamCreationTimeout, remotecommand.SupportedStreamingProtocols)
	}
}

func (n *tabNode) getPortForward(w http.ResponseWriter, r *http.Request) {
	opts, err := portforward.NewV4Options(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pw := n.lookup(w, r)
	if pw == nil {
		return
	}
	portforward.ServePortForward(w, r, &tabStreamer{w: pw}, pw.name, string(pw.uid), opts,
		streamIdleTimeout, streamCreationTimeout, portforward.SupportedProtocols)
}

// tabStreamer is the container runtime side of exec, attach (one container)
// and port-forward (a pod).
type tabStreamer struct {
	c *ctr
	w *podWorker
}

// ExecInContainer runs cmd in a new shell of the container, like the main
// process (startNative). A TTY is not emulated: the output is the same.
func (s *tabStreamer) ExecInContainer(ctx context.Context, _, _, _ string, cmd []string, in io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize, _ time.Duration) error {
	go drainResize(resize)
	c := s.c
	c.w.mu.Lock()
	proc, env, dir, root := c.proc, c.env, c.dir, c.root
	c.w.mu.Unlock()
	if proc == nil {
		return fmt.Errorf("container %q is not running", c.spec.Name)
	}
	_, ip := c.w.podInfo()

	// An exec dies with its container.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-proc.done:
			cancel()
		case <-ctx.Done():
		}
	}()

	var stdout, stderr io.Writer = io.Discard, io.Discard
	if out != nil {
		stdout = out
	}
	if errOut != nil {
		stderr = errOut
	} else if tty {
		stderr = stdout // a terminal has one output
	}
	pe := &procEnv{w: c.w, c: c, root: root, ip: ip, cpu: newThrottle(c.spec.Resources)}
	if tty {
		// kubectl exec -t: Ctrl-C ends the command, unless a shell prompt
		// takes it (runShell).
		pe.tty = newPty(ctx, in, stdout, cancel)
		in, stdout, stderr = pe.tty, pe.tty, pe.tty
	} else if in == nil {
		in = strings.NewReader("")
	}
	code := runArgv(ctx, pe, cmd, env, dir, in, stdout, stderr)
	if code != 0 {
		return utilexec.CodeExitError{Err: fmt.Errorf("command terminated with exit code %d", code), Code: code}
	}
	return nil
}

// AttachContainer streams the output of the running container from now on,
// and gives the client's stdin to a container with stdin: true. It returns
// when the run of the container ends.
func (s *tabStreamer) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, errOut io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	go drainResize(resize)
	c := s.c
	c.w.mu.Lock()
	logs, hub, running, ttyOut, proc := c.logs, c.stdin, c.state.Running != nil, c.ttyOut, c.proc
	c.w.mu.Unlock()
	if !running {
		return fmt.Errorf("container %q is not running", c.spec.Name)
	}
	if in != nil && hub != nil {
		go hub.attach(in)
	}
	var stdout, stderr io.Writer
	if out != nil {
		stdout = out
	}
	if errOut != nil {
		stderr = errOut
	} else if tty {
		stderr = stdout
	}
	if ttyOut != nil && stdout != nil && proc != nil {
		// A terminal shows its output as it comes, prompts included.
		ttyOut.follow(ctx, stdout, proc.done)
		return nil
	}
	err := logs.follow(ctx, stdout, stderr)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func drainResize(ch <-chan remotecommand.TerminalSize) {
	for range ch {
	}
}

// follow writes the lines that come into the log from now on to stdout and
// stderr by their stream, until the log closes (the run ended).
func (l *logBuf) follow(ctx context.Context, stdout, stderr io.Writer) error {
	l.mu.Lock()
	pos := l.dropped + len(l.lines) // absolute index of the next line
	l.mu.Unlock()
	for {
		l.mu.Lock()
		idx := max(pos-l.dropped, 0)
		lines := append([]logLine(nil), l.lines[idx:]...)
		pos = l.dropped + len(l.lines)
		changed, closed := l.changed, l.closed
		l.mu.Unlock()
		for _, ln := range lines {
			w := stdout
			if ln.stream == "stderr" {
				w = stderr
			}
			if w == nil {
				continue
			}
			if _, err := w.Write(ln.b); err != nil {
				return err
			}
		}
		if closed {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// PortForward connects one forwarded connection to port of the pod in the
// in-process network.
func (s *tabStreamer) PortForward(ctx context.Context, _, _ string, port int32, stream io.ReadWriteCloser) error {
	_, ip := s.w.podInfo()
	if ip == "" {
		return fmt.Errorf("pod %s has no IP address yet", s.w.key())
	}
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(int(port))))
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		// The client half-closes when its local connection is done; the
		// pod gets an EOF and can still answer.
		_, err := io.Copy(conn, stream)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok && err == nil {
			cw.CloseWrite()
		} else {
			conn.Close()
		}
	}()
	_, err = io.Copy(stream, conn)
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}
