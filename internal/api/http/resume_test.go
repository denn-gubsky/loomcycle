package http

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// appendResumeEvent persists one transcript event for a run under test.
func appendResumeEvent(t *testing.T, srv *Server, runID, typ string, payload any) {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", typ, err)
	}
	if err := srv.store.AppendEvent(context.Background(), runID, typ, b); err != nil {
		t.Fatalf("AppendEvent(%s): %v", typ, err)
	}
}

// TestResumePausedRuns_ResumesMidConversationToCompletion is the F42 / RFC X
// Phase 2 core test: a pause_state='paused' run whose transcript ends on a
// pending tool_result turn is re-dispatched (loop reconstructed from the
// transcript) and runs to completion — the exact "snapshot a mid-run, continue
// it elsewhere" capability F42 was about. Fail-before: nothing backed a
// restored paused run; resume 409'd and a restart didn't relaunch it.
func TestResumePausedRuns_ResumesMidConversationToCompletion(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"resumer": {Provider: "scripted", Model: "stub-model", SystemPrompt: "you resume work", AllowedTools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	// On resume the loop makes one provider call; return end_turn so it finishes.
	prov := &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "resumed and done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()

	// A run whose transcript ends on a pending tool_result (a clean pause
	// boundary). replayTranscript → [user, assistant(tool_use), user(tool_result)].
	sess, err := srv.store.CreateSession(ctx, "", "resumer", "alice")
	if err != nil {
		t.Fatal(err)
	}
	run, err := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_resume", UserID: "alice", Model: "stub-model"})
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

	n, warnings := srv.ResumePausedRuns(ctx)
	if n != 1 {
		t.Fatalf("ResumePausedRuns re-dispatched %d, want 1 (warnings: %v)", n, warnings)
	}

	// The loop runs in a detached goroutine — poll for the terminal state.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, gerr := srv.store.GetRun(ctx, run.ID)
		if gerr != nil {
			t.Fatalf("GetRun: %v", gerr)
		}
		if got.Status == store.RunCompleted {
			if got.PauseState != store.PauseStateRunning {
				t.Errorf("resumed run pause_state = %q, want running (flipped on resume)", got.PauseState)
			}
			break
		}
		if got.Status == store.RunFailed {
			t.Fatalf("resumed run failed: %s", got.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("resumed run did not complete (status=%q)", got.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if prov.calls.Load() < 1 {
		t.Errorf("provider never called — the loop wasn't resumed")
	}
}

// TestResumePausedRuns_FlagsRunWithMissingAgent: a paused run whose agent no
// longer resolves is flagged failed (not left a zombie) and surfaced as a
// warning, not silently dropped or hung.
func TestResumePausedRuns_FlagsRunWithMissingAgent(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{}, // no agents — "ghost" won't resolve
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	ctx := context.Background()

	sess, _ := srv.store.CreateSession(ctx, "", "ghost", "alice")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_ghost", UserID: "alice"})
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}},
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	n, warnings := srv.ResumePausedRuns(ctx)
	if n != 0 {
		t.Errorf("re-dispatched %d, want 0 (agent missing)", n)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want exactly 1 (the unresolvable run)", warnings)
	}
	// The run is flagged terminal so it isn't a permanent "running" zombie.
	got, _ := srv.store.GetRun(ctx, run.ID)
	if got.Status != store.RunFailed {
		t.Errorf("unresumable run status = %q, want failed (flagged, not zombie)", got.Status)
	}
}
