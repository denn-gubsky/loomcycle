package http

// The HTTP side of a run start. POST /v1/runs and POST /v1/sessions/{id}/messages
// check their request's wire shape, build a runner.RunInput and start it through
// RunOnce — the start every other transport uses — answering with the run's SSE
// stream. What is HTTP's own lives here: the stream, and the status and body
// each refusal has always been answered with on these routes.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/heartbeat"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// unknownAgentError is RunOnce's refusal of an agent that does not resolve.
// Its text is the runner sentinel's ("unknown agent: x"); the name is kept so
// an HTTP caller can be answered in the routes' own words.
type unknownAgentError struct{ agent string }

func (e *unknownAgentError) Error() string { return runner.ErrUnknownAgent.Error() + ": " + e.agent }
func (e *unknownAgentError) Unwrap() error { return runner.ErrUnknownAgent }

// tokenLimitError is RunOnce's refusal of a run over a hard token budget. It
// carries the budget, which the HTTP answer reports in full.
type tokenLimitError struct{ info *providers.LimitInfo }

func (e *tokenLimitError) Error() string {
	return runner.ErrTokenLimitExceeded.Error() + ": " + e.info.Message
}
func (e *tokenLimitError) Unwrap() error { return runner.ErrTokenLimitExceeded }

// holdSession takes the lock a run in session id holds while it runs: this
// replica's, and with acrossReplicas the cluster's too (sessionHold). It
// reports false when another request holds it.
func (s *Server) holdSession(id string, acrossReplicas bool) (*sessionHold, bool) {
	if acrossReplicas {
		return s.trySessionLock(id)
	}
	local, ok := s.sessionLocks.TryLock(id)
	if !ok {
		return nil, false
	}
	return &sessionHold{id: id, local: local}, true
}

// runStartStream is one HTTP run start's SSE stream. It opens when RunOnce
// registers the run — when the run queues, for one that has to wait — with the
// `session` and `agent` frames, and then carries the run's events.
type runStartStream struct {
	s      *Server
	w      http.ResponseWriter
	r      *http.Request
	parent *store.ParentContext // echoed on the agent frame
	stream *sse
}

// run starts in through RunOnce and answers the request: with the run's
// stream once RunOnce registers it, or — for a refusal before that — with
// refuse. A queued run that then does not start ends its stream with why.
func (rs *runStartStream) run(in runner.RunInput, refuse func(error)) {
	defer func() {
		if rs.stream != nil {
			rs.stream.end()
		}
	}()
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, sessionID, _ string) {
			st, ok := newSSE(rs.w)
			if !ok {
				return
			}
			st.start()
			// Keeps the connection warm through long quiet stretches (a parent
			// waiting on its sub-agents, a run waiting for its slot). Ends with
			// the request, or with the deferred end above.
			st.startKeepalive(rs.r.Context(), rs.s.cfg().Env.SSEKeepaliveInterval)
			rs.stream = st
			if sessionID != "" {
				st.send(providers.Event{Type: "session", Text: sessionID})
			}
			st.sendRaw("agent", map[string]any{
				"agent_id":        agentID,
				"run_id":          runID,
				"session_id":      sessionID,
				"parent_agent_id": nil,
				"parent_context":  rs.parent,
			})
		},
		OnEvent: func(ev providers.Event) {
			if rs.stream != nil {
				rs.stream.send(ev)
			}
		},
		// The loop's failure, as the stream's last frame. Not recorded: the
		// loop recorded its own error event.
		OnRunError: func(err error) {
			if rs.stream != nil {
				rs.stream.send(runErrorEvent(err))
			}
		},
	}
	err := rs.s.RunOnce(rs.r.Context(), in, cb)
	switch {
	case rs.stream == nil && err == nil:
		// RunOnce registers every run it starts; reaching here without a
		// stream means the ResponseWriter could not flush.
		http.Error(rs.w, "server does not support streaming on this transport", http.StatusInternalServerError)
	case rs.stream == nil:
		refuse(err)
	case err != nil:
		var tl *tokenLimitError
		if errors.As(err, &tl) {
			rs.stream.send(providers.Event{Type: providers.EventError, Error: tl.info.Message})
			return
		}
		rs.stream.send(providers.Event{Type: providers.EventError, Error: "the run did not start: " + startDetail(err)})
	}
}

