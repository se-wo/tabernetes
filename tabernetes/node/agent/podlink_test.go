package main

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParsePodLinkHeader(t *testing.T) {
	src, dst, err := parsePodLinkHeader("TABP/1 tcp 10.244.0.3:0 10.244.1.5:80")
	if err != nil || src.String() != "10.244.0.3:0" || dst.String() != "10.244.1.5:80" {
		t.Errorf("got %v %v %v", src, dst, err)
	}
	for _, bad := range []string{"", "TABP/2 tcp 1.2.3.4:0 1.2.3.5:80", "TABP/1 udp 1.2.3.4:0 1.2.3.5:80", "TABP/1 tcp 1.2.3.4:0"} {
		if _, _, err := parsePodLinkHeader(bad); err == nil {
			t.Errorf("parsePodLinkHeader(%q) did not fail", bad)
		}
	}
}

func TestParseFlannelSubnet(t *testing.T) {
	n, err := parseFlannelSubnet(strings.NewReader("FLANNEL_NETWORK=10.244.0.0/16\nFLANNEL_SUBNET=10.244.3.1/24\nFLANNEL_MTU=1450\n"))
	if err != nil || n.String() != "10.244.3.0/24" {
		t.Errorf("got %v, %v", n, err)
	}
	if _, err := parseFlannelSubnet(strings.NewReader("FLANNEL_MTU=1450\n")); err == nil {
		t.Error("no subnet, no error")
	}
}

// TestPodLinkNetns checks the TPROXY rule and the transparent dial in a
// network namespace like the one of a rootless node. It runs with
// TABERNETES_NETNS_TEST=1 as root of new user, network and mount namespaces
// whose uids and gids 0-65535 map to themselves, like agent/userns.go sets
// them up, for example as root on the host:
//
//	TABERNETES_NETNS_TEST=1 unshare -Unm --map-users=0:0:65536 --map-groups=0:0:65536 \
//	  --setuid 0 --setgid 0 --propagation private go test -run Netns .
//
// It needs ip, nft, python3 and TPROXY in the kernel.
func TestPodLinkNetns(t *testing.T) {
	if os.Getenv("TABERNETES_NETNS_TEST") == "" {
		t.Skip("set TABERNETES_NETNS_TEST=1 and run in unshare, see above")
	}
	sh := func(cmd string) {
		t.Helper()
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", cmd, err, out)
		}
	}
	// The node: cni0 to a pod 10.244.1.5 of this node, a default route
	// elsewhere (eth0). A tab pod is 10.244.0.7.
	sh("mount -t tmpfs none /run && mkdir -p /run/netns && ip link set lo up")
	sh("ip link add cni0 type veth peer name eth0p && ip addr add 10.244.1.1/24 dev cni0 && ip link set cni0 up")
	sh("ip link add eth0 type veth peer name eth0up && ip addr add 192.168.65.3/24 dev eth0 && ip link set eth0 up && ip link set eth0up up && ip route add default via 192.168.65.1")
	sh("ip netns add pod && ip link set eth0p netns pod")
	sh("ip netns exec pod sh -c 'ip link set lo up; ip addr add 10.244.1.5/24 dev eth0p; ip link set eth0p up; ip route add default via 10.244.1.1'")
	if err := setupPodLinkNet(); err != nil {
		t.Fatal(err)
	}
	// Count the packets of uid podLinkUID: what kube-network-policies sees.
	sh("nft add table inet test && nft 'add chain inet test out { type filter hook output priority 0; }' && nft add rule inet test out meta skuid 65534 counter")

	// Pod here -> tab pod: the listener gets the original destination.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan [2]string, 1)
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error { return setTransparent(c) }}
	tl, err := lc.Listen(ctx, "tcp4", "0.0.0.0:15001")
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	go func() {
		if c, err := tl.Accept(); err == nil {
			got <- [2]string{c.RemoteAddr().String(), c.LocalAddr().String()}
			c.Close()
		}
	}()
	out, err := exec.Command("ip", "netns", "exec", "pod", "python3", "-I", "-c", "import socket; socket.create_connection(('10.244.0.7', 80), 2)").CombinedOutput()
	if err != nil {
		t.Fatalf("pod -> tab pod: %v %s", err, out)
	}
	select {
	case a := <-got:
		if !strings.HasPrefix(a[0], "10.244.1.5:") || a[1] != "10.244.0.7:80" {
			t.Errorf("TPROXY: from %s to %s, want from 10.244.1.5 to 10.244.0.7:80", a[0], a[1])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TPROXY listener got nothing")
	}

	// Tab pod -> pod here: the pod sees the tab pod as the source.
	srv := exec.Command("ip", "netns", "exec", "pod", "python3", "-I", "-c",
		"import socket; s=socket.socket(); s.bind(('10.244.1.5', 8080)); s.listen(); c,a=s.accept(); print(a[0], flush=True)")
	srvOut := &strings.Builder{}
	srv.Stdout = srvOut
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	var c net.Conn
	for i := 0; i < 50; i++ {
		dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
		c, err = dialTransparent(dctx, net.ParseIP("10.244.0.7"), &net.TCPAddr{IP: net.ParseIP("10.244.1.5"), Port: 8080})
		dcancel()
		if err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond) // the server starts
	}
	if err != nil {
		t.Fatalf("transparent dial: %v", err)
	}
	c.Close()
	srv.Wait()
	if s := strings.TrimSpace(srvOut.String()); s != "10.244.0.7" {
		t.Errorf("the pod sees %q, want 10.244.0.7", s)
	}
	counter, _ := exec.Command("nft", "list", "chain", "inet", "test", "out").CombinedOutput()
	if strings.Contains(string(counter), "packets 0 ") {
		t.Errorf("no packet of uid %d:\n%s", podLinkUID, counter)
	}

	// A port without listener: refused, not a timeout.
	dctx, dcancel := context.WithTimeout(ctx, 2*time.Second)
	defer dcancel()
	if _, err := dialTransparent(dctx, net.ParseIP("10.244.0.7"), &net.TCPAddr{IP: net.ParseIP("10.244.1.5"), Port: 8081}); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Errorf("dial to a closed port: %v, want refused", err)
	}
}
