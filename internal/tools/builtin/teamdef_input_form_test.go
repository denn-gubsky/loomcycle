package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/teamrun"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// formGraph is an input state whose form requires chunk_id, then one agent.
const formGraph = `{"entry":"form","states":[` +
	`{"state":"form","handler":{"kind":"input","schema":{"type":"object","required":["chunk_id"]}}},` +
	`{"state":"write","handler":{"kind":"agent","agent":"writer"}},` +
	`{"state":"done","handler":{"kind":"terminal"}}],` +
	`"transitions":[{"from":"form","to":"write","on":"success"},{"from":"write","to":"done","on":"success"}]}`

// formFixture is a TeamDef tool holding formGraph as "parts", with a run
// opener and a spawn that count what op=run started.
func formFixture(t *testing.T) (*TeamDef, context.Context, *walkRunRecorder, func() int) {
	t.Helper()
	tool, ctx, done := teamDefFixture(t)
	t.Cleanup(done)
	var mu sync.Mutex
	spawned := 0
	tool.Spawn = textSpawn(func(context.Context, string, teamrun.Prompt, string) (string, error) {
		mu.Lock()
		spawned++
		mu.Unlock()
		return "written", nil
	})
	rec := &walkRunRecorder{}
	tool.WalkRun = rec.open
	createTeam(t, tool, ctx, "parts", formGraph)
	return tool, ctx, rec, func() int { mu.Lock(); defer mu.Unlock(); return spawned }
}

// The refusal is a validation error naming the field, and it comes before the
// walk's run is opened — so it costs no row, and no member is spawned.
func TestTeamDefTool_Run_InputFormRefusedBeforeTheRunIsOpened(t *testing.T) {
	tool, ctx, rec, spawned := formFixture(t)

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"parts","input":"{\"document_id\":\"d\"}"}`))
	if !res.IsError || !strings.Contains(res.Text, `input field "chunk_id" is required`) {
		t.Fatalf("run = IsError:%v %s, want the form refused naming chunk_id", res.IsError, res.Text)
	}
	if res.Error == nil || res.Error.Category != tools.CategoryValidation {
		t.Errorf("error = %+v, want category validation", res.Error)
	}
	if opened, _ := rec.counts(); opened != 0 {
		t.Errorf("the walk's run was opened %d time(s) for a refused form", opened)
	}
	if n := spawned(); n != 0 {
		t.Errorf("spawned %d member(s) for a refused form", n)
	}

	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"parts","input":"{\"chunk_id\":\"c\"}"}`))
	if res.IsError {
		t.Fatalf("a filled-in form was refused: %s", res.Text)
	}
	if opened, _ := rec.counts(); opened != 1 || spawned() != 1 {
		t.Errorf("opened=%d spawned=%d for a filled-in form, want 1/1", opened, spawned())
	}
}

// statusBoard is a board whose chunk already records a state.
type statusBoard struct{ status string }

func (b statusBoard) GetChunkStatus(context.Context, string, string) (string, bool, error) {
	return b.status, true, nil
}
func (statusBoard) SetChunkStatus(context.Context, string, string, string) error { return nil }

// A board-bound walk resumed past the entry hands its input to a later
// state, which the entry's form does not describe — so it is not checked
// there; resumed AT the entry, it is.
func TestTeamDefTool_Run_InputFormCheckedOnlyWhenTheWalkStartsAtTheEntry(t *testing.T) {
	tool, ctx, _, spawned := formFixture(t)

	tool.Board = statusBoard{status: "write"}
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"parts","input":"rework it","board_chunk_id":"ch1"}`))
	if res.IsError {
		t.Fatalf("a walk resumed at a later state was refused by the entry's form: %s", res.Text)
	}
	if spawned() != 1 {
		t.Errorf("spawned %d, want the resumed state's one member", spawned())
	}

	tool.Board = statusBoard{status: "form"}
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"run","name":"parts","input":"rework it","board_chunk_id":"ch1"}`))
	if !res.IsError || !strings.Contains(res.Text, `input field "chunk_id" is required`) {
		t.Errorf("a walk resumed at the entry skipped its form: IsError=%v %s", res.IsError, res.Text)
	}
}
