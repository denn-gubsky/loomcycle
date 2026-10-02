package builtin

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/throughput"
)

// TestContextTool_SelfReportsTheCurrentModelsMeasuredSlowdown — `self` names how
// fast the model this run is on actually is, so an agent (or an operator reading
// its answer) can tell a slow model from a stuck one; it is omitted when the
// estimate is off.
func TestContextTool_SelfReportsTheCurrentModelsMeasuredSlowdown(t *testing.T) {
	tool, ctx := contextFixture(t) // resolved anthropic / claude-opus-4-test
	est := throughput.New(&config.Config{}, nil)
	for i := 0; i < 6; i++ {
		// 100 output tokens in 6 s: 3× the reference's 2 s.
		u := &providers.Usage{Provider: "anthropic", Model: "claude-opus-4-test", OutputTokens: 100,
			Timing: &providers.CallTiming{DurationMs: 6000, TTFTMs: 1000}}
		s, _ := throughput.SampleFromUsage(fmt.Sprintf("r%d", i), time.Now(), u)
		est.Observe(s)
	}
	tool.Throughput = est

	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	out := decodeResult(t, res.Text)
	tm, ok := out["timeouts"].(map[string]any)
	if !ok {
		t.Fatalf("self has no timeouts block: %v", out)
	}
	if tm["mode"] != "measure" || tm["source"] != "measured" || tm["samples"] != float64(6) {
		t.Errorf("timeouts = %v, want mode measure, 6 measured samples", tm)
	}
	if s, _ := tm["slowdown"].(float64); s < 2.99 || s > 3.01 {
		t.Errorf("slowdown = %v, want 3", tm["slowdown"])
	}

	tool.Throughput = nil
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"self"}`))
	if _, present := decodeResult(t, res.Text)["timeouts"]; present {
		t.Error("timeouts reported with no estimate")
	}
}
