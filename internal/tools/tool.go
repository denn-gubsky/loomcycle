// Package tools defines the Tool interface and the dispatcher that routes
// tool_use calls from the model to a built-in or MCP-backed implementation.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/denn-gubsky/loomcycle/internal/config"
	lcotel "github.com/denn-gubsky/loomcycle/internal/otel"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
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
// the calling agent's effective tools list (after agent + caller
// narrowing). Tools that need to apply secondary subset checks (like
// the built-in Skill tool, which validates skill `allowed-tools` ⊆
// agent `tools` at call time) read it via AgentTools.
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

// ctxKeyVolumePolicy is the context key for the run's effective
// filesystem-volume policy (RFC AH). It mirrors HostPolicy exactly:
// the run-start path resolves the agent's declared `volumes` against
// the operator's top-level `volumes:` config into a binding set and
// attaches it here; the file/exec tools read it via VolumePolicy to
// resolve which root a given call targets; sub-agents read the
// parent's policy from ctx and re-apply NARROW-ONLY narrowing (child ⊆
// parent), the filesystem analog of the host-allowlist narrowing.
//
// A run with NO policy attached (unbound agent, no `volumes:` config)
// falls back to each tool's construction-time Root — the legacy global
// jail — so behaviour is byte-identical to pre-feature deployments.
type ctxKeyVolumePolicy struct{}

// VolumeBinding is one filesystem volume an agent is bound to. Root is
// the already-resolved absolute path (validated to exist + be a dir at
// config-load for static volumes); ReadOnly enforces the ro/rw axis
// (Write/Edit/NotebookEdit and Bash require ReadOnly=false); Default
// marks the binding used when a tool call omits the `volume` argument.
type VolumeBinding struct {
	Name     string
	Root     string
	ReadOnly bool
	Default  bool
}

// VolumePolicyValue is the run's resolved volume policy.
//
// Active distinguishes "volume confinement is in force" from "no policy",
// which is load-bearing for both spawn confinement and sandbox-by-default
// (RFC AH Phase 3 — the legacy jail is gone):
//   - Active == false (the zero value / never attached): the agent is bound
//     to NO volume, so every file/exec tool REFUSES (no filesystem access).
//     Disk access is an explicit grant — declare a `volumes:` block (with a
//     `default` volume to restore the old single-jail behaviour).
//   - Active == true: the run is confined to Bindings. An Active policy with
//     an EMPTY Bindings slice DENIES every file-tool call (e.g. a sub-agent
//     whose declared volumes share none of the parent's), exactly like the
//     inactive case — there is no longer any fallback root to leak to.
type VolumePolicyValue struct {
	Active   bool
	Bindings []VolumeBinding
}

// WithVolumePolicy attaches the run's volume policy to ctx. It ALWAYS sets
// the value — even an inactive/empty policy must OVERWRITE any policy a child
// ctx inherited from its parent. (A sub-agent narrowed to an empty binding
// set would otherwise keep the parent's policy via ctx-value inheritance, a
// confinement-widening bug.)
func WithVolumePolicy(ctx context.Context, p VolumePolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyVolumePolicy{}, p)
}

// VolumePolicy returns the run's volume policy from ctx, or the zero value
// (Active=false) when none was attached. Active=false is the "no volume
// bound" signal — the file/exec tools refuse (RFC AH Phase 3: no legacy
// fallback root survives).
func VolumePolicy(ctx context.Context) VolumePolicyValue {
	v, _ := ctx.Value(ctxKeyVolumePolicy{}).(VolumePolicyValue)
	return v
}

// ctxKeyEphemeralVolumes carries the run-tree's EphemeralVolumeSet (RFC AH
// Phase 2b). Created ONCE at each top-level run-start and attached to the
// loop ctx; sub-agents inherit the SAME pointer via the ctx chain
// (runSubAgent derives the child ctx from the parent's, so the value flows
// down — and must NOT be overwritten). The set is the run-scoped resolution
// source for ephemeral volumes and is NEVER shared across different
// top-level runs (a fresh set per run-start is the load-bearing isolation
// property — no cross-run leak).
type ctxKeyEphemeralVolumes struct{}

