package anthropic_oauth_dev

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// TestMaskOutbound_RenamesLoomcycleOnlyBuiltins pins the rename map.
// The mask is what makes loomcycle's substrate primitives usable under
// OAuth — a regression here breaks the OAuth-dev provider's reason for
// existing.
func TestMaskOutbound_RenamesLoomcycleOnlyBuiltins(t *testing.T) {
	in := []providers.ToolSpec{
		{Name: "Memory", Description: "memory tool", InputSchema: json.RawMessage(`{}`)},
		{Name: "Channel", Description: "channel tool"},
		{Name: "Agent", Description: "spawn"},
		{Name: "VolumeDef", Description: "volume def"},            // RFC AH Phase 2a
		{Name: "Read", Description: "file read"},                  // Claude Code overlap; untouched
		{Name: "mcp__github__list_issues", Description: "github"}, // real MCP; untouched
	}
	out := MaskOutbound(in)
	if out[0].Name != "mcp__loomcycle__memory" {
		t.Errorf("Memory → %q, want mcp__loomcycle__memory", out[0].Name)
	}
	if out[1].Name != "mcp__loomcycle__channel" {
		t.Errorf("Channel → %q", out[1].Name)
	}
	if out[2].Name != "mcp__loomcycle__agent" {
		t.Errorf("Agent → %q", out[2].Name)
	}
	if out[3].Name != "mcp__loomcycle__volumedef" {
		t.Errorf("VolumeDef → %q, want mcp__loomcycle__volumedef", out[3].Name)
	}
	if out[4].Name != "Read" {
		t.Errorf("Read should pass through unmasked: %q", out[4].Name)
	}
	if out[5].Name != "mcp__github__list_issues" {
		t.Errorf("real mcp__* should pass through unmasked: %q", out[5].Name)
	}
	// Description gets the prefix so transcripts make the masking
	// visible to operators + model.
	if !strings.HasPrefix(out[0].Description, DescriptionPrefix) {
		t.Errorf("masked descriptor missing prefix: %q", out[0].Description)
	}
	// Non-masked tools' descriptions stay unchanged.
	if strings.HasPrefix(out[4].Description, DescriptionPrefix) {
		t.Errorf("Read description should not get mask prefix: %q", out[4].Description)
	}
}

// TestMaskOutbound_CanonicalizesNonMaskedToolNames (v0.11.10 A1):
// Claude-Code overlap names sent in non-canonical casing (operator
// yaml typo or legacy lowercase convention) get canonicalized on the
// outbound wire so Anthropic's subscription-billing layer sees the
// exact casing Claude Code itself ships with.
func TestMaskOutbound_CanonicalizesNonMaskedToolNames(t *testing.T) {
	in := []providers.ToolSpec{
		{Name: "read"},                     // canonical: Read
		{Name: "WRITE"},                    // canonical: Write
		{Name: "notebookedit"},             // canonical: NotebookEdit
		{Name: "mcp__github__list_issues"}, // not in overlap; passes through
	}
	out := MaskOutbound(in)
	if out[0].Name != "Read" {
		t.Errorf("read should canonicalize to Read; got %q", out[0].Name)
	}
	if out[1].Name != "Write" {
		t.Errorf("WRITE should canonicalize to Write; got %q", out[1].Name)
	}
	if out[2].Name != "NotebookEdit" {
		t.Errorf("notebookedit should canonicalize to NotebookEdit; got %q", out[2].Name)
	}
	if out[3].Name != "mcp__github__list_issues" {
		t.Errorf("MCP tool should pass through unchanged; got %q", out[3].Name)
	}
}

// TestMaskOutbound_DoesNotMutateInput is a non-trivial concern — the
// loop layer may pass the same Tools slice to multiple providers in
// one iteration (e.g., tier fallback). Mutating it would corrupt the
// other provider's call.
func TestMaskOutbound_DoesNotMutateInput(t *testing.T) {
	in := []providers.ToolSpec{{Name: "Memory", Description: "x"}}
	_ = MaskOutbound(in)
	if in[0].Name != "Memory" {
		t.Errorf("input mutated: %q", in[0].Name)
	}
	if in[0].Description != "x" {
		t.Errorf("input description mutated: %q", in[0].Description)
	}
}

// TestUnmaskInbound_RoundTrip pins the symmetry: every masked name
// from MaskOutbound reverses cleanly via UnmaskInbound.
func TestUnmaskInbound_RoundTrip(t *testing.T) {
	originals := []string{"Memory", "Channel", "Agent", "AgentDef", "Evaluation", "Interruption", "Context", "HTTP", "SkillDef", "MCPServerDef"}
	for _, orig := range originals {
		masked := MaskOutbound([]providers.ToolSpec{{Name: orig}})[0].Name
		reversed := UnmaskInbound(masked)
		if reversed != orig {
			t.Errorf("round-trip broken: %q → %q → %q", orig, masked, reversed)
		}
	}
}

