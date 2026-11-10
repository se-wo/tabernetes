package main

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/health/grpc_health_v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestGRPCProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hs, err := serveGRPCHealth(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), "", "web")
	if err != nil {
		t.Fatal(err)
	}
	w := testWorker(t, v1.PodSpec{Containers: []v1.Container{{Name: "app"}}})
	c := w.ctrs[0]
	probe := func(port int, service *string) error {
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return c.probeOnce(pctx, &v1.Probe{ProbeHandler: v1.ProbeHandler{GRPC: &v1.GRPCAction{Port: int32(port), Service: service}}})
	}

	if err := probe(port, nil); err != nil {
		t.Errorf("serving: %v", err)
	}
	if err := probe(port, ptr.To("web")); err != nil {
		t.Errorf("serving service: %v", err)
	}
	hs.SetServingStatus("", grpc_health_v1.HealthCheckResponse_NOT_SERVING)
	if err := probe(port, nil); err == nil || err.Error() != `service unhealthy (responded with "NOT_SERVING")` {
		t.Errorf("not serving: %v", err)
	}
	if err := probe(port, ptr.To("other")); err == nil || !strings.Contains(err.Error(), "NotFound") {
		t.Errorf("unknown service: %v", err)
	}
	if err := probe(port+1, nil); err == nil || !strings.HasPrefix(err.Error(), "timeout: failed to connect service") && !strings.HasPrefix(err.Error(), "error: failed to connect service") {
		t.Errorf("closed port: %v", err)
	}
}
