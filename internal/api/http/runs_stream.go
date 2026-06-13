// runs_stream.go — SSE stream of run state transitions for one user.
// Phase 0 of the n8n integration RFC: trigger nodes in the n8n
// community-node package need a way to fire when a loomcycle run
// completes (or transitions to any specific state). This handler
// opens a long-lived SSE connection scoped to one user_id and emits
// one frame per transition.
//
// Transport is the existing sse.go primitives (event-stream content
// type, keepalive comment frames every 25s so reverse proxies don't
// drop the idle connection). The producer is the in-process
// runstate.Bus — fanout is lock-free per subscription, slow
// consumers drop events rather than block the publisher (the
// publisher being a finishRun* call site we MUST NEVER stall).
//
// Filter shape (all optional):
//
//	?status=running,completed,failed,cancelled   — comma-separated allowlist
//	?agent=<name>                                 — exact-match agent name
//
// Filters apply at the handler (not the bus) so the bus stays
// uniform across all subscribers and the same Subscription is
// reusable in tests with different filters.
package http

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
)

const (
	// streamKeepaliveInterval is the cadence of SSE comment-only
	// frames. Picked to be under the 30s idle-timeout of common
	// reverse proxies (nginx default, Cloudflare's free tier).
	streamKeepaliveInterval = 25 * time.Second

	// streamMaxLifetime caps any single SSE connection. Forces
	// clients to reconnect periodically, which is the right shape
	// for n8n's trigger framework (and lets us bound goroutine
	// leakage if a client never sends FIN).
	streamMaxLifetime = 30 * time.Minute
)

// parseStreamFilter converts URL query params to a
// connector.StreamUserRunStatesRequest. `?status=...,...` is
// comma-decomposed; `?agent=<name>` is taken literally.
func parseStreamFilter(userID string, q map[string][]string) connector.StreamUserRunStatesRequest {
	out := connector.StreamUserRunStatesRequest{UserID: userID}
	if vals := q["status"]; len(vals) > 0 && vals[0] != "" {
		var statuses []string
		for _, v := range vals {
			for _, s := range strings.Split(v, ",") {
				s = strings.TrimSpace(s)
				if s != "" {
					statuses = append(statuses, s)
				}
			}
		}
		out.Statuses = statuses
	}
	if vals := q["agent"]; len(vals) > 0 {
		out.Agent = strings.TrimSpace(vals[0])
	}
	return out
}

// handleStreamUserAgents serves GET /v1/users/{user_id}/agents/stream.
// Bearer-authed (the mux already wraps in authMiddleware).
//
// Failure modes:
//   - missing user_id path arg     → 400 invalid_request
//   - writer doesn't support SSE   → 500 sse_not_supported (rare; most
//     test recorders DO support Flusher)
//   - runStateBus not configured   → 503 stream_unavailable (operator
//     wired loomcycle without the bus;
//     defensive — main.go always wires it)
func (s *Server) handleStreamUserAgents(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	if userID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "user_id path arg required")
		return
	}
	if s.runStateBus == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "stream_unavailable", "run-state bus not configured")
		return
	}

	stream, ok := newSSE(w)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "sse_not_supported", "response writer does not support streaming")
		return
	}

	req := parseStreamFilter(userID, r.URL.Query())
	// RFC L/N tenant isolation: a tenant principal sees only its own tenant's
	// run transitions (run_ids/user_ids aren't secret, so without this a token
	// could stream another tenant's run states by passing its user_id).
	// principalTenantScope: tenant → own (TenantScoped), admin/legacy/open →
	// all (no scope). ?tenant= lets an admin focus one tenant, like the lists.
	tenantID, all := s.principalTenantScope(r.Context(), r.URL.Query().Get("tenant"))
	req.TenantID = tenantID
	req.TenantScoped = !all

	stream.start()

	ctx, cancel := context.WithTimeout(r.Context(), streamMaxLifetime)
	defer cancel()
	stream.startKeepalive(ctx, streamKeepaliveInterval)

	// stream_open frame: adapters consume this to confirm the
	// connection is live before any real events flow. Useful for
	// n8n's trigger-setup phase where the workflow waits for the
	// credential test before arming.
	stream.sendRaw("stream_open", map[string]any{
		"user_id":            userID,
		"filter_status":      sortedCopy(req.Statuses),
		"filter_agent":       req.Agent,
		"keepalive_interval": int(streamKeepaliveInterval.Seconds()),
	})

	visit := func(evt connector.RunStateEvent) error {
		stream.sendRaw("run_state", evt)
		return nil
	}
	_ = s.StreamUserRunStates(ctx, req, visit)
}

// sortedCopy returns a sorted copy of statuses so the stream_open
// frame's filter_status field is byte-stable regardless of query
// ordering (adapters that snapshot the open frame for caching need
// stable bytes).
func sortedCopy(statuses []string) []string {
	if len(statuses) == 0 {
		return nil
	}
	out := append([]string(nil), statuses...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
