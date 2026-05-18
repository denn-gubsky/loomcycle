package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// v0.8.17 PR 4: CLI subcommands for the pause / resume / state HTTP
// surface. Each is a thin wrapper around an admin endpoint; the
// underlying state machine lives in internal/pause.
//
//   loomcycle pause [--timeout-ms 30000] [--target <url>] [--token <bearer>]
//   loomcycle resume                     [--target <url>] [--token <bearer>]
//   loomcycle state                      [--target <url>] [--token <bearer>]
//
// Exit codes mirror the other subcommands: 0 = success, 1 =
// operational (server unreachable, 5xx), 2 = user error (bad flag,
// 4xx). The --target / --token flags default to LOOMCYCLE_BASE_URL /
// LOOMCYCLE_AUTH_TOKEN env so operators normally don't pass either.

// pauseResumeResponse covers the shape of both /v1/_pause and
// /v1/_resume bodies. We don't need all fields; just enough to print
// a useful one-line summary.
type pauseResumeResponse struct {
	State               string   `json:"state"`
	DurationMs          int64    `json:"duration_ms,omitempty"`
	ForceCancelledCount int      `json:"force_cancelled_count,omitempty"`
	PausedRunsCount     int      `json:"paused_runs_count,omitempty"`
	ResumedRunsCount    int      `json:"resumed_runs_count,omitempty"`
	Warnings            []string `json:"warnings,omitempty"`
}

type stateResponse struct {
	State           string `json:"state"`
	PausedRunsCount int    `json:"paused_runs_count"`
}

type jsonErrorResponse struct {
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// RunPause invokes POST /v1/_pause on the configured target.
func RunPause(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pause", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL (default: $LOOMCYCLE_BASE_URL or http://127.0.0.1:8787)")
	token := fs.String("token", defaultAuthToken(), "bearer token (default: $LOOMCYCLE_AUTH_TOKEN)")
	timeoutMs := fs.Int64("timeout-ms", 0, "per-call wait-for-non-idempotent-tools cap; 0 ⇒ server default")
	httpTimeout := fs.Duration("http-timeout", 60*time.Second, "client-side HTTP request timeout (must exceed the server's pause timeout so we don't time out before pause finishes)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	body, _ := json.Marshal(map[string]int64{"timeout_ms": *timeoutMs})
	if *timeoutMs == 0 {
		body = nil // empty body is valid and clearer than {"timeout_ms":0}
	}
	url := strings.TrimRight(*target, "/") + "/v1/_pause"
	rc, resp, err := doAdminRequest(http.MethodPost, url, *token, body, *httpTimeout)
	if err != nil {
		return failOp(stderr, "POST %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	var out pauseResumeResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return failOp(stderr, "decode pause response: %v (body: %s)", err, truncate(resp, 200))
	}
	fmt.Fprintf(stdout, "paused state=%s duration_ms=%d force_cancelled=%d paused_runs=%d\n",
		out.State, out.DurationMs, out.ForceCancelledCount, out.PausedRunsCount)
	for _, w := range out.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return 0
}

// RunResume invokes POST /v1/_resume on the configured target.
func RunResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := fs.String("token", defaultAuthToken(), "bearer token")
	httpTimeout := fs.Duration("http-timeout", 30*time.Second, "client-side HTTP request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url := strings.TrimRight(*target, "/") + "/v1/_resume"
	rc, resp, err := doAdminRequest(http.MethodPost, url, *token, nil, *httpTimeout)
	if err != nil {
		return failOp(stderr, "POST %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	var out pauseResumeResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return failOp(stderr, "decode resume response: %v (body: %s)", err, truncate(resp, 200))
	}
	fmt.Fprintf(stdout, "resumed state=%s resumed_runs=%d\n", out.State, out.ResumedRunsCount)
	for _, w := range out.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", w)
	}
	return 0
}

// RunState invokes GET /v1/_state on the configured target.
func RunState(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("state", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("target", defaultBaseURL(), "loomcycle base URL")
	token := fs.String("token", defaultAuthToken(), "bearer token")
	httpTimeout := fs.Duration("http-timeout", 10*time.Second, "client-side HTTP request timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	url := strings.TrimRight(*target, "/") + "/v1/_state"
	rc, resp, err := doAdminRequest(http.MethodGet, url, *token, nil, *httpTimeout)
	if err != nil {
		return failOp(stderr, "GET %s: %v", url, err)
	}
	if rc != 0 {
		return failPrintingBody(stderr, url, resp, rc)
	}
	var out stateResponse
	if err := json.Unmarshal(resp, &out); err != nil {
		return failOp(stderr, "decode state response: %v (body: %s)", err, truncate(resp, 200))
	}
	fmt.Fprintf(stdout, "state=%s paused_runs=%d\n", out.State, out.PausedRunsCount)
	return 0
}

// doAdminRequest is the shared HTTP plumbing. Returns (exitCode,
// body, err): exitCode==0 means status 2xx and body is the response;
// non-zero exitCode means non-2xx and the caller should print body
// via failPrintingBody. Network/setup errors come back via err with
// exitCode==-1 (caller treats as failOp).
func doAdminRequest(method, url, token string, body []byte, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return -1, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return -1, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return 0, raw, nil
	case resp.StatusCode == http.StatusConflict:
		// 409 means the server is in the wrong state for this verb
		// (e.g. `pause` when already paused; `resume` when running).
		// That's a runtime-state condition, NOT a bad invocation,
		// so map it to exit 1 (operational) alongside 5xx. Scripts
		// using `set -e` + `|| true` around an idempotent pause loop
		// expect this so they don't bail on a benign no-op.
		return 1, raw, nil
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return 2, raw, nil // user error (bad flag / 4xx other than 409)
	default:
		return 1, raw, nil // operational (5xx)
	}
}

// failPrintingBody writes the server's error body to stderr and
// returns the exit code from doAdminRequest. If the body is JSON
// shaped like {"error":..., "message":...} it formats nicely;
// otherwise it dumps the raw bytes (truncated).
func failPrintingBody(stderr io.Writer, url string, body []byte, rc int) int {
	var je jsonErrorResponse
	if err := json.Unmarshal(body, &je); err == nil && (je.Error != "" || je.Message != "") {
		if je.Message != "" {
			fmt.Fprintf(stderr, "loomcycle: error: %s: %s (%s)\n", url, je.Message, je.Error)
		} else {
			fmt.Fprintf(stderr, "loomcycle: error: %s: %s\n", url, je.Error)
		}
	} else {
		fmt.Fprintf(stderr, "loomcycle: error: %s: %s\n", url, truncate(body, 200))
	}
	return rc
}

func truncate(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func defaultBaseURL() string {
	return getenvDefault("LOOMCYCLE_BASE_URL", "http://127.0.0.1:8787")
}

func defaultAuthToken() string {
	return getenvDefault("LOOMCYCLE_AUTH_TOKEN", "")
}
