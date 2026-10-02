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
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// newStubProviderServer wires a server whose only provider is p. A bare
// &stubProvider{} opens a stream and closes it without a done event or an
// error — a call that never finished.
func newStubProviderServer(t *testing.T, p providers.Provider) (*Server, store.Store) {
	t.Helper()
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"default": {Model: "stub-model"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(cfg, &stubResolver{p: p}, nil, concurrency.New(4, 4, time.Second), st), st
}

// TestRuns_UnfinishedProviderStreamRecordsRunFailed: POST /v1/runs against a
// provider whose call never finished must leave the run row failed with the
// cause. It used to be recorded completed — no text, no stop reason, zero
// usage, no error.
func TestRuns_UnfinishedProviderStreamRecordsRunFailed(t *testing.T) {
	srv, st := newStubProviderServer(t, &stubProvider{})
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"default","segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	sessionID := extractSessionID(string(body))
	if sessionID == "" {
		t.Fatalf("no session id in the stream:\n%s", body)
	}
	if !strings.Contains(string(body), "event: error") {
		t.Errorf("the SSE stream carries no error frame:\n%s", body)
	}

	runs, err := st.RunsForSession(context.Background(), sessionID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("RunsForSession = (%d runs, %v), want 1", len(runs), err)
	}
	if runs[0].Status != store.RunFailed {
		t.Errorf("run status = %q, want %q", runs[0].Status, store.RunFailed)
	}
	if !strings.Contains(runs[0].ErrorMsg, "ended before the model finished") {
		t.Errorf("run error = %q, want the unfinished-stream cause", runs[0].ErrorMsg)
	}
}

// TestSpawnRun_UnfinishedProviderStreamEnvelopeIsFailed: the envelope MCP
// spawn_run / spawn_runs, gRPC SpawnRunBatch and POST /v1/runs:batch return
// (Server.SpawnRun) must say failed and carry the cause. It used to say
// completed with no text and zero usage.
func TestSpawnRun_UnfinishedProviderStreamEnvelopeIsFailed(t *testing.T) {
	srv, _ := newStubProviderServer(t, &stubProvider{})

	res, err := srv.SpawnRun(context.Background(), connector.SpawnRunRequest{
		Agent:    "default",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("SpawnRun: %v", err)
	}
	if res.Status != string(store.RunFailed) {
		t.Errorf("envelope status = %q, want %q (stop_reason=%q, usage=%+v)", res.Status, store.RunFailed, res.StopReason, res.Usage)
	}
	if !strings.Contains(res.Error, "ended before the model finished") {
		t.Errorf("envelope error = %q, want the unfinished-stream cause", res.Error)
	}
}

// TestSpawnRun_FinishedProviderStreamStillCompletes: the inverse — a call that
// ends with its done event is untouched.
func TestSpawnRun_FinishedProviderStreamStillCompletes(t *testing.T) {
	srv, _ := newStubProviderServer(t, &stubProvider{events: []providers.Event{
		{Type: providers.EventText, Text: "hello"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 5, OutputTokens: 1}},
	}})

	res, err := srv.SpawnRun(context.Background(), connector.SpawnRunRequest{
		Agent:    "default",
		Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("SpawnRun: %v", err)
	}
	if res.Status != string(store.RunCompleted) || res.FinalText != "hello" || res.Error != "" {
		t.Errorf("envelope = (status %q, text %q, error %q), want (completed, hello, none)", res.Status, res.FinalText, res.Error)
	}
}
