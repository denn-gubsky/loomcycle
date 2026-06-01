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
