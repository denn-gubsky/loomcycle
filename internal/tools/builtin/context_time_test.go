package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// RFC S / F34 — Context op=time gives an agent a clock. These tests
// fail on main (op=time is an unknown op there).

func TestContextTool_TimeWithRunMeta(t *testing.T) {
	tool := &Context{}
	started := time.Now().Add(-5 * time.Second)
	ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{StartedAt: started})

	before := time.Now().UnixMilli()
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"time"}`))
	after := time.Now().UnixMilli()
	if res.IsError {
		t.Fatalf("time: %s", res.Text)
	}
	out := decodeResult(t, res.Text)

	// now_rfc3339 parses as RFC3339Nano.
	nowStr, _ := out["now_rfc3339"].(string)
	if _, err := time.Parse(time.RFC3339Nano, nowStr); err != nil {
		t.Errorf("now_rfc3339 %q does not parse as RFC3339Nano: %v", nowStr, err)
	}

	// unix_ms is within the call window.
	unixMs, ok := out["unix_ms"].(float64)
	if !ok {
		t.Fatalf("unix_ms missing or not a number: %v", out["unix_ms"])
	}
	if int64(unixMs) < before || int64(unixMs) > after {
		t.Errorf("unix_ms %d not in [%d, %d]", int64(unixMs), before, after)
	}

	// run_started_at parses + matches the anchor.
	rsa, _ := out["run_started_at"].(string)
	parsed, err := time.Parse(time.RFC3339Nano, rsa)
	if err != nil {
		t.Errorf("run_started_at %q does not parse: %v", rsa, err)
	} else if d := parsed.Sub(started); d > time.Millisecond || d < -time.Millisecond {
		t.Errorf("run_started_at %v drifted from anchor %v by %v", parsed, started, d)
	}

	// elapsed_ms ≈ 5000; wide tolerance for CI scheduling jitter.
	elapsed, ok := out["elapsed_ms"].(float64)
	if !ok {
		t.Fatalf("elapsed_ms missing or not a number: %v", out["elapsed_ms"])
	}
	if elapsed < 4000 || elapsed > 8000 {
		t.Errorf("elapsed_ms = %v, want ~5000 (±tol)", elapsed)
	}
}

func TestContextTool_TimeNoRunMetaOmitsElapsed(t *testing.T) {
	tool := &Context{}
	res, _ := tool.Execute(context.Background(), json.RawMessage(`{"op":"time"}`))
	if res.IsError {
		t.Fatalf("time: %s", res.Text)
	}
	out := decodeResult(t, res.Text)
	// now_rfc3339 + unix_ms always present.
	if _, ok := out["unix_ms"].(float64); !ok {
		t.Errorf("unix_ms missing without RunMeta — should always be present")
	}
	// run_started_at + elapsed_ms OMITTED (not fabricated) when no anchor.
	if _, ok := out["run_started_at"]; ok {
		t.Errorf("run_started_at present without RunMeta — should be omitted, not fabricated")
	}
	if _, ok := out["elapsed_ms"]; ok {
		t.Errorf("elapsed_ms present without RunMeta — should be omitted, not fabricated")
	}
}

func TestContextTool_TimeUnstampedRunMetaOmitsElapsed(t *testing.T) {
	// RunMeta present but StartedAt zero (test/non-loop caller) → omit,
	// don't fabricate an epoch.
	tool := &Context{}
	ctx := providers.WithRunMeta(context.Background(), providers.RunMeta{AgentName: "x"})
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"time"}`))
	out := decodeResult(t, res.Text)
	if _, ok := out["run_started_at"]; ok {
		t.Errorf("run_started_at present for zero StartedAt — should be omitted")
	}
	if _, ok := out["elapsed_ms"]; ok {
		t.Errorf("elapsed_ms present for zero StartedAt — should be omitted")
	}
}

func TestContextTool_TimeInSchemaEnum(t *testing.T) {
	// Schema guard: "time" must be in the op enum so MCP auto-propagates it.
	var schema struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(contextInputSchema), &schema); err != nil {
		t.Fatalf("contextInputSchema invalid JSON: %v", err)
	}
	found := false
	for _, op := range schema.Properties.Op.Enum {
		if op == "time" {
			found = true
		}
	}
	if !found {
		t.Errorf("op enum %v missing \"time\"", schema.Properties.Op.Enum)
	}
	// Description op-list also lists it (human-facing surface).
	if !strings.Contains(contextDescription, "time") {
		t.Errorf("contextDescription op-list missing \"time\"")
	}
}
