package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// statefulAgentTool builds an AgentTool whose children return a fixed Σ (as a
// stateful RFC CR L2 sub-agent would).
func statefulAgentTool(state map[string]any) *AgentTool {
	a := &AgentTool{
		RunDetailed: func(_ context.Context, name, _, _ string) (string, map[string]any, string, error) {
			return "answer-" + name, state, "run-" + name, nil
		},
	}
	a.Run = func(ctx context.Context, name, prompt, defID string) (string, error) {
		out, _, _, err := a.RunDetailed(ctx, name, prompt, defID)
		return out, err
	}
	return a
}

// RFC CR D5: a stateful child's final Σ is folded into the single-spawn
// tool_result, so the parent gets the structured hand-off, not just prose.
func TestAgent_Spawn_CarriesChildState(t *testing.T) {
	a := statefulAgentTool(map[string]any{"count": 2, "done": true})
	res, err := a.Execute(context.Background(), json.RawMessage(`{"op":"spawn","name":"worker","prompt":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("error: %s", res.Text)
	}
	if !strings.Contains(res.Text, "answer-worker") {
		t.Errorf("child text missing: %q", res.Text)
	}
	if !strings.Contains(res.Text, "Final state") || !strings.Contains(res.Text, "count") {
		t.Errorf("child Σ not folded into the result: %q", res.Text)
	}
}

// A non-stateful child (nil Σ) → result unchanged, no state block.
func TestAgent_Spawn_NoStateNoBlock(t *testing.T) {
	a := statefulAgentTool(nil)
	res, err := a.Execute(context.Background(), json.RawMessage(`{"op":"spawn","name":"w","prompt":"go"}`))
	if err != nil || res.IsError {
		t.Fatalf("spawn: err=%v text=%s", err, res.Text)
	}
	if strings.Contains(res.Text, "Final state") {
		t.Errorf("a non-stateful child must not get a Σ block: %q", res.Text)
	}
}

// parallel_spawn: each stateful child's Σ appears as a structured `state` field
// in the envelope, so a fan-out parent gets N structured results.
func TestAgent_ParallelSpawn_CarriesChildState(t *testing.T) {
	a := statefulAgentTool(map[string]any{"k": "v"})
	res, err := a.Execute(context.Background(), json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"a","prompt":"x"},{"name":"b","prompt":"y"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("error: %s", res.Text)
	}
	var env struct {
		Results []ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatalf("envelope: %v (%s)", err, res.Text)
	}
	if len(env.Results) != 2 {
		t.Fatalf("want 2 results, got %d", len(env.Results))
	}
	for _, r := range env.Results {
		if r.State == nil || r.State["k"] != "v" {
			t.Errorf("child %q missing Σ in the envelope: %+v", r.Agent, r)
		}
	}
	if !strings.Contains(res.Text, `"state"`) {
		t.Errorf("envelope JSON missing the state field: %s", res.Text)
	}
}

func TestWithSubAgentState(t *testing.T) {
	if got := withSubAgentState("hi", nil); got != "hi" {
		t.Errorf("nil state must be a no-op, got %q", got)
	}
	got := withSubAgentState("hi", map[string]any{"a": 1})
	if !strings.Contains(got, "hi") || !strings.Contains(got, "Final state") || !strings.Contains(got, `"a"`) {
		t.Errorf("state not appended: %q", got)
	}
	if got := withSubAgentState("", map[string]any{"a": 1}); !strings.HasPrefix(got, "Final state") {
		t.Errorf("empty-text case should lead with the state block: %q", got)
	}
}