// EphemeralVolumeRef is one resolved ephemeral volume: its on-disk root +
// the ro/rw axis. Stored in the EphemeralVolumeSet keyed by name.
type EphemeralVolumeRef struct {
	Root     string
	ReadOnly bool
}

// EphemeralVolumeSet is the run-tree's in-memory registry of ephemeral
// volumes (RFC AH Phase 2b). It is a POINTER struct shared down the spawn
// tree, so concurrent sub-agents add to and read from the same map under a
// mutex. It is the resolution source effectiveRoot consults FIRST for a
// named volume; it is created fresh per top-level run-start (never reused
// across runs).
type EphemeralVolumeSet struct {
	mu sync.RWMutex
	m  map[string]EphemeralVolumeRef
}

// NewEphemeralVolumeSet returns an empty set ready to share down a run tree.
func NewEphemeralVolumeSet() *EphemeralVolumeSet {
	return &EphemeralVolumeSet{m: make(map[string]EphemeralVolumeRef)}
}

// Add registers (or overwrites) an ephemeral volume by name. Thread-safe —
// concurrent sub-agents may create ephemeral volumes simultaneously.
func (s *EphemeralVolumeSet) Add(name string, ref EphemeralVolumeRef) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]EphemeralVolumeRef)
	}
	s.m[name] = ref
}

