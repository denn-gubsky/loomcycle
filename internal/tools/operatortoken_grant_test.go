package tools

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// The token tool's grant comes from who the caller is: everything for the
// operator, a tenant operator's own scrape tokens, and nothing for anyone
// else a route might let through.
func TestOperatorTokenDefPolicyFor_GrantsByCallerClass(t *testing.T) {
	admin := OperatorTokenDefPolicyValue{Admin: true}
	for name, c := range map[string]struct {
		p    auth.Principal
		ok   bool
		want OperatorTokenDefPolicyValue
	}{
		"open mode":   {ok: false, want: admin},
		"admin":       {p: auth.Principal{TenantID: "ops", Subject: "a", Scopes: []string{auth.ScopeAdmin}}, ok: true, want: admin},
		"legacy":      {p: auth.Principal{TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true}, ok: true, want: admin},
		"tenant op":   {p: auth.Principal{TenantID: "acme", Subject: "o", Scopes: []string{auth.ScopeTenant}}, ok: true, want: OperatorTokenDefPolicyValue{TenantScrape: true, Tenant: "acme"}},
		"no tenant":   {p: auth.Principal{Subject: "o", Scopes: []string{auth.ScopeTenant}}, ok: true},
		"member":      {p: auth.Principal{TenantID: "acme", Subject: "m", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}}, ok: true},
		"isolated":    {p: auth.Principal{TenantID: "acme", Subject: "i", Scopes: []string{auth.ScopeUser}}, ok: true},
		"scraper":     {p: auth.Principal{TenantID: "acme", Subject: "p", Scopes: []string{auth.ScopeMetricsRead}}, ok: true},
		"auth manage": {p: auth.Principal{TenantID: "acme", Subject: "w", Scopes: []string{auth.ScopeAuthManage}}, ok: true},
	} {
		if got := OperatorTokenDefPolicyFor(c.p, c.ok); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}
