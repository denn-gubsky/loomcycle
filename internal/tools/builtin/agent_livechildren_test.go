package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// gatedChildren is a RunDetailed whose children stay alive until gate closes.
func gatedChildren(gate <-chan struct{}, started *atomic.Int32) SubAgentRunnerDetailed {
	return func(ctx context.Context, name, _, _ string) (string, map[string]any, string, error) {
		started.Add(1)
		select {
		case <-gate:
		case <-ctx.Done():
			return "", nil, "", ctx.Err()
		}
		return "done", nil, "r_" + name, nil
	}
}

// The live-children limit holds across separate calls of one run: with 32
// children alive (a parallel_spawn of 32, most of them queued behind its
// concurrency), a 33rd — a spawn or a parallel_spawn — is refused naming the
// number alive and the limit, and is admitted again once they have ended.
func TestAgentTool_LiveChildLimitRefusesThe33rdChildAcrossCalls(t *testing.T) {
	gate := make(chan struct{})
	var started atomic.Int32
	a := &AgentTool{
		Run:          func(context.Context, string, string, string) (string, error) { return "ok", nil },
		RunDetailed:  gatedChildren(gate, &started),
		LiveChildren: tools.NewLiveChildren(func() int { return 32 }),
	}
	ctx := tools.WithRunID(context.Background(), "r_parent")
	spawns := make([]string, 32)
	for i := range spawns {
		spawns[i] = `{"name":"w","prompt":"x"}`
	}
	first := make(chan tools.Result, 1)
	go func() {
		res, _ := a.Execute(ctx, json.RawMessage(`{"op":"parallel_spawn","spawns":[`+strings.Join(spawns, ",")+`]}`))
		first <- res
	}()
	for deadline := time.Now().Add(5 * time.Second); a.LiveChildren.Alive("r_parent") < 32; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d children admitted", a.LiveChildren.Alive("r_parent"))
		}
	}
	if n := started.Load(); n >= 32 {
		t.Fatalf("%d children running; the fixture needs most of them queued", n)
	}
	for _, in := range []string{`{"name":"w","prompt":"one more"}`, `{"op":"parallel_spawn","spawns":[{"name":"w","prompt":"one more"}]}`} {
		// Bounded: a 33rd child wrongly admitted would wait on the gate.
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		res, err := a.Execute(attempt, json.RawMessage(in))
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Text, "32 children alive") || !strings.Contains(res.Text, "at most 32") {
			t.Errorf("%s: result = %+v, want a refusal naming 32 alive and the limit 32", in, res)
		}
		if res.Error == nil || res.Error.Category != tools.CategoryBusiness {
			t.Errorf("%s: refusal category = %+v, want business", in, res.Error)
		}
	}
	// Another run's children are not this run's.
	if _, err := a.LiveChildren.Admit("r_other", 32); err != nil {
		t.Errorf("another run refused: %v", err)
	}
	close(gate)
	if res := <-first; res.IsError {
		t.Fatalf("the 32-child call failed: %s", res.Text)
	}
	if n := a.LiveChildren.Alive("r_parent"); n != 0 {
		t.Fatalf("%d children still counted after they all ended", n)
	}
	if res, _ := a.Execute(ctx, json.RawMessage(`{"name":"w","prompt":"again"}`)); res.IsError {
		t.Errorf("a spawn after the children ended was refused: %s", res.Text)
	}
}

// A parallel_spawn that would take the run past the limit is refused whole,
// before any child starts, even when some of it would fit.
func TestAgentTool_ParallelSpawnPastTheLiveLimitIsRefusedWhole(t *testing.T) {
	a := &AgentTool{
		Run: func(context.Context, string, string, string) (string, error) {
			t.Error("a child started despite the refusal")
			return "", nil
		},
		LiveChildren: tools.NewLiveChildren(func() int { return 4 }),
	}
	held, err := a.LiveChildren.Admit("r_parent", 3)
	if err != nil {
		t.Fatal(err)
	}
	defer held[0]()
	res, err := a.Execute(tools.WithRunID(context.Background(), "r_parent"),
		json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"a","prompt":"x"},{"name":"b","prompt":"y"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Text, "3 children alive") || !strings.Contains(res.Text, "2 more") {
		t.Errorf("result = %+v, want the whole call refused naming 3 alive and 2 more", res)
	}
	if n := a.LiveChildren.Alive("r_parent"); n != 3 {
		t.Errorf("a refused call changed the count to %d", n)
	}
}
