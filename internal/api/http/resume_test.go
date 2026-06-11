package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
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

// ---- RFC X Phase 3: parked fan-out parent reconcile ----

func mkEvent(typ string, payload any) store.Event {
	b, _ := json.Marshal(payload)
	return store.Event{Type: typ, Payload: b}
}

// TestDetectFanoutParent covers the detection predicate: flag-gated, and keyed
// on "a tool_use with a spawn_child_started ledger AND no tool_result".
func TestDetectFanoutParent(t *testing.T) {
	dangling := []store.Event{
		mkEvent("tool_call", providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: "tu_fan", Name: "Agent", Input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"s","prompt":"p"}]}`)}}),
		mkEvent(string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
			SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: "r0", Agent: "s"}}),
	}
	answered := append(append([]store.Event{}, dangling...),
		mkEvent("tool_result", providers.Event{Type: providers.EventToolResult,
			ToolUse: &providers.ToolUse{ID: "tu_fan"}, Text: `{"results":[]}`}))
	noLedger := []store.Event{
		mkEvent("tool_call", providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: "tu_read", Name: "Read"}}),
	}

	tests := []struct {
		name    string
		enabled bool
		events  []store.Event
		want    bool
		wantTU  string
	}{
		{"flag off ⇒ never (even with ledger)", false, dangling, false, ""},
		{"dangling + ledger ⇒ detected", true, dangling, true, "tu_fan"},
		{"answered tool_use ⇒ not detected", true, answered, false, ""},
		{"no ledger ⇒ not detected", true, noLedger, false, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fanout, ok := detectFanoutParent(tc.enabled, tc.events)
			if ok != tc.want {
				t.Fatalf("detected = %v, want %v", ok, tc.want)
			}
			if ok && fanout.toolUseID != tc.wantTU {
				t.Errorf("toolUseID = %q, want %q", fanout.toolUseID, tc.wantTU)
			}
		})
	}
}

func TestLastTurnIsSoleToolUse(t *testing.T) {
	asst := func(blocks ...providers.ContentBlock) providers.Message {
		return providers.Message{Role: "assistant", Content: blocks}
	}
	tu := func(id string) providers.ContentBlock { return providers.ContentBlock{Type: "tool_use", ToolUseID: id} }
	txt := providers.ContentBlock{Type: "text", Text: "hello"}

	cases := []struct {
		name string
		msgs []providers.Message
		want bool
	}{
		{"sole tool_use (+ text) ⇒ true", []providers.Message{asst(txt, tu("tu_fan"))}, true},
		{"two tool_use ⇒ false (mixed turn)", []providers.Message{asst(tu("tu_fan"), tu("tu_other"))}, false},
		{"wrong id ⇒ false", []providers.Message{asst(tu("tu_other"))}, false},
		{"last turn is user ⇒ false", []providers.Message{asst(tu("tu_fan")), {Role: "user"}}, false},
		{"empty ⇒ false", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lastTurnIsSoleToolUse(tc.msgs, "tu_fan"); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseSpawnNames(t *testing.T) {
	n, names := parseSpawnNames(json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"a"},{"name":"b"}]}`))
	if n != 2 || len(names) != 2 || names[0] != "a" || names[1] != "b" {
		t.Errorf("got (%d, %v), want (2, [a b])", n, names)
	}
	if n, names := parseSpawnNames(json.RawMessage(`not json`)); n != 0 || names != nil {
		t.Errorf("malformed: got (%d, %v), want (0, nil)", n, names)
	}
	if n, _ := parseSpawnNames(nil); n != 0 {
		t.Errorf("empty: got %d, want 0", n)
	}
}

