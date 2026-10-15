// concurrency_handlers.go — v0.10.1 bearer-authed introspection for
// the run-admitting semaphore. Operators inspect the global active +
// queued counts AND the per-user breakdown to validate that per-tenant
// fairness is engaging as configured. Read-only.
package http

import (
	"encoding/json"
	"net/http"

	"github.com/denn-gubsky/loomcycle/internal/config"
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
	// Providers is every provider that caps anything, keyed by provider id.
	// omitempty so the (common) uncapped deployment returns the leaner shape
	// without a providers block.
	Providers map[string]providerGateStat `json:"providers,omitempty"`
}

// providerGateStat is one capped provider's live counts in the stats response.
//
// Active and Queued are its CHAT lane (model calls in flight, and waiting for a
// slot) — the two fields this block has always had; 0 for a provider that caps
// no chat calls. Lanes is every lane the provider has, by kind, and the model
// fields are its cap on models computing at once.
type providerGateStat struct {
	Active int `json:"active"`
	Queued int `json:"queued"`
	// Lanes is the provider's lanes by model kind (chat, decision, embedder).
	Lanes map[string]laneStat `json:"lanes,omitempty"`
	// ModelsInFlight is the calls ADMITTED per model identity, as the provider's
	// cap on models computing at once counts them (a model at another context
	// size is another model). A call is counted from the moment its model is
	// admitted, so it includes calls still waiting for a slot in their lane:
	// the number of KEYS is the models in flight, a value can exceed the lane's
	// cap. Present only while that cap is set and something is admitted.
	ModelsInFlight map[string]int `json:"models_in_flight,omitempty"`
	// ModelWaiters is the calls waiting for their model to be admitted.
	ModelWaiters int `json:"model_waiters,omitempty"`
}

// laneStat is one lane's live counts.
type laneStat struct {
	MaxConcurrent int `json:"max_concurrent"`
	// Active is the calls in flight; Reserved the slots idle but held for the
	// run that last used them (affinity_hold_ms); Queued every call waiting, of
	// which QueuedBackground are maintenance work.
	Active           int `json:"active"`
	Reserved         int `json:"reserved"`
	Queued           int `json:"queued"`
	QueuedBackground int `json:"queued_background"`
	// Grants counts calls admitted since the lane was built; Preemptions the
	// times a run gave its held slot up to a waiter (quantum_calls).
	Grants      uint64 `json:"grants"`
	Preemptions uint64 `json:"preemptions"`
	// BlockedOnModel is the calls among Queued held back by their model's own
	// cap in the lane (model_caps): they wait even while a slot is free.
	BlockedOnModel int `json:"blocked_on_model"`
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
	resp := concurrencyStatsResponse{
		Active:  st.Active,
		Queued:  st.Queued,
		PerUser: st.PerUser,
	}
	// Fold in the per-provider breakdown when any provider caps anything. nil
	// (none) leaves the field omitted.
	if ps := s.callSched.Stats(); len(ps) > 0 {
		resp.Providers = make(map[string]providerGateStat, len(ps))
		for id, p := range ps {
			chat := p.Lanes[config.ModelKindChat]
			st := providerGateStat{
				Active:         chat.Active,
				Queued:         chat.Queued,
				ModelsInFlight: p.Models.InFlight,
				ModelWaiters:   p.Models.Waiting,
			}
			if len(p.Lanes) > 0 {
				st.Lanes = make(map[string]laneStat, len(p.Lanes))
				for kind, l := range p.Lanes {
					st.Lanes[kind] = laneStat{
						MaxConcurrent: l.MaxConcurrent, Active: l.Active, Reserved: l.Reserved,
						Queued: l.Queued, QueuedBackground: l.QueuedBackground,
						Grants: l.Grants, Preemptions: l.Preemptions,
						BlockedOnModel: l.BlockedOnModel,
					}
				}
			}
			resp.Providers[id] = st
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
