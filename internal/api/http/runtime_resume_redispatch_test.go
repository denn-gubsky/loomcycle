package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// unheldPausedRun builds a single-replica server with a pause manager and one
// paused row no loop here holds — what a restore or a refused resume leaves —
// whose transcript ends on a clean tool_result boundary.
func unheldPausedRun(t *testing.T) (*Server, store.Run) {
	t.Helper()
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work", Tools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8},
	}
	cfg.Env.AuthToken = ""
	prov := &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "resumed and done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	srv.SetPauseManager(pause.NewManager(srv.store, 100*time.Millisecond))
	ctx := context.Background()
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_unheld", UserID: "alice", Model: "stub-model"})
	if err != nil {
		t.Fatal(err)
	}
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "do the thing"}}},
	})
	appendResumeEvent(t, srv, run.ID, "tool_call", providers.Event{
		Type:    providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read", Input: json.RawMessage(`{"path":"/x"}`)},
	})
	appendResumeEvent(t, srv, run.ID, "tool_result", providers.Event{
		Type:    providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Read"},
		Text:    "FILE CONTENTS",
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.pauseMgr.Pause(ctx, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	return srv, run
}

// On a single replica a runtime resume over HTTP re-dispatches a paused row no
// loop here holds: the resume leaves its row paused (it has no gate to wake),
// and the resume pass started behind it runs it to completion.
func TestRuntimeResume_HTTPRedispatchesAnUnheldPausedRun(t *testing.T) {
	srv, run := unheldPausedRun(t)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/_resume", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var res pause.ResumeResult
	_ = json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || res.ResumedRunsCount != 0 {
		t.Fatalf("resume = %d %+v; want 200 with no held run flipped", resp.StatusCode, res)
	}
	waitWalkRunStatus(t, srv.store, run.ID, store.RunCompleted)
}

// The connector's resume (gRPC, MCP) does the same.
func TestRuntimeResume_ConnectorRedispatchesAnUnheldPausedRun(t *testing.T) {
	srv, run := unheldPausedRun(t)
	if _, err := srv.ResumeRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitWalkRunStatus(t, srv.store, run.ID, store.RunCompleted)
}

// The pass a runtime resume starts dispatches nothing when the runtime is
// paused again by the time it runs: the unheld row stays paused, unclaimed.
func TestRuntimeResume_PassSkipsWhenTheRuntimeIsPausedAgain(t *testing.T) {
	srv, run := unheldPausedRun(t) // leaves the runtime paused
	srv.resumeUnheldPausedRuns(context.Background())
	got, err := srv.store.GetRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PauseState != store.PauseStatePaused || got.Status != store.RunRunning {
		t.Errorf("run = %s/%s, want running/paused (not dispatched while paused)", got.Status, got.PauseState)
	}
	if _, live := srv.cancelReg.Get(run.AgentID); live {
		t.Error("the run was dispatched while the runtime is paused")
	}
}
