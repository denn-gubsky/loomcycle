package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// op=self reports the turns a run has used and has left, and for an
// iteration-unbounded run says so instead of a meaningless remainder.
func TestContextSelf_ReportsTheIterationBudget(t *testing.T) {
	c := &Context{}
	self := func(ctx context.Context) map[string]any {
		t.Helper()
		res, _ := c.Execute(ctx, json.RawMessage(`{"op":"self"}`))
		if res.IsError {
			t.Fatalf("self: %s", res.Text)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
			t.Fatalf("self JSON: %v", err)
		}
		return out
	}

	out := self(tools.WithIterationBudget(context.Background(), 3, 16, false))
	if out["iterations_used"] != float64(3) || out["iterations_remaining"] != float64(13) || out["iterations_unbounded"] != false {
		t.Errorf("capped run: used=%v remaining=%v unbounded=%v, want 3/13/false",
			out["iterations_used"], out["iterations_remaining"], out["iterations_unbounded"])
	}

	out = self(tools.WithIterationBudget(context.Background(), 3, 1<<20, true))
	if out["iterations_used"] != float64(3) || out["iterations_unbounded"] != true {
		t.Errorf("unbounded run: used=%v unbounded=%v, want 3/true", out["iterations_used"], out["iterations_unbounded"])
	}
	if _, has := out["iterations_remaining"]; has {
		t.Errorf("unbounded run reports a remainder: %v", out["iterations_remaining"])
	}

	out = self(context.Background())
	for _, k := range []string{"iterations_used", "iterations_remaining", "iterations_unbounded"} {
		if _, has := out[k]; has {
			t.Errorf("outside a loop iteration op=self reports %s", k)
		}
	}
}
