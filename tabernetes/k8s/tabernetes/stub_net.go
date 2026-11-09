package main

// STUB: replaced by tabnode_net.go (net agent). Delete this file then.

import (
	"context"

	"k8s.io/client-go/kubernetes"
)

// serviceProxy is kube-proxy for the tab: it serves cluster IPs and node
// ports in the in-process network and resolves service names.
type serviceProxy struct{}

func newServiceProxy(client kubernetes.Interface) *serviceProxy { return &serviceProxy{} }

// run watches services and endpoint slices until ctx ends.
func (s *serviceProxy) run(ctx context.Context) {}

// serviceEnv is the docker-link style environment of services of a namespace
// (KUBERNETES_SERVICE_HOST etc. always, the others with links).
func (s *serviceProxy) serviceEnv(namespace string, links bool) []string { return nil }

// resolve resolves a DNS name of the cluster seen from namespace: web,
// web.ns, web.ns.svc, web.ns.svc.cluster.local, 10-244-0-5.ns.pod...
func (s *serviceProxy) resolve(host, namespace string) (string, bool) { return "", false }
