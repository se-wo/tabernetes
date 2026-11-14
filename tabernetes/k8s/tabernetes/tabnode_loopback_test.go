package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	npafake "sigs.k8s.io/network-policy-api/pkg/client/clientset/versioned/fake"
)

// loopbackIPs gives each test pod IPs of its own on the in-process network.
var loopbackIPs struct {
	sync.Mutex
	n int
}

func nextLoopbackIP() string {
	loopbackIPs.Lock()
	defer loopbackIPs.Unlock()
	loopbackIPs.n++
	return "10.244.88." + strconv.Itoa(loopbackIPs.n)
}

// podCtx is the context of a container of the pod with IP ip. It ends with
// the test.
func podCtx(t *testing.T, ip string) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(withProc(context.Background(), &procEnv{ip: ip}))
	t.Cleanup(cancel)
	return ctx
}

// withTabNode sets theTabNode for one test.
func withTabNode(t *testing.T, n *tabNode) {
	saved := theTabNode
	theTabNode = n
	t.Cleanup(func() { theTabNode = saved })
}

// serveEcho accepts connections on ln and writes the remote address of each
// one, then closes it.
func serveEcho(ln net.Listener) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, c.RemoteAddr().String()+"\n")
			c.Close()
		}
	}()
}

// firstLine reads the first line of c, then closes c.
func firstLine(t *testing.T, c net.Conn) string {
	t.Helper()
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return strings.TrimSpace(line)
}

func TestLoopbackTable(t *testing.T) {
	ip := nextLoopbackIP()
	dst := tcpAddr(ip + ":7000")
	if loopbackRefused("tcp", nil, dst) {
		t.Fatal("refused without a row")
	}
	release := addLoopback("tcp", ip, 7000)
	for _, c := range []struct {
		proto string
		src   net.IP
		dst   *net.TCPAddr
		want  bool
	}{
		{"tcp", nil, dst, true},
		{"tcp", loopbackIP, dst, true},
		{"tcp", net.ParseIP("10.244.88.250"), dst, true},
		{"tcp", net.ParseIP(ip), dst, false},
		{"tcp", net.ParseIP("::ffff:" + ip), dst, false},
		{"tcp", nil, tcpAddr(ip + ":7001"), false},
		{"udp", nil, dst, false}, // W4.1
	} {
		if got := loopbackRefused(c.proto, c.src, c.dst); got != c.want {
			t.Errorf("%s %v -> %v: %v, want %v", c.proto, c.src, c.dst, got, c.want)
		}
	}
	release()
	if loopbackRefused("tcp", nil, dst) {
		t.Error("refused after release")
	}
	// A second release does nothing, also not to a new row of the key.
	again := addLoopback("tcp", ip, 7000)
	release()
	if !loopbackRefused("tcp", nil, dst) {
		t.Error("an old release removed a new row")
	}
	again()
	again()
	if loopbackRefused("tcp", nil, dst) {
		t.Error("refused after the second release")
	}
}

// TC9 and rule 3: the table of the ports in use.
func TestReservePort(t *testing.T) {
	ip := nextLoopbackIP()
	got := map[int]func(){}
	for range 3 {
		port, release, err := reservePort("tcp", ip, 0)
		if err != nil {
			t.Fatal(err)
		}
		if port < 32768 || port > 60999 || got[port] != nil {
			t.Fatalf("port 0 gave %d", port)
		}
		if !portReserved("tcp", ip, port) {
			t.Errorf("%d is not reserved", port)
		}
		got[port] = release
	}
	for port, release := range got {
		release()
		release() // a second call does nothing
		if portReserved("tcp", ip, port) {
			t.Errorf("%d is reserved after release", port)
		}
	}
	// Two reservations without a listener do not conflict (SO_REUSEADDR).
	_, r1, err1 := reservePort("tcp", ip, 7100)
	_, r2, err2 := reservePort("tcp", ip, 7100)
	if err1 != nil || err2 != nil {
		t.Fatalf("second reservation: %v %v", err1, err2)
	}
	r1()
	if !portReserved("tcp", ip, 7100) {
		t.Error("one release freed two reservations")
	}
	r2()
	if portReserved("tcp", ip, 7100) || portReserved("udp", ip, 7100) {
		t.Error("reserved after the two releases")
	}
	// A listener has the port: a reservation gets errPortInUse.
	ln, err := listenPod(podCtx(t, ip), ip+":7101")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reservePort("tcp", ip, 7101); !errors.Is(err, errPortInUse) {
		t.Errorf("reservation of a listener port: %v", err)
	}
	if _, _, err := reservePort("tcp", "10.244.88.251", 7101); err != nil {
		t.Errorf("the same port of another IP: %v", err)
	}
	if !portReserved("tcp", ip, 7101) {
		t.Error("the listener is not in the table")
	}
	ln.Close()
	_, r, err := reservePort("tcp", ip, 7101)
	if err != nil {
		t.Errorf("reservation after the listener closed: %v", err)
	} else {
		r()
	}
	// Port 0 does not give a port in use.
	ln, err = listenPod(podCtx(t, ip), ip+":32768")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if port, r, err := reservePort("tcp", ip, 0); err != nil || port == 32768 {
		t.Errorf("port 0 with 32768 in use: %d %v", port, err)
	} else {
		r()
	}
}

