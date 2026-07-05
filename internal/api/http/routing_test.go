package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

func routingTestServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.Config{
		ProviderPriority: []string{"deepseek", "anthropic"},
		Tiers: map[string][]config.TierCandidate{
			"middle": {
				{Provider: "deepseek", Model: "deepseek-v4-pro"},
				{Provider: "anthropic", Model: "claude-sonnet-4-6"},
			},
		},
		Concurrency: config.Concurrency{MaxConcurrentRuns: 1, MaxQueueDepth: 1, QueueTimeoutMS: 100},
	}
	cfg.Env.AuthToken = ""
	srv := New(cfg, &stubResolver{}, nil, concurrency.New(1, 1, time.Second), nil)
	res := resolve.NewResolver([]string{"deepseek", "anthropic"}, map[string][]resolve.Candidate{
		"middle": {
			{Provider: "deepseek", Model: "deepseek-v4-pro"},
			{Provider: "anthropic", Model: "claude-sonnet-4-6"},
		},
	})
	// Seed availability so Snapshot() has entries (the admin view's provider
	// header + per-candidate availability). deepseek up, anthropic down — so the
	// admin view's "selected" lands on the reachable primary.
	res.SetReachable("deepseek", true, []string{"deepseek-v4-pro"}, "")
	res.SetReachable("anthropic", false, nil, "probe failed")
	srv.SetResolver(res)
	return srv
}

func routingFor(t *testing.T, srv *Server, scopes []string) routingResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/_routing", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), auth.Principal{Scopes: scopes}))
	rr := httptest.NewRecorder()
	srv.handleRouting(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp routingResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rr.Body.String())
	}
	return resp
}

// TestRouting_AdminSeesCascadeAndAvailability: an admin gets the ordered cascade
// (deepseek primary, anthropic fallback) with live-availability fields populated
// and the active-providers header.
func TestRouting_AdminSeesCascadeAndAvailability(t *testing.T) {
	srv := routingTestServer(t)
	resp := routingFor(t, srv, []string{auth.ScopeAdmin})

	if !resp.Admin {
		t.Error("admin=false for an admin principal")
	}
	if len(resp.UserTiers) != 1 { // no user_tiers configured → single library-mode entry
		t.Fatalf("user_tiers = %d, want 1", len(resp.UserTiers))
	}
	var mid *routingTier
	for i := range resp.UserTiers[0].Tiers {
		if resp.UserTiers[0].Tiers[i].Tier == "middle" {
			mid = &resp.UserTiers[0].Tiers[i]
		}
	}
	if mid == nil || len(mid.Cascade) != 2 {
		t.Fatalf("middle cascade = %+v, want 2 candidates", mid)
	}
	if mid.Cascade[0].Provider != "deepseek" || !mid.Cascade[0].Primary {
		t.Errorf("cascade[0] = %+v, want deepseek primary", mid.Cascade[0])
	}
	if mid.Cascade[1].Provider != "anthropic" || mid.Cascade[1].Primary {
		t.Errorf("cascade[1] = %+v, want anthropic non-primary", mid.Cascade[1])
	}
	// deepseek is up + listed → available and selected (what runs now).
	if mid.Cascade[0].Available == nil || !*mid.Cascade[0].Available ||
		mid.Cascade[0].Selected == nil || !*mid.Cascade[0].Selected {
		t.Errorf("cascade[0] (deepseek) should be available + selected; got %+v", mid.Cascade[0])
	}
	// anthropic is down → not available, not selected.
	if mid.Cascade[1].Available == nil || *mid.Cascade[1].Available ||
		mid.Cascade[1].Selected == nil || *mid.Cascade[1].Selected {
		t.Errorf("cascade[1] (anthropic, down) should be unavailable + not selected; got %+v", mid.Cascade[1])
	}
	if len(resp.Providers) == 0 {
		t.Fatal("admin response must include the active-providers header")
	}
	// Admin sees the raw provider probe error (anthropic was seeded "probe failed").
	var anthropic *routingProvider
	for i := range resp.Providers {
		if resp.Providers[i].Provider == "anthropic" {
			anthropic = &resp.Providers[i]
		}
	}
	if anthropic == nil || anthropic.LastError != "probe failed" {
		t.Errorf("admin providers header must carry the raw last_error; got %+v", anthropic)
	}
}

// routingForPrincipal is routingFor with a full principal (not just scopes) so a
// test can supply a tenant + subject — the RFC AX keyable filter reads them.
func routingForPrincipal(t *testing.T, srv *Server, p auth.Principal) routingResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/_routing", nil)
	req = req.WithContext(auth.WithPrincipal(req.Context(), p))
	rr := httptest.NewRecorder()
	srv.handleRouting(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rr.Code, rr.Body.String())
	}
	var resp routingResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v; body: %s", err, rr.Body.String())
	}
	return resp
}

// middleTier finds the "middle" tier in the (single library-mode) user tier.
func middleTier(t *testing.T, resp routingResponse) *routingTier {
	t.Helper()
	if len(resp.UserTiers) == 0 {
		t.Fatalf("no user_tiers in response")
	}
	for i := range resp.UserTiers[0].Tiers {
		if resp.UserTiers[0].Tiers[i].Tier == "middle" {
			return &resp.UserTiers[0].Tiers[i]
		}
	}
	t.Fatalf("no middle tier in response")
	return nil
}

