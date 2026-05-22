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
//   ?status=running,completed,failed,cancelled   — comma-separated allowlist
//   ?agent=<name>                                 — exact-match agent name
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

	"github.com/denn-gubsky/loomcycle/internal/runstate"
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

// runStateFilter is the per-request filter parsed from query params.
// A zero filter matches every event.
type runStateFilter struct {
	statuses map[string]bool
	agent    string
}

func parseRunStateFilter(q map[string][]string) runStateFilter {
	f := runStateFilter{}
	if vals := q["status"]; len(vals) > 0 && vals[0] != "" {
		f.statuses = make(map[string]bool)
		for _, v := range vals {
			for _, s := range strings.Split(v, ",") {
				s = strings.TrimSpace(s)
				if s != "" {
					f.statuses[s] = true
				}
			}
		}
	}
	if vals := q["agent"]; len(vals) > 0 {
		f.agent = strings.TrimSpace(vals[0])
	}
	return f
}

func (f runStateFilter) matches(evt runstate.RunStateEvent) bool {
	if f.agent != "" && evt.Agent != f.agent {
		return false
	}
	if f.statuses != nil && !f.statuses[evt.Status] {
		return false
	}
	return true
}

// handleStreamUserAgents serves GET /v1/users/{user_id}/agents/stream.
// Bearer-authed (the mux already wraps in authMiddleware).
//
// Failure modes:
//   - missing user_id path arg     → 400 invalid_request
//   - writer doesn't support SSE   → 500 sse_not_supported (rare; most
//                                     test recorders DO support Flusher)
//   - runStateBus not configured   → 503 stream_unavailable (operator
//                                     wired loomcycle without the bus;
//                                     defensive — main.go always wires it)
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

	filter := parseRunStateFilter(r.URL.Query())

	stream.start()

	// Cap connection lifetime so a hung client doesn't pin a
	// goroutine forever.
	ctx, cancel := context.WithTimeout(r.Context(), streamMaxLifetime)
	defer cancel()
	stream.startKeepalive(ctx, streamKeepaliveInterval)

	sub := s.runStateBus.Subscribe(userID)
	defer sub.Close()

	// Emit an initial "stream_open" frame so adapters can confirm
	// the connection is live before any real events flow. Helpful
	// for n8n's trigger-setup phase where the workflow waits for
	// the credential test before arming.
	stream.sendRaw("stream_open", map[string]any{
		"user_id":            userID,
		"filter_status":      filterStatusesList(filter),
		"filter_agent":       filter.agent,
		"keepalive_interval": int(streamKeepaliveInterval.Seconds()),
	})

	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-sub.C:
			if !ok {
				return
			}
			if !filter.matches(evt) {
				continue
			}
			stream.sendRaw("run_state", evt)
		}
	}
}

// filterStatusesList is a stable representation of f.statuses for
// the stream_open frame (the map iteration order is non-deterministic;
// adapters that snapshot the open frame need stable bytes).
func filterStatusesList(f runStateFilter) []string {
	if len(f.statuses) == 0 {
		return nil
	}
	out := make([]string, 0, len(f.statuses))
	for s := range f.statuses {
		out = append(out, s)
	}
	// Tiny deterministic sort — len(statuses) is at most 5 in
	// practice (running, completed, failed, cancelled, queued).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