// TC2, PX5: a loopback host listens on the pod IP with a row; Close and the
// end of the context remove the row and free the port.
func TestListenPodLoopback(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "localhost", "127.0.0.2", "::1"} {
		ip := nextLoopbackIP()
		ctx, cancel := context.WithCancel(podCtx(t, ip))
		ln, err := listenPod(ctx, net.JoinHostPort(host, "7200"))
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if ln.Addr().String() != ip+":7200" {
			t.Errorf("%s: listens on %s", host, ln.Addr())
		}
		dst := tcpAddr(ip + ":7200")
		if !loopbackRefused("tcp", nil, dst) {
			t.Errorf("%s: no row", host)
		}
		_, err = listenPod(ctx, ip+":7200")
		if !errors.Is(err, errPortInUse) || !strings.Contains(err.Error(), "in use") {
			t.Errorf("%s: second listener: %v", host, err)
		}
		if host == "127.0.0.1" {
			ln.Close()
		} else {
			cancel() // the container stops
		}
		for i := 0; loopbackRefused("tcp", nil, dst) || portReserved("tcp", ip, 7200); i++ {
			if i == 100 {
				t.Fatalf("%s: the row stays after the end of the listener", host)
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}
	// Other hosts get no row.
	ip := nextLoopbackIP()
	ctx := podCtx(t, ip)
	for i, host := range []string{"0.0.0.0", "", "::", ip} {
		port := strconv.Itoa(7300 + i)
		ln, err := listenPod(ctx, net.JoinHostPort(host, port))
		if err != nil {
			t.Fatalf("%q: %v", host, err)
		}
		defer ln.Close()
		if ln.Addr().String() != ip+":"+port || loopbackRefused("tcp", nil, tcpAddr(ip+":"+port)) {
			t.Errorf("%q: %s with a row", host, ln.Addr())
		}
	}
	// A hostNetwork pod has the node IP: no row.
	ln, err := listenPod(podCtx(t, tabNodeIP), "127.0.0.1:7310")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if loopbackRefused("tcp", nil, tcpAddr(tabNodeIP+":7310")) {
		t.Error("a hostNetwork pod has a row")
	}
}

// TC9: a dial with a local port. The listener sees the port. Port 0 gives an
// ephemeral port, which Close frees.
func TestDialClusterFrom(t *testing.T) {
	for _, node := range []*tabNode{nil, {}} { // the host path and dialFromPod
		withTabNode(t, node)
		server, client := nextLoopbackIP(), nextLoopbackIP()
		ln, err := listenPod(podCtx(t, server), ":7400")
		if err != nil {
			t.Fatal(err)
		}
		serveEcho(ln)
		ctx := podCtx(t, client)
		c, err := dialClusterFrom(ctx, server+":7400", 40001)
		if err != nil {
			t.Fatal(err)
		}
		if got := firstLine(t, c); got != client+":40001" {
			t.Errorf("node %v: the listener sees %s", node, got)
		}
		c, err = dialClusterFrom(ctx, server+":7400", 0)
		if err != nil {
			t.Fatal(err)
		}
		local := c.LocalAddr().(*net.TCPAddr)
		if local.Port < 32768 || local.Port > 60999 || !portReserved("tcp", client, local.Port) {
			t.Errorf("node %v: ephemeral port %d", node, local.Port)
		}
		if got := firstLine(t, c); got != local.String() {
			t.Errorf("node %v: the listener sees %s, want %s", node, got, local)
		}
		if portReserved("tcp", client, local.Port) {
			t.Errorf("node %v: port %d stays reserved after Close", node, local.Port)
		}
		if _, ok := c.(interface{ CloseWrite() error }); !ok {
			t.Error("no CloseWrite")
		}
		// A failed dial frees the port.
		if _, err := dialClusterFrom(ctx, server+":7401", 0); !isRefused(err) {
			t.Errorf("node %v: dial to a closed port: %v", node, err)
		}
		if portReserved("tcp", client, proxyEphemeralFirst) {
			t.Errorf("node %v: a failed dial keeps its port", node)
		}
		ln.Close()
	}
}

// loopbackPolicyNode is a tab node with the policy of fetchPolicyNode: web
// (10.244.0.2) accepts only role=client on port 80. other (10.244.0.4) has
// no policy.
func loopbackPolicyNode(t *testing.T) *tabNode {
	t.Helper()
	saved := theTabNode
	t.Cleanup(func() { theTabNode = saved })
	fetchPolicyNode(t)
	return theTabNode
}

// TC3, TC4, TC10: the checks of dialFromPod.
func TestLoopbackDialFromPod(t *testing.T) {
	n := loopbackPolicyNode(t)
	setDurations(t, 200*time.Millisecond, 10*time.Second)
	// other listens on 127.0.0.1: no policy selects it.
	ln, err := listenPod(podCtx(t, "10.244.0.4"), "127.0.0.1:7500")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serveEcho(ln)
	// TC3: the pod itself connects.
	c, err := dialCluster(podCtx(t, "10.244.0.4"), "tcp", "localhost:7500")
	if err != nil {
		t.Fatalf("same pod: %v", err)
	}
	firstLine(t, c)
	c, err = dialClusterFrom(podCtx(t, "10.244.0.4"), "10.244.0.4:7500", 0)
	if err != nil {
		t.Fatalf("same pod through the pod IP: %v", err)
	}
	firstLine(t, c)
	// TC4: web is allowed, then refused at once.
	start := time.Now()
	_, err = dialCluster(podCtx(t, "10.244.0.2"), "tcp", "10.244.0.4:7500")
	if !isRefused(err) || time.Since(start) > 150*time.Millisecond {
		t.Errorf("another pod: %v after %v, want connection refused at once", err, time.Since(start))
	}
	if n.policy.consume(net.ParseIP("10.244.0.2"), tcpAddr("10.244.0.4:7500")) {
		t.Error("the refused dial left an admission")
	}
	// TC10: web listens on 127.0.0.1, other is denied: it waits for the
	// timeout, not connection-refused.
	ln2, err := listenPod(podCtx(t, "10.244.0.2"), "127.0.0.1:80")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	start = time.Now()
	_, err = dialCluster(podCtx(t, "10.244.0.4"), "tcp", "10.244.0.2:80")
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() || time.Since(start) < 200*time.Millisecond {
		t.Errorf("denied pod: %v after %v, want a timeout after synTimeout", err, time.Since(start))
	}
}

// TC15: the terminal (dialCluster without a pod) is refused.
func TestLoopbackTerminal(t *testing.T) {
	withTabNode(t, nil)
	ip := nextLoopbackIP()
	ln, err := listenPod(podCtx(t, ip), "127.0.0.1:7600")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, err := dialCluster(context.Background(), "tcp", ip+":7600"); !isRefused(err) {
		t.Errorf("terminal: %v", err)
	}
	// A hostNetwork pod is the host of the node too.
	if _, err := dialCluster(podCtx(t, tabNodeIP), "tcp", ip+":7600"); !isRefused(err) {
		t.Errorf("hostNetwork pod: %v", err)
	}
	// Without a row, the terminal connects.
	ln2, err := listenPod(podCtx(t, ip), "0.0.0.0:7601")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	serveEcho(ln2)
	c, err := dialCluster(context.Background(), "tcp", ip+":7601")
	if err != nil {
		t.Fatalf("terminal without a row: %v", err)
	}
	firstLine(t, c)
}

// TC15: the Service proxy refuses an endpoint with a row for the node IP and
// for another pod. The pod itself connects through its Service (section 11).
func TestLoopbackServiceEndpoint(t *testing.T) {
	ip := nextLoopbackIP()
	ln, err := listenPod(podCtx(t, ip), "127.0.0.1:7700")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serveEcho(ln)
	s := testProxy()
	e := endpoint{ip: ip, port: 7700}
	ctx := context.Background()
	if _, err := s.dialEndpoint(ctx, e, nil); !isRefused(err) {
		t.Errorf("node IP: %v", err)
	}
	if _, err := s.dialEndpoint(ctx, e, &net.TCPAddr{IP: net.ParseIP(nextLoopbackIP())}); !isRefused(err) {
		t.Errorf("another pod: %v", err)
	}
	c, err := s.dialEndpoint(ctx, e, &net.TCPAddr{IP: net.ParseIP(ip), Port: 40100})
	if err != nil {
		t.Fatalf("the pod itself: %v", err)
	}
	if got := firstLine(t, c.(net.Conn)); got != ip+":40100" {
		t.Errorf("the listener sees %s", got)
	}
}

// TC15: the destination side of the pod channel answers refused.
func TestLoopbackPodLink(t *testing.T) {
	client := fake.NewClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", Labels: map[string]string{"kubernetes.io/metadata.name": "default"}}},
		&v1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "tab-worker-1"},
			Spec:       v1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24"}},
		},
		testPod("web", "10.244.0.2", "tab", map[string]string{"app": "web"}),
		testPod("remote", "10.244.1.3", "tab-worker-1", map[string]string{"role": "remote"}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pol := readyNetPolicy(ctx, t, client, npafake.NewSimpleClientset())
	for deadline := time.Now().Add(5 * time.Second); !pol.nodeOwns("tab-worker-1", net.ParseIP("10.244.1.3")); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("node not in the informer")
		}
	}
	_, cidr, _ := net.ParseCIDR("10.244.0.0/24")
	n := &tabNode{policy: pol, podCIDR: cidr}
	withTabNode(t, n)
	serve := func(port int) string {
		a, b := net.Pipe()
		defer b.Close()
		go n.servePodLinkHeader(a, "tab-worker-1", "TABP/1 tcp 10.244.1.3:40000 10.244.0.2:"+strconv.Itoa(port))
		b.SetReadDeadline(time.Now().Add(5 * time.Second))
		line, _ := readPodLinkLine(b, 64)
		return line
	}
	ln, err := listenPod(podCtx(t, "10.244.0.2"), "127.0.0.1:7800")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serveEcho(ln)
	if got := serve(7800); got != "refused" {
		t.Errorf("a row: %q, want refused", got)
	}
	ln2, err := listenPod(podCtx(t, "10.244.0.2"), "0.0.0.0:7801")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	serveEcho(ln2)
	if got := serve(7801); got != "ok" {
		t.Errorf("no row: %q, want ok", got)
	}
}

