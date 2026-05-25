package anthropic_oauth_dev

import "strings"

// claudeCodeCanonical maps lowercased tool names to Claude Code's
// canonical casing. Used to normalize tool names before sending under
// OAuth-dev — Anthropic's subscription-billing detection may check the
// casing of canonical tool names (Pi's reference applies the same
// normalization). The 10-tool overlap below is what Claude Code itself
// ships; the v0.8.24 additions (Grep / Glob / NotebookEdit) are
// included even though they postdate the original Pi reference.
//
// Tools outside this map pass through unchanged — either they're
// loomcycle-only built-ins (handled by the mask layer in
// loomcycle_mask.go) or real MCP tools (already use the `mcp__*` wire
// pattern).
var claudeCodeCanonical = map[string]string{
	"read":         "Read",
	"write":        "Write",
	"edit":         "Edit",
	"bash":         "Bash",
	"grep":         "Grep",
	"glob":         "Glob",
	"notebookedit": "NotebookEdit",
	"webfetch":     "WebFetch",
	"websearch":    "WebSearch",
	"skill":        "Skill",
}

// CanonicalizeToolName returns the Claude Code canonical casing for a
// tool name when the lowercased form matches the 10-tool overlap.
// Anything else (loomcycle-only built-ins, masked names, real MCP
// tools) passes through unchanged.
//
// Stub for v0.11.9 — wired into outbound request building in v0.11.10
// alongside the rest of the stealth-mode parity work.
func CanonicalizeToolName(name string) string {
	if canonical, ok := claudeCodeCanonical[strings.ToLower(name)]; ok {
		return canonical
	}
	return name
}

// IsClaudeCodeCanonical reports whether the given (already-canonical)
// name is one of Claude Code's 10 built-in tools. Used by the mask
// layer's defensive guards + by future per-tool wire adaptations.
func IsClaudeCodeCanonical(name string) bool {
	_, ok := claudeCodeCanonical[strings.ToLower(name)]
	return ok
}
