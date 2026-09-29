package teamrun

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type walkStateKey struct{}

type walkState struct {
	walkID, state string
	visit         int
}

// spawnRecord is one member spawn: the walk state its ctx carried, and the
// per-state iteration count its prompt was built with.
type spawnRecord struct {
	walkState
	ok        bool
	iteration string
}

// TestWalk_StateVisitIncreasesAcrossACapContinue: every member spawn carries the
// walk, its state and a state_visit that grows with each visit — including
// across a cap `continue`, where the per-state iteration count restarts at 1
// and so gives the same number to two different visits of one state.
func TestWalk_StateVisitIncreasesAcrossACapContinue(t *testing.T) {
	d := mustParse(t, pingPongJSON) // a↔b, max_iterations 2
	var mu sync.Mutex
	var spawns []spawnRecord
	spawn := func(ctx context.Context, _ string, p Prompt, _ string) (SpawnResult, error) {
		ws, ok := ctx.Value(walkStateKey{}).(walkState)
		mu.Lock()
		spawns = append(spawns, spawnRecord{walkState: ws, ok: ok, iteration: p.Values["team.iteration"]})
		mu.Unlock()
		return SpawnResult{Output: "ok"}, nil
	}
	r := NewAgentRunner(spawn, WithWalkContext(func(ctx context.Context, walkID, state string, visit int) context.Context {
		return context.WithValue(ctx, walkStateKey{}, walkState{walkID, state, visit})
	}))
	caps := 0
	_, err := Walk(context.Background(), d, &Task{WalkID: "r_walk"}, r,
		OnCap(func(context.Context, *ErrIterationCap) (CapDecision, error) {
			caps++
			if caps == 1 {
				return CapDecision{Action: CapContinue}, nil
			}
			return CapDecision{Action: CapAbort}, nil
		}))
	var capErr *ErrIterationCap
	if !errors.As(err, &capErr) {
		t.Fatalf("want the second cap to abort, got %v", err)
	}

	// a, b, a, b, then a again after the continue: five visits.
	wantStates := []string{"a", "b", "a", "b", "a"}
	if len(spawns) != len(wantStates) {
		t.Fatalf("%d spawns, want %d: %+v", len(spawns), len(wantStates), spawns)
	}
	for i, s := range spawns {
		if !s.ok || s.walkID != "r_walk" || s.state != wantStates[i] {
			t.Errorf("spawn %d carried %+v (ok=%v), want walk r_walk state %s", i, s.walkState, s.ok, wantStates[i])
		}
		if s.visit != i+1 {
			t.Errorf("spawn %d (state %s) carried state_visit %d, want %d", i, s.state, s.visit, i+1)
		}
	}
	// The case the iteration count gets wrong: a's first visit and its visit
	// after the continue share iteration 1, and must not share a visit.
	first, again := spawns[0], spawns[4]
	if first.iteration != "1" || again.iteration != "1" {
		t.Fatalf("fixture drifted: a's iterations are %s and %s, want 1 and 1", first.iteration, again.iteration)
	}
	if first.visit == again.visit {
		t.Errorf("two visits of a share state_visit %d", first.visit)
	}
}
