package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	prostometrics "github.com/prostoteam/prostometrics-go"
)

func TestDiagnosticsCapabilityIsAlwaysAdvertised(t *testing.T) {
	// Old service env files may still contain the retired opt-out switch.
	// Only the server decides whether to issue diagnostic tasks now.
	t.Setenv("PROSTOMETRICS_DIAGNOSTICS", "0")
	t.Setenv("PROSTOMETRICS_DIAGNOSTICS_DIR", t.TempDir())
	capability := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capability <- r.Header.Get("X-PM-Diagnostics")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	if err := initClient("test-host", server.URL+"/api/i/batch", "test-key", false); err != nil {
		t.Fatal(err)
	}
	if diagnosticClient == nil {
		t.Fatal("diagnostic capability was not initialized")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if c := prostometrics.Default(); c != nil {
			_ = c.Close(ctx)
		}
		diagnosticClient.Close()
		diagnosticClient = nil
	})
	req, err := http.NewRequest(http.MethodPost, server.URL+"/api/i/batch", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := diagnosticClient.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if got := <-capability; got != "v2" {
		t.Fatalf("capability=%q", got)
	}
}
