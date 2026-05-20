// Package tools defines the Tool interface and the dispatcher that routes
// tool_use calls from the model to a built-in or MCP-backed implementation.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// Tool is one tool the agent can invoke. The Name is what the model sees and
// what allowlists are matched against. The InputSchema is JSON Schema; the
// dispatcher passes the raw model-generated input straight through.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, input json.RawMessage) (Result, error)
}

// Result is the output of one tool invocation. Text is the human-readable
// payload the model will see in the next tool_result block. IsError flags a
// failed execution (the model should self-correct, not surface to the user).
type Result struct {
	Text    string
	IsError bool
}

// Spec converts a Tool to the providers.ToolSpec the model receives.
func Spec(t Tool) providers.ToolSpec {
	return providers.ToolSpec{
		Name:        t.Name(),
		Description: t.Description(),
		InputSchema: t.InputSchema(),
	}
}

// Dispatcher resolves tool names to Tool implementations and invokes them.
// A new Dispatcher is built per run with the run's allowed-tools list so
// off-policy calls fail fast.
type Dispatcher struct {
	tools    map[string]Tool
	fallback FallbackFunc
}

// FallbackFunc is consulted by Dispatcher.Execute when a tool name isn't
// in the static map. It returns (Result, true) when it has a definitive
// outcome for the call (success OR a typed error to surface to the model);
// (zero, false) means "I don't know about this name, fall through to
// the dispatcher's default 'tool not found' error".
//
// Used to implement lazy MCP server registration: a configured server
// that failed initial handshake registers no tools at boot, but a
// fallback can detect mcp__<server>__<tool> names, retry the handshake,
// register the tools, and dispatch. Memoising successful resolutions
// in the fallback avoids re-handshaking on every subsequent call.
type FallbackFunc func(ctx context.Context, name string, input json.RawMessage) (Result, bool)

// NewDispatcher builds a dispatcher from the given tools. No fallback —
// unknown names always return "tool not found".
func NewDispatcher(tools []Tool) *Dispatcher {
	m := make(map[string]Tool, len(tools))
	for _, t := range tools {
		m[t.Name()] = t
	}
	return &Dispatcher{tools: m}
}

// NewDispatcherWithFallback is NewDispatcher plus a FallbackFunc consulted
// for unknown names before returning "tool not found".
func NewDispatcherWithFallback(tools []Tool, fallback FallbackFunc) *Dispatcher {
	d := NewDispatcher(tools)
	d.fallback = fallback
	return d
}

// Specs returns the providers.ToolSpec slice for all registered tools, in the
// order they were passed to NewDispatcher (map iteration would be non-deterministic).
func (d *Dispatcher) Specs(order []Tool) []providers.ToolSpec {
	out := make([]providers.ToolSpec, 0, len(order))
	for _, t := range order {
		if _, ok := d.tools[t.Name()]; ok {
			out = append(out, Spec(t))
		}
	}
	return out
}

// ctxKeyAgentTools is the context key under which the runtime stores
// the calling agent's effective allowed_tools list (after agent + caller
// narrowing). Tools that need to apply secondary subset checks (like
// the built-in Skill tool, which validates skill `allowed-tools` ⊆
// agent `allowed_tools` at call time) read it via AgentTools.
type ctxKeyAgentTools struct{}

// WithAgentTools attaches the agent's effective tool names to ctx. The
// HTTP server calls this once per run before invoking the loop so any
// tool that resolves dynamic permissions has the same view of "what
// the agent can use."
func WithAgentTools(ctx context.Context, names []string) context.Context {
	return context.WithValue(ctx, ctxKeyAgentTools{}, names)
}

// AgentTools returns the agent's effective tool names from ctx, or
// nil if not attached. Returning nil from a tool that requires this
// list should cause the tool to refuse with a clear "misconfigured
// runtime" message.
func AgentTools(ctx context.Context) []string {
	v, _ := ctx.Value(ctxKeyAgentTools{}).([]string)
	return v
}

// ctxKeyHostPolicy is the context key for the run's effective HTTP
// host narrowing policy — what the CALLER (top-level: HTTP request
// body; sub-agent: inherited from the parent's ctx) asked for in
// allowed_hosts / web_search_filter. Sub-agents read this via
// HostPolicy and re-apply the parent's narrowing to their own tools,
// so a parent that worked against ["localhost"] under
// CALLER_AUTHORITATIVE doesn't spawn children that mysteriously fall
// back to the operator's static allowlist (which typically doesn't
// include localhost). See server.runSubAgent.
type ctxKeyHostPolicy struct{}

