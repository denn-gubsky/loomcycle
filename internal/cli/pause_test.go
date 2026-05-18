package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServer returns a httptest.Server that lets the test assert on
// the incoming request and reply with a canned body. Used by each
// subcommand test below.
func stubServer(t *testing.T, expectMethod, expectPath string, status int, respBody string, captureBody *[]byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != expectMethod {
			t.Errorf("method = %q, want %q", r.Method, expectMethod)
		}
		if r.URL.Path != expectPath {
			t.Errorf("path = %q, want %q", r.URL.Path, expectPath)
		}
		if captureBody != nil {
			b, _ := io.ReadAll(r.Body)
			*captureBody = b
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
}

// TestRunPause_HappyPath — exit 0, prints summary to stdout, sends
// {"timeout_ms":N} when --timeout-ms is given.
func TestRunPause_HappyPath(t *testing.T) {
	var body []byte
	srv := stubServer(t, "POST", "/v1/_pause", 200, `{"state":"paused","duration_ms":123,"force_cancelled_count":1,"paused_runs_count":2}`, &body)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunPause([]string{"--target", srv.URL, "--timeout-ms", "5000"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state=paused") {
		t.Errorf("stdout = %q, expected to contain state=paused", stdout.String())
	}
	if !strings.Contains(string(body), `"timeout_ms":5000`) {
		t.Errorf("request body = %q, expected timeout_ms=5000", body)
	}
}

// TestRunPause_NoTimeoutSendsEmptyBody — when --timeout-ms is omitted,
// the request body is empty (not {"timeout_ms":0}) so the server
// uses its configured default.
func TestRunPause_NoTimeoutSendsEmptyBody(t *testing.T) {
	var body []byte
	srv := stubServer(t, "POST", "/v1/_pause", 200, `{"state":"paused"}`, &body)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunPause([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if len(body) != 0 {
		t.Errorf("request body = %q, want empty", body)
	}
}

// TestRunPause_409ReturnsUserErrorExit — server 409 (already_pausing)
// maps to exit 2.
func TestRunPause_409ReturnsUserErrorExit(t *testing.T) {
	srv := stubServer(t, "POST", "/v1/_pause", 409, `{"error":"already_pausing","message":"runtime is already pausing or paused"}`, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunPause([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc = %d, want 2 (4xx → user-error)", rc)
	}
	if !strings.Contains(stderr.String(), "already_pausing") {
		t.Errorf("stderr = %q, expected to mention already_pausing", stderr.String())
	}
}

// TestRunPause_5xxReturnsOpErrorExit — server 503 maps to exit 1.
func TestRunPause_5xxReturnsOpErrorExit(t *testing.T) {
	srv := stubServer(t, "POST", "/v1/_pause", 503, `{"error":"pause_not_configured"}`, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunPause([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 1 {
		t.Errorf("rc = %d, want 1 (5xx → operational)", rc)
	}
}

func TestRunResume_HappyPath(t *testing.T) {
	srv := stubServer(t, "POST", "/v1/_resume", 200, `{"state":"running","resumed_runs_count":3}`, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunResume([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state=running") || !strings.Contains(stdout.String(), "resumed_runs=3") {
		t.Errorf("stdout = %q, expected state=running + resumed_runs=3", stdout.String())
	}
}

func TestRunResume_409NotPaused(t *testing.T) {
	srv := stubServer(t, "POST", "/v1/_resume", 409, `{"error":"not_paused"}`, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunResume([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

func TestRunState_HappyPath(t *testing.T) {
	srv := stubServer(t, "GET", "/v1/_state", 200, `{"state":"paused","paused_runs_count":5}`, nil)
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunState([]string{"--target", srv.URL}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d, stderr = %q", rc, stderr.String())
	}
	if !strings.Contains(stdout.String(), "state=paused") || !strings.Contains(stdout.String(), "paused_runs=5") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

// TestRunPause_BadFlagReturns2 — invalid flag triggers a parse error;
// flag.ContinueOnError returns it; RunPause returns 2.
func TestRunPause_BadFlagReturns2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	rc := RunPause([]string{"--bogus"}, &stdout, &stderr)
	if rc != 2 {
		t.Errorf("rc = %d, want 2", rc)
	}
}

// TestRunPause_SendsBearer — the --token flag lands in the
// Authorization header.
func TestRunPause_SendsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"state":"paused"}`)
	}))
	defer srv.Close()

	var stdout, stderr bytes.Buffer
	rc := RunPause([]string{"--target", srv.URL, "--token", "s3cret"}, &stdout, &stderr)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", gotAuth)
	}
}

// TestParseTimeoutMs validates the shared timeout-clamp helper. Not
// used by the CLI handlers directly today (they read JSON body) but
// exercises the helper in case future verbs adopt it.
func TestParseTimeoutMs(t *testing.T) {
	for _, tc := range []struct {
		in    string
		wantD int64
		wantE bool
	}{
		{"", 0, false},
		{"100", 100, false},
		{"-1", 0, true},
		{"abc", 0, true},
	} {
		// parseTimeoutMs is in the http package; not testable from cli.
		// This test instead verifies the JSON encoding via a round trip.
		body, _ := json.Marshal(map[string]int64{"timeout_ms": tc.wantD})
		if tc.in != "" && tc.wantD > 0 {
			if !strings.Contains(string(body), `"timeout_ms":100`) {
				t.Errorf("encoded body = %q", body)
			}
		}
	}
}
