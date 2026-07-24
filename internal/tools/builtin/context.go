package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/help"
	"github.com/denn-gubsky/loomcycle/internal/providers"
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
// Ten ops:
//
//	self        — identity bundle (agent name, run/agent ids, user, tenant,
//	              tier, resolved provider + model + sampling; the `principal`
//	              credential block — subject/tenant/scopes/token id, never the
//	              bearer; and the `server` it's connected to — listen_addr + url)
//	tools       — post-filter tool catalog
//	doc         — detailed schema/docstring for one tool by name
//	permissions — gates that apply to the caller (tool ACL, host policy, scopes)
//	agents / lineage / evaluations / channels / help / time
//
// (A former `history` op was removed — superseded by the standalone History
// tool, which adds owner scopes + listing/search/annotation; see its docs.)
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

	// Help is the loaded topic registry — bundled defaults overlaid
	// with operator-supplied topics from LOOMCYCLE_HELP_ROOT. nil =
	// the `help` op refuses with "not configured" (e.g. a test
	// fixture that didn't wire it).
	Help *help.Set

	// Embedder powers the `help` op's `query` search mode (RFC BL P1) — the
	// SAME embedder the Memory tool uses. When nil (or the store has no vector
	// support), help query degrades to a substring scan over the in-memory
	// Help set, so query still works. Late-bound in main.go alongside Store.
	Embedder providers.Embedder
}

const contextDescription = `Read-only runtime introspection. ` +
	`Answers "what tools do I have? who am I? what permissions apply to me? ` +
	`what other agents exist? what's my def's lineage and evaluation history? ` +
	`what runtime concepts and recipes does loomcycle document? what time is it / how long have I been running?". ` +
	`Operations: self, tools, doc, permissions, agents, lineage, evaluations, channels, help, time. ` +
	`(To browse/search/read past chats, use the History tool, not this one.) ` +
	`Always safe to call — no side effects, no storage writes, no network calls. ` +
	`Useful for self-evolving agents that build their own task plans and want to inspect ` +
	`their environment before deciding what to do. ` +
	`Tip: start with op=help (no topic) to see the topic index, then op=help with topic=<name> ` +
	`for narrative guidance on cross-cutting patterns like scopes, sub-agents, experimentation. ` +
	`Or op=help with query=<text> to search topic sections directly, then fetch the winning topic.`