// HostPolicyValue captures the caller-authoritative HTTP host policy.
//
// HasList distinguishes "caller didn't supply a list at all" (false:
// fall back to operator's static allowlist) from "caller supplied a
// list, possibly empty" (true: the list IS the policy, deny-all if
// empty). The two cases are different in CALLER_AUTHORITATIVE mode:
// nil → operator's static list; empty → deny everything.
type HostPolicyValue struct {
	AllowedHosts    []string
	HasList         bool
	WebSearchFilter string
}

// WithHostPolicy attaches the caller's host narrowing policy to ctx.
// runRequest sets this once for top-level runs; sub-agents inherit it
// via the ctx chain (Agent tool's Execute → runSubAgent passes the
// parent's ctx through).
func WithHostPolicy(ctx context.Context, p HostPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyHostPolicy{}, p)
}

// HostPolicy returns the caller's host narrowing policy from ctx, or
// the zero value (HasList=false, no narrowing) if not attached.
func HostPolicy(ctx context.Context) HostPolicyValue {
	v, _ := ctx.Value(ctxKeyHostPolicy{}).(HostPolicyValue)
	return v
}

// ctxKeyExtraAllowedHosts is the context key for v0.8.17 per-call
// host-widening grants from permitted Pre-hooks. Distinct from
// HostPolicy: HostPolicy carries the caller-authoritative narrowing
// that applies for the entire run and is inherited by sub-agents.
// ExtraAllowedHosts is per-tool-call only — set by loop.dispatchOneTool
// for the SINGLE Execute() call after a permitted Pre-hook returned
// allow_hosts, then dropped when control returns to the loop.
// Sub-agents do NOT inherit it; the v1 scope is one Execute() per
// hook callback (CLAUDE.md confused-deputy guidance).
type ctxKeyExtraAllowedHosts struct{}

// WithExtraAllowedHosts derives a ctx with the per-call host-widening
// grants attached. extras is the deduplicated list from the
// dispatcher's PreOutcome.AllowHosts. Matching semantics live in the
// enforcement-site helper (httptool.hostAllowedWithExtras): bare
// entries are exact-match; entries with a leading dot are suffix-match.
//
// Calling this with a nil / empty slice is a no-op (returns the
// input ctx unchanged) so the loop can call it unconditionally
// without paying for an extra ctx allocation per tool call.
func WithExtraAllowedHosts(ctx context.Context, extras []string) context.Context {
	if len(extras) == 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyExtraAllowedHosts{}, extras)
}

// ExtraAllowedHosts returns the per-call host-widening grants from
// ctx, or nil when nothing was attached. Tools that enforce a host
// allowlist consult this AFTER their operator-floor + caller-narrowed
// check fails, allowing the per-call grant to widen JUST this call.
func ExtraAllowedHosts(ctx context.Context) []string {
	v, _ := ctx.Value(ctxKeyExtraAllowedHosts{}).([]string)
	return v
}

// ctxKeyRunIdentity is the context key under which the runtime
// stashes the current run's user_id and agent_id (v0.4 tracking
// fields). Sub-agents read these via RunIdentity to inherit the
// parent's user_id and to know whose agent_id is their parent.
type ctxKeyRunIdentity struct{}

// RunIdentityValue is the shape stored in ctx by WithRunIdentity. The
// "Value" suffix avoids a naming collision with store.RunIdentity
// (which is the persistence-layer struct with more fields).
type RunIdentityValue struct {
	UserID  string
	AgentID string
	// UserTier is the v0.8.2 user-facing-tier policy name applied to
	// this run (e.g. "free" / "low" / "medium" / "high"). Sub-agents
	// inherit it via ctx so the resolver applies the same tier
	// overlay for child runs without the caller re-supplying it.
	// Empty when the operator has no user_tiers block or the caller
	// didn't pass user_tier.
	UserTier string
	// AgentDefID is the v0.8.5 substrate def_id pinned to THIS run
	// (when set via the Agent tool's def_id parameter or when the
	// resolver picked a non-static active def at run start). Empty
	// for static-only runs (where the agent body came from
	// cfg.Agents alone). The Context.self op surfaces it so agents
	// can identify "which version of myself am I running" without a
	// store roundtrip.
	AgentDefID string
	// UserBearer is the v0.8.x per-run MCP bearer token supplied by
	// the caller on the run request (wire field "user_bearer"). The
	// HTTP MCP transport substitutes it into header values containing
	// ${run.user_bearer} at outbound request-build time. Sub-agents
	// inherit it identically (NOT narrowed — they act on behalf of
	// the same end-user). Never persisted; never logged in full.
	UserBearer string
}

