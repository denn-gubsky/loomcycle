package http

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A team walk is a RUN.
//
// Everything that makes a walk observable or controllable from outside is
// addressed by run id — the breakpoint set, the Interruption ask a pause is
// answered through, cancel, the event stream. A walk that had no run had no
// handle, so none of them could reach it. That was not a gap in any one of
// them; it was the walk not being a first-class unit of work.
//
// The concrete failure this fixes: loomboard drives loomcycle through the TS
// adapter over `POST /v1/_teamdef {op:"run"}`, which dispatches under
// substrateAdminCtx — no run id, no Interruption policy. A breakpoint armed
// there could not be answered, and a pause that cannot be answered fails safe
// to ABORT, so arming one killed the walk instead of pausing it.

// teamWalkAgentPrefix names the synthetic agent a walk's run is filed under, so
// the runs list shows `team:triage` rather than a bare id. It is a run's
// `agent` label, never a resolvable AgentDef — the walk spawns its members
// through their own defs.
const teamWalkAgentPrefix = "team:"

// openTeamWalkRun gives one op=run walk its own run. Wired into the TeamDef
// tool by SetTeamDefTool.
//
// detach decides the ctx's lifetime, not the run's identity: a detached walk
// keeps the request's ctx VALUES (auth principal, tenant, admission) but is not
// cancelled when the handler returns, mirroring how an interactive run survives
// the client navigating away.
func (s *Server) openTeamWalkRun(ctx context.Context, teamName string, detach bool) (context.Context, string, func(string, error), error) {
	if s.store == nil {
		return ctx, "", func(string, error) {}, fmt.Errorf("run tracking requires a store")
	}
	ident := tools.RunIdentity(ctx)
	agent := teamWalkAgentPrefix + teamName
	sessionID, runID, err := s.openOrCreateSessionAndRun(ctx, "", agent, ident.TenantID, ident.UserID, store.RunIdentity{
		AgentID: agent,
		UserID:  ident.UserID,
	})
	if err != nil {
		return ctx, "", func(string, error) {}, err
	}

	walkCtx := ctx
	if detach {
		walkCtx = context.WithoutCancel(ctx)
	}
	// The walk's own cancel. Every member run spawns under walkCtx, so firing
	// it stops them too; before this nothing could stop a running walk short
	// of a breakpoint's `abort` answer — the run-id cancel route refused it as
	// not interactive, and the agents route cannot address `team:<name>`.
	walkCtx, cancelWalk := context.WithCancelCause(walkCtx)
	s.walks.add(runID, sessionID, cancelWalk)
	walkCtx = tools.WithRunID(walkCtx, runID)
	// The pause machinery is the Interruption tool's `ask`, which is gated on
	// the CALLING AGENT's policy. A walk has no AgentDef to carry one, so the
	// grant is made here, narrowed to the one kind a breakpoint uses: a
	// question. Without it every pause errors, and a pause that cannot be
	// answered aborts the walk.
	walkCtx = tools.WithInterruptionPolicy(walkCtx, tools.InterruptionPolicyValue{
		Enabled: true,
		Kinds:   []string{"question"},
	})

	finish := func(finalText string, walkErr error) {
		s.walks.remove(runID)
		status, stopReason, msg := store.RunCompleted, "", ""
		if cause := context.Cause(walkCtx); errors.Is(cause, cancel.ErrCancelledByAPI) {
			// Cancelled on purpose: recorded as cancelled, with the operator's
			// reason, the way an agent run's API cancel is.
			status, stopReason = store.RunCancelled, cancel.ReasonFromCause(cause)
			if stopReason == "" {
				stopReason = "cancelled by api"
			}
		} else if walkErr != nil {
			status, msg = store.RunFailed, walkErr.Error()
		}
		// A survival ctx: the run row must be closed even when the walk failed
		// because its ctx was cancelled, or a cancelled walk would sit in the
		// runs list as running forever.
		// The walk's answer is its last state's output (RFC DI) — what a caller
		// holding only the walk's run id wants to read once it is over.
		usage := store.Usage{Result: runResultJSON(loop.RunResult{FinalText: finalText})}
		if ferr := s.store.FinishRun(context.WithoutCancel(walkCtx), runID, status, stopReason, usage, msg); ferr != nil {
			log.Printf("teamdef: finish walk run %s: %v", runID, ferr)
		}
		// A walk is a run, and ends like one.
		s.observeRunEnd(runStateMeta{RunID: runID, AgentID: agent, Agent: agent, UserID: ident.UserID, TenantID: ident.TenantID},
			status, stopReason, msg, finalText)
		cancelWalk(nil) // release the ctx; a no-op after a cancel
	}
	return walkCtx, runID, finish, nil
}

// walkCancels is the live-walk cancel table. In-process only: a walk on
// another replica is not reachable here, and the cancel route answers that as
// "no in-flight run" rather than pretending it stopped something — the same
// single-replica limit the breakpoint set has.
type walkCancels struct {
	mu sync.Mutex
	m  map[string]walkCancel
}

type walkCancel struct {
	sessionID string
	cancel    context.CancelCauseFunc
}

func (w *walkCancels) add(runID, sessionID string, fn context.CancelCauseFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil {
		w.m = map[string]walkCancel{}
	}
	w.m[runID] = walkCancel{sessionID: sessionID, cancel: fn}
}

func (w *walkCancels) remove(runID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.m, runID)
}

func (w *walkCancels) get(runID string) (walkCancel, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	e, ok := w.m[runID]
	return e, ok
}

// cancelTeamWalk stops a live walk by its run id. isWalk reports whether runID
// names a walk live on this replica; when false the caller handles the id as
// an ordinary run. A walk the caller does not own folds into the same opaque
// not-in-flight answer an unknown run gets.
func (s *Server) cancelTeamWalk(ctx context.Context, runID, reason string) (stopped, isWalk bool, err error) {
	e, ok := s.walks.get(runID)
	if !ok {
		return false, false, nil
	}
	if e.sessionID != "" && s.store != nil {
		sess, gerr := s.store.GetSession(ctx, e.sessionID)
		if gerr != nil || !sessionOwnershipOK(ctx, sess) {
			return false, true, connector.ErrRunNotInFlight
		}
	}
	e.cancel(cancel.CauseWithReason(strings.TrimSpace(reason)))
	return true, true, nil
}
