package tools

import "github.com/denn-gubsky/loomcycle/internal/auth"

// OperatorTokenDefPolicyFor is the grant for a caller a transport admitted
// to the token tool: every op to an admin (the legacy token included) and to
// a caller with no principal — open mode or the stdio MCP launcher, the
// operator itself — and a non-admin tenant operator its own tenant's scrape
// tokens. Anyone else gets nothing, whatever the route let through.
func OperatorTokenDefPolicyFor(p auth.Principal, ok bool) OperatorTokenDefPolicyValue {
	switch {
	case !ok || auth.HasScope(p.Scopes, auth.ScopeAdmin):
		return OperatorTokenDefPolicyValue{Admin: true}
	case !p.Legacy && p.TenantID != "" && auth.HasScope(p.Scopes, auth.ScopeTenant):
		return OperatorTokenDefPolicyValue{TenantScrape: true, Tenant: p.TenantID}
	}
	return OperatorTokenDefPolicyValue{}
}
