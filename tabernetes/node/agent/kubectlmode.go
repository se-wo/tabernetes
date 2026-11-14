package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// What a token from the page is for: the field "use" of offerToken. Each mode
// of the agent takes only its own kind, so a token that gives cluster-admin to
// kubectl never joins a node, and a bootstrap token never ends up in a
// kubeconfig.
const (
	useNode    = "node"    // "join": a kubelet joins the cluster
	useKubectl = "kubectl" // "join --kubectl": kubectl on this machine reaches the apiserver
)

// tokenUse is the kind of tok. Pages before the field made only node tokens.
func tokenUse(tok offerToken) string {
	if tok.Use == "" {
		return useNode
	}
	return tok.Use
}

// checkUse returns an error with the right command when tok is not for mode.
func checkUse(tok offerToken, mode string) error {
	got := tokenUse(tok)
	if got == mode {
		return nil
	}
	switch got {
	case useKubectl:
		return errors.New(`this token is from "join --kubectl" and gives kubectl access, it cannot join a node. ` +
			`Run the image with "kubectl <token>", as the browser printed it`)
	case useNode:
		return errors.New(`this token is from "join" and joins a node, it gives no kubectl access. ` +
			`Run the image with "join <token>", or type "join --kubectl" in the browser for a kubectl token`)
	}
	return fmt.Errorf("this token is for %q, which this agent does not know: is the image older than the page?", got)
}

// kubectlPort is the port in the container where kubectl connects. The
// page prints -p 127.0.0.1:6443:6443 for it.
const kubectlPort = "6443"

// defaultServer is the apiserver address in the kubeconfig. The serving
// certificate of the apiserver in the tab is valid for 127.0.0.1 and
// localhost, on any port.
const defaultServer = "https://127.0.0.1:" + kubectlPort

// writeKubeconfig writes a kubeconfig for kubectl outside the browser: the
// apiserver at server, trusted with the CA of the tab, and the token of the
// ServiceAccount that the page made for this connection.
func writeKubeconfig(w io.Writer, server string, tok offerToken) error {
	if tok.Token == "" || tok.CA == "" || tok.Name == "" {
		return errors.New("the token has no credentials for kubectl")
	}
	// JSON strings are valid YAML scalars, with every character escaped
	// that YAML would read in another way.
	q := func(s string) string {
		b, _ := json.Marshal(s)
		return string(b)
	}
	ca := base64.StdEncoding.EncodeToString([]byte(tok.CA))
	var b strings.Builder
	b.WriteString("# kubectl for the cluster in the browser tab. It works while the agent\n")
	b.WriteString("# runs and ends with the page: a reload starts a new cluster.\n")
	b.WriteString("apiVersion: v1\nkind: Config\n")
	b.WriteString("clusters:\n- name: tabernetes\n  cluster:\n")
	fmt.Fprintf(&b, "    server: %s\n    certificate-authority-data: %s\n", q(server), q(ca))
	fmt.Fprintf(&b, "users:\n- name: %s\n  user:\n    token: %s\n", q(tok.Name), q(tok.Token))
	fmt.Fprintf(&b, "contexts:\n- name: tabernetes\n  context:\n    cluster: tabernetes\n    user: %s\n", q(tok.Name))
	b.WriteString("current-context: tabernetes\n")
	_, err := io.WriteString(w, b.String())
	return err
}
