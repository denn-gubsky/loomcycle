package auth

// RFC AO — config-declared principals. An operator declares static
// (tenant, subject, scopes) logins in the `principals:` config block, each bound
// to a bearer SECRET held in an env var (in .env.local, never in yaml). At
// config load the runtime resolves those into a []DeclaredPrincipal table; the
// bearer resolver matches a presented token against it (between the minted
// OperatorTokenDef substrate and the legacy fallback). The payoff: one declared
// token used for both the Web UI login and an MCP thin client resolves to the
// SAME (tenant, subject) on every transport (composes with RFC AG's
// UserID = principal.Subject), so an MCP agent's user-scoped Documents/Memory
// land where the UI reads them.

// DeclaredPrincipal binds a config-declared bearer secret to the authoritative
// Principal it resolves to. Secret is the raw token value read from the
// principal's token_env at config load; it is matched constant-time and is never
// logged or echoed.
type DeclaredPrincipal struct {
	Secret    string
	Principal Principal
}

// MatchDeclared returns the Principal for the declared entry whose secret equals
// bearer, matched with the length-independent constant-time CompareBearer (same
// hardened compare the legacy token uses — no timing side channel). It scans ALL
// entries without short-circuiting on a match, so total time does not depend on
// which entry (if any) matched. Returns (_, false) when none match. (Config load
// rejects two declared principals sharing a secret, so at most one matches.)
func MatchDeclared(bearer string, declared []DeclaredPrincipal) (Principal, bool) {
	var matched Principal
	found := false
	for _, d := range declared {
		if d.Secret == "" {
			continue // an inert principal (empty token_env at boot)
		}
		if CompareBearer(bearer, d.Secret) {
			matched = d.Principal
			found = true
		}
	}
	return matched, found
}
