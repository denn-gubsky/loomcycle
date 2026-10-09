package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestTeamDefTool_Run_ALostRaceOnTheKeyIsAnsweredWithTheWinner: two starts
// with one key can both find the key free. The run store's unique index lets
// one create its run; the other must not fail, and must not walk — it is
// answered with the walk that won.
func TestTeamDefTool_Run_ALostRaceOnTheKeyIsAnsweredWithTheWinner(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	var lookups []bool
	tool.ExistingWalk = func(_ context.Context, key string, lostRace bool) (*ExistingWalk, error) {
		lookups = append(lookups, lostRace)
		if !lostRace {
			return nil, nil // the key looked free when this start checked
		}
		return &ExistingWalk{RunID: "r_winner", Name: "triage", DefID: "tdf_1", Status: "running"}, nil
	}
	tool.WalkRun = func(ctx context.Context, _ WalkRunSpec) (context.Context, string, func(WalkEnd), error) {
		return ctx, "", func(WalkEnd) {}, ErrWalkKeyHeld
	}

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","idempotency_key":"k1"}`))
	if res.IsError {
		t.Fatalf("the start that lost the race failed: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	if out["run_id"] != "r_winner" || out["deduplicated"] != true || out["status"] != "running" || out["steps"] != nil {
		t.Errorf("answer = %v, want the winning walk, deduplicated, with no steps", out)
	}
	if len(lookups) != 2 || lookups[0] || !lookups[1] {
		t.Errorf("lookups (lostRace) = %v, want one before the start and one after losing", lookups)
	}
}

// The key reaches whatever opens the walk's run, and a server that cannot
// hold one refuses it: a key that is accepted and not held would let a retry
// start a second walk.
func TestTeamDefTool_Run_TheKeyIsHeldOrRefused(t *testing.T) {
	tool, ctx, _, _, done := breakFixture(t)
	defer done()
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","idempotency_key":"k1"}`))
	if !res.IsError || !strings.Contains(res.Text, "idempotency_key requires run tracking") {
		t.Fatalf("with nothing to hold the key: %s", res.Text)
	}
	if opened, _ := rec.counts(); opened != 0 {
		t.Fatalf("a walk was opened for a key nothing holds")
	}

	tool.ExistingWalk = func(context.Context, string, bool) (*ExistingWalk, error) { return nil, nil }
	if res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","input":"x","idempotency_key":"k1"}`)); res.IsError {
		t.Fatalf("run: %s", res.Text)
	}
	if rec.spec.IdempotencyKey != "k1" {
		t.Errorf("the walk's run was opened with key %q, want k1", rec.spec.IdempotencyKey)
	}
	// Poll mode files the walk in the calling run's table; a walk another
	// call started is not in it.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"triage","mode":"poll","idempotency_key":"k1"}`))
	if !res.IsError || !strings.Contains(res.Text, "does not apply to mode poll") {
		t.Errorf("mode poll with a key: %s", res.Text)
	}
}
