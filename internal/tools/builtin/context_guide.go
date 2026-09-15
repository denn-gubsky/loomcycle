package builtin

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/policy"
)

// execGuide renders a compact "how to call your tools" digest for THIS run's
// resolved tool set: per tool, the op enum + required args parsed from the
// tool's own InputSchema (so the digest can never drift from what the tool
// actually accepts), plus an optional hand-written UsageHint (tools.HintedTool).
//
// It exists because the loop already sends every tool's full JSON schema on each
// request, yet small/local models under-attend to that array — so an agent
// starts "blind," unsure which op to call or which fields are required, and makes
// avoidable tool-call errors. The digest is the high-signal subset (which op,
// which required fields, the one thing to get right) that the system-prompt
// injector renders, and that an agent can also call directly to self-serve.
//
// Read-only, like every Context op. Filtered by the ctx-attached AgentTools list,
// exactly as op=tools is, so it reflects THIS run's effective tools.
func (c *Context) execGuide(ctx context.Context) (tools.Result, error) {
	allowSet, ok := agentToolSet(ctx)
	if !ok {
		return okJSONCount(map[string]any{"tools": []any{}, "count": 0}, 0)
	}
	type guideEntry struct {
		Name            string   `json:"name"`
		SideEffectClass string   `json:"side_effect_class"`
		Ops             []string `json:"ops,omitempty"`
		Required        []string `json:"required,omitempty"`
		Hint            string   `json:"hint,omitempty"`
	}
	out := make([]guideEntry, 0, len(c.Tools))
	for _, t := range c.Tools {
		name := t.Name()
		// Same floor as op=tools: use the ctx allowlist when present
		// (production), else show everything (test fixtures).
		if !policy.Matches(name, allowSet) {
			continue
		}
		ops, required := parseSchemaDigest(t.InputSchema())
		out = append(out, guideEntry{
			Name:            name,
			SideEffectClass: sideEffectClassFor(name),
			Ops:             ops,
			Required:        required,
			Hint:            tools.UsageHintOf(t),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return okJSONCount(map[string]any{"tools": out, "count": len(out)}, len(out))
}

// parseSchemaDigest extracts the `op` enum (properties.op.enum) and the
// top-level required list from a tool's JSON-Schema input. Best-effort: a schema
// that does not parse, or one with no `op` property, yields nils — the digest
// simply omits those fields. It never errors, so a malformed MCP schema cannot
// break the guide.
func parseSchemaDigest(raw json.RawMessage) (ops, required []string) {
	var s struct {
		Properties struct {
			Op struct {
				Enum []string `json:"enum"`
			} `json:"op"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, nil
	}
	return s.Properties.Op.Enum, s.Required
}