// WithRunIdentity attaches the current run's identity to ctx. The
// HTTP server calls this once per run before invoking the loop so the
// AgentTool's SubAgentRunner can read it back via RunIdentity and
// thread userID/parentAgentID through to the new sub-agent's session
// + run records.
func WithRunIdentity(ctx context.Context, ident RunIdentityValue) context.Context {
	return context.WithValue(ctx, ctxKeyRunIdentity{}, ident)
}

// RunIdentity returns the current run's identity from ctx, or zero
// value if not attached. The HTTP server's runSubAgent uses the
// AgentID as the new sub-run's parent_agent_id and the UserID for
// inheritance into the sub-agent's session.
func RunIdentity(ctx context.Context) RunIdentityValue {
	v, _ := ctx.Value(ctxKeyRunIdentity{}).(RunIdentityValue)
	return v
}

// ctxKeyAgentName is the context key under which the runtime stores the
// yaml-declared agent name (e.g. "qa-agent", "company-researcher"). The
// Memory tool reads this to resolve the `agent` scope_id; other future
// tools that need to namespace state by agent can use it the same way.
type ctxKeyAgentName struct{}

// WithAgentName attaches the agent's yaml-declared name to ctx.
// loop.Run threads opts.AgentName through, but ctx-level access lets
// tools read it without plumbing the value through every Execute
// signature. Empty string is acceptable — tools that need the value
// must validate.
func WithAgentName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxKeyAgentName{}, name)
}

// AgentName returns the yaml-declared agent name from ctx, or empty
// string if not attached.
func AgentName(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyAgentName{}).(string)
	return v
}

// ctxKeyMemoryPolicy is the context key for the per-agent Memory tool
// policy (allowed scopes + scope-byte quota override). Set by the HTTP
// server from the agent's yaml definition; read by the Memory tool to
// gate writes and surface "scope not allowed" refusals to the model.
type ctxKeyMemoryPolicy struct{}

// MemoryPolicyValue is the per-agent Memory access policy.
//
//   - AllowedScopes is the yaml `memory_scopes` allowlist; an empty
//     slice means the agent has NO Memory access (the tool itself
//     must already be in allowed_tools for the agent to even call it,
//     but the scope allowlist is a second gate that lets operators
//     grant Memory:read-only on `user` while withholding `agent`).
//   - QuotaBytes is the yaml `memory_quota_bytes` override; 0 falls
//     back to the global LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES default.
type MemoryPolicyValue struct {
	AllowedScopes []string
	QuotaBytes    int
}

// WithMemoryPolicy attaches the agent's resolved Memory policy to ctx.
func WithMemoryPolicy(ctx context.Context, p MemoryPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyMemoryPolicy{}, p)
}

// MemoryPolicy returns the agent's Memory policy from ctx. Zero value
// (empty AllowedScopes, QuotaBytes=0) means "no Memory access".
func MemoryPolicy(ctx context.Context) MemoryPolicyValue {
	v, _ := ctx.Value(ctxKeyMemoryPolicy{}).(MemoryPolicyValue)
	return v
}

// ctxKeyChannelPolicy is the context key for the per-agent Channel
// tool ACL (v0.8.4). Set by the HTTP server from the agent's yaml
// definition; read by the Channel tool to gate publish/subscribe and
// surface "channel not allowed" refusals to the model. Sub-agents
// inherit the parent's policy via this ctx key just like
// MemoryPolicy and HostPolicy.
type ctxKeyChannelPolicy struct{}

