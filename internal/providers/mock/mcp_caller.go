package mock

import (
	"regexp"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// scriptMCPCaller is the v0.12.7 mock variant that exercises MCP tool
// dispatch end-to-end. Unlike the hardcoded researcher/editor/evaluator
// scripts, this variant INSPECTS req.Tools at call time to find tools
// whose name starts with the MCP convention prefix `mcp__` and emits
// tool_use blocks calling them. The compound test fixture
// (internal/api/http/scheduler_bearer_compound_test.go) uses this to
// drive real MCP HTTP requests so the per-run credential substitution
// chain (scheduler → RunInput → ctx → MCP header) gets a live exercise.
//
// FSM (driven by countToolResults like the other scripts):
//
//	step 0: extract user_id from the prompt's text content (look for
//	        the "user_id=<value>" substring), call the FIRST mcp__ tool
//	        with {"user_id": <value>}.
//	step 1: same shape, call the SECOND mcp__ tool with the same user_id.
//	step 2+: emit "done" + end_turn.
//
// Why two tools: the compound test wires up TWO mcptest servers each
// with its own Authorization header expressed in different credential
// keys (${run.credentials.token_a} vs ${run.credentials.token_b}, or
// the same key — orchestrator's choice). Calling both proves the
// per-server header substitution path works in parallel under load.
//
// When fewer than two mcp__ tools are present, the script gracefully
// degrades: 1 tool present → call once + done; 0 present → emit "no
// mcp tools" text + end_turn. The scheduler test asserts on the MCP
// server's request counter, so a misconfigured agent shows up as
// "expected 310 calls, got 0" instead of a silent test pass.

// userIDRe matches the literal token "user_id=<value>" anywhere in
// the prompt text. The compound test embeds this into each fork's
// prompt via the schedule_runs template.
var userIDRe = regexp.MustCompile(`user_id=([A-Za-z0-9_-]+)`)

// scriptMCPCaller emits the MCP-calling FSM. mcpTools is the prefiltered
// list of tools whose name starts with "mcp__" (caller computes this
// from req.Tools to keep the script focused). userID is extracted from
// the prompt's text — empty when the prompt didn't include the token.
func scriptMCPCaller(step int, mcpTools []string, userID string) []providers.Event {
	if len(mcpTools) == 0 {
		return []providers.Event{
			textEvent("no mcp tools in allowed_tools — nothing to call"),
			{Type: providers.EventDone, StopReason: "end_turn"},
		}
	}

	switch step {
	case 0:
		return []providers.Event{
			textEvent("calling " + mcpTools[0]),
			toolEvent(mcpTools[0], map[string]any{"user_id": userID}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	case 1:
		if len(mcpTools) < 2 {
			// Only one mcp tool exposed — done after step 0's reply.
			return []providers.Event{
				textEvent("done"),
				{Type: providers.EventDone, StopReason: "end_turn"},
			}
		}
		return []providers.Event{
			textEvent("calling " + mcpTools[1]),
			toolEvent(mcpTools[1], map[string]any{"user_id": userID}),
			{Type: providers.EventDone, StopReason: "tool_use"},
		}
	default:
		return []providers.Event{
			textEvent("done"),
			{Type: providers.EventDone, StopReason: "end_turn"},
		}
	}
}

// extractMCPTools returns the subset of req.Tools whose name starts
// with "mcp__". Preserves catalog order. Caller treats nil + empty
// identically; the script falls through to the degraded path.
func extractMCPTools(req providers.Request) []string {
	out := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		if strings.HasPrefix(t.Name, "mcp__") {
			out = append(out, t.Name)
		}
	}
	return out
}

// extractUserID parses the most recent user-role text content for the
// literal token "user_id=<value>". Returns empty string when no match
// — the script then emits an empty user_id arg which the mcptest
// server's bearer check would reject, making the failure mode visible
// in the compound test's mismatch counter rather than silent.
func extractUserID(req providers.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		m := req.Messages[i]
		if m.Role != "user" {
			continue
		}
		for _, b := range m.Content {
			if b.Type == "text" && b.Text != "" {
				if hit := userIDRe.FindStringSubmatch(b.Text); len(hit) > 1 {
					return hit[1]
				}
			}
		}
	}
	return ""
}
