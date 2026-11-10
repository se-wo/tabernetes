package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"k8s.io/kubernetes/pkg/probe"
	grpcprobe "k8s.io/kubernetes/pkg/probe/grpc"
	"mvdan.cc/sh/v3/interp"
)

// grpcProbe is the gRPC probe of the kubelet (pkg/probe/grpc): it calls
// grpc.health.v1.Health/Check on the pod IP over the in-process network. The
// error text is the one of the kubelet event, for example
// `service unhealthy (responded with "NOT_SERVING")`.
func grpcProbe(host, service string, port int, timeout time.Duration) error {
	res, out, _ := grpcprobe.New().Probe(host, service, port, timeout, grpcprobe.ProbeOptions{})
	if res != probe.Success {
		return errors.New(out)
	}
	return nil
}

// serveGRPCHealth serves grpc.health.v1.Health on addr until ctx ends, as the
// images with a gRPC health endpoint do. The server starts with the given
// services SERVING (the empty name is the server as a whole).
func serveGRPCHealth(ctx context.Context, addr string, services ...string) (*health.Server, error) {
	ln, err := listenPod(ctx, addr)
	if err != nil {
		return nil, err
	}
	hs := health.NewServer()
	for _, s := range services {
		hs.SetServingStatus(s, healthpb.HealthCheckResponse_SERVING)
	}
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	go srv.Serve(ln)
	context.AfterFunc(ctx, srv.Stop)
	return hs, nil
}

// agnhostGRPCHealth is "agnhost grpc-health-checking": a gRPC health server
// whose status the HTTP port toggles (/make-serving, /make-not-serving), and
// that turns NOT_SERVING by itself after --delay-unhealthy-sec.
func agnhostGRPCHealth(ctx context.Context, hc interp.HandlerContext, fs *pflag.FlagSet, args []string, logger *log.Logger) error {
	port := fs.Int("port", 5000, "Port number for the gRPC health server.")
	httpPort := fs.Int("http-port", 8080, "Port number for the HTTP server that changes the status.")
	service := fs.String("service", "", "Name of the service to report; empty is the server as a whole.")
	delay := fs.Int("delay-unhealthy-sec", -1, "Seconds until the server reports NOT_SERVING; negative: never.")
	fs.Bool("tls", false, "Serve TLS.")
	if err := fs.Parse(args); err != nil {
		return interp.ExitStatus(2)
	}
	hs, err := serveGRPCHealth(ctx, fmt.Sprintf(":%d", *port), *service)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "agnhost: listen tcp :%d: %v\n", *port, unwrap(err))
		return interp.ExitStatus(1)
	}
	set := func(s healthpb.HealthCheckResponse_ServingStatus) {
		hs.SetServingStatus(*service, s)
		logger.Printf("Setting %q to %s", *service, s)
	}
	logger.Printf("Serving gRPC health on port %d, HTTP on port %d", *port, *httpPort)
	if *delay >= 0 {
		t := time.AfterFunc(time.Duration(*delay)*time.Second, func() { set(healthpb.HealthCheckResponse_NOT_SERVING) })
		defer t.Stop()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/make-serving", func(w http.ResponseWriter, r *http.Request) {
		set(healthpb.HealthCheckResponse_SERVING)
		fmt.Fprintln(w, "OK")
	})
	mux.HandleFunc("/make-not-serving", func(w http.ResponseWriter, r *http.Request) {
		set(healthpb.HealthCheckResponse_NOT_SERVING)
		fmt.Fprintln(w, "OK")
	})
	return serveHTTP(ctx, hc, "agnhost", mux, fmt.Sprintf(":%d", *httpPort))
}

