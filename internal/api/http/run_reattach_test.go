package http

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// sseLines reads an SSE response into a channel of (event, data) frames until
// the body closes. Mirrors the reader in the interactive e2e test.
func sseLines(t *testing.T, body io.Reader) <-chan [2]string {
	t.Helper()
	out := make(chan [2]string, 256)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		var typ, data string
		for sc.Scan() {
			line := sc.Text()
			if line == "" {
				if typ != "" {
					out <- [2]string{typ, data}
					typ, data = "", ""
				}
				continue
			}
			if strings.HasPrefix(line, "event:") {
				typ = strings.TrimSpace(line[len("event:"):])
			}
			if strings.HasPrefix(line, "data:") {
				data = strings.TrimSpace(line[len("data:"):])
			}
		}
	}()
	return out
}

func interactiveReattachServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"termagent": {Model: "stub-model", SystemPrompt: "be brief", UnboundedIterations: true},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "reattach.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, st)
	srv.SetSteerRegistry(steer.NewRegistry(0))
	ts := httptest.NewServer(srv.Mux())
	return ts, func() { ts.Close(); _ = st.Close() }
}

// startAndPark posts an interactive run, drains until it parks, and returns the
// run_id + agent_id captured from the side-channel frame. Closes the stream
// (simulating the operator navigating away) before returning.
func startAndPark(t *testing.T, ts *httptest.Server) (runID, agentID string) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"termagent","interactive":true,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatalf("post run: %v", err)
	}
	frames := sseLines(t, resp.Body)
	next := func() [2]string {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before park")
			}
			return f
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for park")
			return [2]string{}
		}
	}
	for {
		f := next()
		if f[0] == "agent" {
			var a struct {
				RunID   string `json:"run_id"`
				AgentID string `json:"agent_id"`
			}
			_ = json.Unmarshal([]byte(f[1]), &a)
			runID, agentID = a.RunID, a.AgentID
		}
		if f[0] == "awaiting_input" {
			break
		}
		if f[0] == "done" || f[0] == "error" {
			t.Fatalf("run ended (%s) before parking", f[0])
		}
	}
	if runID == "" {
		t.Fatal("no run_id captured")
	}
	// Operator navigates away: close the initial stream. Under the OLD
	// (request-bound) model this cancelled the run; the detached model keeps
	// it alive.
	_ = resp.Body.Close()
	return runID, agentID
}

// TestInteractiveRun_SurvivesDisconnect_AndReattaches is the core PR proof: an
// interactive run keeps running after the client disconnects, accepts a steer,
// and is re-attachable via GET /v1/runs/{run_id}/stream (replay + resume).
func TestInteractiveRun_SurvivesDisconnect_AndReattaches(t *testing.T) {
	ts, cleanup := interactiveReattachServer(t)
	defer cleanup()

	runID, agentID := startAndPark(t, ts)

	// The decisive assertion: after disconnect the steer endpoint still finds
	// the live run (200). Under the request-bound model the steer registry
	// would have been deregistered → 404. Poll briefly to let the server
	// observe the disconnect first.
	var delivered bool
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		in, err := http.Post(ts.URL+"/v1/runs/"+runID+"/input", "application/json",
			strings.NewReader(`{"text":"keep going"}`))
		if err != nil {
			t.Fatalf("post input: %v", err)
		}
		body, _ := io.ReadAll(in.Body)
		in.Body.Close()
		if in.StatusCode == 200 {
			delivered = true
			_ = body
			break
		}
		if in.StatusCode != 404 {
			t.Fatalf("unexpected input status %d: %s", in.StatusCode, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !delivered {
		t.Fatal("POST /input never delivered after disconnect — the run did not survive (detach failed)")
	}

	// Re-attach: replay from seq 0 and tail. We must see the agent's resumed
	// `text` turn (the steer woke the park) and a re-park.
	resp, err := http.Get(ts.URL + "/v1/runs/" + runID + "/stream?from_seq=0")
	if err != nil {
		t.Fatalf("get stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	frames := sseLines(t, resp.Body)
	sawText, sawPark := false, false
	timeout := time.After(5 * time.Second)
	for !(sawText && sawPark) {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("re-attach stream closed early")
			}
			switch f[0] {
			case "text":
				sawText = true
			case "awaiting_input":
				sawPark = true
			}
		case <-timeout:
			t.Fatalf("re-attach did not replay text+park (text=%v park=%v)", sawText, sawPark)
		}
	}

	// Clean up the detached goroutine: cancel the run.
	if agentID != "" {
		c, _ := http.Post(ts.URL+"/v1/agents/"+agentID+"/cancel", "application/json", strings.NewReader(`{}`))
		if c != nil {
			c.Body.Close()
		}
	}
}

// TestRunStream_UnknownRun404 — re-attaching to an unknown run is an opaque 404
// (the same shape a cross-tenant attach gets; cross-tenant uses the
// sessionOwnershipOK gate mirrored from handleRunInput).
func TestRunStream_UnknownRun404(t *testing.T) {
	ts, cleanup := interactiveReattachServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/v1/runs/r_does_not_exist/stream")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}