// TestUnmaskInbound_PassesThroughUnmaskedNames: real `mcp__*` tools
// (from operator yaml `mcp_servers:` or substrate `MCPServerDef`) do
// NOT match the mask prefix's known-suffix set + come back verbatim.
// Claude-Code overlap names (Read, Write, etc.) also pass through.
func TestUnmaskInbound_PassesThroughUnmaskedNames(t *testing.T) {
	for _, name := range []string{
		"Read", "Write", "Bash",
		"mcp__github__list_issues",
		"mcp__notion__create_page",
	} {
		if got := UnmaskInbound(name); got != name {
			t.Errorf("non-masked name %q became %q", name, got)
		}
	}
}

// TestUnmaskInbound_UnknownLoomcycleSuffixPassesThrough: defensive —
// if the model emits `mcp__loomcycle__hallucinated`, return it verbatim
// so the dispatcher's "unknown tool" path surfaces a clear error to
// the model + the model self-corrects.
func TestUnmaskInbound_UnknownLoomcycleSuffixPassesThrough(t *testing.T) {
	name := "mcp__loomcycle__hallucinated"
	if got := UnmaskInbound(name); got != name {
		t.Errorf("unknown loomcycle suffix should pass through: %q → %q", name, got)
	}
}

// TestMaskMessages_RenamesToolUseInHistory: the previous-turn
// assistant message contains tool_use ContentBlocks whose names match
// the now-masked tool list. Outbound must keep them masked or
// Anthropic returns "tool_use_id mismatch."
func TestMaskMessages_RenamesToolUseInHistory(t *testing.T) {
	in := []providers.Message{
		{
			Role: "assistant",
			Content: []providers.ContentBlock{
				{Type: "text", Text: "I'll check memory."},
				{Type: "tool_use", ToolUseID: "tu_1", ToolName: "Memory", ToolInput: json.RawMessage(`{}`)},
				{Type: "tool_use", ToolUseID: "tu_2", ToolName: "Read", ToolInput: json.RawMessage(`{}`)},
				{Type: "tool_use", ToolUseID: "tu_3", ToolName: "mcp__github__list_issues"},
			},
		},
	}
	out := MaskMessages(in)
	if out[0].Content[1].ToolName != "mcp__loomcycle__memory" {
		t.Errorf("Memory in history → %q", out[0].Content[1].ToolName)
	}
	if out[0].Content[2].ToolName != "Read" {
		t.Errorf("Read in history should be untouched: %q", out[0].Content[2].ToolName)
	}
	if out[0].Content[3].ToolName != "mcp__github__list_issues" {
		t.Errorf("real mcp__* in history should be untouched: %q", out[0].Content[3].ToolName)
	}
	// Input not mutated.
	if in[0].Content[1].ToolName != "Memory" {
		t.Errorf("input mutated: %q", in[0].Content[1].ToolName)
	}
}

// TestMaskMessages_NoTools is the common path — most user/assistant
// turns carry only text. Verify we don't crash + don't allocate when
// there's nothing to mask.
func TestMaskMessages_NoTools(t *testing.T) {
	in := []providers.Message{
		{Role: "user", Content: []providers.ContentBlock{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []providers.ContentBlock{{Type: "text", Text: "hello"}}},
	}
	out := MaskMessages(in)
	if len(out) != 2 || out[0].Content[0].Text != "hi" || out[1].Content[0].Text != "hello" {
		t.Errorf("text-only messages corrupted: %+v", out)
	}
}

// TestCanonicalizeToolName covers the 10 overlap entries from
// canonical.go. Stub today; wired into outbound builds in v0.11.10.
func TestCanonicalizeToolName(t *testing.T) {
	for in, want := range map[string]string{
		"read":         "Read",
		"READ":         "Read",
		"NotebookEdit": "NotebookEdit",
		"notebookedit": "NotebookEdit",
		"webfetch":     "WebFetch",
		"unknown":      "unknown", // not in the map; pass through
	} {
		if got := CanonicalizeToolName(in); got != want {
			t.Errorf("Canonicalize(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsClaudeCodeCanonical pins the 10 overlap entries against the
// constant — adding a new built-in to canonical.go without adjusting
// the rest of the wire layer would surface here.
func TestIsClaudeCodeCanonical(t *testing.T) {
	for _, name := range []string{"Read", "Write", "Edit", "Bash", "Grep", "Glob", "NotebookEdit", "WebFetch", "WebSearch", "Skill"} {
		if !IsClaudeCodeCanonical(name) {
			t.Errorf("%q should be Claude-Code canonical", name)
		}
	}
	for _, name := range []string{"Memory", "Channel", "Agent", "mcp__github__x"} {
		if IsClaudeCodeCanonical(name) {
			t.Errorf("%q should NOT be Claude-Code canonical", name)
		}
	}
}
