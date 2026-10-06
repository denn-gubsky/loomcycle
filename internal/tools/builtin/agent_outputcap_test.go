package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func answersBy(answers map[string]string) SubAgentRunnerDetailed {
	return func(_ context.Context, name, _, _ string) (string, map[string]any, string, error) {
		return answers[name], nil, "r_" + name, nil
	}
}

// Each parallel_spawn row's output is capped at an equal share of a quarter of
// the parent's window, and a cut row says so; a row within its share is
// untouched.
func TestAgentParallelSpawn_RowOutputsShareAQuarterOfTheWindow(t *testing.T) {
	long, short := strings.Repeat("L", 1000), strings.Repeat("s", 50)
	a := &AgentTool{
		Run:         func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed: answersBy(map[string]string{"long": long, "short": short}),
	}
	// A 400-token window: a quarter is 400 characters, 200 per row.
	ctx := tools.WithEffectiveContextWindow(context.Background(), 400)
	res, err := a.Execute(ctx, json.RawMessage(`{"op":"parallel_spawn","spawns":[{"name":"long","prompt":"x"},{"name":"short","prompt":"y"}]}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute: %v %+v", err, res)
	}
	var env struct {
		Results []ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatal(err)
	}
	l, s := env.Results[0], env.Results[1]
	if len(l.Output) != 200 || !l.Truncated || l.RunID != "r_long" {
		t.Errorf("long row: %d chars, truncated=%v, run_id=%q; want 200, true, r_long", len(l.Output), l.Truncated, l.RunID)
	}
	if s.Output != short || s.Truncated {
		t.Errorf("short row changed: %d chars, truncated=%v", len(s.Output), s.Truncated)
	}
	if !strings.Contains(res.Text, `"truncated":true`) || strings.Count(res.Text, `"truncated"`) != 1 {
		t.Errorf("envelope should carry truncated only on the cut row: %s", res.Text)
	}
}

// A spawn's answer is capped at a quarter of the window, with a note naming
// the run that holds the full answer. With no window known nothing is cut.
func TestAgentSpawn_OutputCappedAtAQuarterOfTheWindow(t *testing.T) {
	long := strings.Repeat("L", 1000)
	a := &AgentTool{
		Run:         func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed: answersBy(map[string]string{"long": long}),
	}
	res, err := a.Execute(tools.WithEffectiveContextWindow(context.Background(), 400), json.RawMessage(`{"name":"long","prompt":"x"}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute: %v %+v", err, res)
	}
	if !strings.HasPrefix(res.Text, strings.Repeat("L", 400)+"\n\n[truncated at 400 characters") || strings.Contains(res.Text, strings.Repeat("L", 401)) ||
		!strings.Contains(res.Text, "run r_long") {
		t.Errorf("result = %q, want 400 characters and a note naming run r_long", res.Text)
	}
	res, _ = a.Execute(context.Background(), json.RawMessage(`{"name":"long","prompt":"x"}`))
	if res.Text != long {
		t.Errorf("with no window known the answer was changed (%d chars)", len(res.Text))
	}
}