// Get returns the ref for name and whether it exists. Nil-safe (a nil set
// reports "not found", so callers don't special-case the no-set path).
func (s *EphemeralVolumeSet) Get(name string) (EphemeralVolumeRef, bool) {
	if s == nil {
		return EphemeralVolumeRef{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ref, ok := s.m[name]
	return ref, ok
}

// Has reports whether name is already registered (used by the create op to
// refuse a duplicate ephemeral name within the same run tree).
func (s *EphemeralVolumeSet) Has(name string) bool {
	_, ok := s.Get(name)
	return ok
}

// WithEphemeralVolumes attaches the run-tree's ephemeral volume set to ctx.
// nil set is a no-op (EphemeralVolumes then returns nil → no ephemeral
// resolution, and the create op refuses with "no ephemeral set on ctx").
func WithEphemeralVolumes(ctx context.Context, set *EphemeralVolumeSet) context.Context {
	if set == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyEphemeralVolumes{}, set)
}

// EphemeralVolumes returns the run-tree's ephemeral volume set from ctx, or
// nil when none was attached. Nil is safe to pass to the set's methods.
func EphemeralVolumes(ctx context.Context) *EphemeralVolumeSet {
	v, _ := ctx.Value(ctxKeyEphemeralVolumes{}).(*EphemeralVolumeSet)
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
	// RootRunID is the id of the TOP-LEVEL run at the root of this spawn
	// tree (RFC AH Phase 2b). At each top-level run-start site it is set to
	// the run's OWN id; a sub-agent INHERITS the parent's RootRunID
	// unchanged (runSubAgent must not overwrite it), so the whole tree
	// shares one root id. Ephemeral filesystem volumes scope to it:
	// resolvable by the entire creating tree, purged when the top-level run
	// completes. Empty for a run started outside the volume-aware run-start
	// path (the ephemeral VolumeDef create op then refuses — no active run).
	RootRunID string
	// TenantID is the RFC L authoritative data-isolation boundary. On
	// authenticated routes it is set from the resolved auth.Principal's
	// TenantID (which overrides the wire tenant_id); for legacy /
	// single-tenant deployments it is "default" (or empty, in which case
	// memory's resolveTenancy backstop applies). It is the key memory
	// tenancy partitions on (NOT UserID) — distinct from the per-actor
	// Subject (which lands in UserID). Sub-agents inherit it unchanged.
	TenantID string
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

	// UserCredentials is the v1.x named-credentials map (RFC F) —
	// per-tool/per-MCP-server bearers keyed by operator-chosen name
	// (convention: the mcp_servers.<name> yaml key). The HTTP MCP
	// transport substitutes values into header expressions of the
	// form ${run.credentials.<name>} at outbound request-build time.
	// Sub-agents inherit the whole map identically (same trust
	// posture as UserBearer).
	//
	// Back-compat with v0.8.x: at WithRunIdentity time, if the
	// caller set UserBearer but did not set UserCredentials["default"],
	// the substrate populates UserCredentials["default"] = UserBearer
	// so the legacy ${run.user_bearer} substitution path and the
	// new ${run.credentials.default} both resolve to the same value.
	//
	// Validation: keys match [a-zA-Z0-9_-]{1,64}; values arbitrary
	// strings (no length cap — operators occasionally pass JWTs).
	// Empty map is valid (no per-tool auth needed). Validation lives
	// at wire entry points, not on this struct (keep this struct
	// dumb-data; the API layer enforces shape).
	//
	// Never persisted to run transcripts, snapshots, or OTEL spans
	// (see Decision 5 in rfcs/per-run-credentials.md and the
	// existing OTEL secret-exclusion posture).
	UserCredentials map[string]string

	// ParentContext is the v0.12.x opaque caller-tracking lineage. Set
	// once on the root run; the Agent tool's SubAgentRunner copies it
	// UNCHANGED onto every sub-agent (same inheritance discipline as
	// UserCredentials), so a deep spawn tree all carries the root's
	// context. The runtime never interprets it — it persists it on each
	// run row and echoes it on the per-agent report surfaces so a
	// consumer can attribute a child sub-agent's usage to the root
	// request. Unlike UserBearer/UserCredentials it is NOT a secret:
	// safe to persist, log, and emit. Nil = no context. The canonical
	// type lives in the store package (importing no internal package)
	// to keep this struct cycle-free — same reason RunIdentityValue and
	// store.RunIdentity are separate structs.
	ParentContext *store.ParentContext

	// OperatorKeyRestricted is the RFC AX negative permission bit: true =
	// this run may NOT fall back to the operator's host provider key. false
	// (the zero value) = allowed, so every unstamped path fails OPEN. Computed
	// once at run-start from the principal + the deployment gate, carried here
	// so it survives to resolution/drivers and INHERITS to sub-agents unchanged
	// (a child cannot escape its parent's restriction). Stage 1 only threads
	// it; enforcement (resolver routing + driver backstop) lands in stage 2.
	OperatorKeyRestricted bool
}

// WithRunIdentity attaches the current run's identity to ctx. The
// HTTP server calls this once per run before invoking the loop so the
// AgentTool's SubAgentRunner can read it back via RunIdentity and
// thread userID/parentAgentID through to the new sub-agent's session
// + run records.
//
// RFC F back-compat sugar (v1.x): if the caller set UserBearer but
// did not populate UserCredentials["default"], promote UserBearer
// into the map at attach time so `${run.user_bearer}` and
// `${run.credentials.default}` both resolve to the same value for
// the lifetime of this ctx. This keeps v0.8.x single-bearer flows
// working unchanged while letting new code consume the map.
//
// The promotion clones the map before mutating — never mutate the
// caller's map; the substitute layer reads identity per request and
// concurrent writes would race.
func WithRunIdentity(ctx context.Context, ident RunIdentityValue) context.Context {
	if ident.UserBearer != "" {
		// Promote when "default" is absent OR present-but-empty.
		// The latter matches the runtime substitution semantics
		// (empty value treated as missing per RFC F Decision 4) —
		// without it, a caller passing `UserCredentials: {default: ""}`
		// alongside a non-empty UserBearer would silently drop
		// ${run.credentials.default} headers while ${run.user_bearer}
		// kept working. Two paths to the same value should resolve
		// identically.
		if existing := ident.UserCredentials["default"]; existing == "" {
			creds := make(map[string]string, len(ident.UserCredentials)+1)
			for k, v := range ident.UserCredentials {
				creds[k] = v
			}
			creds["default"] = ident.UserBearer
			ident.UserCredentials = creds
		}
	}
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
//     must already be in tools for the agent to even call it,
//     but the scope allowlist is a second gate that lets operators
//     grant Memory:read-only on `user` while withholding `agent`).
//   - QuotaBytes is the yaml `memory_quota_bytes` override; 0 falls
//     back to the global LOOMCYCLE_MEMORY_MAX_SCOPE_BYTES default.
//   - Backend is the resolved per-agent `memory_backend` NAME (RFC I
//     MR-3b); "" routes to the operator-default backend. The name is
//     operator-resolved from agent config — never model-supplied, same
//     trust posture as AllowedScopes.
type MemoryPolicyValue struct {
	AllowedScopes []string
	QuotaBytes    int
	Backend       string
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

// ctxKeySqlMemPolicy is the context key for the per-agent RFC AA SQL Memory
// policy (allowed scopes + per-scope byte-quota override). Set by the HTTP
// server from the agent's yaml `sql_scopes` / `sql_quota_bytes`; read by the
// Memory tool's sql_query / sql_exec ops to gate access. Sub-agents inherit
// the parent's policy via this ctx key, like MemoryPolicy / HostPolicy.
type ctxKeySqlMemPolicy struct{}

// SqlMemPolicyValue is the per-agent SQL Memory access policy.
//
//   - AllowedScopes is the yaml `sql_scopes` allowlist {agent,user,run}. An
//     empty slice means the agent has NO SQL access — the DEFAULT-DENY
//     invariant (the Memory tool must be in tools for the agent to
//     call it at all, but the scope allowlist is a second, SQL-specific gate).
//   - QuotaBytes is the yaml `sql_quota_bytes` override; 0 falls back to the
//     global LOOMCYCLE_SQLMEM_QUOTA_BYTES default.
//
// Same trust posture as MemoryPolicyValue: operator-resolved from agent
// config, never model-supplied. The model picks the scope; the operator
// decides which scopes exist.
type SqlMemPolicyValue struct {
	AllowedScopes []string
	QuotaBytes    int
}

// WithSqlMemPolicy attaches the agent's resolved SQL Memory policy to ctx.
func WithSqlMemPolicy(ctx context.Context, p SqlMemPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeySqlMemPolicy{}, p)
}

// SqlMemPolicy returns the agent's SQL Memory policy from ctx. Zero value
// (empty AllowedScopes, QuotaBytes=0) means "no SQL access".
func SqlMemPolicy(ctx context.Context) SqlMemPolicyValue {
	v, _ := ctx.Value(ctxKeySqlMemPolicy{}).(SqlMemPolicyValue)
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

// ctxKeyToolUseID carries the current tool call's tool_use id (RFC X Phase 3).
// The loop stamps it before dispatching each tool so a tool (the Agent tool's
// parallel_spawn) can tag its spawn-ledger events with the parent tool_use id,
// letting the resume reconcile match the ledger to the dangling tool_use.
type ctxKeyToolUseID struct{}

// WithToolUseID attaches the dispatching tool call's tool_use id to ctx.
func WithToolUseID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyToolUseID{}, id)
}

// ToolUseID returns the current tool call's tool_use id, or "" when unset.
func ToolUseID(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyToolUseID{}).(string)
	return v
}

