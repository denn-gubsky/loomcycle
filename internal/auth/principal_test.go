package auth

import (
	"context"
	"testing"
)

func TestPrincipalContext_RoundTrip(t *testing.T) {
	p := Principal{TenantID: "acme", Subject: "alice", Scopes: []string{ScopeRunsCreate}}
	ctx := WithPrincipal(context.Background(), p)
	got, ok := PrincipalFromContext(ctx)
	if !ok {
		t.Fatal("principal not found in ctx")
	}
	if got.TenantID != "acme" || got.Subject != "alice" {
		t.Errorf("got %+v", got)
	}
	if _, ok := PrincipalFromContext(context.Background()); ok {
		t.Error("bare ctx should carry no principal")
	}
}

// TestResolveWireIdentity pins the shared identity-override rule (RFC L
// Decision 5 / RFC AG §3.2) used by both HTTP applyPrincipal and the MCP
// run-lifecycle handlers. The three branches: no-principal passthrough, legacy
// honors-wire-user/keeps-tenant, real-principal strict override.
func TestResolveWireIdentity(t *testing.T) {
	cases := []struct {
		name            string
		p               Principal
		ok              bool
		wireT, wireU    string
		wantT, wantSubj string
	}{
		{
			name:  "no_principal_passthrough",
			ok:    false,
			wireT: "wire-t", wireU: "wire-u",
			wantT: "wire-t", wantSubj: "wire-u",
		},
		{
			name:  "real_principal_strict_override",
			p:     Principal{TenantID: "acme", Subject: "alice"},
			ok:    true,
			wireT: "forged-tenant", wireU: "forged-bob",
			wantT: "acme", wantSubj: "alice",
		},
		{
			name:  "legacy_honors_wire_user_keeps_tenant",
			p:     Principal{TenantID: "default", Subject: "default", Legacy: true},
			ok:    true,
			wireT: "ignored-tenant", wireU: "exp1",
			wantT: "default", wantSubj: "exp1",
		},
		{
			name:  "legacy_empty_wire_falls_back_to_placeholder",
			p:     Principal{TenantID: "default", Subject: "default", Legacy: true},
			ok:    true,
			wireT: "", wireU: "",
			wantT: "default", wantSubj: "default",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotT, gotS := ResolveWireIdentity(tc.p, tc.ok, tc.wireT, tc.wireU)
			if gotT != tc.wantT || gotS != tc.wantSubj {
				t.Errorf("ResolveWireIdentity = (%q,%q), want (%q,%q)", gotT, gotS, tc.wantT, tc.wantSubj)
			}
		})
	}
}

func TestSubjectForFairness(t *testing.T) {
	// Principal present → its Subject wins over the wire fallback.
	ctx := WithPrincipal(context.Background(), Principal{Subject: "alice"})
	if got := SubjectForFairness(ctx, "wire-bob"); got != "alice" {
		t.Errorf("got %q, want alice (principal subject authoritative)", got)
	}
	// No principal → the wire fallback is used (open / un-authed mode).
	if got := SubjectForFairness(context.Background(), "wire-bob"); got != "wire-bob" {
		t.Errorf("got %q, want wire-bob (fallback)", got)
	}
	// Principal with empty subject → fallback (defensive).
	ctx2 := WithPrincipal(context.Background(), Principal{Subject: ""})
	if got := SubjectForFairness(ctx2, "wire-bob"); got != "wire-bob" {
		t.Errorf("got %q, want wire-bob (empty subject falls back)", got)
	}
}
