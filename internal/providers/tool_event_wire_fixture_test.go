package providers

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// webToolEventsFixture is read by web/src/lib/toolSettlement.test.ts.
const webToolEventsFixture = "../../web/src/lib/__fixtures__/tool-events.json"

// ⚠️ THE WEB UI'S TOOL-CALL SETTLEMENT WAS WRITTEN AGAINST A FIELD THIS WIRE
// NEVER CARRIED. It read a top-level `tool_use_id` off each tool_result frame;
// the frame carries the id as `tool_use.id`. So every tool call looked
// unanswered, and an Interruption that had already failed sat on the agent
// pane as "interrupted" for the rest of the run.
//
// A hand-written TS fixture would have encoded the same wrong belief, so the
// fixture is produced HERE, from the real struct, and the TS test reads it.
// Regenerate with LOOMCYCLE_UPDATE_FIXTURES=1 when the Event shape changes.
func TestToolEventWireShape_MatchesTheWebFixture(t *testing.T) {
	call := &ToolUse{ID: "es-act-0", Name: "Interruption", Input: json.RawMessage(`{}`)}
	frames := []Event{
		{Type: EventToolCall, ToolUse: call},
		{Type: EventToolResult, ToolUse: call, Text: "missing required field: op", IsError: true},
	}
	got, err := json.MarshalIndent(frames, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("LOOMCYCLE_UPDATE_FIXTURES") == "1" {
		if err := os.WriteFile(webToolEventsFixture, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(webToolEventsFixture)
	if err != nil {
		t.Skipf("web fixture not readable from here: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the tool_call/tool_result wire shape changed; the Web UI's settlement is "+
			"tested against the old one. Regenerate with LOOMCYCLE_UPDATE_FIXTURES=1 and "+
			"re-run the web tests.\n got: %s\nwant: %s", got, want)
	}
}
