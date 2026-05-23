// concurrency_handlers.go — v0.10.1 bearer-authed introspection for
// the run-admitting semaphore. Operators inspect the global active +
// queued counts AND the per-user breakdown to validate that per-tenant
// fairness is engaging as configured. Read-only.
package http

import (
	"encoding/json"
	"net/http"
)

// concurrencyStatsResponse is the wire shape of GET /v1/_concurrency/stats.
// Mirrors concurrency.Stats but with explicit json tags so external
// adapter consumers see a stable contract. PerUser is omitempty so the
// (common) case of "per-user cap disabled" returns the leaner
// `{"active":N,"queued":M}` shape.
type concurrencyStatsResponse struct {
	Active  int            `json:"active"`
	Queued  int            `json:"queued"`
	PerUser map[string]int `json:"per_user,omitempty"`
}

// handleConcurrencyStats serves a point-in-time snapshot of the
// semaphore's accounting. Bearer-authed; same posture as the
// /v1/_metrics/* family. The shape is intentionally minimal — operators
// graph these counts as gauges in Grafana / Prometheus pull or curl
// them directly. The per-user breakdown is the load-bearing
// observability for v0.10.1 fairness validation.
func (s *Server) handleConcurrencyStats(w http.ResponseWriter, r *http.Request) {
	if s.sem == nil {
		// No semaphore is wired in this test-only / minimal-embed case.
		// Return 503 rather than 500 so a probe checking liveness
		// distinguishes "no fairness wired" from "broken."
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"code":"concurrency_not_wired","error":"semaphore not configured on this server"}`))
		return
	}
	st := s.sem.Stats()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(concurrencyStatsResponse{
		Active:  st.Active,
		Queued:  st.Queued,
		PerUser: st.PerUser,
	})
}
