package auth

import "fmt"

// RFC BX P2c — delegated per-user token minting. When a tenant operator mints a
// bearer token for one of its members, the granted scopes are DERIVED here from
// the member's access_mode — never taken free-form from the request. This is the
// security core of bounded delegation: a tenant operator cannot mint a token
// more powerful than the member's dial, and cannot reach an operator/admin scope
// at all (nothing here returns ScopeTenant / ScopeAdmin).

// GrantableUserScopes returns the closed set of scopes a tenant operator may
// delegate to a member with the given access_mode (RFC BX P2c):
//
//   - "tenant"   — a full whole-tenant member: may run agents and use channels.
//     Whole-tenant DATA access is conferred by NOT being isolated (the data
//     tools admit the tenant/global keyspace for a non-isolated run — see
//     tools.ConfineIsolatedScope), so no extra data scope is needed here; the
//     runs and channels scopes are the delegable surface.
//   - "isolated" — the sandboxed member: substrate:user only. runs:create /
//     runs:read are implied by userImplied, and a run stamped isolated is
//     confined to its own user scope by tools.ConfineIsolatedScope.
//
// providers:operator-key is DELIBERATELY NOT granted (minimal-first, RFC BX §8
// Q2): under the default-off operator-key gate it is inert, and a deployment
// that turns the gate on to force bring-your-own-key should restrict members
// too. Widen on demand later rather than granting it silently now.
//
// An unknown access_mode returns (nil, error) — never a silent default grant, so
// a store invariant break (an access_mode outside the enum) fails closed rather
// than minting an unbounded token.
func GrantableUserScopes(accessMode string) ([]string, error) {
	switch accessMode {
	case "tenant":
		return []string{ScopeRunsCreate, ScopeRunsRead, ScopeChannelPublish, ScopeChannelRead}, nil
	case "isolated":
		return []string{ScopeUser}, nil
	default:
		return nil, fmt.Errorf("auth: no grantable scopes for access_mode %q (want tenant|isolated)", accessMode)
	}
}
