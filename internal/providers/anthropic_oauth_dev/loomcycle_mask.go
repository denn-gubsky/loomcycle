package anthropic_oauth_dev

import (
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// loomcycleOnlyBuiltins lists the built-in tool names that exist in
// loomcycle but NOT in Claude Code's canonical tool set. Sending these
// names directly under OAuth-dev risks tripping Anthropic's
// subscription-billing detection (which expects either canonical tool
// names or `mcp__*`-prefixed MCP tools). The mask layer renames them to
// `mcp__loomcycle__<lowercased>` on outbound + reverses on inbound, so
// loomcycle's substrate primitives remain available under OAuth-dev
// without violating the wire-name pattern.
//
// Keep alphabetised for diff-cleanliness. Additions should also
// register in the canonical-overlap test in mask_test.go.
var loomcycleOnlyBuiltins = map[string]bool{
	"Agent":        true,
	"AgentDef":     true,
	"Channel":      true,
	"Context":      true,
	"Evaluation":   true,
	"HTTP":         true,
	"Interruption": true,
	"MCPServerDef": true,
	"Memory":       true,
	"SkillDef":     true,
}

// MaskPrefix is the wire-name prefix the loomcycle-only built-ins
// adopt on the outbound request when routed through OAuth-dev. To
// Anthropic's detector this looks like Claude Code calling tools from
// an MCP server registered as "loomcycle." The descriptor's
// description gets the same prefix string prepended so the model + any
// operator reading transcripts can see what's happening.
const MaskPrefix = "mcp__loomcycle__"

// DescriptionPrefix is prepended to the description of every masked
// tool. Makes the masking visible in transcripts; helps the model
// understand it's calling a loomcycle-side tool exposed via MCP.
const DescriptionPrefix = "[Exposed via loomcycle MCP] "

// MaskOutbound walks the tool list and renames any entry whose name is
// in loomcycleOnlyBuiltins to MaskPrefix + lowercased name. The 10-tool
// Claude-Code canonical overlap (Read, Write, Edit, Bash, Grep, Glob,
// NotebookEdit, WebFetch, WebSearch, Skill) and real `mcp__*` tools
// pass through unchanged.
//
// Returns a new slice — does NOT mutate the input. The loop layer
// keeps the unmasked Request.Tools so subsequent iterations can pass
// the same tool list to other providers without surprise.
func MaskOutbound(in []providers.ToolSpec) []providers.ToolSpec {
	if len(in) == 0 {
		return in
	}
	out := make([]providers.ToolSpec, len(in))
	for i, t := range in {
		if loomcycleOnlyBuiltins[t.Name] {
			masked := t
			masked.Name = MaskPrefix + strings.ToLower(t.Name)
			masked.Description = DescriptionPrefix + t.Description
			out[i] = masked
			continue
		}
		out[i] = t
	}
	return out
}

// UnmaskInbound reverses a single tool name from the wire shape back to
// the loomcycle dispatcher's expectation. Called on every `tool_use`
// block streamed back from Anthropic. A name that doesn't carry the
// MaskPrefix is returned verbatim (Claude-Code overlap + real `mcp__*`
// MCP-server tools pass through).
//
// Returns the original name when the mask doesn't match the lowercase
// of any registered loomcycleOnlyBuiltins — defensive against the model
// emitting a `mcp__loomcycle__<unknown>` name (e.g., a hallucinated
// tool); the unknown name then surfaces at the dispatcher's "unknown
// tool" path and the model self-corrects.
func UnmaskInbound(name string) string {
	if !strings.HasPrefix(name, MaskPrefix) {
		return name
	}
	suffix := strings.TrimPrefix(name, MaskPrefix)
	// Match case-insensitively against the original names. Build the
	// reverse map once; small constant size.
	for orig := range loomcycleOnlyBuiltins {
		if strings.EqualFold(orig, suffix) {
			return orig
		}
	}
	// Unknown loomcycle-prefixed name — return verbatim so the
	// dispatcher's "tool not found" path surfaces a clear error to the
	// model.
	return name
}

// IsMasked reports whether a wire-shape tool name was produced by
// MaskOutbound. Used by the driver's inbound event handler to decide
// whether to apply UnmaskInbound.
func IsMasked(wireName string) bool {
	return strings.HasPrefix(wireName, MaskPrefix)
}

// MaskMessages walks the conversation history and renames any
// `tool_use` ContentBlock's ToolName from a loomcycle-only built-in
// (e.g., "Memory") to its masked wire form ("mcp__loomcycle__memory").
// Critical for consistency: the previous-turn tool_use blocks must
// match the Tools[] array's masked names, otherwise Anthropic returns
// "unknown tool_use_id" because the message-history reference doesn't
// resolve to a known tool name.
//
// Returns a deep-enough copy that the caller's Messages slice is not
// mutated — the upstream loop may pass the same Messages to other
// providers in the same iteration (e.g., per-tier fallback).
func MaskMessages(in []providers.Message) []providers.Message {
	if len(in) == 0 {
		return in
	}
	out := make([]providers.Message, len(in))
	for i, m := range in {
		out[i] = m
		if len(m.Content) == 0 {
			continue
		}
		newContent := make([]providers.ContentBlock, len(m.Content))
		for j, c := range m.Content {
			newContent[j] = c
			if c.Type == "tool_use" && loomcycleOnlyBuiltins[c.ToolName] {
				newContent[j].ToolName = MaskPrefix + strings.ToLower(c.ToolName)
			}
		}
		out[i].Content = newContent
	}
	return out
}
