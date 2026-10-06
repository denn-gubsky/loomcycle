package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	runcancel "github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// hangingChild is a RunDetailed that never finishes on its own: it waits for
// its ctx and reports the cancellation, as a real sub-run does. It records the
// cause its ctx was cancelled with.
func hangingChild(cause *error) SubAgentRunnerDetailed {
	return func(ctx context.Context, name, _, _ string) (string, map[string]any, string, error) {
		<-ctx.Done()
		*cause = context.Cause(ctx)
		return "", nil, "r_" + name, ctx.Err()
	}
}

// A spawn whose child never ends returns at its timeout_ms, as an error that
// names the timeout and the child's run, and the child's ctx is cancelled with
// a cause its run's terminal write records as a cancellation.
func TestAgentSpawn_TimeoutReportsAndCancelsAChildThatNeverEnds(t *testing.T) {
	var cause error
	a := &AgentTool{Run: func(context.Context, string, string, string) (string, error) { return "", nil }, RunDetailed: hangingChild(&cause)}
	start := time.Now()
	res, err := a.Execute(context.Background(), json.RawMessage(`{"name":"slow","prompt":"x","timeout_ms":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("returned after %v; the bound was 100ms", took)
	}
	if !res.IsError || !strings.Contains(res.Text, "timed out") || !strings.Contains(res.Text, "r_slow") {
		t.Fatalf("result = %+v, want an error naming the timeout and the child's run", res)
	}
	if !errors.Is(cause, runcancel.ErrCancelledByAPI) {
		t.Fatalf("child ctx cause = %v, want a cancel the run records as cancelled", cause)
	}
	if r := runcancel.ReasonFromCause(cause); !strings.Contains(r, "timeout_ms=100") {
		t.Errorf("cancel reason = %q, want it to name the timeout", r)
	}
}

// In parallel_spawn the timed-out child's row says so — ok false, status
// timeout, its run_id — while a sibling that finished keeps its answer. An
// entry's own timeout_ms wins over the call's.
func TestAgentParallelSpawn_TimedOutRowReportsStatusTimeout(t *testing.T) {
	a := &AgentTool{
		Run: func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed: func(ctx context.Context, name, _, _ string) (string, map[string]any, string, error) {
			if name == "fast" {
				return "fast answer", nil, "r_fast", nil
			}
			<-ctx.Done()
			return "", nil, "r_slow", ctx.Err()
		},
	}
	res, err := a.Execute(context.Background(), json.RawMessage(
		`{"op":"parallel_spawn","timeout_ms":60000,"spawns":[{"name":"fast","prompt":"a"},{"name":"slow","prompt":"b","timeout_ms":100}]}`))
	if err != nil || res.IsError {
		t.Fatalf("Execute: %v %+v", err, res)
	}
	var env struct {
		Results []ParallelSpawnResult `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &env); err != nil {
		t.Fatal(err)
	}
	fast, slow := env.Results[0], env.Results[1]
	if !fast.Ok || fast.Status != "" || fast.Output != "fast answer" {
		t.Errorf("fast row = %+v, want its answer and no status", fast)
	}
	if slow.Ok || slow.Status != "timeout" || slow.RunID != "r_slow" || !strings.Contains(slow.Error, "timed out") {
		t.Errorf("slow row = %+v, want ok=false status=timeout run_id=r_slow", slow)
	}
}

// Time the child spends held for a review verdict is not counted: a child held
// for longer than its whole bound, then finishing quickly, keeps its answer.
func TestAgentSpawn_HeldForReviewTimeIsNotCounted(t *testing.T) {
	a := &AgentTool{
		Run: func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed: func(ctx context.Context, _, _, _ string) (string, map[string]any, string, error) {
			held := teamrun.HoldObserver(ctx)
			if held == nil {
				return "", nil, "r_c", errors.New("no hold observer on the child's ctx")
			}
			held(true)
			select {
			case <-ctx.Done():
				return "", nil, "r_c", ctx.Err()
			case <-time.After(400 * time.Millisecond): // 4x the bound, all held
			}
			held(false)
			return "approved answer", nil, "r_c", nil
		},
	}
	res, err := a.Execute(context.Background(), json.RawMessage(`{"name":"writer","prompt":"x","timeout_ms":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.Text != "approved answer" {
		t.Fatalf("result = %+v, want the answer — held time must not count", res)
	}
}

// Without timeout_ms the child is unbounded, and a hold observer inherited
// from an enclosing bound is cleared, so this child's holds cannot stop an
// outer clock.
func TestAgentSpawn_UnboundedChildClearsAnInheritedHoldObserver(t *testing.T) {
	var seen func(bool)
	a := &AgentTool{
		Run: func(context.Context, string, string, string) (string, error) { return "", nil },
		RunDetailed: func(ctx context.Context, _, _, _ string) (string, map[string]any, string, error) {
			seen = teamrun.HoldObserver(ctx)
			return "ok", nil, "r_c", nil
		},
	}
	outer := teamrun.WithHoldObserver(context.Background(), func(bool) {})
	if _, err := a.Execute(outer, json.RawMessage(`{"name":"c","prompt":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if seen != nil {
		t.Error("the unbounded child saw an inherited hold observer")
	}
}

// A timeout_ms above the operator's ceiling is refused, naming the ceiling,
// before any child starts — on spawn, on the call and on an entry.
func TestAgentSpawn_TimeoutAboveTheCeilingIsRefused(t *testing.T) {
	a := &AgentTool{
		MaxChildTimeoutMs: 1000,
		Run: func(context.Context, string, string, string) (string, error) {
			t.Error("a child started despite the refusal")
			return "", nil
		},
	}
	for _, in := range []string{
		`{"name":"c","prompt":"x","timeout_ms":5000}`,
		`{"op":"parallel_spawn","timeout_ms":5000,"spawns":[{"name":"c","prompt":"x"}]}`,
		`{"op":"parallel_spawn","spawns":[{"name":"c","prompt":"x","timeout_ms":5000}]}`,
		`{"name":"c","prompt":"x","timeout_ms":-1}`,
	} {
		res, err := a.Execute(context.Background(), json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("%s: accepted, want a refusal", in)
			continue
		}
		if !strings.Contains(in, "-1") && !strings.Contains(res.Text, "1000") {
			t.Errorf("%s: refusal %q does not name the ceiling", in, res.Text)
		}
	}
	// At the ceiling is accepted.
	a.Run = func(context.Context, string, string, string) (string, error) { return "ok", nil }
	if res, _ := a.Execute(context.Background(), json.RawMessage(`{"name":"c","prompt":"x","timeout_ms":1000}`)); res.IsError {
		t.Errorf("timeout_ms at the ceiling refused: %s", res.Text)
	}
}
