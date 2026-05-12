package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Context is the v0.8.7 built-in tool that lets an agent introspect
// its own runtime — what tools it has, what its identity / lineage
// looks like, what evaluations exist for its definition, what channels
// it can reach. Read-only; no mutations, no side effects beyond reads.
//
// Discriminated `op` field (same shape as Memory / Channel) lets
// agents narrow the output instead of paying for a kitchen-sink dump
// on every call.
//
// Nine ops total — PR 1 ships four of them:
//
//	self        — identity bundle (agent name, run/agent ids, user, tier)
//	tools       — post-filter tool catalog
//	doc         — detailed schema/docstring for one tool by name
//	permissions — gates that apply to the caller (tool ACL, host policy, scopes)
//
// PR 2 adds:  agents / lineage / evaluations
// PR 3 adds:  channels / history + default-add + opt-out
//
// All op results are deterministic and side-effect-free. Calling
// Context never modifies anything in storage or in-process state.
//
// scope_id is NOT relevant here — Context reads from ctx, not from
// per-scope buckets. No model-supplied identifiers route to storage
// reads, so there's no cross-user-leak surface.
type Context struct {
	// Tools is the FULL post-filter tool list available to THIS run.
	// The HTTP server constructs this per-run via filterTools+narrowing
	// and passes it through; Context just reports it. Required.
	Tools []tools.Tool
}

const contextDescription = `Read-only runtime introspection. ` +
	`Answers "what tools do I have? who am I? what permissions apply to me?". ` +
	`Operations: self, tools, doc, permissions (more ops in later versions). ` +
	`Always safe to call — no side effects, no storage writes, no network calls. ` +
	`Useful for self-evolving agents that build their own task plans and want to inspect ` +
	`their environment before deciding what to do.`

const contextInputSchema = `{
  "type": "object",
  "properties": {
    "op":   {"type": "string", "enum": ["self","tools","doc","permissions"], "description": "Which introspection op to run."},
    "name": {"type": "string", "description": "doc only: the tool name to fetch detailed docs for."}
  },
  "required": ["op"],
  "additionalProperties": false
}`

type contextInput struct {
	Op   string `json:"op"`
	Name string `json:"name,omitempty"`
}

// Name implements tools.Tool.
func (c *Context) Name() string { return "Context" }

// Description implements tools.Tool.
func (c *Context) Description() string { return contextDescription }

// InputSchema implements tools.Tool.
func (c *Context) InputSchema() json.RawMessage { return json.RawMessage(contextInputSchema) }

// Execute implements tools.Tool. Dispatch off `op`; everything is
// read-only so there's no scope resolution / quota / cursor path.
func (c *Context) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	var in contextInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	switch in.Op {
	case "self":
		return c.execSelf(ctx)
	case "tools":
		return c.execTools(ctx)
	case "doc":
		return c.execDoc(ctx, in)
	case "permissions":
		return c.execPermissions(ctx)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: self, tools, doc, permissions)", in.Op)), nil
	}
}

// ---- self ----

func (c *Context) execSelf(ctx context.Context) (tools.Result, error) {
	ident := tools.RunIdentity(ctx)
	out := map[string]any{
		"agent_name":   tools.AgentName(ctx),
		"agent_id":     ident.AgentID,
		"user_id":      ident.UserID,
		"user_tier":    ident.UserTier,
		"agent_def_id": ident.AgentDefID,
	}
	return okJSON(out)
}

// ---- tools ----

