package connector

import (
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// parentContextMaxFieldLen bounds each parent_context field so a
// consumer can't push unbounded strings into the run table / event
// stream. The fields are opaque consumer ids/keys; 256 is generous.
const parentContextMaxFieldLen = 256

// ValidateParentContext bounds the opaque caller-tracking fields
// (v0.12.x). Lives here so all transports (HTTP, gRPC, MCP) validate
// identically. A nil or all-empty struct is valid (treated as "no
// context" by the caller). Returns errMsg suitable for a 400 / MCP
// tool-error naming the offending field.
func ValidateParentContext(pc *store.ParentContext) (errMsg string, ok bool) {
	if pc == nil {
		return "", true
	}
	for field, v := range map[string]string{
		"root_agent_run_id": pc.RootAgentRunID,
		"function_key":      pc.FunctionKey,
		"tier_at_run":       pc.TierAtRun,
	} {
		if len(v) > parentContextMaxFieldLen {
			return fmt.Sprintf("parent_context.%s exceeds %d bytes", field, parentContextMaxFieldLen), false
		}
	}
	return "", true
}

// StripRuntimeParentContext returns a copy of a CALLER-supplied parent_context
// with every runtime-owned field cleared, or nil when nothing the caller may set
// remains. Call it wherever a parent_context enters from outside.
//
// The board and walk/wave/state fields are stamped by the runtime onto the runs a
// team walk spawns, and consumers group runs by them — a canvas drawing a walk,
// a board pinning an agent to its card, a run-state stream filtered by walk_id.
// Accepted from a caller, they let any run claim a place inside a walk or on a
// board it was never part of. They are dropped, not refused: a caller echoing a
// parent_context it read off a walk run back into a new run is not an error,
// and the fields it may set still apply.
//
// The runtime's own stamping (the sub-run path) writes these fields onto the
// child's identity directly and never passes through here.
func StripRuntimeParentContext(pc *store.ParentContext) *store.ParentContext {
	if pc == nil {
		return nil
	}
	out := *pc
	out.BoardScope, out.BoardChunkID, out.BoardDocumentID = "", "", ""
	out.WalkID, out.WaveID, out.WaveIndex = "", "", 0
	out.State, out.StateVisit = "", 0
	if out.IsZero() {
		return nil
	}
	return &out
}

// ValidateUserCredentialsMap validates each key in the v1.x RFC F
// per-run credentials map against the wire-locked charset. Lives in
// the connector package so all four transports (HTTP, gRPC, MCP,
// future) share one source of truth — the RFC's "validation enforced
// at all 4 entry points" sharp edge demands it.
//
// Key contract: [a-zA-Z0-9_-]{1,64}. The regex in
// internal/tools/mcp/http/substitute.go's runCredRe MUST match this
// charset — a key passing validation here that fails the substitute
// regex would silently drop headers; that diverges from RFC Decision 4.
//
// Values are NOT validated (operators legitimately pass JWTs, opaque
// tokens, signed payloads — no length or charset constraints make
// sense). Empty map is valid (= run uses no per-tool auth); nil is
// valid.
//
// Returns errMsg suitable for a 400 Bad Request / gRPC
// InvalidArgument / MCP tool-error response naming the offending key.
func ValidateUserCredentialsMap(m map[string]string) (errMsg string, ok bool) {
	for k := range m {
		if !validCredentialKey(k) {
			return fmt.Sprintf(`user_credentials: key %q must match [a-zA-Z0-9_-]{1,64}`, k), false
		}
	}
	return "", true
}

// validCredentialKey reports whether k is a valid key for the v1.x
// RFC F per-run credentials map. Charset: [a-zA-Z0-9_-], length
// 1..64. Keys this strict pass through yaml + JSON + URL paths
// without escaping and align with the regex in
// internal/tools/mcp/http/substitute.go's runCredRe — a single
// source-of-truth shape, validated at every wire entry point.
func validCredentialKey(k string) bool {
	if len(k) == 0 || len(k) > 64 {
		return false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			continue
		default:
			return false
		}
	}
	return true
}

// NormalizeChannelFields checks the fields a runtime channel definition is
// created with that need no caller to judge — scope, semantic, default_ttl and
// max_messages — and returns scope and semantic with their defaults applied
// (global, queue). Shared by ChannelDef create and a team's own channels, so
// the two accept the same definitions; who may create a global channel, and
// whether hooks resolve, are the caller's checks.
func NormalizeChannelFields(scope, semantic string, defaultTTL, maxMessages int) (string, string, error) {
	if scope == "" {
		scope = "global"
	}
	switch scope {
	case "global", "agent", "user", "tenant":
	default:
		return "", "", fmt.Errorf("scope must be one of global|tenant|user|agent, got %q", scope)
	}
	if semantic == "" {
		semantic = "queue"
	}
	switch semantic {
	case "queue", "topic":
	default:
		return "", "", fmt.Errorf("semantic must be one of queue|topic, got %q", semantic)
	}
	if defaultTTL < 0 || maxMessages < 0 {
		return "", "", fmt.Errorf("default_ttl and max_messages must be >= 0")
	}
	return scope, semantic, nil
}

// MaxIdempotencyKeyLen bounds a caller's idempotency_key.
const MaxIdempotencyKeyLen = 200

// ValidateIdempotencyKey checks a caller-supplied idempotency_key: 1 to 200
// characters of [A-Za-z0-9:._-]. Empty is valid (no key). One validator for
// every transport, like ValidateParentContext.
func ValidateIdempotencyKey(k string) (errMsg string, ok bool) {
	if k == "" {
		return "", true
	}
	const msg = "idempotency_key must match [A-Za-z0-9:._-]{1,200}"
	if len(k) > MaxIdempotencyKeyLen {
		return msg, false
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == ':', r == '.', r == '_', r == '-':
		default:
			return msg, false
		}
	}
	return "", true
}

// MaxWallSecondsCeiling bounds max_wall_seconds: 30 days. A larger value is
// almost certainly a unit mistake, and no run is meant to be held that long by
// a per-run setting.
const MaxWallSecondsCeiling = 30 * 24 * 60 * 60

// ValidateMaxWallSeconds checks a caller's max_wall_seconds: 0 (no bound) to
// MaxWallSecondsCeiling. One validator for every transport.
func ValidateMaxWallSeconds(n int) (errMsg string, ok bool) {
	if n < 0 || n > MaxWallSecondsCeiling {
		return fmt.Sprintf("max_wall_seconds must be between 0 and %d", MaxWallSecondsCeiling), false
	}
	return "", true
}
