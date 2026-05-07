package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Happy path: server returns 200 + a healthy body → rc=0.
func TestRunHealth_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunHealth([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 0 {
		t.Errorf("rc=%d, want 0; stderr=%q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "200") {
		t.Errorf("stdout missing 200: %q", stdout.String())
	}
}

// Server up but returning non-200 → rc=1 (operational failure: the
// runtime/infra is sick, not the operator's invocation).
func TestRunHealth_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down for maintenance", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunHealth([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 1 {
		t.Errorf("rc=%d, want 1 (operational: server reachable but unhealthy)", rc)
	}
	if !strings.Contains(stderr.String(), "non-OK status: 503") {
		t.Errorf("stderr missing diagnostic: %q", stderr.String())
	}
}

// Server unreachable → rc=1 (operational: network failure).
func TestRunHealth_Unreachable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunHealth([]string{"--target", "http://127.0.0.1:1", "--timeout", "200ms"}, &stdout, &stderr)
	if rc != 1 {
		t.Errorf("rc=%d, want 1 (operational: connection refused)", rc)
	}
	if !strings.Contains(stderr.String(), "GET ") {
		t.Errorf("stderr missing GET diagnostic: %q", stderr.String())
	}
}

// Default target falls through env → defaults file URL.
func TestDefaultHealthTarget(t *testing.T) {
	t.Setenv("LOOMCYCLE_BASE_URL", "")
	if got := defaultHealthTarget(); got != "http://127.0.0.1:8787" {
		t.Errorf("default fallback: got %q", got)
	}
	t.Setenv("LOOMCYCLE_BASE_URL", "http://other:9000")
	if got := defaultHealthTarget(); got != "http://other:9000" {
		t.Errorf("env override: got %q", got)
	}
}

// Sanity: timeout flag actually takes effect.
func TestRunHealth_TimeoutFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunHealth([]string{"--target", srv.URL, "--timeout", "50ms"}, &stdout, &stderr)
	if rc == 0 {
		t.Errorf("rc=0, want non-zero (timeout should trigger)")
	}
}