const contextInputSchema = `{
  "type": "object",
  "properties": {
    "op":              {"type": "string", "enum": ["self","tools","doc","permissions","agents","lineage","evaluations","channels","help","time","compact"], "description": "Which introspection op to run."},
    "name":            {"type": "string", "description": "doc only: the tool name to fetch detailed docs for."},
    "prefix":          {"type": "string", "description": "agents / channels: optional name prefix filter."},
    "def_id":          {"type": "string", "description": "lineage / evaluations: the agent_defs row id to inspect. Use Context.agents to discover def_ids first."},
    "depth":           {"type": "integer", "description": "lineage only: max depth to walk in each direction (default 10, cap 100)."},
    "include_lineage": {"type": "boolean", "description": "evaluations only: include ancestors' evaluations in the aggregate (default false)."},
    "topic":           {"type": "string", "description": "help only: the topic name to fetch detailed content for. Omitted = return the topic index (name + description for each available topic)."},
    "query":           {"type": "string", "description": "help only: hybrid search across help topic SECTIONS; returns the top matches as {topic_slug, heading, snippet, score}. Then fetch a match's full content with topic=<topic_slug>. When set, topic is ignored."}
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
	Topic          string `json:"topic,omitempty"`
	Query          string `json:"query,omitempty"`
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
	case "channels":
		return c.execChannels(ctx, in)
	case "help":
		return c.execHelp(ctx, in)
	case "time":
		return c.execTime(ctx)
	case "compact":
		return c.execCompact(ctx)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: self, tools, doc, permissions, agents, lineage, evaluations, channels, help, time, compact)", in.Op)), nil
	}
}

// ---- compact ----

// execCompact lets an agent proactively compact its OWN context (self-
// compaction — useful for a long autonomous run that's filling its window). It
// sets the loop's compact-request flag; the loop summarizes + replaces the
// conversation at its NEXT iteration boundary (never mid-tool-cycle), honoring
// the agent's keep_last_n / keep_first / target_percentage. Returns immediately
// — the compaction isn't visible until the next turn.
func (c *Context) execCompact(ctx context.Context) (tools.Result, error) {
	flag := tools.CompactRequest(ctx)
	if flag == nil {
		return errResult("context compaction is not available for this run"), nil
	}
	flag.Store(true)
	return okJSON(map[string]any{"compaction": "scheduled", "applies_at": "the next step"})
}

// ---- self ----

func (c *Context) execSelf(ctx context.Context) (tools.Result, error) {
	ident := tools.RunIdentity(ctx)
	out := map[string]any{
		"agent_name": tools.AgentName(ctx),
		"agent_id":   ident.AgentID,
		"user_id":    ident.UserID,
		// tenant_id is the RFC L data-isolation boundary this run acts within
		// (paired with user_id). Non-secret; always stamped on the run identity
		// ("" / "default" for the single-tenant/legacy case). Surfaced so an
		// agent — especially one over the MCP transport — can tell which tenant
		// it's operating as.
		"tenant_id":    ident.TenantID,
		"user_tier":    ident.UserTier,
		"agent_def_id": ident.AgentDefID,
		// run_id is the store row id (r_<hex>). Surfacing it lets
		// agents pass their own run_id through to siblings via
		// Channel messages — the canonical way an Evaluator agent
		// gets the Editor's run_id for `Evaluation.submit run_id=…`.
		"run_id": tools.RunID(ctx),
		// provider/model are the CURRENTLY-resolved driver + model the
		// agent is running on (tier/effort + fallback). Non-secret — the
		// agent is allowed to know what it is. Empty when the run was
		// started outside the loop's stamping path (e.g. some tests).
		"provider": tools.ResolvedProvider(ctx),
		"model":    tools.ResolvedModel(ctx),
	}
	// sampling: the resolved LLM sampling params (temperature, top_p, …) in
	// effect for this run (per-run > per-agent). Non-secret — the agent is
	// allowed to know how it's being sampled (a self-evolving agent reads this
	// to reason about its own exploration vs. exploitation). Omitted when no
	// sampling is configured (the model sees provider defaults).
	if s := tools.ResolvedSampling(ctx); !s.IsZero() {
		out["sampling"] = s
	}
	// compaction: the resolved context-compaction settings in effect for this run
	// (inherited from the parent + per-run/per-spawn overrides). An agent can read
	// this to decide whether to self-compact (Context op=compact). Omitted when
	// no compaction is configured.
	if cp := tools.CompactionPolicy(ctx); !cp.IsZero() {
		out["compaction"] = cp
	}
	// context: how full the window is as of the last completed turn (used =
	// input + cache tokens; max = the model's window, 0/absent when unknown e.g.
	// Ollama). Paired with `compaction` above, this is what an agent needs to
	// make a conscious self-compact decision (e.g. "used_pct >= autocompact_at_pct
	// → call Context op=compact now"). Omitted before the first turn completes.
	if u := tools.ContextUsage(ctx); u.Used > 0 {
		usage := map[string]any{"used_tokens": u.Used}
		if u.Max > 0 {
			usage["max_tokens"] = u.Max
			usage["used_pct"] = u.Used * 100 / u.Max
		}
		out["context"] = usage
	}
	// volumes: the filesystem volumes (RFC AH) the file/exec tools enforce for
	// this run. Each entry names a root + mode (ro/rw) + whether it's the
	// default for an omitted `volume` arg. Paths passed to Read/Write/Edit/
	// Glob/Grep and Bash commands resolve RELATIVE to a volume's root (~ is
	// not expanded; an absolute path must resolve inside the root). Surfaced
	// so a bound agent knows precisely which volumes it may touch and which
	// verb each allows, instead of guessing host paths.
	//
	// Active volume policy → report the binding list (an active-but-empty
	// policy reports an empty bindings list = confined to no volume).
	// Inactive (no policy) → the agent has NO filesystem access (RFC AH
	// Phase 3 sandbox-by-default; the legacy jail is gone), reported so the
	// model knows file/exec tools will refuse rather than guessing host paths.
	if vp := tools.VolumePolicy(ctx); vp.Active {
		vols := make([]map[string]any, 0, len(vp.Bindings))
		for _, b := range vp.Bindings {
			mode := "rw"
			if b.ReadOnly {
				mode = "ro"
			}
			vols = append(vols, map[string]any{
				"name":    b.Name,
				"path":    b.Root,
				"mode":    mode,
				"default": b.Default,
			})
		}
		out["volumes"] = map[string]any{
			"bindings":        vols,
			"path_convention": "Pass paths RELATIVE to a volume root (e.g. \"src/main.go\"); ~ is not expanded and an absolute path must resolve inside the root. Set the \"volume\" tool argument to target a non-default volume; omit it to use the one marked default.",
		}
	} else {
		out["filesystem"] = "none — no volume bound; Read/Write/Edit/Glob/Grep/Bash refuse. Bind a volume via the agent's `volumes:` list (operator declares the universe in the top-level `volumes:` config)."
	}
	// network: the host allowlist the HTTP/WebFetch/WebSearch tools enforce for
	// this run, so an agent knows which hosts it may reach instead of probing
	// blind. A per-run caller list (allowed_hosts on POST /v1/runs) wins; else
	// the operator's static allowlist is the floor. allowed_hosts is suffix-
	// matched; an empty list means no web egress (deny-all). Omitted only when
	// there's no policy to report (no caller list and Cfg is nil).
	hp := tools.HostPolicy(ctx)
	if hp.HasList {
		out["network"] = map[string]any{
			"allowed_hosts": nonNilStrings(hp.AllowedHosts),
			"source":        "caller",
		}
	} else if c.Cfg != nil {
		out["network"] = map[string]any{
			"allowed_hosts": nonNilStrings(c.Cfg.Env.HTTPHostAllowlist),
			"source":        "operator_default",
		}
	}
	// principal: the resolved auth identity (RFC L) — WHO this run acts as and
	// what its credential may do. Non-secret: subject + tenant + scopes + the
	// token DEF id and the 6-char log-correlation suffix; NEVER the bearer
	// itself. Present when the run carries an authenticated principal (every MCP
	// / authed-HTTP path — the case where an agent needs to identify its own
	// credentials); omitted in open mode (no auth configured), where the flat
	// tenant_id / user_id above are still the identity.
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		out["principal"] = map[string]any{
			"tenant_id":    p.TenantID,
			"subject":      p.Subject,
			"scopes":       nonNilStrings(p.Scopes),
			"is_admin":     auth.HasScope(p.Scopes, auth.ScopeAdmin),
			"legacy":       p.Legacy,
			"token_def_id": p.TokenDefID,
			"token_suffix": p.TokenSuffix,
		}
	}
	// server: which loomcycle instance this run is on, so an agent (especially an
	// MCP client) can identify the server it's connected to. listen_addr is the
	// bind address; url is the operator's advertised public base URL
	// (LOOMCYCLE_PUBLIC_URL, falling back to the A2A advertise URL) when set.
	if c.Cfg != nil {
		server := map[string]any{}
		if c.Cfg.Env.ListenAddr != "" {
			server["listen_addr"] = c.Cfg.Env.ListenAddr
		}
		if u := c.Cfg.Env.PublicURL; u != "" {
			server["url"] = u
		} else if u := c.Cfg.Env.A2APublicBaseURL; u != "" {
			server["url"] = u
		}
		if len(server) > 0 {
			out["server"] = server
		}
	}
	return okJSON(out)
}

// nonNilStrings normalises a nil slice to an empty one so the JSON encodes as
// [] (an explicit "no hosts" / deny-all signal) rather than null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// ---- time ----

// execTime gives the agent a clock (RFC S / F34). Without it an agent
// can't compute a deadline, bucket a periodic cycle, or build a
// `deliver_at` for Channel.publish's deferred-visibility timer — it
// would have to shell out to Bash `date`.
//
// now_rfc3339 uses RFC3339Nano (UTC) to match the format Channel emits
// for published_at / visible_at, so an agent can compare a message
// timestamp against "now" without reformatting. run_started_at /
// elapsed_ms come from providers.RunMeta.StartedAt — the wall-clock
// anchor the loop stamps once per run (the same anchor code-js uses) —
// and are OMITTED when RunMeta is absent or unstamped rather than
// fabricated from a zero epoch.
//
// Determinism (code-js / replay): execTime reads time.Now() on first
// execution only. Code-js runs replay tool RESULTS from the transcript
// rather than re-invoking tools, so a recorded op=time result is
// reproduced verbatim on replay — there's no need to special-case a
// pinned clock here. Context stays side_effect_class "pure" (reading a
// clock mutates nothing).
func (c *Context) execTime(ctx context.Context) (tools.Result, error) {
	now := time.Now().UTC()
	out := map[string]any{
		"now_rfc3339": now.Format(time.RFC3339Nano),
		"unix_ms":     now.UnixMilli(),
	}
	if meta, ok := providers.RunMetaFromContext(ctx); ok && !meta.StartedAt.IsZero() {
		started := meta.StartedAt.UTC()
		out["run_started_at"] = started.Format(time.RFC3339Nano)
		out["elapsed_ms"] = now.Sub(started).Milliseconds()
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
			return errResult(fmt.Sprintf("doc: tool %q is not in this agent's tools", in.Name)), nil
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
	sdPol := tools.SkillPolicy(ctx)
	evPol := tools.EvaluationPolicy(ctx)

	out := map[string]any{
		"tools": tools.AgentTools(ctx),
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
		"skills":            sdPol.Patterns,
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
		Tier        string `json:"tier,omitempty"`
		Model       string `json:"model,omitempty"`
		Provider    string `json:"provider,omitempty"`
		ActiveDefID string `json:"active_def_id,omitempty"`
		Tools       int    `json:"tools_count"`
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
			Name:     name,
			Tier:     def.Tier,
			Model:    def.Model,
			Provider: def.Provider,
			Tools:    len(def.Tools),
		}
		// Active def_id from the v0.8.5 substrate. Best-effort — if
		// Store is nil or no active row, we omit the field. Non-
		// NotFound errors are unusual but possible (e.g. DB connection
		// drop mid-call); surface them as a per-name `error` key + log
		// so operators see the partial failure without losing the rest
		// of the catalog. PR 2 review fix.
		if c.Store != nil {
			// RFC N: read the active pointer within the agent's own tenant
			// (from the authoritative run identity in ctx; "" = shared).
			row, err := c.Store.AgentDefGetActive(ctx, tools.RunIdentity(ctx).TenantID, name)
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

// ---- channels ----

func (c *Context) execChannels(ctx context.Context, in contextInput) (tools.Result, error) {
	pol := tools.ChannelPolicy(ctx)
	type channelSummary struct {
		Name        string `json:"name"`
		Scope       string `json:"scope,omitempty"`
		Semantic    string `json:"semantic,omitempty"`
		DefaultTTL  int    `json:"default_ttl,omitempty"`
		MaxMessages int    `json:"max_messages,omitempty"`
		Publisher   string `json:"publisher,omitempty"`
		Publish     bool   `json:"publish"`
		Subscribe   bool   `json:"subscribe"`
	}
	// Union of operator-declared channels + the caller's publish /
	// subscribe lists. Operator may declare channels the agent has no
	// ACL on — surfaced so the agent sees what exists even when it
	// can't write/read (good for "what bus does the operator run?"
	// discovery).
	allNames := make(map[string]bool, len(pol.Channels))
	for n := range pol.Channels {
		allNames[n] = true
	}
	// publish/subscribe lists may contain wildcards (e.g. `findings/*`);
	// those don't expand into the result set here — we surface the
	// declared channels only. The bools below reflect whether the
	// exact name appears in either list.
	publishSet := stringSet(pol.Publish)
	subscribeSet := stringSet(pol.Subscribe)

	out := make([]channelSummary, 0, len(allNames))
	for name := range allNames {
		if in.Prefix != "" && !strings.HasPrefix(name, in.Prefix) {
			continue
		}
		def := pol.Channels[name]
		out = append(out, channelSummary{
			Name:        name,
			Scope:       def.Scope,
			Semantic:    def.Semantic,
			DefaultTTL:  def.DefaultTTL,
			MaxMessages: def.MaxMessages,
			Publisher:   def.Publisher,
			Publish:     publishSet[name],
			Subscribe:   subscribeSet[name],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return okJSON(map[string]any{
		"channels":            out,
		"count":               len(out),
		"publish_wildcards":   filterWildcards(pol.Publish),
		"subscribe_wildcards": filterWildcards(pol.Subscribe),
	})
}

func stringSet(xs []string) map[string]bool {
	out := make(map[string]bool, len(xs))
	for _, x := range xs {
		out[x] = true
	}
	return out
}

// filterWildcards returns only the entries that look like wildcards
// (anchored prefix patterns ending in `/*`). The non-wildcard entries
// are already surfaced via the per-channel publish/subscribe bools.
func filterWildcards(xs []string) []string {
	var out []string
	for _, x := range xs {
		if strings.HasSuffix(x, "/*") {
			out = append(out, x)
		}
	}
	return out
}

// ---- help ----

func (c *Context) execHelp(ctx context.Context, in contextInput) (tools.Result, error) {
	if c.Help == nil {
		return errResult("help: not configured (no Help registry; operator misconfiguration)"), nil
	}
	// Query mode (RFC BL P1): hybrid section search over the help index. Takes
	// precedence over topic — an agent that knows what it's looking for but not
	// which topic searches first, then fetches the winning topic by slug.
	if q := strings.TrimSpace(in.Query); q != "" {
		res, err := help.QueryIndex(ctx, c.Help, c.Store, c.Embedder, q, 0)
		if err != nil {
			return errResult(fmt.Sprintf("help query: %s", err)), nil
		}
		return okJSON(map[string]any{
			"query":   q,
			"mode":    res.Mode, // "hybrid" (indexed) | "substring" (degraded)
			"results": res.Results,
			"count":   len(res.Results),
			"hint":    "Call help with topic=<topic_slug> to read a matching topic's full content.",
		})
	}
	if in.Topic == "" {
		// Index mode: return all topics' name + description +
		// source. Body is intentionally OMITTED so the index stays
		// compact — agents call back with topic=<name> for content.
		type idxEntry struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
		}
		all := c.Help.All()
		out := make([]idxEntry, 0, len(all))
		for _, t := range all {
			out = append(out, idxEntry{
				Name:        t.Name,
				Description: t.Description,
				Source:      t.Source,
			})
		}
		return okJSON(map[string]any{
			"topics": out,
			"count":  len(out),
			"hint":   "Call help with topic=<name> to read a topic's full content.",
		})
	}
	t, ok := c.Help.Get(in.Topic)
	if !ok {
		// Surface the index in the error so the model can self-
		// correct without a second round-trip.
		return errResult(fmt.Sprintf("help: topic %q not found (available: %s)", in.Topic, strings.Join(c.Help.Names(), ", "))), nil
	}
	return okJSON(map[string]any{
		"name":        t.Name,
		"description": t.Description,
		"content":     t.Content,
		"source":      t.Source,
	})
}

var _ tools.Tool = (*Context)(nil)
