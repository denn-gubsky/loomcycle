package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A restore resumes the paused runs it wrote on every transport, not only
// over HTTP. The connector restore (what gRPC and MCP call) used to write the
// rows and stop, so a run restored that way stayed parked until a boot or an
// HTTP restore happened to resume it.

// restoreResumeServer is a server with a scripted provider that ends the turn,
// so a resumed run reaches a terminal state, plus an envelope holding one
// paused run of its "resumer" agent waiting on a user turn.
func restoreResumeServer(t *testing.T) (srv *Server, prov *scriptedProvider, runID string, envelope []byte) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	prov = &scriptedProvider{defaultS: []providers.Event{
		{Type: providers.EventText, Text: "resumed and done"},
		{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
	}}
	srv, _ = makeServer(t, prov, cfg)
	runID, envelope = capturePausedRun(t, store.RunIdentity{AgentID: "a_restore_resume", UserID: "alice", TenantID: "acme", Model: "stub-model"}, "resumer",
		func(st store.Store, runID string) {
			b, _ := json.Marshal([]loop.PromptSegment{
				{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
			})
			if err := st.AppendEvent(context.Background(), runID, "user_input", b); err != nil {
				t.Fatal(err)
			}
		})
	return srv, prov, runID, envelope
}

// awaitRunCompleted waits for a resumed run to finish its turn.
func awaitRunCompleted(t *testing.T, st store.Store, runID string) store.Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		switch got.Status {
		case store.RunCompleted:
			return got
		case store.RunFailed:
			t.Fatalf("resumed run failed: %s", got.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restored run was never resumed (status=%q pause_state=%q)", got.Status, got.PauseState)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRestore_ResumesTheRestoredPausedRunOnEveryCallSite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restore func(t *testing.T, srv *Server, envelope []byte) (resumed int, restored map[string]int)
	}{
		{"connector", func(t *testing.T, srv *Server, envelope []byte) (int, map[string]int) {
			res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: envelope})
			if err != nil {
				t.Fatalf("RestoreSnapshot: %v", err)
			}
			return res.PausedRunsResumed, res.Restored
		}},
		{"http", func(t *testing.T, srv *Server, envelope []byte) (int, map[string]int) {
			body, _ := json.Marshal(map[string]any{"json": json.RawMessage(envelope)})
			req := httptest.NewRequest("POST", "/v1/_snapshots/inline/restore", bytes.NewReader(body))
			req.SetPathValue("id", "inline")
			rec := httptest.NewRecorder()
			srv.handleRestoreSnapshot(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("restore status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp snapshotRestoreResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			return resp.PausedRunsResumed, resp.Restored
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, prov, runID, envelope := restoreResumeServer(t)
			resumed, restored := tc.restore(t, srv, envelope)
			if resumed != 1 {
				t.Errorf("paused_runs_resumed = %d, want 1", resumed)
			}
			if restored["paused_runs_resumed"] != 1 || restored["paused_runs"] != 1 {
				t.Errorf("restored map = %v, want paused_runs and paused_runs_resumed both 1", restored)
			}
			got := awaitRunCompleted(t, srv.store, runID)
			if got.PauseState != store.PauseStateRunning {
				t.Errorf("resumed run pause_state = %q, want %q", got.PauseState, store.PauseStateRunning)
			}
			if prov.calls.Load() < 1 {
				t.Error("the provider was never called — the restored run's loop did not run")
			}
		})
	}
}

// The connector restore refreshes the caches before it resumes, as the HTTP
// restore does (TestRestoreSnapshot_RefreshRunsBeforePausedRunsResume): the
// refresh records whether the restored run was still waiting to resume.
func TestConnectorRestoreSnapshot_RefreshRunsBeforePausedRunsResume(t *testing.T) {
	srv, st, cleanup := minimalServerWithSnapshotStore(t)
	defer cleanup()
	runID, raw := capturePausedRun(t, store.RunIdentity{AgentID: "a_order_conn", UserID: "alice", TenantID: "acme"}, "qa", nil)

	var statusAtRefresh store.RunStatus
	var refreshed int
	srv.SetMCPRegistryRefresh(func(ctx context.Context) (int, error) {
		refreshed++
		run, err := st.GetRun(ctx, runID)
		if err != nil {
			t.Errorf("refresh: read restored run: %v", err)
			return 0, nil
		}
		statusAtRefresh = run.Status
		return 0, nil
	})

	res, err := srv.RestoreSnapshot(context.Background(), connector.RestoreSnapshotRequest{RawJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 1 {
		t.Fatalf("refresh ran %d times, want 1", refreshed)
	}
	// "qa" does not resolve here, so the resume flags the run failed rather
	// than re-dispatching it — which is what makes it observable that the
	// resume ran at all, and after the refresh.
	after, err := st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != store.RunFailed || len(res.Warnings) == 0 {
		t.Fatalf("the connector restore did not resume the restored run (status %q, warnings %v)", after.Status, res.Warnings)
	}
	if statusAtRefresh != store.RunRunning {
		t.Errorf("the refresh saw the restored run as %q; it ran after the resume (want %q)", statusAtRefresh, store.RunRunning)
	}
}
