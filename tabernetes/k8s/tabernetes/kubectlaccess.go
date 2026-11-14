package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// kubectl access from outside the browser: "join --kubectl" (web/src/join.js)
// and the agent in kubectl mode (node/agent/kubectlmode.go). Each connection
// gets a ServiceAccount of its own, bound to cluster-admin. rmnode, or the
// bye of the agent, deletes both, and that voids the token.

const (
	// kubectlAccessNS holds the ServiceAccounts.
	kubectlAccessNS = "kube-system"
	// kubectlAccessLabel marks the objects of one connection; its value is
	// the name of the connection.
	kubectlAccessLabel = "tabernetes.io/kubectl-access"
	// kubectlTokenLifetime: the token dies with the page anyway (a reload
	// makes a new signing key), and rmnode voids it earlier. A day covers a
	// long session without a token that outlives every sensible use.
	kubectlTokenLifetime = 24 * time.Hour
)

var (
	kubectlMu   sync.Mutex
	nextKubectl = 1
)

// kubectlAccessName is the name of the n-th connection, as the status line
// shows it. Its ServiceAccount and ClusterRoleBinding add a prefix.
func kubectlAccessName(n int) string { return "kubectl-" + strconv.Itoa(n) }

// validKubectlAccessName reports whether name came from kubectlAccessName.
// revokeKubectl deletes nothing else.
func validKubectlAccessName(name string) bool {
	n, ok := strings.CutPrefix(name, "kubectl-")
	if !ok || n == "" || n[0] == '0' {
		return false
	}
	_, err := strconv.ParseUint(n, 10, 16)
	return err == nil
}

// kubectlAccessManifest is the ServiceAccount of a connection and its
// binding to cluster-admin.
func kubectlAccessManifest(name string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: ServiceAccount
metadata:
  name: tabernetes-%[1]s
  namespace: %[2]s
  labels: {%[3]s: %[1]s}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: tabernetes-%[1]s
  labels: {%[3]s: %[1]s}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}
subjects:
- {kind: ServiceAccount, name: tabernetes-%[1]s, namespace: %[2]s}
`, name, kubectlAccessNS, kubectlAccessLabel)
}

// grantKubectl makes the next connection: its ServiceAccount, the binding
// and a token.
func (c *cluster) grantKubectl() (map[string]any, error) {
	kubectlMu.Lock()
	name := kubectlAccessName(nextKubectl)
	nextKubectl++
	kubectlMu.Unlock()

	ctx := context.Background()
	var errs bytes.Buffer
	if code := runKubectl(ctx, []string{"apply", "-f", "-"}, strings.NewReader(kubectlAccessManifest(name)), io.Discard, &errs); code != 0 {
		return nil, fmt.Errorf("creating the ServiceAccount of %s failed: %s", name, strings.TrimSpace(errs.String()))
	}
	var token bytes.Buffer
	if code := runKubectl(ctx, []string{"create", "token", "tabernetes-" + name, "-n", kubectlAccessNS,
		"--duration=" + kubectlTokenLifetime.String()}, nil, &token, &errs); code != 0 {
		_ = revokeKubectl(name)
		return nil, fmt.Errorf("creating the token of %s failed: %s", name, strings.TrimSpace(errs.String()))
	}
	return map[string]any{
		"name":  name,
		"token": strings.TrimSpace(token.String()),
		"ca":    string(c.ca.caPEM),
	}, nil
}

// revokeKubectl deletes the ServiceAccount and the binding of a connection.
// Its token stops working at once.
func revokeKubectl(name string) error {
	if !validKubectlAccessName(name) {
		return fmt.Errorf("%q is not a kubectl connection", name)
	}
	var errs bytes.Buffer
	if code := runKubectl(context.Background(), []string{"delete", "serviceaccount,clusterrolebinding",
		"-n", kubectlAccessNS, "-l", kubectlAccessLabel + "=" + name, "--wait=false"}, nil, io.Discard, &errs); code != 0 {
		fmt.Fprintln(os.Stderr, "tabernetes: revoking", name+":", errs.String())
		return fmt.Errorf("revoking %s failed: %s", name, strings.TrimSpace(errs.String()))
	}
	return nil
}
