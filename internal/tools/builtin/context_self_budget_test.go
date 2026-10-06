package builtin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// op=self reports a time-budgeted run's budget, the active time it has used,
// the time it has waited and what is left; every other run reports none.
func TestContextSelf_ReportsTheRunBudget(t *testing.T) {
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

	// A clock started 2s ago, carrying 1.5s of waiting from before a resume,
	// against a 10s budget: about 2s used, 1.5s waited.
	clock := providers.NewRunClock(time.Now().Add(-2*time.Second), providers.RunClockState{Waited: 1500 * time.Millisecond})
	clock.SetBudget(10 * time.Second)
	b, _ := self(providers.WithRunClock(context.Background(), clock))["run_budget"].(map[string]any)
	if b == nil {
		t.Fatal("a budgeted run reports no run_budget")
	}
	budget, used, waited, left := b["budget_ms"].(float64), b["used_ms"].(float64), b["waited_ms"].(float64), b["remaining_ms"].(float64)
	if budget != 10000 || waited != 1500 {
		t.Errorf("budget_ms=%v waited_ms=%v, want 10000 and 1500", budget, waited)
	}
	if used < 2000 || used > 2500 {
		t.Errorf("used_ms=%v, want about 2000 (the clock's own 2s, nothing of the carried wait)", used)
	}
	if d := budget - used - left; d < 0 || d > 1 {
		t.Errorf("remaining_ms=%v does not equal budget minus used (%v)", left, budget-used)
	}

	// No clock (a model-driven run), or a clock whose provider has not yet
	// published a budget: nothing to plan against.
	if _, has := self(context.Background())["run_budget"]; has {
		t.Error("a run without a clock reports run_budget")
	}
	unpublished := providers.NewRunClock(time.Now(), providers.RunClockState{})
	if _, has := self(providers.WithRunClock(context.Background(), unpublished))["run_budget"]; has {
		t.Error("a clock with no budget yet reports run_budget")
	}
}
