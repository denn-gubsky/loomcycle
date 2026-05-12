package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
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

	// Cfg is the operator config. Used by the `agents` op to enumerate
	// declared agents and surface metadata (name, description, tier,
	// allowed-tools shape). nil = `agents` op refuses with a clear
	// "not configured" error.
	Cfg *config.Config

	// Store is the persistence backend, used by the substrate-coupled
	// ops (`agents` for active-def lookup, `lineage` for parent/child
	// walks, `evaluations` for aggregate). nil = those ops refuse with
	// a clear "not configured" error; the storage-agnostic ops (self /
	// tools / doc / permissions) still work.
	Store store.Store
}

const contextDescription = `Read-only runtime introspection. ` +
	`Answers "what tools do I have? who am I? what permissions apply to me? ` +
	`what other agents exist? what's my def's lineage and evaluation history?". ` +
	`Operations: self, tools, doc, permissions, agents, lineage, evaluations ` +
	`(more ops in later versions). ` +
	`Always safe to call — no side effects, no storage writes, no network calls. ` +
	`Useful for self-evolving agents that build their own task plans and want to inspect ` +
	`their environment before deciding what to do.`

const contextInputSchema = `{
  "type": "object",
  "properties": {
    "op":              {"type": "string", "enum": ["self","tools","doc","permissions","agents","lineage","evaluations"], "description": "Which introspection op to run."},
    "name":            {"type": "string", "description": "doc only: the tool name to fetch detailed docs for."},
    "prefix":          {"type": "string", "description": "agents only: optional name prefix filter."},
    "def_id":          {"type": "string", "description": "lineage / evaluations: the agent_defs row id to inspect. Use Context.agents to discover def_ids first."},
    "depth":           {"type": "integer", "description": "lineage only: max depth to walk in each direction (default 10, cap 100)."},
    "include_lineage": {"type": "boolean", "description": "evaluations only: include ancestors' evaluations in the aggregate (default false)."}
  },
  "required": ["op"],
  "additionalProperties": false
}`