// TestRouting_RestrictedTenantFilteredToKeyableProviders is the RFC AX routing-view
// regression: with the operator-key gate ON, a non-admin caller's cascade is
// filtered to providers the tenant can key itself. The one keyed provider survives
// only when the tenant has a credential for its env-var; with nothing keyable the
// tier renders empty (the true picture of what the tenant may run), and
// operator_key_restricted flags the filtered view. Fails on the pre-filter handler,
// which showed the keyed provider regardless. NOTE it fires for a substrate:tenant
// principal (admin=false) even though that scope is tenant-IMPLIED
// providers:operator-key — the view is keyed off (gate && !admin), NOT
// auth.OperatorKeyRestricted (which would never fire here; see handleRouting).
func TestRouting_RestrictedTenantFilteredToKeyableProviders(t *testing.T) {
	tenant := auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}

	// (a) tenant can key nothing (credKeyable nil) → the keyed provider is
	// filtered out and the tier is empty.
	srvNone, _ := operatorKeyTierServer(t, completingKeyed("KEYED_API_KEY", "", ""), true, nil)
	respNone := routingForPrincipal(t, srvNone, tenant)
	if !respNone.OperatorKeyRestricted {
		t.Error("operator_key_restricted=false for a gate-on non-admin caller")
	}
	if mid := middleTier(t, respNone); len(mid.Cascade) != 0 {
		t.Errorf("middle cascade = %+v, want 0 candidates (tenant can key nothing)", mid.Cascade)
	}

	// (b) tenant CAN key the provider → it survives the filter AND carries live
	// availability (RFC AX visibility). The keyed provider is seeded reachable +
	// listed, so it reads available + selected.
	keyable := func(_ context.Context, _, _, _, name string) bool { return name == "KEYED_API_KEY" }
	srvKey, _ := operatorKeyTierServer(t, completingKeyed("KEYED_API_KEY", "", ""), true, keyable)
	respKey := routingForPrincipal(t, srvKey, tenant)
	mid := middleTier(t, respKey)
	if len(mid.Cascade) != 1 || mid.Cascade[0].Provider != "keyed" {
		t.Fatalf("middle cascade = %+v, want the keyed provider kept", mid.Cascade)
	}
	if mid.Cascade[0].Available == nil || !*mid.Cascade[0].Available ||
		mid.Cascade[0].Selected == nil || !*mid.Cascade[0].Selected {
		t.Errorf("restricted-tenant kept candidate must carry availability; got %+v", mid.Cascade[0])
	}
	// The (filtered) active-providers header is shown to the tenant with
	// last_error redacted, and lists only the keyable provider.
	if len(respKey.Providers) != 1 || respKey.Providers[0].Provider != "keyed" {
		t.Fatalf("providers header = %+v, want just the keyable provider", respKey.Providers)
	}
	if respKey.Providers[0].LastError != "" {
		t.Errorf("tenant provider header must redact last_error; got %q", respKey.Providers[0].LastError)
	}
}

// TestRouting_AdminUnaffectedByOperatorKeyGate: with the gate ON an admin still
// sees the full cascade (no keyable filter) and operator_key_restricted stays
// false — the filter is a tenant-only view.
func TestRouting_AdminUnaffectedByOperatorKeyGate(t *testing.T) {
	srv, _ := operatorKeyTierServer(t, completingKeyed("KEYED_API_KEY", "", ""), true, nil)
	resp := routingForPrincipal(t, srv, auth.Principal{Scopes: []string{auth.ScopeAdmin}})
	if resp.OperatorKeyRestricted {
		t.Error("operator_key_restricted=true for an admin; the gate must not filter the admin view")
	}
	if mid := middleTier(t, resp); len(mid.Cascade) != 1 {
		t.Errorf("admin middle cascade = %+v, want 1 candidate (unfiltered)", mid.Cascade)
	}
}

// TestRouting_TenantSeesAvailabilityWithRedactedError: a substrate:tenant
// principal now gets the SAME live availability an admin does (per-candidate
// fields + the active-providers header), so it can see which providers are up —
// but the raw last_error string is redacted, since it can leak operator infra
// detail. Fails on the pre-change handler, which withheld availability + the
// header from a tenant entirely.
func TestRouting_TenantSeesAvailabilityWithRedactedError(t *testing.T) {
	srv := routingTestServer(t)
	resp := routingFor(t, srv, []string{auth.ScopeTenant})

	if resp.Admin {
		t.Error("admin=true for a tenant principal")
	}
	mid := middleTier(t, resp)
	if len(mid.Cascade) != 2 {
		t.Fatalf("middle cascade = %+v, want 2 candidates", mid.Cascade)
	}
	if mid.Cascade[0].Provider != "deepseek" || mid.Cascade[0].Model != "deepseek-v4-pro" {
		t.Errorf("cascade[0] = %+v, want deepseek/deepseek-v4-pro", mid.Cascade[0])
	}
	// Cascade now carries live availability for a tenant too (deepseek is
	// reachable + listed → available + selected).
	if mid.Cascade[0].Available == nil || !*mid.Cascade[0].Available ||
		mid.Cascade[0].Selected == nil || !*mid.Cascade[0].Selected {
		t.Errorf("tenant cascade[0] (deepseek) should be available + selected; got %+v", mid.Cascade[0])
	}
	// The active-providers header is shown to the tenant...
	if len(resp.Providers) == 0 {
		t.Fatal("tenant response must include the active-providers header")
	}
	// ...but every last_error is redacted, even anthropic's real "probe failed".
	for _, p := range resp.Providers {
		if p.LastError != "" {
			t.Errorf("tenant provider %q must have last_error redacted; got %q", p.Provider, p.LastError)
		}
	}
}