// TestResumePausedRuns_ReconcilesFanoutParent is the Phase-3 cross-instance
// case: a snapshotted fan-out PARENT (transcript ends on a dangling
// parallel_spawn tool_use + a spawn ledger) restores alongside its paused child.
// ResumePausedRuns re-dispatches both; the parent awaits the child, synthesizes
// the parallel_spawn envelope into its transcript, and runs to completion.
// Fail-before: pre-Phase-3 such a parent ended on an assistant turn → flagged
// unresumable (provider would 400 on the dangling tool_use).
func TestResumePausedRuns_ReconcilesFanoutParent(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"breeder": {Provider: "scripted", Model: "stub-model", SystemPrompt: "breed", AllowedTools: []string{"Agent"}},
			"solver":  {Provider: "scripted", Model: "stub-model", SystemPrompt: "solve", AllowedTools: []string{}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.ResumeFanout = true // Phase-3 opt-in
	prov := &scriptedProvider{
		defaultS: []providers.Event{
			{Type: providers.EventText, Text: "done"},
			{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{}},
		},
	}
	srv, _ := makeServer(t, prov, cfg)
	ctx := context.Background()

	// The child (solver): paused, resumable (transcript ends on a user turn).
	childSess, _ := srv.store.CreateSession(ctx, "", "solver", "alice")
	child, _ := srv.store.CreateRun(ctx, childSess.ID, store.RunIdentity{AgentID: "a_child", UserID: "alice", Model: "stub-model"})
	appendResumeEvent(t, srv, child.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "solve 0"}}},
	})
	if err := srv.store.SetRunPauseState(ctx, child.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	// The parent (breeder): parked mid parallel_spawn — a dangling tool_use +
	// a spawn_child_started ledger pointing at the child run.
	parentSess, _ := srv.store.CreateSession(ctx, "", "breeder", "alice")
	parent, _ := srv.store.CreateRun(ctx, parentSess.ID, store.RunIdentity{AgentID: "a_parent", UserID: "alice", Model: "stub-model"})
	appendResumeEvent(t, srv, parent.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "breed"}}},
	})
	appendResumeEvent(t, srv, parent.ID, "tool_call", providers.Event{
		Type:    providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_fan", Name: "Agent", Input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"solve 0"}]}`)},
	})
	appendResumeEvent(t, srv, parent.ID, string(providers.EventSpawnChildStarted), providers.Event{
		Type:       providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: child.ID, Agent: "solver"},
	})
	if err := srv.store.SetRunPauseState(ctx, parent.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	n, warnings := srv.ResumePausedRuns(ctx)
	if n != 2 {
		t.Fatalf("re-dispatched %d, want 2 (parent + child); warnings=%v", n, warnings)
	}

	// Poll the PARENT to completion.
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, gerr := srv.store.GetRun(ctx, parent.ID)
		if gerr != nil {
			t.Fatalf("GetRun(parent): %v", gerr)
		}
		if got.Status == store.RunCompleted {
			break
		}
		if got.Status == store.RunFailed {
			t.Fatalf("parent failed: %s", got.ErrorMsg)
		}
		if time.Now().After(deadline) {
			t.Fatalf("parent did not complete (status=%q)", got.Status)
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The synthesized parallel_spawn tool_result must be on the parent transcript.
	events, _ := srv.store.GetTranscript(ctx, parentSess.ID)
	var envText string
	for _, e := range events {
		if e.RunID != parent.ID || e.Type != "tool_result" {
			continue
		}
		var pe providers.Event
		if json.Unmarshal(e.Payload, &pe) == nil && pe.ToolUse != nil && pe.ToolUse.ID == "tu_fan" {
			envText = pe.Text
		}
	}
	if envText == "" {
		t.Fatal("no synthesized tool_result for tu_fan on the parent transcript")
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(envText), &env); err != nil {
		t.Fatalf("envelope unmarshal: %v; raw=%s", err, envText)
	}
	if len(env.Results) != 1 || !env.Results[0].Ok {
		t.Fatalf("reconciled envelope wrong: %+v", env.Results)
	}
	if !strings.Contains(env.Results[0].Output, "done") || !strings.Contains(env.Results[0].Output, "sub-agent agent_id=") {
		t.Errorf("child output not reconciled from its transcript: %q", env.Results[0].Output)
	}
}

// TestResumePausedRuns_FanoutFlagOff_NotReconciled: with LOOMCYCLE_RESUME_FANOUT
// off, a dangling-parallel_spawn parent is NOT detected as a fan-out — it takes
// the pre-Phase-3 path (ends on an assistant turn ⇒ flagged unresumable), proving
// the feature is byte-identically dormant until opt-in.
func TestResumePausedRuns_FanoutFlagOff_NotReconciled(t *testing.T) {
	cfg := &config.Config{
		Defaults: config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents: map[string]config.AgentDef{
			"breeder": {Provider: "scripted", Model: "stub-model", SystemPrompt: "breed", AllowedTools: []string{"Agent"}},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.ResumeFanout = false // dormant
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	ctx := context.Background()

	sess, _ := srv.store.CreateSession(ctx, "", "breeder", "alice")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_parent", UserID: "alice", Model: "stub-model"})
	appendResumeEvent(t, srv, run.ID, "user_input", []loop.PromptSegment{
		{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "breed"}}},
	})
	appendResumeEvent(t, srv, run.ID, "tool_call", providers.Event{
		Type:    providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_fan", Name: "Agent", Input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"x"}]}`)},
	})
	appendResumeEvent(t, srv, run.ID, string(providers.EventSpawnChildStarted), providers.Event{
		Type:       providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: "r_orphan", Agent: "solver"},
	})
	if err := srv.store.SetRunPauseState(ctx, run.ID, store.PauseStatePaused); err != nil {
		t.Fatal(err)
	}

	n, warnings := srv.ResumePausedRuns(ctx)
	if n != 0 {
		t.Errorf("re-dispatched %d, want 0 (flag off ⇒ treated as idle, not a fan-out)", n)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings = %v, want exactly 1", warnings)
	}
	got, _ := srv.store.GetRun(ctx, run.ID)
	if got.Status != store.RunFailed {
		t.Errorf("status = %q, want failed (flagged unresumable, not reconciled)", got.Status)
	}
}
