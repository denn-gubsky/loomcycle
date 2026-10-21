package http

// The HTTP side of a run start. POST /v1/runs and POST /v1/sessions/{id}/messages
// check their request's wire shape, build a runner.RunInput and start it through
// RunOnce — the start every other transport uses — answering with the run's SSE
// stream. What is HTTP's own lives here: the stream, and the status and body
// each refusal has always been answered with on these routes.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/runner"
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
