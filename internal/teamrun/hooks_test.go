package teamrun

import (
	"context"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
)

// A state's hooks ride on the ctx every run it starts is spawned from, added to
// what the walk was already carrying — for a single agent and for each member
// of a parallel fan-out and its consolidator alike.
func TestRunHandler_AStatesHooksReachEveryRunItStarts(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]hooks.Additions{}
	r := NewAgentRunner(textSpawn(func(ctx context.Context, agent string, _ Prompt, _ string) (string, error) {
		mu.Lock()
		seen[agent] = hooks.AdditionsFrom(ctx)
		mu.Unlock()
		return agent + " done", nil
	}))
	stateHooks := hooks.EventHooks{hooks.PhaseRunEnd: {{Ref: "audit"}}}
	inherited := hooks.Additions{Hooks: hooks.EventHooks{hooks.PhaseRunEnd: {{Ref: "walk-wide"}}}}
	ctx := hooks.WithAdditions(context.Background(), inherited)

	agent := teamgraph.State{ID: "write", Handler: teamgraph.Handler{Kind: teamgraph.HandlerAgent, Agent: "writer", Hooks: stateHooks}}
	if _, err := r.RunHandler(ctx, agent, &Task{Input: "go"}); err != nil {
		t.Fatal(err)
	}
	par := teamgraph.State{ID: "fan", Handler: teamgraph.Handler{Kind: teamgraph.HandlerParallel,
		Agents: []string{"a", "b"}, Consolidator: "merge", ToolHooks: hooks.ToolHooks{"WebFetch": {hooks.PhasePre: {{Ref: "gate"}}}}}}
	if _, err := r.RunHandler(ctx, par, &Task{Input: "go"}); err != nil {
		t.Fatal(err)
	}

	if got := seen["writer"].Hooks[hooks.PhaseRunEnd]; len(got) != 2 || got[0].Ref != "walk-wide" || got[1].Ref != "audit" {
		t.Fatalf("writer got %v; want what the walk carried, then the state's", got)
	}
	for _, name := range []string{"a", "b", "merge"} {
		if got := seen[name].ToolHooks["WebFetch"][hooks.PhasePre]; len(got) != 1 || got[0].Ref != "gate" {
			t.Errorf("%s got %v; want the state's tool hook", name, seen[name])
		}
	}
	// The state's hooks do not leak to the ctx the walk goes on with.
	if got := hooks.AdditionsFrom(ctx).Hooks[hooks.PhaseRunEnd]; len(got) != 1 {
		t.Fatalf("the walk's ctx changed: %v", got)
	}
}
