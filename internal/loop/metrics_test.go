package loop

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func renderMetrics(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := lcotel.WritePrometheus(context.Background(), &b, lcotel.PromOptions{}); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	return b.String()
}

func installMetrics(t *testing.T) {
	t.Helper()
	restore, err := lcotel.SetMetricsForTest(false, 0)
	if err != nil {
		t.Fatalf("SetMetricsForTest: %v", err)
	}
	t.Cleanup(restore)
}

// A run that moves off a failing provider counts the move from the pair it
// left to the pair it moved to, and each model call it made is timed — with
// tracing off, as the default deployment runs.
func TestFallback_CountsTheMoveAndTimesEachModelCall(t *testing.T) {
	installMetrics(t)
	failing := &tieredProvider{id: "metrics-a", errors: []error{fmt.Errorf("metrics-a 429: rate limit exceeded")}}
	healthy := &tieredProvider{id: "metrics-b", responses: [][]providers.Event{successResponse()}}
	_, err := Run(context.Background(), RunOptions{
		Provider:       failing,
		Model:          "model-a",
		MaxIterations:  5,
		Segments:       []PromptSegment{{Role: "user", Content: []PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
		FallbackPolicy: FallbackPolicy{Enabled: true, MaxAttempts: 3, UserTierName: "medium"},
		ReResolve: func(context.Context, string, string, error) (providers.Provider, string, string, error) {
			return healthy, "model-b", "", nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := renderMetrics(t)
	for _, want := range []string{
		`loomcycle_provider_fallbacks_total{from="metrics-a/model-a",to="metrics-b/model-b"} 1`,
		`loomcycle_model_call_duration_seconds_count{model="model-a",provider="metrics-a",purpose="turn"} 1`,
		`loomcycle_model_call_duration_seconds_count{model="model-b",provider="metrics-b",purpose="turn"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}

// With tracing off — no span records — the TTFT metric still takes the
// call-lane wait out, as the traced span does.
func TestModelCallMetric_TTFTExcludesTheLaneWaitWithTracingOff(t *testing.T) {
	installMetrics(t)
	now := time.Unix(1000, 0)
	timer := providers.StartCallTimer(func() time.Time { return now })
	_, span := lcotel.RecordModelCall(context.Background(), lcotel.ModelCallAttrs{Provider: "lw-p", Model: "lw-m", Purpose: "turn"})
	if span.IsRecording() {
		t.Fatal("the fixture is wrong: the span records")
	}
	mc := &ModelCall{span: span, runCtx: context.Background(), turnCtx: context.Background(), timer: timer}
	now = now.Add(300 * time.Millisecond) // 120ms in the lane, 180ms to the first token
	timer.Observe(providers.Event{Type: providers.EventText, Text: "x"})
	mc.Observe(providers.Event{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{LaneWaitMs: 120}})
	want := `loomcycle_model_ttft_seconds_sum{model="lw-m",provider="lw-p",purpose="turn"} 0.18`
	if out := renderMetrics(t); !strings.Contains(out, want) {
		t.Errorf("metrics lack %q:\n%s", want, out)
	}
}
