package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// minimalServerWithResolver wires just enough of Server to exercise the
// /v1/_resolver handler, with a real resolve.Resolver attached.
func minimalServerWithResolver(t *testing.T, r *resolve.Resolver) *Server {
	t.Helper()
	cfg := &config.Config{}
	hookReg := hooks.NewRegistry()
	s := &Server{
		cfg:            cfg,
		cancelReg:      cancel.NewRegistry(),
		sessionLocks:   runner.NewSessionLockMap(),
		hookRegistry:   hookReg,
		hookDispatcher: hooks.NewDispatcher(hookReg, nil),
		sem:            concurrency.New(8, 16, 30000),
	}
	if r != nil {
		s.SetResolver(r)
	}
	return s
}

func TestResolverSnapshot_ReflectsLiveState(t *testing.T) {
	r := resolve.NewResolver(
		[]string{"deepseek", "anthropic"},
		map[string][]resolve.Candidate{
			"low":    {{Provider: "deepseek", Model: "deepseek-v4-flash"}, {Provider: "anthropic", Model: "claude-haiku-4-5"}},
			"middle": {{Provider: "deepseek", Model: "deepseek-v4-pro"}, {Provider: "anthropic", Model: "claude-sonnet-4-6"}},
		},
	)
	// Simulate a startup-probe sweep: deepseek excluded (no key),
	// anthropic reachable with two listed models, one of which is
	// stalled.
	r.SetExcluded("deepseek", "DEEPSEEK_API_KEY not set")
	r.SetReachable("anthropic", true, []string{"claude-haiku-4-5", "claude-sonnet-4-6"}, "")
	r.MarkStalled("anthropic", "claude-sonnet-4-6", "test stall")

	s := minimalServerWithResolver(t, r)
	rec := httptest.NewRecorder()
	s.handleResolverSnapshot(rec, httptest.NewRequest("GET", "/v1/_resolver", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var resp resolverSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	if resp.GeneratedAt.IsZero() {
		t.Error("generated_at zero — handler did not stamp it")
	}

	// deepseek: excluded with reason
	ds, ok := resp.Providers["deepseek"]
	if !ok {
		t.Fatal("missing deepseek in snapshot")
	}
	if !ds.Excluded {
		t.Error("deepseek.excluded = false, want true")
	}
	if !strings.Contains(ds.LastError, "DEEPSEEK_API_KEY") {
		t.Errorf("deepseek.last_error = %q, want exclusion reason", ds.LastError)
	}

	// anthropic: reachable, two models, one stalled
	an, ok := resp.Providers["anthropic"]
	if !ok {
		t.Fatal("missing anthropic in snapshot")
	}
	if an.Excluded || !an.Reachable {
		t.Errorf("anthropic state = excluded:%v reachable:%v, want excluded:false reachable:true", an.Excluded, an.Reachable)
	}
	if got := sortedKeys(an.Models); len(got) != 2 || got[0] != "claude-haiku-4-5" || got[1] != "claude-sonnet-4-6" {
		t.Errorf("anthropic models = %v, want [claude-haiku-4-5 claude-sonnet-4-6]", got)
	}
	if !an.Models["claude-sonnet-4-6"].Stalled {
		t.Error("claude-sonnet-4-6.stalled = false, want true")
	}
	if an.Models["claude-haiku-4-5"].Stalled {
		t.Error("claude-haiku-4-5.stalled = true, want false (only sonnet was stalled)")
	}
	if !an.Models["claude-haiku-4-5"].Listed || !an.Models["claude-sonnet-4-6"].Listed {
		t.Error("listed flags incomplete; both models came back from probe")
	}
}

// TestResolverSnapshot_503WhenNoResolver covers the degraded-startup
// branch: the Server boots without a resolver wired (cmd/loomcycle
// has a brief window before SetResolver is called) — the endpoint
// should return 503 with a clear code so dashboards can render
// "matrix not available" rather than misinterpret an empty-body 200.
func TestResolverSnapshot_503WhenNoResolver(t *testing.T) {
	s := minimalServerWithResolver(t, nil)
	rec := httptest.NewRecorder()
	s.handleResolverSnapshot(rec, httptest.NewRequest("GET", "/v1/_resolver", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "resolver_unavailable") {
		t.Errorf("body = %s, want resolver_unavailable code", rec.Body.String())
	}
}

// TestResolverSnapshot_JSONShape pins the wire field names. Adapters
// (TS client, dashboards) lock onto these — a rename has to be a
// deliberate version bump, not a silent drift.
func TestResolverSnapshot_JSONShape(t *testing.T) {
	r := resolve.NewResolver(nil, nil)
	r.SetReachable("anthropic", true, []string{"x"}, "")
	s := minimalServerWithResolver(t, r)

	rec := httptest.NewRecorder()
	s.handleResolverSnapshot(rec, httptest.NewRequest("GET", "/v1/_resolver", nil))
	body := rec.Body.String()

	wantFields := []string{
		`"generated_at":`,
		`"providers":`,
		`"excluded":`,
		`"reachable":`,
		`"models":`,
		`"last_check":`,
		`"listed":`,
		`"stalled":`,
	}
	for _, f := range wantFields {
		if !strings.Contains(body, f) {
			t.Errorf("missing wire field %s in response: %s", f, body)
		}
	}
}

// TestHandleResolveProbe_TriggersForceProbeAndReturnsRefreshedMatrix is
// the core behaviour of issue #88: POST /v1/_resolve/probe runs the
// force-probe callback synchronously and returns the *post*-probe
// matrix. The callback here flips deepseek from excluded → reachable,
// simulating an operator unsticking a provider that a transient outage
// had stalled. The response must reflect the flip, and the callback
// must have run exactly once.
func TestHandleResolveProbe_TriggersForceProbeAndReturnsRefreshedMatrix(t *testing.T) {
	r := resolve.NewResolver(
		[]string{"deepseek"},
		map[string][]resolve.Candidate{
			"low": {{Provider: "deepseek", Model: "deepseek-v4-flash"}},
		},
	)
	// Pre-probe state: deepseek excluded (e.g. a probe during a blip
	// failed to reach it).
	r.SetExcluded("deepseek", "transient outage")

	calls := 0
	r.SetForceProbeCallback(func(ctx context.Context) {
		calls++
		// A successful re-probe: deepseek is reachable again.
		r.SetReachable("deepseek", true, []string{"deepseek-v4-flash"}, "")
	})

	s := minimalServerWithResolver(t, r)
	rec := httptest.NewRecorder()
	s.handleResolveProbe(rec, httptest.NewRequest("POST", "/v1/_resolve/probe", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Fatalf("force-probe callback ran %d times, want exactly 1", calls)
	}

	var resp resolverSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, rec.Body.String())
	}
	ds, ok := resp.Providers["deepseek"]
	if !ok {
		t.Fatal("missing deepseek in probe response")
	}
	if ds.Excluded || !ds.Reachable {
		t.Errorf("deepseek post-probe = excluded:%v reachable:%v, want excluded:false reachable:true (the probe should have refreshed the matrix)", ds.Excluded, ds.Reachable)
	}
	if !ds.Models["deepseek-v4-flash"].Listed {
		t.Error("deepseek-v4-flash not listed post-probe; the refreshed matrix was not returned")
	}
}

// TestHandleResolveProbe_503WhenNoResolver covers the degraded-startup
// branch — same posture as the GET handler: 503 with a clear code
// rather than a misleading empty 200.
func TestHandleResolveProbe_503WhenNoResolver(t *testing.T) {
	s := minimalServerWithResolver(t, nil)
	rec := httptest.NewRecorder()
	s.handleResolveProbe(rec, httptest.NewRequest("POST", "/v1/_resolve/probe", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "resolver_unavailable") {
		t.Errorf("body = %s, want resolver_unavailable code", rec.Body.String())
	}
}

// TestHandleResolveProbe_503WhenProbeNotWired guards the honesty
// invariant: a resolver with no force-probe callback installed (no
// probe loop, as in a degraded startup) must 503 rather
// than 200 — ForceProbe is a silent no-op when unwired, and returning
// 200 with a stale matrix would be misleading for a "re-probe now"
// endpoint.
func TestHandleResolveProbe_503WhenProbeNotWired(t *testing.T) {
	r := resolve.NewResolver(nil, nil)
	r.SetReachable("anthropic", true, []string{"x"}, "")
	// Deliberately do NOT call SetForceProbeCallback.
	s := minimalServerWithResolver(t, r)
	rec := httptest.NewRecorder()
	s.handleResolveProbe(rec, httptest.NewRequest("POST", "/v1/_resolve/probe", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (probe not wired)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "probe_unavailable") {
		t.Errorf("body = %s, want probe_unavailable code", rec.Body.String())
	}
}