// ctxKeySpawnIndex carries a child's index within its parent's parallel_spawn
// (RFC X Phase 3). parallel_spawn stamps it per child so the sub-run runner
// can emit the child's spawn-ledger "started" event with the right index.
// Stored as index+1 internally so a genuine index 0 is distinguishable from
// "unset"; SpawnIndex returns (index, ok).
type ctxKeySpawnIndex struct{}

// WithSpawnIndex attaches a child's parallel_spawn index to ctx.
func WithSpawnIndex(ctx context.Context, index int) context.Context {
	return context.WithValue(ctx, ctxKeySpawnIndex{}, index+1)
}

// SpawnIndex returns the child's parallel_spawn index and whether it was set.
func SpawnIndex(ctx context.Context) (int, bool) {
	v, ok := ctx.Value(ctxKeySpawnIndex{}).(int)
	if !ok || v == 0 {
		return 0, false
	}
	return v - 1, true
}

// PauseGate is the minimal pause-park surface a tool needs (RFC X Phase 3).
// The Agent tool's parallel_spawn uses it to PARK the fan-out parent run while
// it's blocked awaiting children (the loop's own top-of-iteration park is
// unreachable mid-tool-call). The concrete implementation lives in the HTTP
// server (internal/api/http) and is threaded in via ctx; this interface keeps
// the tools package free of a pause/server import. It is a superset of the
// loop's PauseGate (adds PauseCh) so the same concrete value satisfies both.
type PauseGate interface {
	// PauseRequested reports whether the runtime is pausing/paused.
	PauseRequested() bool
	// PauseCh returns the channel closed when a pause is declared (re-fetch
	// each cycle — a fresh channel is allocated on Resume).
	PauseCh() <-chan struct{}
	// Park persists pause_state=paused for this run, blocks until Resume (or
	// ctx cancel), then restores pause_state=running. Returns ctx.Err() on
	// cancel, nil on a clean resume.
	Park(ctx context.Context) error
}

