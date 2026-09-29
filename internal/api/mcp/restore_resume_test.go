package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	lchttp "github.com/denn-gubsky/loomcycle/internal/api/http"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The restore_snapshot tool resumes the paused runs it writes, as the HTTP
// restore does — against the REAL HTTP server as the connector, since the
// resume lives there.

// endTurnProvider ends every turn, so a resumed run reaches a terminal state.
type endTurnProvider struct{ calls atomic.Int32 }

func (p *endTurnProvider) ID() string                  { return "stub" }
func (p *endTurnProvider) Probe(context.Context) error { return nil }
func (p *endTurnProvider) ListModels(context.Context) ([]string, error) {
	return []string{"stub-model"}, nil
}
func (p *endTurnProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true}
}
func (p *endTurnProvider) Call(context.Context, providers.Request) (<-chan providers.Event, error) {
	p.calls.Add(1)
	ch := make(chan providers.Event, 2)
	ch <- providers.Event{Type: providers.EventText, Text: "resumed and done"}
	ch <- providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}}
	close(ch)
	return ch, nil
}

// capturePausedRunEnvelope builds, on a store of its own, one paused run of
// agent waiting on a user turn, and returns its id and the snapshot envelope.
func capturePausedRunEnvelope(t *testing.T, agent string) (string, []byte) {
	t.Helper()
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	sess, err := src.CreateSession(ctx, "acme", agent, "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := src.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_mcp_restore", UserID: "alice", TenantID: "acme", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal([]loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
	})
	if err := src.AppendEvent(ctx, run.ID, "user_input", b); err != nil {
		t.Fatal(err)
	}
	if err := src.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID, raw
}

func TestRestoreSnapshotTool_ResumesTheRestoredPausedRun(t *testing.T) {
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "mcp-restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "stub", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"resumer": {Model: "stub-model", SystemPrompt: "you resume work"}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 4, MaxQueueDepth: 4, QueueTimeoutMS: 1000},
	}
	prov := &endTurnProvider{}
	httpSrv := lchttp.New(cfg, singleProvider{prov}, []tools.Tool{}, concurrency.New(4, 4, time.Second), st)
	srv := New(Config{Connector: httpSrv, Logf: func(string, ...any) {}})

	runID, raw := capturePausedRunEnvelope(t, "resumer")
	res := callTool(t, srv, "restore_snapshot", map[string]any{"raw_json": json.RawMessage(raw)})
	if res.IsError {
		t.Fatalf("restore_snapshot errored: %+v", res.Content)
	}
	var out struct {
		PausedRunsRestored int            `json:"paused_runs_restored"`
		PausedRunsResumed  int            `json:"paused_runs_resumed"`
		Restored           map[string]int `json:"restored"`
		Warnings           []string       `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("restore_snapshot result: %v (%s)", err, res.Content[0].Text)
	}
	if out.PausedRunsRestored != 1 || out.PausedRunsResumed != 1 {
		t.Errorf("paused_runs_restored = %d, paused_runs_resumed = %d; want 1 and 1 (warnings %v)",
			out.PausedRunsRestored, out.PausedRunsResumed, out.Warnings)
	}
	if out.Restored["paused_runs_resumed"] != 1 {
		t.Errorf("restored[paused_runs_resumed] = %d, want 1", out.Restored["paused_runs_resumed"])
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		run, err := st.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status == store.RunCompleted {
			if run.PauseState != store.PauseStateRunning {
				t.Errorf("resumed run pause_state = %q, want %q", run.PauseState, store.PauseStateRunning)
			}
			break
		}
		if run.Status == store.RunFailed {
			t.Fatalf("resumed run failed: %s", run.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run restored over MCP was never resumed (status=%q pause_state=%q)", run.Status, run.PauseState)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if prov.calls.Load() < 1 {
		t.Error("the provider was never called — the restored run's loop did not run")
	}
}