type contextInput struct {
	Op             string `json:"op"`
	Name           string `json:"name,omitempty"`
	Prefix         string `json:"prefix,omitempty"`
	DefID          string `json:"def_id,omitempty"`
	Depth          int    `json:"depth,omitempty"`
	IncludeLineage bool   `json:"include_lineage,omitempty"`
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
	case "agents":
		return c.execAgents(ctx, in)
	case "lineage":
		return c.execLineage(ctx, in)
	case "evaluations":
		return c.execEvaluations(ctx, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: self, tools, doc, permissions, agents, lineage, evaluations)", in.Op)), nil
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

// ---- agents ----

func (c *Context) execAgents(ctx context.Context, in contextInput) (tools.Result, error) {
	if c.Cfg == nil {
		return errResult("agents: not configured (no Cfg)"), nil
	}
	type agentSummary struct {
		Name string `json:"name"`
		// config.AgentDef has no Description field today — the
		// MD-frontmatter description lives on agents.Agent (loader) and
		// doesn't get propagated into cfg.Agents. Omitted from this op
		// pending a follow-up that threads it through (low priority;
		// agents already know their own description from their prompt).
		Tier         string `json:"tier,omitempty"`
		Model        string `json:"model,omitempty"`
		Provider     string `json:"provider,omitempty"`
		ActiveDefID  string `json:"active_def_id,omitempty"`
		AllowedTools int    `json:"allowed_tools_count"`
		// Error surfaces a per-name lookup failure (e.g. DB connection
		// drop while resolving ActiveDefID). Empty when the lookup
		// succeeded or the name has no DB row (the NotFound case
		// silently omits ActiveDefID rather than reporting an error —
		// not-yet-forked is the common "no active def" state).
		Error string `json:"error,omitempty"`
	}
	names := make([]string, 0, len(c.Cfg.Agents))
	for n := range c.Cfg.Agents {
		if in.Prefix != "" && !strings.HasPrefix(n, in.Prefix) {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]agentSummary, 0, len(names))
	for _, name := range names {
		def := c.Cfg.Agents[name]
		s := agentSummary{
			Name:         name,
			Tier:         def.Tier,
			Model:        def.Model,
			Provider:     def.Provider,
			AllowedTools: len(def.AllowedTools),
		}
		// Active def_id from the v0.8.5 substrate. Best-effort — if
		// Store is nil or no active row, we omit the field. Non-
		// NotFound errors are unusual but possible (e.g. DB connection
		// drop mid-call); surface them as a per-name `error` key + log
		// so operators see the partial failure without losing the rest
		// of the catalog. PR 2 review fix.
		if c.Store != nil {
			row, err := c.Store.AgentDefGetActive(ctx, name)
			if err == nil {
				s.ActiveDefID = row.DefID
			} else {
				var nf *store.ErrNotFound
				if !errors.As(err, &nf) {
					s.Error = err.Error()
					log.Printf("context agents: AgentDefGetActive(%q): %v", name, err)
				}
			}
		}
		out = append(out, s)
	}
	return okJSON(map[string]any{"agents": out, "count": len(out)})
}

// ---- lineage ----

func (c *Context) execLineage(ctx context.Context, in contextInput) (tools.Result, error) {
	if c.Store == nil {
		return errResult("lineage: not configured (no Store backend)"), nil
	}
	if in.DefID == "" {
		return errResult("lineage: missing required field: def_id (use Context.agents to discover def_ids)"), nil
	}
	depth := in.Depth
	if depth <= 0 {
		depth = 10
	}
	if depth > 100 {
		depth = 100
	}

	root, err := c.Store.AgentDefGet(ctx, in.DefID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			return errResult(fmt.Sprintf("lineage: def_id %q not found", in.DefID)), nil
		}
		return errResult(fmt.Sprintf("lineage: %s", err)), nil
	}

	type defSummary struct {
		DefID       string `json:"def_id"`
		Name        string `json:"name"`
		Version     int    `json:"version"`
		ParentDefID string `json:"parent_def_id,omitempty"`
		Retired     bool   `json:"retired"`
		Description string `json:"description,omitempty"`
	}
	toSummary := func(row store.AgentDefRow) defSummary {
		return defSummary{
			DefID:       row.DefID,
			Name:        row.Name,
			Version:     row.Version,
			ParentDefID: row.ParentDefID,
			Retired:     row.Retired,
			Description: row.Description,
		}
	}

	// Walk ancestors via parent_def_id chain.
	ancestors := make([]defSummary, 0, depth)
	cur := root
	for i := 0; i < depth && cur.ParentDefID != ""; i++ {
		parent, err := c.Store.AgentDefGet(ctx, cur.ParentDefID)
		if err != nil {
			var nf *store.ErrNotFound
			if errors.As(err, &nf) {
				break // chain ended at a row that's been deleted
			}
			return errResult(fmt.Sprintf("lineage: walk ancestors: %s", err)), nil
		}
		ancestors = append(ancestors, toSummary(parent))
		cur = parent
	}

	// Walk descendants breadth-first via AgentDefListChildren. Cap
	// the TOTAL node count (not just depth) so a high-fan-out lineage
	// doesn't produce a multi-megabyte JSON blob — depth alone could
	// still blow up under heavy fork branching (PR 2 review fix).
	// On overflow we stop the BFS, set truncated=true, and return
	// whatever was collected so far.
	const maxDescendants = 500
	var descendants []defSummary
	truncated := false
	frontier := []store.AgentDefRow{root}
	for d := 1; d <= depth && len(frontier) > 0 && !truncated; d++ {
		var next []store.AgentDefRow
		for _, r := range frontier {
			children, err := c.Store.AgentDefListChildren(ctx, r.DefID)
			if err != nil {
				return errResult(fmt.Sprintf("lineage: walk descendants: %s", err)), nil
			}
			for _, ch := range children {
				if len(descendants) >= maxDescendants {
					truncated = true
					break
				}
				descendants = append(descendants, toSummary(ch))
				next = append(next, ch)
			}
			if truncated {
				break
			}
		}
		frontier = next
	}

	return okJSON(map[string]any{
		"root":        toSummary(root),
		"ancestors":   ancestors,
		"descendants": descendants,
		"depth":       depth,
		"truncated":   truncated,
	})
}

// ---- evaluations ----

func (c *Context) execEvaluations(ctx context.Context, in contextInput) (tools.Result, error) {
	if c.Store == nil {
		return errResult("evaluations: not configured (no Store backend)"), nil
	}
	if in.DefID == "" {
		return errResult("evaluations: missing required field: def_id (use Context.agents to discover def_ids)"), nil
	}
	agg, err := c.Store.EvaluationAggregate(ctx, in.DefID, store.AggregateOpts{
		IncludeLineage: in.IncludeLineage,
	})
	if err != nil {
		return errResult(fmt.Sprintf("evaluations: %s", err)), nil
	}
	return okJSON(agg)
}

var _ tools.Tool = (*Context)(nil)