// ctxKeyPauseGate carries the run's PauseGate so a tool can cooperatively park
// while blocked (RFC X Phase 3). Stashed by the server at run dispatch.
type ctxKeyPauseGate struct{}

// WithPauseGate attaches the run's PauseGate to ctx. nil gate is a no-op.
func WithPauseGate(ctx context.Context, g PauseGate) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyPauseGate{}, g)
}

// PauseGateFromContext returns the run's PauseGate, or nil when unset.
func PauseGateFromContext(ctx context.Context) PauseGate {
	g, _ := ctx.Value(ctxKeyPauseGate{}).(PauseGate)
	return g
}

// --- Compaction (context-compaction v2) ----------------------------------

type ctxKeyCompactRequest struct{}

// WithCompactRequest stashes a "compact requested" flag on ctx so a tool
// (Context op=compact) can ask the loop to compact at its next iteration
// boundary. The loop allocates the flag + checks/clears it; the tool only sets
// it. nil flag is a no-op (no loop is listening).
func WithCompactRequest(ctx context.Context, flag *atomic.Bool) context.Context {
	if flag == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyCompactRequest{}, flag)
}

// CompactRequest returns the compact-request flag, or nil when unset (the
// op=compact tool then reports compaction isn't available for this run).
func CompactRequest(ctx context.Context) *atomic.Bool {
	v, _ := ctx.Value(ctxKeyCompactRequest{}).(*atomic.Bool)
	return v
}

type ctxKeyCompactionPolicy struct{}

// WithCompactionPolicy attaches the run's RESOLVED-but-sparse compaction settings
// to ctx (per-run/per-spawn > parent-inherited > child def, defaults applied at
// use-time). A sub-agent inherits this; the spawn path blends a child def +
// per-spawn override over it. nil/IsZero is a no-op.
func WithCompactionPolicy(ctx context.Context, c *config.Compaction) context.Context {
	if c.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyCompactionPolicy{}, c)
}

// CompactionPolicy returns the inherited compaction settings (sparse), or nil.
func CompactionPolicy(ctx context.Context) *config.Compaction {
	c, _ := ctx.Value(ctxKeyCompactionPolicy{}).(*config.Compaction)
	return c
}

type ctxKeyCompactionOverride struct{}

// WithCompactionOverride carries a per-spawn compaction override (the Agent
// tool's `compaction` field on a spawn entry) from the tool to runSubAgent,
// which blends it on top of the child's effective policy. nil/IsZero is a no-op.
func WithCompactionOverride(ctx context.Context, c *config.Compaction) context.Context {
	if c.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyCompactionOverride{}, c)
}

// CompactionOverride returns the per-spawn compaction override, or nil.
func CompactionOverride(ctx context.Context) *config.Compaction {
	c, _ := ctx.Value(ctxKeyCompactionOverride{}).(*config.Compaction)
	return c
}

// ctxKeyResolvedProvider / ctxKeyResolvedModel carry the run's
// CURRENTLY-RESOLVED provider id and model name so the Context tool's
// op=self can report them to the agent. The model never supplies these
// (the operator's resolver picks them per tier/effort + fallback), so they
// travel via ctx. Stamped per-iteration in loop.Run from opts.Provider.ID()
// / opts.Model, so a mid-run provider fallback is reflected truthfully.
// Non-secret — the agent is allowed to know what it is running on.
type ctxKeyResolvedProvider struct{}
type ctxKeyResolvedModel struct{}

