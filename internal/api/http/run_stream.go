package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// runStreamPollInterval is how often the store-tail re-reads a run's events.
// Sub-second so a human-driven interactive terminal feels live, but not so
// tight that a long-idle parked run hammers the store.
const runStreamPollInterval = 250 * time.Millisecond

// runEventToFrame converts a persisted transcript row into the streamable
// providers.Event frame a re-attaching client should see, or reports false to
// skip it. Most rows round-trip directly (text / tool_call / tool_result / done
// / …). The exceptions:
//
//   - system_prompt — the resolved system prompt; not a conversational frame.
//     Skipped.
//   - user_input — the operator's own turns (the opening prompt + every steer
//     message), persisted as a []loop.PromptSegment, NOT a providers.Event.
//     RFC AI makes the re-attach stream SELF-SUFFICIENT: a cold client (e.g.
//     resuming on another device) must reconstruct the operator's side, so we
//     synthesize an EventSteer frame (source="replay") from the segments
//     rather than dropping it. The live run already emits its own EventSteer
//     for in-flight steers, so a same-session client de-dupes the replay
//     against its optimistic echo (web/src/hooks/useRunStream.ts).
func runEventToFrame(ev store.Event) (providers.Event, bool) {
	switch ev.Type {
	case "system_prompt":
		return providers.Event{}, false
	case "user_input":
		return userInputToSteerFrame(ev.Payload)
	default:
		var pe providers.Event
		if json.Unmarshal(ev.Payload, &pe) != nil {
			return providers.Event{}, false // not a providers.Event — skip defensively
		}
		return pe, true
	}
}

// userInputToSteerFrame turns a persisted user_input row (a JSON
// []loop.PromptSegment) into a replayable EventSteer frame. Joins the segments'
// trusted-text so a cold re-attach shows the operator's turn. Returns false if
// the payload is malformed or carries no text (defensive — never fires in
// practice; the writer always persists a well-formed user segment).
func userInputToSteerFrame(payload json.RawMessage) (providers.Event, bool) {
	var segs []loop.PromptSegment
	if json.Unmarshal(payload, &segs) != nil {
		return providers.Event{}, false
	}
	var b strings.Builder
	for _, seg := range segs {
		for _, c := range seg.Content {
			if c.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Text)
		}
	}
	if b.Len() == 0 {
		return providers.Event{}, false
	}
	return providers.Event{
		Type:      providers.EventSteer,
		UserInput: &providers.UserInputEventInfo{Text: b.String(), Source: "replay"},
	}, true
}

// streamRunEvents tails a run's persisted events to a visitor by polling the
// store, re-emitting each loop event (text / tool_call / tool_result / done /
// …) as the same providers.Event frame the live run stream produces, plus
// replayed operator turns (see runEventToFrame). It is the shared engine behind
// three callers: the interactive POST /v1/runs handler (whose loop runs
// detached in a goroutine), the HTTP re-attach endpoint GET /v1/runs/{run_id}/
// stream, and the gRPC StreamRun RPC (via connector.StreamRunEvents) — so the
// visitor abstraction keeps a single tail engine across transports (RFC AI).
//
// It returns nil when ctx is done (the client disconnected — which for an
// interactive run does NOT stop the run itself) or the run reaches a terminal
// state and all its events have been streamed. If visit returns an error
// (e.g. a broken SSE/gRPC stream), streamRunEvents returns it immediately.
// fromSeq lets a re-attaching client skip events it already rendered.
func (s *Server) streamRunEvents(ctx context.Context, runID string, fromSeq int64, visit func(providers.Event) error) error {
	if s.store == nil {
		return nil
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
				pe, ok := runEventToFrame(ev)
				if !ok {
					continue
				}
				if err := visit(pe); err != nil {
					return err
				}
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
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
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
	// The SSE writer never returns an error from send (it buffers + flushes),
	// so the visitor always returns nil; the tail ends on ctx (disconnect) or
	// terminal status.
	_ = s.streamRunEvents(r.Context(), runID, fromSeq, func(pe providers.Event) error {
		stream.send(pe)
		return nil
	})
}
