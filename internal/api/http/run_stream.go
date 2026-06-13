package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// runStreamPollInterval is how often the store-tail re-reads a run's events.
// Sub-second so a human-driven interactive terminal feels live, but not so
// tight that a long-idle parked run hammers the store.
const runStreamPollInterval = 250 * time.Millisecond

// nonStreamableEventTypes are persisted transcript rows whose payload is NOT a
// providers.Event (so they can't round-trip to an SSE frame) — the operator's
// own prompt segments and the resolved system prompt. A re-attaching client
// renders those from the one-shot transcript fetch; the live tail only carries
// the loop's typed events. Steer/session/agent frames are never persisted, so
// they never appear here.
var nonStreamableEventTypes = map[string]bool{
	"user_input":    true,
	"system_prompt": true,
}

// streamRunEvents tails a run's persisted events to an SSE stream by polling
// the store, re-emitting each loop event (text / tool_call / tool_result /
// done / …) as the same providers.Event frame the live run stream produces.
// It is the shared engine behind two callers: the interactive POST /v1/runs
// handler (whose loop runs detached in a goroutine) and the re-attach endpoint
// GET /v1/runs/{run_id}/stream.
//
// It returns when ctx is done (the client disconnected — which for an
// interactive run does NOT stop the run itself) or the run reaches a terminal
// state and all its events have been streamed. fromSeq lets a re-attaching
// client skip events it already rendered from the transcript snapshot.
func (s *Server) streamRunEvents(ctx context.Context, stream *sse, runID string, fromSeq int64) {
	if s.store == nil {
		return
	}
	cursor := fromSeq
	ticker := time.NewTicker(runStreamPollInterval)
	defer ticker.Stop()
	for {
		// Drain everything past the cursor before deciding whether to stop.
		drainErr := false
		for {
			evs, more, err := s.runEventsSince(ctx, runID, cursor)
			if err != nil {
				drainErr = true
				break
			}
			for _, ev := range evs {
				cursor = ev.Seq
				if nonStreamableEventTypes[ev.Type] {
					continue
				}
				var pe providers.Event
				if json.Unmarshal(ev.Payload, &pe) != nil {
					continue // payload not a providers.Event — skip defensively
				}
				stream.send(pe)
			}
			if !more {
				break
			}
		}
		// If the run is terminal and we've drained it, we're done. A parked
		// interactive run never reaches terminal until cancelled, so this loop
		// stays live until the client disconnects (ctx) — exactly what we want.
		if !drainErr {
			if run, err := s.store.GetRun(ctx, runID); err == nil && isTerminalRunStatus(run.Status) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runEventsSince returns the run's events with seq > cursor, ordered ascending.
// `more` reports whether the page hit the limit (caller should drain again).
// Run-scoped + incremental via GetRunEventsSince, so a long-running interactive
// agent's tail doesn't re-read the whole session transcript each tick. Returns
// ([], false, err) on a store error so the caller backs off.
func (s *Server) runEventsSince(ctx context.Context, runID string, cursor int64) ([]store.Event, bool, error) {
	const pageLimit = 500
	out, err := s.store.GetRunEventsSince(ctx, runID, cursor, pageLimit)
	if err != nil {
		return nil, false, err
	}
	return out, len(out) == pageLimit, nil
}

// isTerminalRunStatus reports whether a run has reached an end state (so a
// store-tail can stop). "running" is the only non-terminal status.
func isTerminalRunStatus(st store.RunStatus) bool {
	switch st {
	case store.RunCompleted, store.RunFailed, store.RunCancelled:
		return true
	default:
		return false
	}
}

// handleRunStream is GET /v1/runs/{run_id}/stream — re-attach to a running (or
// finished) run's event stream. Replays from ?from_seq (default 0) and live-
// tails. The companion to interactive runs surviving a client disconnect: the
// operator leaves the /run terminal and returns to the same live run via the
// runs list. Read scope (ScopeRunsRead, mapped in auth_principal.go) +
// tenant-ownership gated. Single-replica: a run owned by another replica is
// not tailable here (its events are in the shared store, so for a DB-backed
// deployment this still works; an in-memory store on another replica 404s).
func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "run streaming requires a persistence backend", http.StatusServiceUnavailable)
		return
	}
	runID := r.PathValue("run_id")
	if !validIdent(runID) {
		http.Error(w, "run_id must match [A-Za-z0-9_-]{1,128}", http.StatusBadRequest)
		return
	}
	// Tenant-ownership gate via the tenant-scoped accessor: a cross-tenant run
	// is folded into *store.ErrNotFound, so an unknown run and a cross-tenant
	// run both return the same opaque 404 (run_ids are not secret, so the gate
	// must not become an existence oracle). Super-admin / legacy / open see all.
	run, err := s.tenantStore(r.Context()).GetRun(r.Context(), runID)
	if err != nil {
		http.Error(w, "no such run", http.StatusNotFound)
		return
	}

	var fromSeq int64
	if q := r.URL.Query().Get("from_seq"); q != "" {
		if n, perr := strconv.ParseInt(q, 10, 64); perr == nil && n >= 0 {
			fromSeq = n
		}
	}

	stream, ok := newSSE(w)
	if !ok {
		http.Error(w, "server does not support streaming on this transport", http.StatusInternalServerError)
		return
	}
	stream.start()
	stream.startKeepalive(r.Context(), s.cfg.Env.SSEKeepaliveInterval)
	// Announce the run/session so the re-attached terminal can address steer /
	// cancel without a separate lookup (parity with the POST /v1/runs stream).
	stream.sendRaw("agent", map[string]any{
		"agent_id":   run.AgentID,
		"run_id":     runID,
		"session_id": run.SessionID,
	})
	s.streamRunEvents(r.Context(), stream, runID, fromSeq)
}
