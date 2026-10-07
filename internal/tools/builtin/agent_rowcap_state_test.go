package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// bigState is a structured state whose JSON is far larger than any share used
// below; smallState fits every share whole.
var (
	bigState   = map[string]any{"notes": strings.Repeat("n", 10_000)}
	smallState = map[string]any{"k": "v"}
)

// rowOverhead bounds what one row costs besides its free text and state: keys,
// ids, flags. Generous, so the bound asserts the share, not the exact framing.
const rowOverhead = 200

// A parallel_spawn envelope stays within its quarter of the window when a
// child hands back a huge state or error: an oversized state is left out whole
// (never cut into invalid JSON) and flagged with its size, an error is cut,
// and a state that fits is kept intact.
func TestAgentParallelSpawn_RowStateAndErrorStayWithinTheShare(t *testing.T) {
	a := &AgentTool{
		Run: func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed: func(_ context.Context, name, _, _ string) (string, map[string]any, string, error) {
			switch name {
			case "big":
				return "answer", bigState, "r_big", nil
			case "small":
				return "answer", smallState, "r_small", nil
			}
			return "", nil, "r_" + name, nil
		},
	}
	ctx := tools.WithEffectiveContextWindow(context.Background(), 600) // 200 characters per row
	res, err := a.Execute(ctx, json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"big","prompt":"x"},{"name":"small","prompt":"y"},{"name":"plain","prompt":"z"}]}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute: %v %+v", err, res)
	}
	if len(res.Text) > 600+3*rowOverhead {
		t.Errorf("envelope is %d bytes, over its quarter of 600 plus framing", len(res.Text))
	}
	var env struct {
		Results []ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatalf("envelope does not parse: %v", err)
	}
	big, small := env.Results[0], env.Results[1]
	wantBytes := jsonSize(bigState)
	if big.State != nil || !big.StateOmitted || big.StateBytes != wantBytes || !big.Truncated || big.Output != "answer" || big.RunID != "r_big" {
		t.Errorf("big row = %+v; want state left out (state_omitted, state_bytes=%d, truncated), its output and run_id kept", big, wantBytes)
	}
	if small.State["k"] != "v" || small.StateOmitted || small.Truncated {
		t.Errorf("small row = %+v; want its state kept whole and nothing cut", small)
	}

	// An error larger than the share is cut to it.
	rows := []ParallelSpawnResult{{Index: 0, Error: strings.Repeat("e", 8_000)}, {Index: 1, Output: "ok"}}
	capRowOutputs(rows, 400)
	if len(rows[0].Error) != 200 || !rows[0].Truncated || rows[1].Truncated {
		t.Errorf("error row: %d chars, truncated=%v; other row truncated=%v; want 200, true, false",
			len(rows[0].Error), rows[0].Truncated, rows[1].Truncated)
	}
}

// A spawn's state counts against the same quarter as its text: one that fits
// is kept and the text gets the rest; one that does not is left out with a
// note naming its size and the run that holds it.
func TestAgentSpawn_StateCountsAgainstTheQuarter(t *testing.T) {
	answer := func(state map[string]any) *AgentTool {
		return &AgentTool{
			Run: func(context.Context, string, string, string) (string, error) { return "", nil },
			RunDetailed: func(context.Context, string, string, string) (string, map[string]any, string, error) {
				return strings.Repeat("L", 1000), state, "r_w", nil
			},
		}
	}
	ctx := tools.WithEffectiveContextWindow(context.Background(), 400)

	res, err := answer(bigState).Execute(ctx, json.RawMessage(`{"name":"w","prompt":"x"}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute: %v %+v", err, res)
	}
	if strings.Contains(res.Text, "nnnn") || !strings.Contains(res.Text, "final state omitted") || !strings.Contains(res.Text, "run r_w") {
		t.Errorf("oversized state not left out with a note: %q", res.Text)
	}
	if strings.Contains(res.Text, strings.Repeat("L", 401)) || len(res.Text) > 400+2*rowOverhead {
		t.Errorf("result is %d bytes, over its quarter of 400 plus its two notes", len(res.Text))
	}

	res, _ = answer(smallState).Execute(ctx, json.RawMessage(`{"name":"w","prompt":"x"}`))
	if !strings.Contains(res.Text, "Final state") || !strings.Contains(res.Text, `"k": "v"`) || strings.Contains(res.Text, strings.Repeat("L", 400)) {
		t.Errorf("a state that fits must be kept and the text cut to what is left: %q", res.Text)
	}
}

// Agent poll rows are bounded the same way: structured state left out whole
// when it does not fit, the error cut, a fitting state kept.
func TestCapPollRows_StructuredAndErrorStayWithinTheShare(t *testing.T) {
	rows := []pollRow{
		{ChildRunID: "r1", State: tools.ChildCompleted, Output: "answer", Structured: bigState},
		{ChildRunID: "r2", State: tools.ChildFailed, Error: strings.Repeat("e", 8_000)},
		{ChildRunID: "r3", State: tools.ChildCompleted, Output: "answer", Structured: smallState},
	}
	capPollRows(rows, 600)
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > 600+3*rowOverhead {
		t.Errorf("rows are %d bytes, over their quarter of 600 plus framing", len(body))
	}
	if r := rows[0]; r.Structured != nil || !r.StructuredOmitted || r.StructuredBytes != jsonSize(bigState) || !r.Truncated || r.Output != "answer" {
		t.Errorf("row 1 = %+v; want structured left out and flagged with its size, output kept", r)
	}
	if r := rows[1]; len(r.Error) != 200 || !r.Truncated {
		t.Errorf("row 2: error %d chars, truncated=%v; want 200, true", len(r.Error), r.Truncated)
	}
	if r := rows[2]; r.Structured["k"] != "v" || r.StructuredOmitted || r.Truncated {
		t.Errorf("row 3 = %+v; want its structured state kept whole", r)
	}
}

// A walk's error is part of what its row may put in the caller's context.
func TestCapWalkRows_ErrorStaysWithinTheShare(t *testing.T) {
	rows := []map[string]any{
		{"run_id": "w1", "state": tools.ChildFailed, "error": strings.Repeat("e", 8_000)},
		{"run_id": "w2", "state": tools.ChildCompleted, "final_output": "done"},
	}
	capWalkRows(rows, 400)
	if e, _ := rows[0]["error"].(string); len(e) != 200 || rows[0]["truncated"] != true {
		t.Errorf("walk 1: error %d chars, truncated=%v; want 200, true", len(e), rows[0]["truncated"])
	}
	if rows[1]["final_output"] != "done" || rows[1]["truncated"] != nil {
		t.Errorf("walk 2 changed: %+v", rows[1])
	}
}