// WithResolvedProvider attaches the resolved provider id to ctx.
func WithResolvedProvider(ctx context.Context, providerID string) context.Context {
	if providerID == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyResolvedProvider{}, providerID)
}

// ResolvedProvider returns the resolved provider id from ctx, or "" when unset.
func ResolvedProvider(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyResolvedProvider{}).(string)
	return v
}

// WithResolvedModel attaches the resolved model name to ctx.
func WithResolvedModel(ctx context.Context, model string) context.Context {
	if model == "" {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyResolvedModel{}, model)
}

// ResolvedModel returns the resolved model name from ctx, or "" when unset.
func ResolvedModel(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyResolvedModel{}).(string)
	return v
}

// ctxKeyResolvedSampling carries the run's RESOLVED LLM sampling params
// (per-run > per-agent, already merged) so Context op=self can report them to
// the agent — non-secret introspection, like provider/model. Stamped per-
// iteration in loop.Run from opts.Sampling.
type ctxKeyResolvedSampling struct{}

// WithResolvedSampling attaches the resolved sampling params to ctx. nil is a
// no-op (no sampling configured → op=self omits the field).
func WithResolvedSampling(ctx context.Context, s *config.Sampling) context.Context {
	if s.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyResolvedSampling{}, s)
}

// ResolvedSampling returns the resolved sampling params from ctx, or nil.
func ResolvedSampling(ctx context.Context) *config.Sampling {
	v, _ := ctx.Value(ctxKeyResolvedSampling{}).(*config.Sampling)
	return v
}

// ctxKeyContextUsage carries the run's CURRENT context footprint (tokens used as
// of the last completed turn + the model's window ceiling) so Context op=self
// can report it. An agent reads this alongside its compaction settings to decide
// whether to self-compact (Context op=compact). Stamped per-iteration in loop.Run.
type ctxKeyContextUsage struct{}

// ContextUsageValue is the footprint op=self reports. Used is input + cache
// tokens of the last turn's request; Max is the window ceiling (0 = unknown).
type ContextUsageValue struct {
	Used int
	Max  int
}

// WithContextUsage attaches the current context footprint to ctx. used<=0 is a
// no-op (no turn has completed yet → op=self omits the field).
func WithContextUsage(ctx context.Context, used, max int) context.Context {
	if used <= 0 {
		return ctx
	}
	return context.WithValue(ctx, ctxKeyContextUsage{}, ContextUsageValue{Used: used, Max: max})
}

// ContextUsage returns the current context footprint from ctx (zero value when
// unset — Used==0).
func ContextUsage(ctx context.Context) ContextUsageValue {
	v, _ := ctx.Value(ctxKeyContextUsage{}).(ContextUsageValue)
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

// HasEventEmitter reports whether a REAL emitter was attached to ctx (vs the
// no-op EventEmitter hands back by default). A caller that only wants to do work
// when its events will actually be recorded — e.g. the RFC X Phase 3 spawn
// ledger, which is pointless if it drains into the no-op — gates on this rather
// than on EventEmitter(ctx) != nil (which is ALWAYS true, since the accessor
// never returns nil).
func HasEventEmitter(ctx context.Context) bool {
	fn, ok := ctx.Value(ctxKeyEventEmitter{}).(EventEmitterFunc)
	return ok && fn != nil
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

type ctxKeyScheduleDefPolicy struct{}

// ScheduleDefPolicyValue is the per-agent ScheduleDef-tool access
// policy (v1.x RFC E). Same shape as AgentDefPolicyValue + same
// "self / descendants / named:<n> / any" closed scope set.
// Default-deny when Scopes is empty.
type ScheduleDefPolicyValue struct {
	Scopes   []string
	SelfName string
}

// WithScheduleDefPolicy attaches the policy to ctx.
func WithScheduleDefPolicy(ctx context.Context, p ScheduleDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyScheduleDefPolicy{}, p)
}

// ScheduleDefPolicy returns the policy from ctx. Zero value =
// default-deny.
func ScheduleDefPolicy(ctx context.Context) ScheduleDefPolicyValue {
	v, _ := ctx.Value(ctxKeyScheduleDefPolicy{}).(ScheduleDefPolicyValue)
	return v
}

type ctxKeyA2AServerCardDefPolicy struct{}

// A2AServerCardDefPolicyValue is the per-agent A2AServerCardDef-tool
// access policy (v1.x RFC G). Same shape as ScheduleDefPolicyValue +
// same "self / descendants / named:<n> / any" closed scope set.
// Default-deny when Scopes is empty.
type A2AServerCardDefPolicyValue struct {
	Scopes   []string
	SelfName string
}

// WithA2AServerCardDefPolicy attaches the policy to ctx.
func WithA2AServerCardDefPolicy(ctx context.Context, p A2AServerCardDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyA2AServerCardDefPolicy{}, p)
}