// TC15, PX5: tcpSocket, httpGet and gRPC probes and an httpGet hook fail
// with connection refused. TC16: port-forward connects.
func TestLoopbackProbesHooksPortForward(t *testing.T) {
	ip := nextLoopbackIP()
	ctx := podCtx(t, ip)
	ln, err := listenPod(ctx, "127.0.0.1:7900")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if _, err := serveGRPCHealth(ctx, "127.0.0.1:7901", ""); err != nil {
		t.Fatal(err)
	}
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	w.mu.Lock()
	w.ip = ip
	w.mu.Unlock()
	c := w.ctrs[0]
	probe := func(h v1.ProbeHandler) error {
		pctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		return c.probeOnce(pctx, &v1.Probe{ProbeHandler: h})
	}
	port := intstr.FromInt32(7900)
	if err := probe(v1.ProbeHandler{TCPSocket: &v1.TCPSocketAction{Port: port}}); !isRefused(err) {
		t.Errorf("tcpSocket probe: %v", err)
	}
	if err := probe(v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Port: port}}); !isRefused(err) {
		t.Errorf("httpGet probe: %v", err)
	}
	if err := probe(v1.ProbeHandler{GRPC: &v1.GRPCAction{Port: 7901}}); err == nil || !strings.HasPrefix(err.Error(), "timeout: failed to connect service") {
		t.Errorf("gRPC probe: %v", err)
	}
	msg, err := c.runHook(context.Background(), nil, &v1.LifecycleHandler{HTTPGet: &v1.HTTPGetAction{Port: port}})
	if !isRefused(err) || !strings.Contains(strings.ToLower(msg), "connection refused") {
		t.Errorf("httpGet hook: %v %s", err, msg)
	}

	// TC16: port-forward connects.
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- (&tabStreamer{w: w}).PortForward(context.Background(), "", "", 7900, a) }()
	io.WriteString(b, "GET / HTTP/1.0\r\n\r\n")
	b.SetReadDeadline(time.Now().Add(5 * time.Second))
	status, err := bufio.NewReader(b).ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.0 200") {
		t.Errorf("port-forward: %q %v", status, err)
	}
	b.Close()
	<-done

	// Without the row, the probes pass again.
	ln.Close()
	ln, err = listenPod(ctx, "0.0.0.0:7900")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if err := probe(v1.ProbeHandler{TCPSocket: &v1.TCPSocketAction{Port: port}}); err != nil {
		t.Errorf("tcpSocket probe without a row: %v", err)
	}
	if err := probe(v1.ProbeHandler{HTTPGet: &v1.HTTPGetAction{Port: port}}); err != nil {
		t.Errorf("httpGet probe without a row: %v", err)
	}
}

// isRefused is refused, also for no error.
func isRefused(err error) bool { return err != nil && refused(err) }
