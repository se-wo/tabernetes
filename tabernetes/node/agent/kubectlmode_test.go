package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

func TestCheckUse(t *testing.T) {
	for _, c := range []struct {
		use, mode string
		ok        bool
		hint      string
	}{
		{"", useNode, true, ""}, // a token of an older page
		{useNode, useNode, true, ""},
		{useKubectl, useKubectl, true, ""},
		{useKubectl, useNode, false, `"kubectl <token>"`},
		{"", useKubectl, false, `"join --kubectl"`},
		{useNode, useKubectl, false, `"join --kubectl"`},
		{"other", useNode, false, "older than the page"},
	} {
		err := checkUse(offerToken{Use: c.use}, c.mode)
		if (err == nil) != c.ok {
			t.Errorf("checkUse(%q, %q) = %v, want ok=%v", c.use, c.mode, err, c.ok)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), c.hint) {
			t.Errorf("checkUse(%q, %q) = %q, want a hint with %s", c.use, c.mode, err, c.hint)
		}
	}
}

// TestTokenUseRoundTrip checks the field name that web/src/join.js writes.
func TestTokenUseRoundTrip(t *testing.T) {
	s, err := encode(map[string]any{"v": 1, "use": "kubectl", "n": "kubectl-1", "t": "tok", "ca": "ca", "sdp": "sdp", "p": 50001})
	if err != nil {
		t.Fatal(err)
	}
	var tok offerToken
	if err := decode(s, &tok); err != nil {
		t.Fatal(err)
	}
	if tokenUse(tok) != useKubectl || tok.Name != "kubectl-1" || tok.Port != 50001 {
		t.Errorf("got %+v", tok)
	}
}

// kubeconfigFields reads the "key: value" lines of the kubeconfig, with the
// JSON quoted values unquoted. Lists ("- name:") count like keys, keys of
// mappings ("cluster:") are skipped.
func kubeconfigFields(t *testing.T, s string) map[string][]string {
	t.Helper()
	got := map[string][]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("not a key: %q", line)
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue // a mapping follows
		}
		if strings.HasPrefix(v, `"`) {
			if err := json.Unmarshal([]byte(v), &v); err != nil {
				t.Fatalf("%s: %v", line, err)
			}
		}
		got[k] = append(got[k], v)
	}
	return got
}

func TestWriteKubeconfig(t *testing.T) {
	ca := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	tok := offerToken{Use: useKubectl, Name: "kubectl-1", Token: `eyJhbGciOi.x"y.z`, CA: ca}
	var out bytes.Buffer
	if err := writeKubeconfig(&out, "https://127.0.0.1:16443", tok); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	// Only the kubeconfig: no accept line, no escape sequences of the logs.
	if strings.Contains(s, "accept") || strings.Contains(s, "\033") {
		t.Errorf("more than a kubeconfig:\n%s", s)
	}
	f := kubeconfigFields(t, s)
	want := map[string]string{
		"apiVersion": "v1", "kind": "Config", "server": "https://127.0.0.1:16443",
		"token": tok.Token, "current-context": "tabernetes", "cluster": "tabernetes", "user": "kubectl-1",
	}
	for k, v := range want {
		if len(f[k]) != 1 || f[k][0] != v {
			t.Errorf("%s: got %q, want %q", k, f[k], v)
		}
	}
	if got := f["name"]; strings.Join(got, ",") != "tabernetes,kubectl-1,tabernetes" {
		t.Errorf("names: %q", got)
	}
	data := f["certificate-authority-data"]
	if len(data) != 1 {
		t.Fatalf("certificate-authority-data: %q", data)
	}
	if b, err := base64.StdEncoding.DecodeString(data[0]); err != nil || string(b) != ca {
		t.Errorf("CA: %q, %v", b, err)
	}

	for _, bad := range []offerToken{{Name: "kubectl-1", CA: ca}, {Name: "kubectl-1", Token: "x"}, {Token: "x", CA: ca}} {
		if err := writeKubeconfig(io.Discard, defaultServer, bad); err == nil {
			t.Errorf("writeKubeconfig(%+v) did not fail", bad)
		}
	}
}

// pipeChannel is one end of a message channel between two ctlChannels.
type pipeChannel struct {
	in  <-chan string
	out chan<- string
}

func (p pipeChannel) ReadDataChannel(b []byte) (int, bool, error) {
	m, ok := <-p.in
	if !ok {
		return 0, false, io.EOF
	}
	return copy(b, m), true, nil
}

func (p pipeChannel) WriteDataChannel(b []byte, _ bool) (int, error) {
	p.out <- string(b)
	return len(b), nil
}

// tabEnd returns the agent's end of a ctl channel and the tab's end.
func tabEnd() (agent pipeChannel, tabIn <-chan string, tabOut chan<- string) {
	toTab, toAgent := make(chan string, 4), make(chan string, 4)
	return pipeChannel{in: toAgent, out: toTab}, toTab, toAgent
}

func TestCtlBye(t *testing.T) {
	agent, tabIn, tabOut := tabEnd()
	l := newCtlLink(func() { t.Error("close without a close message") })
	l.use(agent)
	go func() {
		if m := <-tabIn; m == ctlBye {
			tabOut <- ctlByeSeen
		}
	}()
	if !l.bye(5 * time.Second) {
		t.Error("the bye was not seen")
	}
}

func TestCtlByeWithoutAnswer(t *testing.T) {
	agent, tabIn, _ := tabEnd()
	l := newCtlLink(func() {})
	l.use(agent)
	start := time.Now()
	if l.bye(100 * time.Millisecond) {
		t.Error("seen without an answer")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waited %v", d)
	}
	if m := <-tabIn; m != ctlBye {
		t.Errorf("sent %q", m)
	}
	// Before the channel is open there is nobody to tell.
	if newCtlLink(func() {}).bye(time.Second) {
		t.Error("bye without a channel")
	}
}

func TestCtlClose(t *testing.T) {
	agent, _, tabOut := tabEnd()
	closed := make(chan struct{})
	l := newCtlLink(func() { close(closed) })
	l.use(agent)
	tabOut <- "something new" // ignored
	tabOut <- ctlClose
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close message not handled")
	}
	if !l.closedByTab() {
		t.Error("closedByTab is false")
	}
}
