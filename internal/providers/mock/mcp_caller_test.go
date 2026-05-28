package mock

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

func TestExtractMCPTools_FiltersByPrefix(t *testing.T) {
	req := providers.Request{
		Tools: []providers.ToolSpec{
			{Name: "Memory"},
			{Name: "mcp__server_a__check_user"},
			{Name: "Channel"},
			{Name: "mcp__server_b__check_user"},
			{Name: "Context"},
		},
	}
	got := extractMCPTools(req)
	want := []string{"mcp__server_a__check_user", "mcp__server_b__check_user"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, n := range want {
		if got[i] != n {
			t.Errorf("got[%d] = %q, want %q (order must be preserved)", i, got[i], n)
		}
	}
}

func TestExtractUserID_FindsTokenInLatestUserMessage(t *testing.T) {
	req := providers.Request{
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "boilerplate"}}},
			{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "ack"}}},
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "run for user_id=u_077 now"}}},
		},
	}
	if got := extractUserID(req); got != "u_077" {
		t.Errorf("got %q, want u_077", got)
	}
}

func TestExtractUserID_EmptyWhenAbsent(t *testing.T) {
	req := providers.Request{
		Messages: []providers.Message{
			{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "no token here"}}},
		},
	}
	if got := extractUserID(req); got != "" {
		t.Errorf("got %q, want empty string", got)
	}
}

func TestScriptMCPCaller_TwoToolFSM(t *testing.T) {
	tools := []string{"mcp__server_a__check_user", "mcp__server_b__check_user"}
	userID := "u_007"

	// Step 0 — calls first tool.
	events0 := scriptMCPCaller(0, tools, userID)
	if !hasToolUse(events0, "mcp__server_a__check_user") {
		t.Errorf("step 0 should call tool A; events=%v", events0)
	}
	if !hasArg(events0, "user_id", userID) {
		t.Errorf("step 0 should pass user_id=%q in tool input", userID)
	}

	// Step 1 — calls second tool.
	events1 := scriptMCPCaller(1, tools, userID)
	if !hasToolUse(events1, "mcp__server_b__check_user") {
		t.Errorf("step 1 should call tool B; events=%v", events1)
	}

	// Step 2 — terminal.
	events2 := scriptMCPCaller(2, tools, userID)
	if events2[len(events2)-1].StopReason != "end_turn" {
		t.Errorf("step 2 should terminate (end_turn); got %q", events2[len(events2)-1].StopReason)
	}
}

func TestScriptMCPCaller_NoMCPTools(t *testing.T) {
	events := scriptMCPCaller(0, nil, "u_001")
	if events[len(events)-1].StopReason != "end_turn" {
		t.Errorf("with no MCP tools, step 0 should terminate immediately")
	}
	// Should have a text event explaining the situation.
	found := false
	for _, e := range events {
		if e.Type == providers.EventText && strings.Contains(e.Text, "no mcp tools") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected explanatory text event; got %v", events)
	}
}

func TestScriptMCPCaller_OneMCPToolDoesntDoubleCall(t *testing.T) {
	// With only one mcp tool, step 1 should terminate (no second tool).
	tools := []string{"mcp__server_a__check_user"}
	events := scriptMCPCaller(1, tools, "u_001")
	if events[len(events)-1].StopReason != "end_turn" {
		t.Errorf("with 1 tool, step 1 should terminate; got %v", events)
	}
}

func hasToolUse(events []providers.Event, name string) bool {
	for _, e := range events {
		if e.Type == providers.EventToolCall && e.ToolUse != nil && e.ToolUse.Name == name {
			return true
		}
	}
	return false
}

func hasArg(events []providers.Event, key, value string) bool {
	for _, e := range events {
		if e.Type != providers.EventToolCall || e.ToolUse == nil {
			continue
		}
		var args map[string]any
		if err := json.Unmarshal(e.ToolUse.Input, &args); err != nil {
			continue
		}
		if got, ok := args[key].(string); ok && got == value {
			return true
		}
	}
	return false
}
