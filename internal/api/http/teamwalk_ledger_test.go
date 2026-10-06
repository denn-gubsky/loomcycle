package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// teamWalkLedgerRow is the ledger row a lead's poll-mode TeamDef run records.
func teamWalkLedgerRow(toolUseID, runID string) store.Event {
	return mkEvent(string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
		SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: toolUseID, Index: 0, RunID: runID, Agent: "team:rev",
			Mode: "poll", Kind: "team", Team: "rev", DefID: "tdf_1"}})
}

// A poll-mode walk's ledger row is never taken for a parked fan-out — not
// alone, under a TeamDef call snapshotted before its answer was recorded, and
// not beside a real parked parallel_spawn, which is still the one found.
func TestDetectFanoutParent_SkipsAPollModeWalkRow(t *testing.T) {
	teamCall := mkEvent("tool_call", providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_team", Name: "TeamDef", Input: json.RawMessage(`{"op":"run","name":"rev","mode":"poll"}`)}})
	if _, ok := detectFanoutParent(true, []store.Event{teamCall, teamWalkLedgerRow("tu_team", "r_walk")}); ok {
		t.Error("a poll-mode walk's ledger row was taken for a parked fan-out")
	}
	fan := []store.Event{
		mkEvent("tool_call", providers.Event{Type: providers.EventToolCall,
			ToolUse: &providers.ToolUse{ID: "tu_fan", Name: "Agent", Input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"s","prompt":"p"}]}`)}}),
		mkEvent(string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
			SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: "r0", Agent: "s"}}),
	}
	got, ok := detectFanoutParent(true, append([]store.Event{teamCall, teamWalkLedgerRow("tu_team", "r_walk")}, fan...))
	if !ok || got.toolUseID != "tu_fan" {
		t.Errorf("detected %+v %v beside a walk row, want the parked tu_fan", got, ok)
	}
}

// The reconcile of a parked fan-out reads only its own call's ledger: a
// poll-mode walk row on the same transcript, at the same index, does not
// replace or add a child.
func TestReconcileFanoutParent_IgnoresAPollModeWalkRow(t *testing.T) {
	srv, _ := makeServer(t, &stubProvider{}, makeBaseConfig())
	ctx := context.Background()
	sess, _ := srv.store.CreateSession(ctx, "", "lead", "alice")
	run, _ := srv.store.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_lead", UserID: "alice", Model: "stub-model"})
	input := json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"s","prompt":"p"}]}`)
	events := []store.Event{
		teamWalkLedgerRow("tu_team", "r_walk"),
		mkEvent("tool_call", providers.Event{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_fan", Name: "Agent", Input: input}}),
		mkEvent(string(providers.EventSpawnChildStarted), providers.Event{Type: providers.EventSpawnChildStarted,
			SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: "r_child", Agent: "s"}}),
		mkEvent(string(providers.EventSpawnChildResult), providers.Event{Type: providers.EventSpawnChildResult,
			SpawnChild: &providers.SpawnChildEventInfo{ToolUseID: "tu_fan", Index: 0, RunID: "r_child", Agent: "s", Ok: true, Output: "done"}}),
	}
	msg, err := srv.reconcileFanoutParent(ctx, run, events, fanoutParkInfo{toolUseID: "tu_fan", input: input}, func(providers.Event) {})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(msg.Content[0].Text), &env); err != nil {
		t.Fatal(err)
	}
	want := []builtin.ParallelSpawnResult{{Index: 0, Agent: "s", Ok: true, Output: "done", RunID: "r_child"}}
	if !reflect.DeepEqual(env.Results, want) {
		t.Errorf("reconciled %+v, want only the fan-out's own child %+v", env.Results, want)
	}
}

// A conversation rebuilt from a transcript is the same with a poll-mode
// walk's ledger row on it as without.
func TestReplayTranscript_SkipsAPollModeWalkRow(t *testing.T) {
	user := mkEvent("user_input", []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "go"}}}})
	call := mkEvent("tool_call", providers.Event{Type: providers.EventToolCall,
		ToolUse: &providers.ToolUse{ID: "tu_team", Name: "TeamDef", Input: json.RawMessage(`{"op":"run","name":"rev","mode":"poll"}`)}})
	result := mkEvent("tool_result", providers.Event{Type: providers.EventToolResult,
		ToolUse: &providers.ToolUse{ID: "tu_team"}, Text: `{"run_id":"r_walk","state":"running"}`})
	text := mkEvent("text", providers.Event{Type: providers.EventText, Text: "started"})
	without := replayTranscript([]store.Event{user, call, result, text})
	with := replayTranscript([]store.Event{user, call, teamWalkLedgerRow("tu_team", "r_walk"), result, text})
	if len(without) == 0 || !reflect.DeepEqual(with, without) {
		t.Errorf("replay with the walk row = %+v\nwithout = %+v", with, without)
	}
}

// End to end: a lead's poll-mode walk is recorded on the lead's own run as a
// spawn_child_started row naming the TeamDef call, the walk's run, mode poll,
// kind team and the team — persisted, and not sent on the lead's stream.
func TestTeamWalkPoll_TheLeadsTranscriptRecordsThePollModeWalk(t *testing.T) {
	prov := &scriptedParent{release: make(chan struct{})}
	prov.script = []func(providers.Request) []providers.Event{
		fixed(toolCallEv("tu_1", "TeamDef", `{"op":"run","name":"rev","input":"a","mode":"poll"}`)),
		func(req providers.Request) []providers.Event {
			return toolCallEv("tu_2", "TeamDef", `{"op":"cancel","run_ids":["`+runIDIn(lastToolText(req))+`"]}`)
		},
		fixed(answer("done")),
	}
	srv, st := newPollWalkServer(t, prov)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	defer cancelAllRuns(srv)
	defer prov.unblock()
	var stream string
	select {
	case stream = <-startLead(t, ts):
	case <-time.After(30 * time.Second):
		t.Fatal("the lead never finished")
	}
	if strings.Contains(stream, "spawn_child_started") {
		t.Error("the ledger row reached the lead's stream")
	}
	walkID := runIDIn(lastToolText(prov.leadCalls()[1]))
	walk, err := st.GetRun(context.Background(), walkID)
	if err != nil || walk.ParentRunID == "" {
		t.Fatalf("walk run %+v, %v", walk, err)
	}
	events, err := st.GetRunEventsSince(context.Background(), walk.ParentRunID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var rows []providers.SpawnChildEventInfo
	for _, e := range events {
		if e.Type != string(providers.EventSpawnChildStarted) {
			continue
		}
		var pe providers.Event
		if err := json.Unmarshal(e.Payload, &pe); err != nil || pe.SpawnChild == nil {
			t.Fatalf("ledger row %s: %v", e.Payload, err)
		}
		rows = append(rows, *pe.SpawnChild)
	}
	if len(rows) != 1 {
		t.Fatalf("the lead's run has %d ledger rows, want 1: %+v", len(rows), rows)
	}
	if r := rows[0]; r.ToolUseID != "tu_1" || r.RunID != walkID || r.Mode != "poll" || r.Kind != "team" ||
		r.Team != "rev" || r.Agent != "team:rev" || !strings.HasPrefix(r.DefID, "tdf_") {
		t.Errorf("ledger row = %+v", r)
	}
}