// writeRunStartError answers a run start RunOnce refused before the stream
// opened, with the status and body the HTTP routes have always used. agent is
// the agent the request named ("" for a continuation, whose agent is its
// session's).
func (s *Server) writeRunStartError(w http.ResponseWriter, r *http.Request, err error, continuation bool) {
	var (
		dup *runner.DuplicateRunError
		nt  *noTraceRefusal
		tl  *tokenLimitError
		ua  *unknownAgentError
	)
	switch {
	case errors.As(err, &dup):
		s.serveExistingRun(w, r, dup)
	case errors.As(err, &nt):
		writeNoTraceRefusal(w, nt)
	case errors.As(err, &tl):
		writeTokenLimitError(w, tl.info)
	case errors.Is(err, errContinuationRefused):
		http.Error(w, "session cannot be continued: "+continuationRefusal(err).Error(), http.StatusBadRequest)
	case errors.As(err, &ua) && continuation:
		http.Error(w, fmt.Sprintf("session refers to unknown agent %q", ua.agent), http.StatusBadRequest)
	case errors.As(err, &ua):
		http.Error(w, fmt.Sprintf("unknown agent %q", ua.agent), http.StatusBadRequest)
	case errors.Is(err, runner.ErrRuntimePaused):
		writeRuntimePaused(w)
	case errors.Is(err, runner.ErrPerUserQuotaExhausted), errors.Is(err, runner.ErrBackpressure):
		writeQuotaError(w, concurrencyCause(err))
	case errors.Is(err, runner.ErrSessionNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, runner.ErrSessionBusy):
		writeJSONError(w, http.StatusConflict, "session_busy", detail(err, runner.ErrSessionBusy))
	case errors.Is(err, runner.ErrAgentIDInUse):
		writeJSONError(w, http.StatusConflict, "agent_id_in_use", detail(err, runner.ErrAgentIDInUse))
	case errors.Is(err, runner.ErrUnknownProvider):
		http.Error(w, detail(err, runner.ErrUnknownProvider), http.StatusBadRequest)
	case errors.Is(err, runner.ErrInternal):
		http.Error(w, detail(err, runner.ErrInternal), http.StatusInternalServerError)
	default:
		// The token-restriction refusals (typed bodies), the resolver's and the
		// per-run overrides' refusals: what both routes answered them with.
		writeResolveError(w, err)
	}
}

// detail is err's text without the sentinel's own leading "<sentinel>: ",
// which is how the HTTP routes worded the same refusal.
func detail(err, sentinel error) string {
	return strings.TrimPrefix(err.Error(), sentinel.Error()+": ")
}

// startDetail is why a run that had already opened its stream did not start.
func startDetail(err error) string {
	for _, sentinel := range []error{runner.ErrInternal, runner.ErrSessionBusy, runner.ErrPerUserQuotaExhausted, runner.ErrBackpressure} {
		if errors.Is(err, sentinel) {
			return detail(err, sentinel)
		}
	}
	return err.Error()
}

// concurrencyCause is the semaphore's own refusal inside an admission error,
// which writeQuotaError answers with its details.
func concurrencyCause(err error) error {
	var pue *concurrency.ErrPerUserQuotaExhausted
	if errors.As(err, &pue) {
		return pue
	}
	var be *concurrency.BackpressureError
	if errors.As(err, &be) {
		return be
	}
	return err
}

// continuationRefusal is the reason RunOnce wrapped beside ErrUnknownAgent
// when a session's team version can no longer be continued.
func continuationRefusal(err error) error {
	if u, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range u.Unwrap() {
			if errors.Is(e, errContinuationRefused) {
				return e
			}
		}
	}
	return err
}

// parkLock lets the cluster half of a continuation's session lock go while
// the run is parked — waiting for its operator's next message, or for a
// review's verdict — and takes it back for the run's next turn. The cluster
// lock is a Postgres advisory lock, which pins a pool connection, and a
// parked run holds no run slot: kept through the park, it would let a user
// pin a connection per parked session, outside every cap. While the lock is
// let go, a second turn on the session is still refused on every replica by
// the session's live run (sessionHasLiveRun), and on this one by the
// in-process lock, which the run keeps until it ends.
//
// Used only on the run's goroutine (the loop's park, steer and turn hooks).
type parkLock struct {
	hold      *sessionHold
	sessionID string
	released  bool
	// retakeFailed: the operator turn that ended the park could not take the
	// lock back; the turn it starts is cancelled when it is armed.
	retakeFailed bool
}

// park lets the cluster lock go, once per park.
func (p *parkLock) park() {
	if p.released {
		return
	}
	p.hold.releaseCluster()
	p.released = true
}

// resumeBackoff is how long resume waits between tries, about 1.6 s in all.
// A failed re-take is a contender that is about to be refused: while this
// run's row is live, every other request on the session is refused by
// sessionHasLiveRun, and holds the lock only for that one query. Waiting it
// out keeps a retrying or polling client on another replica from cancelling a
// legitimate turn.
var resumeBackoff = []time.Duration{25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
	200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}

// resume takes the cluster lock back for the run's next turn, trying again
// through resumeBackoff. false: another replica held it throughout (or ctx
// ended), and the turn must not run.
func (p *parkLock) resume(ctx context.Context) bool {
	if !p.released {
		return true
	}
	for i := 0; ; i++ {
		if p.hold.reacquire() {
			p.released = false
			return true
		}
		if i == len(resumeBackoff) {
			return false
		}
		t := time.NewTimer(resumeBackoff[i])
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return false
		}
	}
}

func (p *parkLock) busy() string {
	return fmt.Sprintf("another request is in flight on session %q", p.sessionID)
}

