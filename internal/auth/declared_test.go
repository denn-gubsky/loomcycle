package auth

import "testing"

func TestMatchDeclared(t *testing.T) {
	declared := []DeclaredPrincipal{
		{Secret: "lct_marketing", Principal: Principal{TenantID: "acme", Subject: "marketing", Scopes: []string{ScopeTenant}}},
		{Secret: "lct_ops", Principal: Principal{TenantID: "", Subject: "ops", Scopes: []string{ScopeAdmin}}},
		{Secret: "", Principal: Principal{Subject: "inert"}}, // empty token_env at boot → inert, must be skipped
	}

	t.Run("matches by secret to its principal", func(t *testing.T) {
		p, ok := MatchDeclared("lct_marketing", declared)
		if !ok || p.TenantID != "acme" || p.Subject != "marketing" {
			t.Errorf("got (%+v, %v), want the acme/marketing principal", p, ok)
		}
		p, ok = MatchDeclared("lct_ops", declared)
		if !ok || p.Subject != "ops" || !HasScope(p.Scopes, ScopeAdmin) {
			t.Errorf("got (%+v, %v), want the ops/admin principal", p, ok)
		}
	})

	t.Run("non-matching bearer", func(t *testing.T) {
		if _, ok := MatchDeclared("lct_nope", declared); ok {
			t.Error("a non-declared bearer must not match")
		}
	})

	t.Run("empty bearer never matches the inert entry", func(t *testing.T) {
		if _, ok := MatchDeclared("", declared); ok {
			t.Error("empty bearer must not match (incl. the inert empty-secret entry)")
		}
	})

	t.Run("nil table", func(t *testing.T) {
		if _, ok := MatchDeclared("lct_marketing", nil); ok {
			t.Error("no declared principals → no match")
		}
	})
}
