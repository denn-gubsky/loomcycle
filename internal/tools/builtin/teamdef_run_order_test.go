package builtin

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamgraph"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// TestTeamDefTool_Run_StepsRunInTheirOrder pins the order op=run takes its
// steps in, and where each injected effect lands among them: the key is
// looked up before the team is read; admission comes before the walk's run
// row; the walk's triggers are armed last, once nothing can refuse the walk;
// members are spawned only after that; and the triggers are disarmed before
// the run is closed.
func TestTeamDefTool_Run_StepsRunInTheirOrder(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()

	var mu sync.Mutex
	var got []string
	log := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		// A walk spawns once per state; one entry stands for the run of them.
		if n := len(got); n > 0 && got[n-1] == s {
			return
		}
		got = append(got, s)
	}
	tool.runStepHook = func(step string) { log("step:" + step) }
	tool.ExistingWalk = func(context.Context, string, bool) (*ExistingWalk, error) {
		log("existing_walk")
		return nil, nil
	}
	tool.Admit = func(c context.Context) (context.Context, error) {
		log("admit")
		return c, nil
	}
	tool.WalkRun = func(c context.Context, _ WalkRunSpec) (context.Context, string, func(WalkEnd), error) {
		log("open_run")
		return c, "r_walk1", func(WalkEnd) { log("close_run") }, nil
	}
	tool.ArmWalkTriggers = func(context.Context, teamgraph.Definition) (func(), error) {
		log("arm_triggers")
		return func() { log("disarm_triggers") }, nil
	}
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) {
		log("spawn")
		return "reviewed", nil
	})

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","idempotency_key":"k1"}`))
	if res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	want := []string{
		"step:idempotency_key", "existing_walk",
		"step:resolve_team",
		"step:admit", "admit",
		"step:open_walk", "open_run",
		"step:walk_options",
		"step:build_runner", "arm_triggers",
		"step:walk", "spawn",
		"disarm_triggers", "close_run",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("op=run order\n got %v\nwant %v", got, want)
	}
}