// ChannelPolicyValue is the per-agent Channel access policy.
//
//   - Publish is the operator-yaml `channels.publish` allowlist —
//     channel names (with optional trailing "/*" wildcard) the agent
//     may post to.
//   - Subscribe is the matching allowlist for subscribe / peek / ack.
//   - Channels is a snapshot of the operator's `channels:` block,
//     keyed by channel name. Carries per-channel defaults (scope,
//     default_ttl, max_messages, semantic) so the tool layer can
//     resolve them without round-tripping through config.
//
// Empty Publish / Subscribe means "no channel access on that side"
// — the tool returns a typed refusal with the allowlist enumerated.
type ChannelPolicyValue struct {
	Publish   []string
	Subscribe []string
	Channels  map[string]ChannelDef
}

// ChannelDef mirrors config.Channel for the tool layer. Lives here
// (not in config) so the dependency arrow stays internal/config →
// internal/tools, not the reverse.
type ChannelDef struct {
	Name        string
	Scope       string // "agent" | "user" | "global"
	DefaultTTL  int    // seconds; 0 = no default
	MaxMessages int    // 0 = unbounded (overflow trim disabled)
	Semantic    string // "queue" | "broadcast" (informational; storage shape identical)
	// v0.8.6: when "system", agent publishes are refused; only the
	// internal Go publisher and the admin endpoint may write. Empty =
	// agents may publish (ACL permitting).
	Publisher string
}

// WithChannelPolicy attaches the agent's resolved Channel policy to ctx.
func WithChannelPolicy(ctx context.Context, p ChannelPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyChannelPolicy{}, p)
}

// ChannelPolicy returns the agent's Channel policy from ctx. Zero
// value (nil Publish/Subscribe, empty Channels) means "no Channel
// access" — the tool surfaces this as a clear "not configured"
// refusal so operators see one explicit failure instead of a
// stack-trace.
func ChannelPolicy(ctx context.Context) ChannelPolicyValue {
	v, _ := ctx.Value(ctxKeyChannelPolicy{}).(ChannelPolicyValue)
	return v
}

// InterruptionPolicyValue is the per-agent Interruption access
// policy (v0.8.16). Default-deny: an absent block means Enabled is
// false and every Interruption op returns is_error with a clear
// "not enabled" message.
//
//   - Enabled gates the tool entirely. False → tool returns is_error
//     on every op.
//   - Kinds is the allowlist of interrupt kinds this agent may
//     create. v0.8.16 supports only "question"; future "pause" /
//     "wait_until" / "approval" will land here as additive opt-ins.
//     Empty means default (["question"]) when Enabled=true.
//   - MaxPending caps the number of pending interrupts this run may
//     hold simultaneously. 0 = use the operator's global default
//     (LOOMCYCLE_INTERRUPTION_MAX_PENDING_PER_RUN).
type InterruptionPolicyValue struct {
	Enabled    bool
	Kinds      []string
	MaxPending int
}

type ctxKeyInterruptionPolicy struct{}

// WithInterruptionPolicy attaches the agent's resolved Interruption
// policy to ctx. The HTTP server's RunOnce / handleRuns wraps the
// loop ctx with this so any Interruption.Execute downstream can
// recover the policy in one call.
func WithInterruptionPolicy(ctx context.Context, p InterruptionPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyInterruptionPolicy{}, p)
}

// InterruptionPolicy returns the agent's Interruption policy from
// ctx. Zero value (Enabled=false) means "not enabled" — the tool
// surfaces this as a clear refusal so operators see one explicit
// failure instead of a stack trace.
func InterruptionPolicy(ctx context.Context) InterruptionPolicyValue {
	v, _ := ctx.Value(ctxKeyInterruptionPolicy{}).(InterruptionPolicyValue)
	return v
}

// ctxKeyDispatcher is the context key carrying the run's tool
// dispatcher (v0.8.16). The Interruption tool's mcp_server delivery
// backend uses this to call the consumer's `mcp__<name>__ask` tool
// without re-implementing dispatch + retry + bearer substitution.
//
// Nil-safe — the helper Dispatcher(ctx) returns nil when no
// dispatcher is attached; the Interruption tool falls back to its
// webui code path when the operator config requests
// mcp_server:<name> but no dispatcher is available.
type ctxKeyDispatcher struct{}

// WithDispatcher attaches the run's Dispatcher to ctx. Called at run
// start in the HTTP server (same locations as WithRunID /
// WithRunIdentity).
func WithDispatcher(ctx context.Context, d *Dispatcher) context.Context {
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyDispatcher{}, d)
}