// sessionHasLiveRun reports whether a run of the session other than except
// is a live turn. It is the one-live-turn rule for a moment the session's
// cluster lock may be let go (parkLock), so it applies only where there is a
// cluster lock: on a single replica the in-process lock is the rule — a parked
// continuation keeps it to its end — and a crash takes that lock with it, so
// no row of a dead process can block a session there.
//
// Across replicas a row is live when it is queued, or running with a fresh
// heartbeat — the heartbeat sweeper's own rule (SweepStaleRuns): its last
// heartbeat, or its start when it never beat, is within HeartbeatStaleAfter —
// or held by a runtime pause, which beats no heartbeat on purpose. A parked
// run is running and beats while it waits. So a row orphaned by a crash stops
// blocking its session once its heartbeat is stale, with or without the
// sweeper. A queued row is judged by its start, the same way.
func (s *Server) sessionHasLiveRun(ctx context.Context, sessionID, except string) (bool, error) {
	if s.sessionLockPG == nil {
		return false, nil
	}
	runs, err := s.store.RunsForSession(ctx, sessionID)
	if err != nil {
		return false, err
	}
	cutoff := time.Now().Add(-s.heartbeatStaleAfter())
	for _, r := range runs {
		if r.ID != except && runIsLive(r, cutoff) {
			return true, nil
		}
	}
	return false, nil
}

// runIsLive is sessionHasLiveRun's rule for one row; cutoff is now less
// HeartbeatStaleAfter.
func runIsLive(r store.Run, cutoff time.Time) bool {
	switch r.Status {
	case store.RunQueued:
		return !r.StartedAt.Before(cutoff)
	case store.RunRunning:
		if r.PauseState == store.PauseStatePaused || r.PauseState == store.PauseStatePausing {
			return true
		}
		beat := r.LastHeartbeatAt
		if beat.IsZero() {
			beat = r.StartedAt
		}
		return !beat.Before(cutoff)
	}
	return false
}

// heartbeatStaleAfter is how long a run may go without a heartbeat before it
// is presumed dead: the sweeper's setting, and its default when unset.
func (s *Server) heartbeatStaleAfter() time.Duration {
	if d := s.cfg().Env.HeartbeatStaleAfter; d > 0 {
		return d
	}
	return heartbeat.DefaultStaleAfter
}

// sessionLiveAfterWait is sessionHasLiveRun for a continuation that waited for
// its slot with its cluster lock let go: another replica may have started a
// run on the session meanwhile, and parked it. row is the waiting run's own.
func (s *Server) sessionLiveAfterWait(ctx context.Context, in runner.RunInput, isContinuation bool, row string) (bool, error) {
	if !isContinuation || !in.SessionLockAcrossReplicas {
		return false, nil
	}
	return s.sessionHasLiveRun(ctx, in.SessionID, row)
}

// releaseSessionWhileParked wires p into the loop's hooks.
//
// The lock is let go when the run parks for its operator (awaiting_input) or
// is held for a verdict, and taken back at the start of the turn that ends the
// park — the operator's message (OnSteer, which both loops' parks and a
// review's send-back call as that turn begins). It is NOT taken back when a
// hold ends without a turn — approved, rejected, expired, aborted: an approved
// interactive run then parks for input, and a run that ends needs no lock. An
// OnSteer that is not ending a park (a message drained mid-turn) finds the
// lock held and takes nothing.
//
// A turn that cannot take the lock back does not run. An interactive run's
// turn is cancelled as it is armed (ArmTurnCancel, wired for every
// continuation here; it never fires otherwise) with the busy reason, and the
// run parks again. A run with no per-turn cancel — a stateful one, or one that
// is not interactive (a review's send-back) — is cancelled with the busy
// reason, which its end records.
func (s *Server) releaseSessionWhileParked(opts *loop.RunOptions, p *parkLock, loopCtx context.Context, stateful bool, cancelBusy func(reason string)) {
	onEvent := opts.OnEvent
	opts.OnEvent = func(ev providers.Event) {
		onEvent(ev)
		if ev.Type == providers.EventAwaitingInput {
			p.park()
		}
	}
	opts.OnReviewHold = func(held bool) {
		if held {
			p.park()
		}
	}
	interactiveNow := func() bool {
		if opts.InteractiveNow != nil {
			return opts.InteractiveNow(loopCtx)
		}
		return opts.Interactive
	}
	onSteer := opts.OnSteer
	opts.OnSteer = func(m steer.Message) {
		if onSteer != nil {
			onSteer(m)
		}
		if p.resume(loopCtx) {
			return
		}
		if stateful || !interactiveNow() {
			cancelBusy(p.busy())
			return
		}
		p.retakeFailed = true
	}
	arm := opts.ArmTurnCancel
	opts.ArmTurnCancel = func(turnCancel context.CancelCauseFunc) func() {
		if p.retakeFailed {
			p.retakeFailed = false
			turnCancel(loop.TurnCancelCause(p.busy()))
		}
		if arm == nil {
			return func() {}
		}
		return arm(turnCancel)
	}
}
