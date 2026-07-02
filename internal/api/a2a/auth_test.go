package a2a

import (
	"context"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// TestFrontierAuthenticator_OperatorTokenOnlyIsGated is the regression for the
// A2A open-in-operator-token-only-mode bug: when auth is configured (an admin
// OperatorTokenDef exists) but NO legacy LOOMCYCLE_AUTH_TOKEN is set, the
// authenticator must still REQUIRE a valid bearer — the old code built the
// authenticator only from the legacy token, so this posture left A2A open.
func TestFrontierAuthenticator_OperatorTokenOnlyIsGated(t *testing.T) {
	authConfigured := func(context.Context) bool { return true } // an admin token exists
	resolve := func(_ context.Context, bearer string) (auth.Principal, bool) {
		if bearer == "lct_alice" {
			return auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}, true
		}
		return auth.Principal{}, false
	}
	authn := FrontierAuthenticator(authConfigured, resolve)

	// No credential → rejected (the interceptor turns this into ErrUnauthenticated).
	if _, ok := authn(http.Header{}); ok {
		t.Error("operator-token-only mode: missing bearer must be rejected, got ok=true (A2A open!)")
	}
	// Wrong credential → rejected.
	bad := http.Header{"Authorization": []string{"Bearer nope"}}
	if _, ok := authn(bad); ok {
		t.Error("operator-token-only mode: invalid bearer must be rejected")
	}
	// Valid operator token → accepted, attributed by subject.
	good := http.Header{"Authorization": []string{"Bearer lct_alice"}}
	name, ok := authn(good)
	if !ok || name != "alice" {
		t.Errorf("valid operator token: name=%q ok=%v, want (\"alice\", true)", name, ok)
	}
}

// TestFrontierAuthenticator_OpenModeAnonymous: with no auth configured (true
// open dev mode), the authenticator passes requests through as anonymous,
// mirroring the HTTP authMiddleware.
func TestFrontierAuthenticator_OpenModeAnonymous(t *testing.T) {
	authn := FrontierAuthenticator(func(context.Context) bool { return false }, nil)
	name, ok := authn(http.Header{})
	if !ok || name != "anonymous" {
		t.Errorf("open mode: name=%q ok=%v, want (\"anonymous\", true)", name, ok)
	}
}

// TestFrontierAuthenticator_LegacyPeerKeepsName: a legacy-token peer keeps the
// historical "a2a-peer" attribution name (no run-attribution regression), and
// the legacy token resolving is delegated to resolve() — so once HTTP retires
// it (an admin token exists → resolve returns false) A2A rejects it too.
func TestFrontierAuthenticator_LegacyPeerKeepsName(t *testing.T) {
	resolve := func(_ context.Context, bearer string) (auth.Principal, bool) {
		if bearer == "legacy-secret" {
			return auth.Principal{TenantID: "default", Subject: "default", Legacy: true, Scopes: []string{auth.ScopeAdmin}}, true
		}
		return auth.Principal{}, false
	}
	authn := FrontierAuthenticator(func(context.Context) bool { return true }, resolve)
	name, ok := authn(http.Header{"Authorization": []string{"Bearer legacy-secret"}})
	if !ok || name != "a2a-peer" {
		t.Errorf("legacy peer: name=%q ok=%v, want (\"a2a-peer\", true)", name, ok)
	}
}