// A2AServerCardDefPolicy returns the policy from ctx. Zero value =
// default-deny.
func A2AServerCardDefPolicy(ctx context.Context) A2AServerCardDefPolicyValue {
	v, _ := ctx.Value(ctxKeyA2AServerCardDefPolicy{}).(A2AServerCardDefPolicyValue)
	return v
}

type ctxKeyA2AAgentDefPolicy struct{}

// A2AAgentDefPolicyValue is the per-agent A2AAgentDef-tool access
// policy (v1.x RFC G). Same shape as ScheduleDefPolicyValue + same
// "self / descendants / named:<n> / any" closed scope set.
// Default-deny when Scopes is empty.
type A2AAgentDefPolicyValue struct {
	Scopes   []string
	SelfName string
}

// WithA2AAgentDefPolicy attaches the policy to ctx.
func WithA2AAgentDefPolicy(ctx context.Context, p A2AAgentDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyA2AAgentDefPolicy{}, p)
}

// A2AAgentDefPolicy returns the policy from ctx. Zero value =
// default-deny.
func A2AAgentDefPolicy(ctx context.Context) A2AAgentDefPolicyValue {
	v, _ := ctx.Value(ctxKeyA2AAgentDefPolicy{}).(A2AAgentDefPolicyValue)
	return v
}

type ctxKeyWebhookDefPolicy struct{}

// WebhookDefPolicyValue is the per-agent WebhookDef-tool access policy
// (v1.x RFC H). Same shape as A2AAgentDefPolicyValue + same
// "self / descendants / named:<n> / any" closed scope set.
// Default-deny when Scopes is empty.
type WebhookDefPolicyValue struct {
	Scopes   []string
	SelfName string
}

// WithWebhookDefPolicy attaches the policy to ctx.
func WithWebhookDefPolicy(ctx context.Context, p WebhookDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyWebhookDefPolicy{}, p)
}

// WebhookDefPolicy returns the policy from ctx. Zero value =
// default-deny.
func WebhookDefPolicy(ctx context.Context) WebhookDefPolicyValue {
	v, _ := ctx.Value(ctxKeyWebhookDefPolicy{}).(WebhookDefPolicyValue)
	return v
}

type ctxKeyMemoryBackendDefPolicy struct{}

// MemoryBackendDefPolicyValue is the per-agent MemoryBackendDef-tool
// access policy (RFC I MR-3a). Same shape as WebhookDefPolicyValue +
// same "self / descendants / named:<n> / any" closed scope set.
// Default-deny when Scopes is empty.
type MemoryBackendDefPolicyValue struct {
	Scopes   []string
	SelfName string
}

// WithMemoryBackendDefPolicy attaches the policy to ctx.
func WithMemoryBackendDefPolicy(ctx context.Context, p MemoryBackendDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyMemoryBackendDefPolicy{}, p)
}

// MemoryBackendDefPolicy returns the policy from ctx. Zero value =
// default-deny.
func MemoryBackendDefPolicy(ctx context.Context) MemoryBackendDefPolicyValue {
	v, _ := ctx.Value(ctxKeyMemoryBackendDefPolicy{}).(MemoryBackendDefPolicyValue)
	return v
}

