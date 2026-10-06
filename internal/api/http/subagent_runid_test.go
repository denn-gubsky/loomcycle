package http

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// parentToolResult returns the text of the tool_result the parent run
// recorded for toolUseID.
func parentToolResult(t *testing.T, st store.Store, parent store.Run, toolUseID string) string {
	t.Helper()
	events, err := st.GetTranscript(context.Background(), parent.SessionID)
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	for _, e := range events {
		if e.RunID != parent.ID || e.Type != "tool_result" {
			continue
		}
		var pe providers.Event
		if json.Unmarshal(e.Payload, &pe) == nil && pe.ToolUse != nil && pe.ToolUse.ID == toolUseID {
			return pe.Text
		}
	}
	t.Fatalf("no tool_result for %s on the parent's transcript", toolUseID)
	return ""
}

// A spawn's result names the child's run in its header, so the parent model
// can address the run it just delegated to.
func TestSubAgent_SpawnResultHeaderNamesTheChildRun(t *testing.T) {
	parent, children, st := spawnParentChildStore(t, "a_parent_runid", `{"name":"child","prompt":"hi"}`, 1)
	text := parentToolResult(t, st, parent, "tu_spawn")
	want := "[sub-agent agent_id=" + children[0].AgentID + " run_id=" + children[0].ID + "]\n"
	if !strings.HasPrefix(text, want) {
		t.Errorf("spawn result = %q, want it to start with %q", text, want)
	}
}

// Each parallel_spawn row carries its child's run_id, matching the child runs
// that were actually created.
func TestSubAgent_ParallelSpawnRowsNameTheirChildRuns(t *testing.T) {
	parent, children, st := spawnParentChildStore(t, "a_parent_fan_runid",
		`{"op":"parallel_spawn","spawns":[{"name":"child","prompt":"one"},{"name":"child","prompt":"two"}]}`, 2)
	var env struct {
		Results []builtin.ParallelSpawnResult `json:"results"`
	}
	text := parentToolResult(t, st, parent, "tu_spawn")
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatalf("envelope: %v; raw=%s", err, text)
	}
	runs := map[string]bool{}
	for _, c := range children {
		runs[c.ID] = true
	}
	for _, r := range env.Results {
		if !runs[r.RunID] {
			t.Errorf("row %d run_id = %q, not one of the child runs %v; raw=%s", r.Index, r.RunID, runs, text)
		}
		if !strings.Contains(r.Output, "run_id="+r.RunID+"]") {
			t.Errorf("row %d output header does not name its run: %q", r.Index, r.Output)
		}
	}
}

// A restored fan-out parent's synthesized envelope has the live envelope's
// shape: a child whose result is in the spawn ledger keeps the run_id the
// ledger recorded.
func TestResumeFanout_LedgerResultRowKeepsTheChildRunID(t *testing.T) {
	cfg := &config.Config{
		Defaults:    config.Defaults{Provider: "scripted", Model: "stub-model"},
		Agents:      map[string]config.AgentDef{"breeder": {Provider: "scripted", Model: "stub-model", Tools: []string{"Agent"}}},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 8, MaxQueueDepth: 8, QueueTimeoutMS: 1000},
	}
	cfg.Env.AuthToken = ""
	cfg.Env.ResumeFanout = true
	srv, _ := makeServer(t, &scriptedProvider{}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	payload, _ := json.Marshal(providers.Event{
		Type: providers.EventSpawnChildResult,
		SpawnChild: &providers.SpawnChildEventInfo{
			ToolUseID: "tu_fan", Index: 0, RunID: "r_child_done", Agent: "solver", Ok: true,
			Output: "[sub-agent agent_id=a_c run_id=r_child_done]\nanswer",
		},
	})
	events := []store.Event{{Type: string(providers.EventSpawnChildResult), Payload: payload}}
	msg, err := srv.reconcileFanoutParent(ctx, store.Run{ID: "r_parent"}, events,
		fanoutParkInfo{toolUseID: "tu_fan", input: json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"solver","prompt":"x"}]}`)},
		func(providers.Event) {})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var env struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal([]byte(msg.Content[0].Text), &env); err != nil {
		t.Fatalf("envelope: %v", err)
	}
	if got := env.Results[0]["run_id"]; got != "r_child_done" {
		t.Errorf("reconciled ledger row run_id = %v, want r_child_done; raw=%s", got, msg.Content[0].Text)
	}
}
