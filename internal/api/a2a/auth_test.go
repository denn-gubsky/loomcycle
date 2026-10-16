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
	authn := FrontierAuthenticator(authConfigured, resolve, false)

	// No credential → rejected (the interceptor turns this into ErrUnauthenticated).
	if _, ok := authn(context.Background(), http.Header{}); ok {
		t.Error("operator-token-only mode: missing bearer must be rejected, got ok=true (A2A open!)")
	}
	// Wrong credential → rejected.
	bad := http.Header{"Authorization": []string{"Bearer nope"}}
	if _, ok := authn(context.Background(), bad); ok {
		t.Error("operator-token-only mode: invalid bearer must be rejected")
	}
	// Valid operator token → accepted, attributed by subject.
	good := http.Header{"Authorization": []string{"Bearer lct_alice"}}
	peer, ok := authn(context.Background(), good)
	if !ok || peer.Name != "alice" {
		t.Errorf("valid operator token: name=%q ok=%v, want (\"alice\", true)", peer.Name, ok)
	}
}

// TestFrontierAuthenticator_RestrictionDerivedFromScopes pins the RFC AX
// anti-bypass: with the gate ON, the frontier derives each peer's operator-key
// restriction from its OWN resolved scopes — a granular peer lacking
// providers:operator-key is restricted; a peer holding it (or the whole
// substrate:tenant scope) is not. With the gate OFF nobody is restricted.
func TestFrontierAuthenticator_RestrictionDerivedFromScopes(t *testing.T) {
	resolve := func(_ context.Context, bearer string) (auth.Principal, bool) {
		switch bearer {
		case "granular": // runs:create only — no operator-key scope
			return auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate}}, true
		case "keyed": // explicitly granted the operator-key scope
			return auth.Principal{TenantID: "acme", Subject: "cara", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeProvidersOperatorKey}}, true
		}
		return auth.Principal{}, false
	}
	hdr := func(b string) http.Header { return http.Header{"Authorization": []string{"Bearer " + b}} }
	authConfigured := func(context.Context) bool { return true }

	// Gate ON: granular peer is restricted; keyed peer is not.
	on := FrontierAuthenticator(authConfigured, resolve, true)
	if peer, ok := on(context.Background(), hdr("granular")); !ok || !peer.Restricted {
		t.Errorf("gate on, granular peer: (restricted=%v, ok=%v), want (true, true)", peer.Restricted, ok)
	}
	if peer, ok := on(context.Background(), hdr("keyed")); !ok || peer.Restricted {
		t.Errorf("gate on, keyed peer: (restricted=%v, ok=%v), want (false, true)", peer.Restricted, ok)
	}

	// Gate OFF: nobody is restricted (byte-identical to pre-RFC-AX).
	off := FrontierAuthenticator(authConfigured, resolve, false)
	if peer, ok := off(context.Background(), hdr("granular")); !ok || peer.Restricted {
		t.Errorf("gate off, granular peer: (restricted=%v, ok=%v), want (false, true)", peer.Restricted, ok)
	}
}

// TestFrontierAuthenticator_OpenModeAnonymous: with no auth configured (true
// open dev mode), the authenticator passes requests through as anonymous,
// mirroring the HTTP authMiddleware.
func TestFrontierAuthenticator_OpenModeAnonymous(t *testing.T) {
	authn := FrontierAuthenticator(func(context.Context) bool { return false }, nil, true)
	peer, ok := authn(context.Background(), http.Header{})
	if !ok || peer.Name != "anonymous" || peer.Restricted {
		t.Errorf("open mode: name=%q restricted=%v ok=%v, want (\"anonymous\", false, true)", peer.Name, peer.Restricted, ok)
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
	// Gate ON to also prove a legacy peer is never restricted (fail-open).
	authn := FrontierAuthenticator(func(context.Context) bool { return true }, resolve, true)
	peer, ok := authn(context.Background(), http.Header{"Authorization": []string{"Bearer legacy-secret"}})
	if !ok || peer.Name != "a2a-peer" || peer.Restricted {
		t.Errorf("legacy peer: name=%q restricted=%v ok=%v, want (\"a2a-peer\", false, true)", peer.Name, peer.Restricted, ok)
	}
}

// A token limited to certain agents is refused on this plane outright: a card
// starts whichever agent it exposes, and no principal reaches the run for an
// allowlist to be checked against.
func TestFrontierAuthenticator_RefusesATokenWithAnAgentAllowlist(t *testing.T) {
	resolve := func(_ context.Context, bearer string) (auth.Principal, bool) {
		switch bearer {
		case "restricted":
			return auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsCreate},
				Restrictions: auth.Restrictions{Agents: []string{"score"}}}, true
		case "free":
			return auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeRunsCreate}}, true
		}
		return auth.Principal{}, false
	}
	authn := FrontierAuthenticator(func(context.Context) bool { return true }, resolve, false)
	if _, ok := authn(context.Background(), http.Header{"Authorization": []string{"Bearer restricted"}}); ok {
		t.Error("a restricted token was accepted as an A2A peer")
	}
	if _, ok := authn(context.Background(), http.Header{"Authorization": []string{"Bearer free"}}); !ok {
		t.Error("an unrestricted token was refused")
	}
}

// A peer starts runs, so it needs the scope that starts runs, and a token whose
// rate or retention this plane cannot enforce is refused as an allowlisted one
// is: no principal reaches the run, so the rate would never be counted and a
// no-trace token's task would be stored. An override lock holds trivially here
// — a message carries no per-run overrides — so it is not refused.
func TestFrontierAuthenticator_RefusesWhatThisPlaneCannotHold(t *testing.T) {
	member := []string{auth.ScopeRunsCreate}
	principals := map[string]auth.Principal{
		"rated":     {TenantID: "acme", Subject: "dev", Scopes: member, Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1}}},
		"quiet":     {TenantID: "acme", Subject: "dev", Scopes: member, Restrictions: auth.Restrictions{Retention: auth.RetentionNone}},
		"readonly":  {TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeRunsRead, auth.ScopeChannelPublish}},
		"locked":    {TenantID: "acme", Subject: "dev", Scopes: member, Restrictions: auth.Restrictions{Overrides: &auth.OverridePolicy{}}},
		"isolated":  {TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeUser}},
		"operator":  {TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}},
		"unlimited": {TenantID: "acme", Subject: "dev", Scopes: member},
	}
	resolve := func(_ context.Context, bearer string) (auth.Principal, bool) {
		p, ok := principals[bearer]
		return p, ok
	}
	authn := FrontierAuthenticator(func(context.Context) bool { return true }, resolve, false)
	for bearer, want := range map[string]bool{
		"rated": false, "quiet": false, "readonly": false,
		"locked": true, "isolated": true, "operator": true, "unlimited": true,
	} {
		if _, ok := authn(context.Background(), http.Header{"Authorization": []string{"Bearer " + bearer}}); ok != want {
			t.Errorf("%s: accepted=%v, want %v", bearer, ok, want)
		}
	}
}