// DispatcherFromCtx returns the run's Dispatcher from ctx, or nil
// when none was attached.
func DispatcherFromCtx(ctx context.Context) *Dispatcher {
	v, _ := ctx.Value(ctxKeyDispatcher{}).(*Dispatcher)
	return v
}

// ctxKeyRunID is the context key under which the runtime stores
// the current run's store row ID. The Interruption tool uses this
// at create time to associate the interrupt row with the run for
// listing and ACL purposes — the model never supplies it, so it
// must travel via ctx alongside RunIdentity (which carries the
// agent-facing IDs; run.ID is operator-facing and stays out of the
// model's view).
type ctxKeyRunID struct{}

// WithRunID attaches the run's store row ID to ctx. Same call site
// as WithRunIdentity (HTTP handler at run start).
func WithRunID(ctx context.Context, runID string) context.Context {
	if runID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyRunID{}, runID)
}

// RunID returns the run row ID from ctx, or "" when unset.
func RunID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyRunID{}).(string)
	return v
}

// ctxKeyEventEmitter is the context key for the v0.8.4 typed-audit-
// event emitter. The loop's OnEvent callback is attached at run
// start; tools that want to surface structured wire events (e.g.
// the Channel tool's EventChannelPublish / EventChannelDelivery)
// retrieve it via EventEmitter(ctx) and call it synchronously.
//
// nil is the no-op shape — when no emitter is attached (most
// short-lived contexts, including unit-test ctx), tools should
// silently skip emission. EventEmitter never panics; it returns
// a guaranteed-callable function (no-op when none was attached).
type ctxKeyEventEmitter struct{}

// EventEmitterFunc is the callback shape tools invoke to push a
// typed event onto the run's SSE stream. Same signature as
// loop.RunOptions.OnEvent.
type EventEmitterFunc func(providers.Event)

// WithEventEmitter attaches an emitter to ctx. The HTTP server
// (Server.handleRuns and friends) calls this with the loop's
// `emit` closure so any tool downstream can surface a typed
// event onto the same SSE stream the loop uses. Sub-agent contexts
// inherit the parent's emitter automatically — the parent and
// child run share the same SSE stream.
func WithEventEmitter(ctx context.Context, fn EventEmitterFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyEventEmitter{}, fn)
}

// EventEmitter returns the emitter from ctx, or a no-op function
// when none was attached. Callers can invoke the result directly
// without nil-checking:
//
//	tools.EventEmitter(ctx)(providers.Event{Type: providers.EventChannelPublish, ...})
func EventEmitter(ctx context.Context) EventEmitterFunc {
	if fn, ok := ctx.Value(ctxKeyEventEmitter{}).(EventEmitterFunc); ok && fn != nil {
		return fn
	}
	return func(providers.Event) {}
}

// ctxKeyAgentDefPolicy carries the v0.8.5 AgentDef-tool capability
// gate. Mirrors MemoryPolicy / ChannelPolicy shape.
type ctxKeyAgentDefPolicy struct{}

// AgentDefPolicyValue is the per-agent AgentDef-tool access policy.
//
//   - Scopes is the operator-yaml agent_def_scopes list. Closed set:
//     "self" / "descendants" / "any" / "named:<name>". Empty =
//     default-deny.
//
// Auxiliary identity fields stamped server-side at ctx-attach so the
// AgentDef tool can resolve self / siblings / descendants without
// re-reading ctx values that may have been overwritten by sub-agent
// chains:
//
//   - SelfName is the yaml agent name (== tools.AgentName(ctx) at
//     attach time). Used for the "self" scope check.
type AgentDefPolicyValue struct {
	Scopes   []string
	SelfName string
}

// WithAgentDefPolicy attaches the policy to ctx.
func WithAgentDefPolicy(ctx context.Context, p AgentDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyAgentDefPolicy{}, p)
}

// AgentDefPolicy returns the policy from ctx. Zero value = no
// access (default-deny — the tool refuses every mutation op until
// scopes are explicitly granted via yaml).
func AgentDefPolicy(ctx context.Context) AgentDefPolicyValue {
	v, _ := ctx.Value(ctxKeyAgentDefPolicy{}).(AgentDefPolicyValue)
	return v
}

