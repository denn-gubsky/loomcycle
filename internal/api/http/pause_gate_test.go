package http

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// TestHandleRuns_503WhilePaused asserts the runtime-pause admission gate
// (RFC X / F41): a new POST /v1/runs is rejected with 503 while the runtime is
// paused, and admitted again after resume.
func TestHandleRuns_503WhilePaused(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {Model: "stub-model", SystemPrompt: "hi"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = "" // open mode
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "ok"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "pausegate.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, st)
	mgr := pause.NewManager(st, 200*time.Millisecond)
	srv.SetPauseManager(mgr)

	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	post := func() (int, string) {
		resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
			`{"agent":"agent","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
		))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// Pause → new runs rejected with 503.
	if _, err := mgr.Pause(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if code, body := post(); code != http.StatusServiceUnavailable {
		t.Fatalf("POST /v1/runs while paused = %d (%s), want 503", code, body)
	}

	// Resume → admitted again (200, SSE stream).
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if code, _ := post(); code != http.StatusOK {
		t.Fatalf("POST /v1/runs after resume = %d, want 200", code)
	}
}

// TestRunOnce_RuntimePausedRejected asserts the RunOnce admission gate (the
// gRPC / webhook / A2A / scheduler surface) returns runner.ErrRuntimePaused
// while paused.
func TestRunOnce_RuntimePausedRejected(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"agent": {Model: "stub-model", SystemPrompt: "hi"},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	provider := &stubProvider{events: []providers.Event{
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	sem := concurrency.New(4, 4, 100*time.Millisecond)
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "pausegate2.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	srv := New(cfg, &stubResolver{p: provider}, []tools.Tool{}, sem, st)
	mgr := pause.NewManager(st, 200*time.Millisecond)
	srv.SetPauseManager(mgr)

	if _, err := mgr.Pause(context.Background(), 100*time.Millisecond); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if got := srv.pausedRunErr(); got == nil {
		t.Fatal("pausedRunErr() = nil while paused, want runner.ErrRuntimePaused")
	}
	if _, err := mgr.Resume(context.Background()); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := srv.pausedRunErr(); got != nil {
		t.Errorf("pausedRunErr() = %v after resume, want nil", got)
	}
}
