package teamrun

import (
	"context"
	"fmt"
	"testing"
)

func TestAgentRunner_WalksLinearTeamViaSpawn(t *testing.T) {
	d := mustParse(t, linearJSON)

	// Fake spawn: echoes which agent ran + the input, so we can assert the
	// output threads state to state.
	var spawned []string
	spawn := func(_ context.Context, agent, input, defID string) (string, error) {
		spawned = append(spawned, agent)
		return fmt.Sprintf("%s(%s)", agent, input), nil
	}

	task := &Task{Input: "go"}
	trace, err := Walk(context.Background(), d, task, NewAgentRunner(spawn))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(spawned) != 2 || spawned[0] != "agent-a" || spawned[1] != "agent-b" {
		t.Errorf("spawned %v, want [agent-a agent-b]", spawned)
	}
	// b's input is a's output: agent-a(go) → agent-b(agent-a(go)).
	if task.Input != "agent-b(agent-a(go))" {
		t.Errorf("final input = %q, want agent-b(agent-a(go))", task.Input)
	}
	if trace[1].Edge != "success" {
		t.Errorf("linear agent handler should advance on success, got %q", trace[1].Edge)
	}
}

func TestAgentRunner_SpawnErrorStopsWalk(t *testing.T) {
	d := mustParse(t, linearJSON)
	spawn := func(_ context.Context, agent, input, defID string) (string, error) {
		if agent == "agent-b" {
			return "", fmt.Errorf("boom")
		}
		return "ok", nil
	}
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	if err == nil || !contains(err.Error(), "boom") {
		t.Fatalf("spawn error should abort the walk, got %v", err)
	}
}

func TestAgentRunner_ParallelHandlerNotYetSupported(t *testing.T) {
	d := mustParse(t, `{
	  "entry":"fan",
	  "states":[
	    {"state":"fan","handler":{"kind":"parallel","agents":["x","y"],"consolidator":"c"}},
	    {"state":"end","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[{"from":"fan","to":"end","on":"success"}]}`)
	called := false
	spawn := func(_ context.Context, agent, input, defID string) (string, error) { called = true; return "", nil }
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	if err == nil || !contains(err.Error(), "Phase 3") {
		t.Fatalf("parallel handler should be a Phase-3 not-supported error, got %v", err)
	}
	if called {
		t.Errorf("spawn must not be called for an unsupported handler")
	}
}

func TestAgentRunner_ConsolidatorOnAgentNotYetSupported(t *testing.T) {
	// An agent handler with a consolidator = consolidator-driven edge selection
	// (pushback) — Phase 3. Must fail loudly, not silently take success.
	d := mustParse(t, `{
	  "entry":"impl",
	  "states":[
	    {"state":"impl","handler":{"kind":"agent","agent":"coder","consolidator":"judge"}},
	    {"state":"impl2","handler":{"kind":"agent","agent":"coder2"}},
	    {"state":"done","handler":{"kind":"terminal"}}
	  ],
	  "transitions":[
	    {"from":"impl","to":"impl2","on":"success"},
	    {"from":"impl2","to":"done","on":"success"}
	  ]}`)
	spawn := func(_ context.Context, agent, input, defID string) (string, error) { return "", nil }
	_, err := Walk(context.Background(), d, &Task{}, NewAgentRunner(spawn))
	if err == nil || !contains(err.Error(), "consolidator") {
		t.Fatalf("agent-with-consolidator should be a Phase-3 error, got %v", err)
	}
}