// ctxKeySkillDefPolicy carries the v0.8.22 SkillDef-tool capability
// gate. Mirrors AgentDefPolicy shape, sans the SelfName field —
// skills have no agent identity so a "self" scope is meaningless.
type ctxKeySkillDefPolicy struct{}

// SkillDefPolicyValue is the per-agent SkillDef-tool access policy.
//
//   - Scopes is the operator-yaml skill_def_scopes list. Closed set:
//     "any" / "descendants" / "named:<skill-name>". Empty =
//     default-deny.
//
// `descendants` is reserved for symmetry with AgentDefPolicy and
// currently behaves equivalent to "any" pending lineage-walk
// implementation (same v0.9.x TODO as AgentDef).
type SkillDefPolicyValue struct {
	Scopes []string
}

// WithSkillDefPolicy attaches the policy to ctx.
func WithSkillDefPolicy(ctx context.Context, p SkillDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeySkillDefPolicy{}, p)
}

// SkillDefPolicy returns the policy from ctx. Zero value = no
// access (default-deny — the tool refuses every mutation op until
// scopes are explicitly granted via yaml).
func SkillDefPolicy(ctx context.Context) SkillDefPolicyValue {
	v, _ := ctx.Value(ctxKeySkillDefPolicy{}).(SkillDefPolicyValue)
	return v
}

// ctxKeyEvaluationPolicy carries the v0.8.5 Evaluation-tool gate.
type ctxKeyEvaluationPolicy struct{}

// EvaluationPolicyValue is the per-agent Evaluation policy.
// Multi-select scopes; default-deny when Scopes is empty. See
// config.AgentDef.EvaluationScopes docstring for the closed set.
type EvaluationPolicyValue struct {
	Scopes []string
}

// WithEvaluationPolicy attaches the policy to ctx.
func WithEvaluationPolicy(ctx context.Context, p EvaluationPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyEvaluationPolicy{}, p)
}

// EvaluationPolicy returns the policy from ctx. Zero value =
// no access.
func EvaluationPolicy(ctx context.Context) EvaluationPolicyValue {
	v, _ := ctx.Value(ctxKeyEvaluationPolicy{}).(EvaluationPolicyValue)
	return v
}

// ctxKeyHistoryPolicy is the key under which the runtime stashes the
// v0.8.7 Context.history scope policy. Multi-select scopes; same shape
// as AgentDefPolicy and EvaluationPolicy. The closed set lives on
// config.AgentDef.HistoryScope.
type ctxKeyHistoryPolicy struct{}

// HistoryPolicyValue is the per-agent Context.history gate. Multi-
// select; empty Scopes = default-deny (Context.history refuses).
//
// Closed set:
//   - "self"        — caller may read its own run's transcript
//   - "siblings"    — RESERVED (not yet active in v0.8.7 PR 3;
//     RunIdentityValue lacks ParentAgentID, so the
//     server can't derive sibling relationships
//     without a separate plumbing PR)
//   - "descendants" — RESERVED (same reason)
//   - "named:<n>"   — RESERVED (same reason)
//   - "any"         — UNRESTRICTED. Caller may read ANY agent's
//     transcript INCLUDING transcripts owned by
//     other users. Operator-trust grant; use only
//     for admin/debug agents.
type HistoryPolicyValue struct {
	Scopes []string
}

// WithHistoryPolicy attaches the policy to ctx.
func WithHistoryPolicy(ctx context.Context, p HistoryPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyHistoryPolicy{}, p)
}

// HistoryPolicy returns the policy from ctx. Zero value = no access.
func HistoryPolicy(ctx context.Context) HistoryPolicyValue {
	v, _ := ctx.Value(ctxKeyHistoryPolicy{}).(HistoryPolicyValue)
	return v
}

// Execute looks up the named tool and runs it. Unknown tool names consult
// the optional Fallback before returning the standard "tool not found"
// error result (the model can self-correct on the error result; we never
// return a hard Go error here).
func (d *Dispatcher) Execute(ctx context.Context, name string, input json.RawMessage) Result {
	if t, ok := d.tools[name]; ok {
		res, err := t.Execute(ctx, input)
		if err != nil {
			return Result{Text: err.Error(), IsError: true}
		}
		return res
	}
	if d.fallback != nil {
		if res, handled := d.fallback(ctx, name, input); handled {
			return res
		}
	}
	return Result{Text: fmt.Sprintf("tool not found: %s", name), IsError: true}
}
