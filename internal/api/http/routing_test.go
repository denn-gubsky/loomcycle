package http

import (
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
		t.Error("admin response must include the active-providers header")
	}
}

// TestRouting_TenantGetsStrippedView: a substrate:tenant principal sees the
// config cascade (provider/model per tier) but NOT the live availability fields
// or the active-providers infra header.
func TestRouting_TenantGetsStrippedView(t *testing.T) {
	srv := routingTestServer(t)
	resp := routingFor(t, srv, []string{auth.ScopeTenant})

	if resp.Admin {
		t.Error("admin=true for a tenant principal")
	}
	if len(resp.Providers) != 0 {
		t.Errorf("tenant must not see the active-providers header; got %+v", resp.Providers)
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
	// Cascade (provider/model + primary) is present...
	if mid.Cascade[0].Provider != "deepseek" || mid.Cascade[0].Model != "deepseek-v4-pro" {
		t.Errorf("cascade[0] = %+v, want deepseek/deepseek-v4-pro", mid.Cascade[0])
	}
	// ...but the live-availability/infra fields are stripped.
	if mid.Cascade[0].Available != nil || mid.Cascade[0].Selected != nil ||
		mid.Cascade[0].Stalled != nil || mid.Cascade[0].Reachable != nil {
		t.Errorf("tenant cascade must NOT carry availability/infra fields; got %+v", mid.Cascade[0])
	}
}
