// Package runident carries WHO is running — the current run's identity and
// its store row id — through a context.Context.
//
// It is a leaf: it imports nothing from this module. That is the reason it
// exists. The identity used to live in internal/tools, so every package that
// only needed to ask "which tenant is this call for" (the memory backends, the
// remote document client) had to import the whole tool layer above it. Here it
// can be read from any layer.
//
// internal/tools keeps its historical names (RunIdentityValue, RunIdentity,
// WithRunIdentity, HasRunIdentity, WithRunID, RunID) as aliases and forwarders
// to this package, and internal/store keeps ParentContext as an alias, so
// existing call sites read unchanged. There is ONE context key for each value,
// defined here: a value set through either name is read through either name.
package runident

import (
	"context"
	"sync/atomic"
)

// ctxKeyIdentity is the context key under which the runtime stashes the
// current run's identity. Sub-agents read it via From to inherit the parent's
// user_id and to know whose agent_id is their parent.
type ctxKeyIdentity struct{}

// Identity is the shape stored in ctx by With. It is distinct from
// store.RunIdentity, which is the persistence-layer struct with more fields.
type Identity struct {
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
	// SessionID is the CHAT this run belongs to. It exists so a write made
	// during a run can record WHERE it came from: `Memory op=add` enqueues a
	// consolidation item, the fact distilled from it inherits the pointer, and a
	// recalled fact can then hand back the turn it was derived from.
	//
	// It used to be absent, and the absence was load-bearing in the wrong
	// direction — the enqueue path said so in a comment ("No session-id ctx
	// helper today … so SourceSessionID stays empty") and every fact distilled
	// from a queued item was therefore unreachable from its source. Measured on a
	// benchmark store: 8 of 215 facts carried a session, and all 8 came from the
	// harness's own agents rather than from the corpus.
	//
	// A sub-agent gets its OWN session, so this is NOT inherited like TenantID —
	// runSubAgent stamps the child's.
	SessionID string
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
	// ModelTier is the model tier this run's agent routes by ("low",
	// "middle", "high", …), or "pinned" for an agent that names its provider
	// and model. The usage ledger records it on the run's own model calls so
	// spend can be reported per tier. Each run sets its own: a sub-agent's
	// comes from its agent, not its parent's.
	ModelTier string
	// InternalAgent marks a run of an agent declared `internal:` — the
	// runtime's own maintenance (memory consolidation). Its model calls are
	// charged as consolidation, not as conversation.
	InternalAgent bool
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
	// Back-compat with v0.8.x: at With time, if the caller set
	// UserBearer but did not set UserCredentials["default"], the
	// substrate populates UserCredentials["default"] = UserBearer
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
	// safe to persist, log, and emit. Nil = no context. Its type is
	// defined in this package (store.ParentContext aliases it) so this
	// struct can carry it without importing anything from the module.
	ParentContext *ParentContext

	// OperatorKeyRestricted is the RFC AX negative permission bit: true =
	// this run may NOT fall back to the operator's host provider key. false
	// (the zero value) = allowed, so every unstamped path fails OPEN. Computed
	// once at run-start from the principal + the deployment gate, carried here
	// so it survives to resolution/drivers and INHERITS to sub-agents unchanged
	// (a child cannot escape its parent's restriction). Stage 1 only threads
	// it; enforcement (resolver routing + driver backstop) lands in stage 2.
	OperatorKeyRestricted bool

	// CredentialsNoUserScope: a $cred: reference this run resolves is looked
	// up for its agent and tenant only, never in UserID's own scope (see
	// runner.RunInput.CredentialsNoUserScope). Inherited by sub-agents.
	CredentialsNoUserScope bool
	// UserScopeWithheld is the same restriction arriving while the run is
	// live: set (and never cleared) when someone other than the run's user
	// sends it a turn, so every turn from then on — and every child spawned
	// after — resolves for the agent and tenant only. Shared by every copy of
	// this identity; nil on a run nobody can send turns to. Read both through
	// NoUserScope, never this field or CredentialsNoUserScope alone.
	UserScopeWithheld *atomic.Bool

	// Isolated is the RFC BX P2b confinement bit: true = this run belongs to an
	// ISOLATED MEMBER (a substrate:user principal — see auth.IsIsolated) and the
	// data tools must refuse the tenant-shared / cross-tenant-global keyspace,
	// leaving only the run's own user + agent scope reachable (see
	// tools.ConfineIsolatedScope). Computed once at run-start from the
	// principal (never a wire/body/model field) and carried here so it survives
	// to the tool dispatch and INHERITS to sub-agents unchanged (a child cannot
	// escape its parent's confinement). false (the zero value) = unconfined, so
	// every unstamped path fails OPEN — matching the OperatorKeyRestricted
	// posture and keeping existing runs byte-identical until a substrate:user
	// token is minted.
	Isolated bool

	// Retention is the run's retention mode: "" or "none" (its content is
	// stored nowhere). A sub-agent of a "none" run is "none" too, and a tool
	// that would store what the run says (asking a person a question) reads
	// it here to refuse.
	Retention string
}

// NoUserScope reports whether a $cred: this run resolves must skip its user's
// own scope: since it started, or since another user sent it a turn.
func (v Identity) NoUserScope() bool {
	return v.CredentialsNoUserScope || (v.UserScopeWithheld != nil && v.UserScopeWithheld.Load())
}

// With attaches the current run's identity to ctx. The HTTP server calls this
// once per run before invoking the loop so the AgentTool's SubAgentRunner can
// read it back via From and thread userID/parentAgentID through to the new
// sub-agent's session + run records.
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
func With(ctx context.Context, ident Identity) context.Context {
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
			creds := make(map[string]string, len(ident.UserCredentials))
			for k, v := range ident.UserCredentials {
				creds[k] = v
			}
			creds["default"] = ident.UserBearer
			ident.UserCredentials = creds
		}
	}
	return context.WithValue(ctx, ctxKeyIdentity{}, ident)
}

// From returns the current run's identity from ctx, or the zero value if none
// is attached. The HTTP server's runSubAgent uses the AgentID as the new
// sub-run's parent_agent_id and the UserID for inheritance into the
// sub-agent's session.
func From(ctx context.Context) Identity {
	v, _ := ctx.Value(ctxKeyIdentity{}).(Identity)
	return v
}

// Has reports whether ctx belongs to a RUN at all.
//
// It exists because "the identity is empty" and "there is no identity" are
// different facts, and one caller has to act on the difference: a per-run
// credential that will not resolve is a refusal when a run made the call, and
// merely absent when nothing did. Boot-time MCP enumeration handshakes on a
// bare context.Background() — there is no run, so "this run does not carry the
// credential" is not a statement about anything.
//
// Deliberately keyed on the ctx VALUE's presence rather than on whether the
// fields are zero: a run whose identity happens to be sparse is still a run,
// and a zero-field check would silently re-file it as infrastructure.
func Has(ctx context.Context) bool {
	_, ok := ctx.Value(ctxKeyIdentity{}).(Identity)
	return ok
}

// ctxKeyRunID is the context key under which the runtime stores
// the current run's store row ID. The Interruption tool uses this
// at create time to associate the interrupt row with the run for
// listing and ACL purposes — the model never supplies it, so it
// must travel via ctx alongside the Identity (which carries the
// agent-facing IDs; run.ID is operator-facing and stays out of the
// model's view).
type ctxKeyRunID struct{}

// WithRunID attaches the run's store row ID to ctx. Same call site
// as With (HTTP handler at run start).
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
