package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestFetchFallbackCount_HappyPath confirms the driver hits
// /v1/_events?type=provider_fallback with the correct query string
// and reads `total` (NOT `len(events)`) for the fallback count. The
// `Total` field is the unbounded match count for the filter; using it
// — and asking for limit=1 — keeps the response cheap regardless of
// how many fallbacks fired across an x1000 run.
func TestFetchFallbackCount_HappyPath(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Header.Get("Authorization"), "Bearer test-token"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		gotURL = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"events": []any{},
			"total":  42,
			"limit":  1,
			"offset": 0,
		})
	}))
	defer srv.Close()

	c := newClient(srv.URL, "test-token")
	from := time.Date(2026, 5, 27, 10, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	got := fetchFallbackCount(c, from, to)
	if got != 42 {
		t.Errorf("fetchFallbackCount = %d, want 42", got)
	}
	// URL should encode filter + the limit=1 micro-optimization.
	want := "/v1/_events?type=provider_fallback&from=2026-05-27T10:00:00Z&to=2026-05-27T11:00:00Z&limit=1"
	if gotURL != want {
		t.Errorf("URL = %q, want %q", gotURL, want)
	}
}

// TestFetchFallbackCount_ServerError verifies the driver surfaces
// failure as -1 (the "unavailable" sentinel) rather than treating a
// failed query as zero fallbacks — which would silently mask a real
// API outage during a load test.
func TestFetchFallbackCount_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newClient(srv.URL, "test-token")
	got := fetchFallbackCount(c, time.Now(), time.Now().Add(time.Minute))
	if got != -1 {
		t.Errorf("fetchFallbackCount on 500 = %d, want -1", got)
	}
}

// TestProviderDistribution_PivotsByProvider exercises the
// circuit-result → distribution pivot that drives the "Providers:"
// summary line. Three circuits, mixed providers, with one
// pre-migration row (empty Provider — surfaces as "unknown" via the
// runOneCircuit fallback, NOT counted as anthropic/etc).
func TestProviderDistribution_PivotsByProvider(t *testing.T) {
	results := []circuitResult{
		{
			CircuitID: 1,
			AgentProvider: map[string]string{
				"researcher": "anthropic-oauth-dev",
				"editor":     "anthropic-oauth-dev",
				"evaluator":  "anthropic-oauth-dev",
			},
		},
		{
			CircuitID: 2,
			// Researcher got routed to deepseek mid-flight (v0.8.2
			// fallback fired on a 429), the other two stayed on the
			// primary. This is exactly the kind of asymmetry the new
			// telemetry exists to surface.
			AgentProvider: map[string]string{
				"researcher": "deepseek",
				"editor":     "anthropic-oauth-dev",
				"evaluator":  "anthropic-oauth-dev",
			},
		},
		{
			CircuitID: 3,
			// Pre-v0.12.7 server case — empty Provider on the wire
			// becomes "unknown" in the result map (see
			// runOneCircuit fallback). All three roles unknown.
			AgentProvider: map[string]string{
				"researcher": "unknown",
				"editor":     "unknown",
				"evaluator":  "unknown",
			},
		},
	}
	dist := providerDistribution(results)
	if dist["anthropic-oauth-dev"] != 5 {
		t.Errorf("anthropic-oauth-dev = %d, want 5", dist["anthropic-oauth-dev"])
	}
	if dist["deepseek"] != 1 {
		t.Errorf("deepseek = %d, want 1", dist["deepseek"])
	}
	if dist["unknown"] != 3 {
		t.Errorf("unknown = %d, want 3", dist["unknown"])
	}
	if len(dist) != 3 {
		var keys []string
		for k := range dist {
			keys = append(keys, k)
		}
		t.Errorf("len(dist) = %d (keys=%s), want 3", len(dist), strings.Join(keys, ","))
	}
}
