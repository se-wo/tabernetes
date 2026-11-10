package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An HTTPS probe accepts the self-signed certificate of the pod, like the
// kubelet's probes do (metrics-server serves one).
func TestHTTPSProbeSelfSigned(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	resp, err := probeClient.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}
