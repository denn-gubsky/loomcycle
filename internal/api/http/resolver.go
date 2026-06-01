package http

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/resolve"
)

// resolverSnapshotResponse is the wire shape returned by GET /v1/_resolver.
// Uses snake_case explicitly (the in-process resolve.Availability struct
// has no JSON tags on purpose — wire and runtime concerns are kept
// separate so internal renames don't churn the public surface).
type resolverSnapshotResponse struct {
	// GeneratedAt is when this snapshot was assembled (server time).
	// Useful when polling: dashboards can show "matrix as of …" rather
	// than guessing from the LastCheck of one provider.
	GeneratedAt time.Time `json:"generated_at"`
	// Providers maps provider id ("anthropic", "openai", "deepseek",
	// "ollama") to the wire-shape availability struct.
	Providers map[string]wireProviderAvailability `json:"providers"`
}

type wireProviderAvailability struct {
	Excluded  bool                       `json:"excluded"`
	Reachable bool                       `json:"reachable"`
	Models    map[string]wireModelStatus `json:"models"`
	LastCheck time.Time                  `json:"last_check"`
	LastError string                     `json:"last_error,omitempty"`
}

type wireModelStatus struct {
	Listed  bool `json:"listed"`
	Stalled bool `json:"stalled"`
}

// handleResolverSnapshot returns the resolver's availability matrix
// as JSON. Bearer-authed (operator-only — the matrix exposes which
// providers and models are wired up, which is config-shape that
// external consumers shouldn't see).
//
// 503 when no resolver is configured (degraded-startup mode where
// the Server boots but Resolve falls back to explicit-pin only).
// Operators should treat 503 here as "matrix not available" rather
// than "matrix is empty" — the v0.6.x explicit-pin path doesn't
// populate one.
func (s *Server) handleResolverSnapshot(w http.ResponseWriter, _ *http.Request) {
	if s.resolver == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "resolver_unavailable",
			"resolver not configured; the server is in degraded startup mode")
		return
	}
	snap := s.resolver.Snapshot()
	resp := resolverSnapshotResponse{
		GeneratedAt: time.Now().UTC(),
		Providers:   make(map[string]wireProviderAvailability, len(snap)),
	}
	for provider, avail := range snap {
		resp.Providers[provider] = toWireAvailability(avail)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// handleResolveProbe triggers an immediate, synchronous re-probe of
// every configured provider and returns the refreshed matrix in the
// same wire shape as GET /v1/_resolver. Bearer-authed (operator-only).
//
// The motivating case (issue #88): a transient network blip during a
// periodic probe marks every provider stalled, and production 503s for
// up to a full probe interval (default 15 min) until the next tick.
// This endpoint lets an operator unstick the matrix immediately —
// without a restart, which would drop in-flight runs.
//
// Per-provider failures are NOT errors here: a provider that is still
// unreachable after the probe comes back as reachable:false with its
// last_error set, exactly like the periodic sweep records it. The
// handler only fails when it cannot run a probe at all (no resolver, or
// no probe loop wired — see ForceProbeWired).
func (s *Server) handleResolveProbe(w http.ResponseWriter, r *http.Request) {
	if s.resolver == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "resolver_unavailable",
			"resolver not configured; the server is in degraded startup mode")
		return
	}
	if !s.resolver.ForceProbeWired() {
		writeJSONError(w, http.StatusServiceUnavailable, "probe_unavailable",
			"no probe loop wired; the runtime cannot trigger an immediate re-probe")
		return
	}
	// Blocking: returns once the probe completes. main.go's probe loop
	// applies per-provider timeouts, so a slow/unreachable upstream
	// bounds this to a few seconds rather than hanging the request.
	s.resolver.ForceProbe(r.Context())

	snap := s.resolver.Snapshot()
	resp := resolverSnapshotResponse{
		GeneratedAt: time.Now().UTC(),
		Providers:   make(map[string]wireProviderAvailability, len(snap)),
	}
	for provider, avail := range snap {
		resp.Providers[provider] = toWireAvailability(avail)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

func toWireAvailability(a resolve.Availability) wireProviderAvailability {
	models := make(map[string]wireModelStatus, len(a.Models))
	for name, st := range a.Models {
		models[name] = wireModelStatus{Listed: st.Listed, Stalled: st.Stalled}
	}
	return wireProviderAvailability{
		Excluded:  a.Excluded,
		Reachable: a.Reachable,
		Models:    models,
		LastCheck: a.LastCheck,
		LastError: a.LastError,
	}
}

// sortedKeys is a small helper used by the snapshot test to compare
// model lists deterministically. Lives next to the producing handler
// so the test can import it via the same package.
func sortedKeys(m map[string]wireModelStatus) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