func (c *Context) execTools(ctx context.Context) (tools.Result, error) {
	// The agent's effective tool name list is attached to ctx by
	// the server at run start (tools.WithAgentTools). Filter c.Tools
	// against it so the result reflects THIS run's allowlist
	// (post-narrowing) — c.Tools is the runtime-wide catalog, the
	// ctx list is the per-run subset.
	allowed := tools.AgentTools(ctx)
	allowSet := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		allowSet[n] = true
	}

	type toolSummary struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		// SideEffectClass is a hint to the model about what calling
		// the tool can do. Closed set (v0.8.7):
		//
		//	"pure"        — no side effects (Read, WebFetch, WebSearch,
		//	                Memory.get/list, Channel.peek, AgentDef.get/list,
		//	                Evaluation.get/aggregate, Context.*)
		//	"state"       — mutates loomcycle-managed state (Memory.set/incr/
		//	                delete, Channel.publish/subscribe/ack,
		//	                AgentDef.create/fork/promote/retire,
		//	                Evaluation.submit)
		//	"network"     — outbound HTTP/HTTPS (HTTP, WebFetch, WebSearch)
		//	"filesystem"  — reads/writes the runtime's filesystem (Read,
		//	                Write, Edit)
		//	"privileged"  — executes arbitrary code on the host (Bash, Agent)
		//	"unknown"     — MCP-served tool; class isn't introspectable
		//	                without operator metadata. Defaults here.
		//
		// One tool may legitimately be in multiple classes — the field
		// is a single string by design (the model wants a quick filter,
		// not a fine-grained taxonomy). When in doubt, pick the
		// strongest class.
		SideEffectClass string `json:"side_effect_class"`
	}
	out := make([]toolSummary, 0, len(c.Tools))
	for _, t := range c.Tools {
		name := t.Name()
		// When the ctx-attached list is present (production path), use
		// it as the floor; when absent (test fixtures, unwired test
		// harnesses) fall back to showing everything in c.Tools so
		// Context is still useful in unit tests.
		if len(allowSet) > 0 && !allowSet[name] {
			continue
		}
		out = append(out, toolSummary{
			Name:            name,
			Description:     t.Description(),
			SideEffectClass: sideEffectClassFor(name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return okJSON(map[string]any{"tools": out, "count": len(out)})
}

// sideEffectClassFor maps the closed-set classifier to a tool name.
// MCP tools (prefixed `mcp__`) fall through to "unknown" — the class
// depends on the server's contract and we don't have operator metadata
// to introspect. Built-ins are listed by canonical name.
func sideEffectClassFor(name string) string {
	switch name {
	// Pure reads (no state mutation, no network beyond reading a URL).
	case "Read":
		return "filesystem"
	case "WebFetch", "WebSearch", "HTTP":
		return "network"
	// State mutations.
	case "Write", "Edit":
		return "filesystem"
	case "Bash", "Agent":
		return "privileged"
	case "Memory", "Channel", "AgentDef", "Evaluation":
		// These have op-discriminated surfaces; some ops are pure
		// (get/list/peek) and some are state (set/publish/create).
		// Without per-op classification we conservatively pick "state"
		// — operators reading the catalog see "this might mutate."
		return "state"
	case "Context", "Skill":
		return "pure"
	}
	if strings.HasPrefix(name, "mcp__") {
		return "unknown"
	}
	return "unknown"
}

// ---- doc ----

func (c *Context) execDoc(ctx context.Context, in contextInput) (tools.Result, error) {
	if in.Name == "" {
		return errResult("doc: missing required field: name"), nil
	}
	allowed := tools.AgentTools(ctx)
	allowSet := make(map[string]bool, len(allowed))
	for _, n := range allowed {
		allowSet[n] = true
	}
	for _, t := range c.Tools {
		if t.Name() != in.Name {
			continue
		}
		if len(allowSet) > 0 && !allowSet[in.Name] {
			// Tool exists in the runtime catalog but isn't in THIS
			// run's effective list. Refuse with a clear message
			// rather than leaking the docs of a tool the agent
			// can't actually call.
			return errResult(fmt.Sprintf("doc: tool %q is not in this agent's allowed_tools", in.Name)), nil
		}
		schema := t.InputSchema()
		return okJSON(map[string]any{
			"name":              t.Name(),
			"description":       t.Description(),
			"input_schema":      json.RawMessage(schema),
			"side_effect_class": sideEffectClassFor(t.Name()),
		})
	}
	return errResult(fmt.Sprintf("doc: tool %q not found (use op=tools to list available)", in.Name)), nil
}

// ---- permissions ----

func (c *Context) execPermissions(ctx context.Context) (tools.Result, error) {
	// Surface the per-run policy bundles that gate the agent's
	// behaviour. The model can compare its intended actions to these
	// gates before attempting them (avoids "tool refused" surprises).
	hostPol := tools.HostPolicy(ctx)
	memPol := tools.MemoryPolicy(ctx)
	chPol := tools.ChannelPolicy(ctx)
	adPol := tools.AgentDefPolicy(ctx)
	evPol := tools.EvaluationPolicy(ctx)

	out := map[string]any{
		"allowed_tools": tools.AgentTools(ctx),
		"host_policy": map[string]any{
			"has_list":          hostPol.HasList,
			"allowed_hosts":     hostPol.AllowedHosts,
			"web_search_filter": hostPol.WebSearchFilter,
		},
		"memory": map[string]any{
			"allowed_scopes": memPol.AllowedScopes,
			"quota_bytes":    memPol.QuotaBytes,
		},
		"channels": map[string]any{
			"publish":   chPol.Publish,
			"subscribe": chPol.Subscribe,
		},
		"agent_def_scopes":  adPol.Scopes,
		"evaluation_scopes": evPol.Scopes,
	}
	return okJSON(out)
}

var _ tools.Tool = (*Context)(nil)