type ctxKeyOperatorTokenDefPolicy struct{}

// OperatorTokenDefPolicyValue is the per-caller OperatorTokenDef-tool
// access gate (RFC L). Unlike the other Def policies there is no
// "self"/"named" granularity — minting auth tokens is an operator-admin
// capability, full stop. Admin == true grants all ops; the zero value is
// default-deny. The HTTP/gRPC/MCP admin paths set Admin=true after the
// caller has cleared the substrate:admin scope check.
type OperatorTokenDefPolicyValue struct {
	Admin bool
}

// WithOperatorTokenDefPolicy attaches the policy to ctx.
func WithOperatorTokenDefPolicy(ctx context.Context, p OperatorTokenDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyOperatorTokenDefPolicy{}, p)
}

// OperatorTokenDefPolicy returns the policy from ctx. Zero value =
// default-deny.
func OperatorTokenDefPolicy(ctx context.Context) OperatorTokenDefPolicyValue {
	v, _ := ctx.Value(ctxKeyOperatorTokenDefPolicy{}).(OperatorTokenDefPolicyValue)
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

// ctxKeyVolumeDefPolicy carries the RFC AH Phase 2a VolumeDef-tool
// capability gate. Mirrors SkillDefPolicy (no SelfName — volumes have no
// agent identity, so no "self" scope).
type ctxKeyVolumeDefPolicy struct{}

// VolumeDefPolicyValue is the per-agent VolumeDef-tool access policy.
//
//   - Scopes is the operator-yaml volume_def_scopes list. Closed set:
//     "any" / "named:<volume-name>". Empty = default-deny.
//
// Gates create/delete/purge only — get/list are tenant-scoped reads
// available to any agent the tool is attached to (mirrors the other Def
// families' read posture).
type VolumeDefPolicyValue struct {
	Scopes []string
}

// WithVolumeDefPolicy attaches the policy to ctx.
func WithVolumeDefPolicy(ctx context.Context, p VolumeDefPolicyValue) context.Context {
	return context.WithValue(ctx, ctxKeyVolumeDefPolicy{}, p)
}

// VolumeDefPolicy returns the policy from ctx. Zero value = default-deny
// (the tool refuses create/delete/purge until scopes are granted via yaml).
func VolumeDefPolicy(ctx context.Context) VolumeDefPolicyValue {
	v, _ := ctx.Value(ctxKeyVolumeDefPolicy{}).(VolumeDefPolicyValue)
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
//
// v0.10.0 OTEL: opens loomcycle.tool.call for every dispatched call.
// Errors (Go-level + IsError-true results) mark the span Error. MCP
// tools open a nested loomcycle.mcp.call inside this outer span (the
// MCP-aware nesting happens in the mcpTool wrapper at
// internal/tools/mcp/pool.go).
func (d *Dispatcher) Execute(ctx context.Context, name string, input json.RawMessage) Result {
	ctx, span := lcotel.RecordToolCall(ctx, name)
	defer span.End()
	if t, ok := d.tools[name]; ok {
		res, err := t.Execute(ctx, input)
		if err != nil {
			lcotel.SetSpanError(span, err)
			return Result{Text: err.Error(), IsError: true}
		}
		if res.IsError {
			lcotel.SetSpanErrorMessage(span, firstLineForSpan(res.Text))
		}
		return res
	}
	if d.fallback != nil {
		if res, handled := d.fallback(ctx, name, input); handled {
			if res.IsError {
				lcotel.SetSpanErrorMessage(span, firstLineForSpan(res.Text))
			}
			return res
		}
	}
	lcotel.SetSpanErrorMessage(span, "tool not found")
	return Result{Text: fmt.Sprintf("tool not found: %s", name), IsError: true}
}

// firstLineForSpan extracts the first line of a tool's error text for
// the span status description. Multi-line tool errors otherwise blow
// up Jaeger's status field; the truncation in SetSpanErrorMessage
// hard-caps at 500 chars on top of this.
func firstLineForSpan(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
