package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lookup"
)

// The per-agent `telemetry:` block (RFC GG P3) round-trips through AgentDef
// create/fork and the lookup read path, is content-identifying, and a fork
// cannot take an opt-out away.

func TestAgentDef_TelemetryNeverRoundTripsIsHashedAndSticks(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()

	got := storedRouting(t, tool, ctx, `{"op":"create","name":"secretive","overlay":{"tier":"middle","system_prompt":"s"}}`)
	if got.Telemetry != nil {
		t.Fatalf("an agent without the block stored %+v", got.Telemetry)
	}
	// A fork that only opts out is a new version, not a dedup.
	res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"secretive","overlay":{"telemetry":{"content_capture":"never"}}}`))
	if res.IsError {
		t.Fatalf("fork: %s", res.Text)
	}
	if deduped, _ := decodeResult(t, res.Text)["deduped"].(bool); deduped {
		t.Fatalf("a fork that only sets telemetry.content_capture deduped: %s", res.Text)
	}
	defID, _ := decodeResult(t, res.Text)["def_id"].(string)
	row, err := tool.Store.AgentDefGet(ctx, defID)
	if err != nil {
		t.Fatalf("AgentDefGet(%s): %v", defID, err)
	}
	never, ok := lookup.AgentFromDefRow(row)
	if !ok || !never.Telemetry.NeverCapturesContent() {
		t.Fatalf("forked telemetry = %+v, want content_capture never", never.Telemetry)
	}
	// An overlay without the block keeps the parent's opt-out.
	res, _ = tool.Execute(ctx, json.RawMessage(`{"op":"fork","name":"secretive","parent_def_id":"`+defID+`","overlay":{"system_prompt":"t"}}`))
	if res.IsError {
		t.Fatalf("second fork: %s", res.Text)
	}
	child, _ := decodeResult(t, res.Text)["def_id"].(string)
	row, err = tool.Store.AgentDefGet(ctx, child)
	if err != nil {
		t.Fatalf("AgentDefGet(%s): %v", child, err)
	}
	if again, ok := lookup.AgentFromDefRow(row); !ok || !again.Telemetry.NeverCapturesContent() {
		t.Errorf("a fork of an opted-out agent lost the opt-out: %+v", again.Telemetry)
	}
}

func TestAgentDef_TelemetryUnknownValueIsRefused(t *testing.T) {
	tool, ctx, cleanup := agentDefFixture(t)
	defer cleanup()
	for _, v := range []string{"always", "on", "off"} {
		res, _ := tool.Execute(ctx, json.RawMessage(`{"op":"create","name":"bad","overlay":{"tier":"middle","system_prompt":"s","telemetry":{"content_capture":"`+v+`"}}}`))
		if !res.IsError || !strings.Contains(res.Text, "telemetry.content_capture") {
			t.Errorf("content_capture %q: %s, want a refusal naming the field", v, res.Text)
		}
	}
}
