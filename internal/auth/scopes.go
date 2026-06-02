package auth

// RFC L scope catalog — a CLOSED, documented set. Operators cannot
// invent scope names (the runtime wouldn't enforce them), so create/
// rotate validate every requested scope against this catalog. The
// per-route required-scope map (which route demands which scope) lands
// with the middleware in PR2; this file is only the vocabulary.

const (
	// ScopeAdmin is full operator power, including minting/rotating/
	// retiring tokens (the OperatorTokenDef substrate). The create-time
	// default so "single token, full power" is preserved.
	ScopeAdmin = "substrate:admin"

	ScopeRunsCreate     = "runs:create"
	ScopeRunsRead       = "runs:read"
	ScopeChannelPublish = "channel:publish"
	ScopeChannelRead    = "channel:read"

	// NOTE: memory:read / memory:write are intentionally NOT in the
	// catalog. They were inert — grantable but enforced by no route: the
	// HTTP memory surface (/v1/_memory/*) is operator-admin, and
	// per-tenant memory read/write is the agent-facing Memory tool gated
	// by the run's memory policy, not an HTTP scope. A scope the runtime
	// never checks is a false limitation, so it's removed (same
	// dead-config posture as the retired WebhookDefScopes /
	// MemoryBackendDefScopes). Reintroduce only alongside a route that
	// actually enforces them.
)

// scopeCatalog is the closed set — every entry is enforced by at least
// one route in requiredScopeFor. A map for O(1) validation.
var scopeCatalog = map[string]struct{}{
	ScopeAdmin:          {},
	ScopeRunsCreate:     {},
	ScopeRunsRead:       {},
	ScopeChannelPublish: {},
	ScopeChannelRead:    {},
}

// ValidScope reports whether s is in the closed catalog.
func ValidScope(s string) bool {
	_, ok := scopeCatalog[s]
	return ok
}

// UnknownScopes returns any of the supplied scopes that are NOT in the
// catalog (nil if all are valid) — so create/rotate can refuse with a
// precise "these scopes don't exist" message rather than silently
// granting an unenforceable name.
func UnknownScopes(scopes []string) []string {
	var bad []string
	for _, s := range scopes {
		if !ValidScope(s) {
			bad = append(bad, s)
		}
	}
	return bad
}

// HasScope reports whether the granted set includes want. ScopeAdmin is
// a superuser scope: it satisfies every required scope.
func HasScope(granted []string, want string) bool {
	for _, g := range granted {
		if g == want || g == ScopeAdmin {
			return true
		}
	}
	return false
}
