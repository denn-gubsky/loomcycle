package loop

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

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
		`loomcycle_model_call_duration_seconds_count{model="model-a",provider="metrics-a"} 1`,
		`loomcycle_model_call_duration_seconds_count{model="model-b",provider="metrics-b"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}
