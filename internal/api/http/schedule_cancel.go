package http

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// CancelScheduledRun implements scheduler.RunCanceller: it stops one run a
// schedule started, so that a `concurrency_policy: replace` slot can take its
// place (RFC DZ). stopped reports that the run is over or is being stopped;
// false with a nil error means it could not be reached, and the caller must
// not start a run over it.
//
// NO PRINCIPAL GATE, deliberately. The scheduler calls this with nobody on
// ctx, for a run its own schedule_active_runs row lists under the def it is
// firing — the def's own authority, the same authority that started the run.
// It is not reachable from any wire surface.
//
// Either kind stops on whichever replica runs it. An agent run goes through
// the cancel registry, which falls back to the cluster canceller. A team walk
// has no entry there — its cancel is held in its replica's walk table — so a
// walk running elsewhere goes through the turn-cancel registry, which routes a
// run-id cancel to the owning replica, where the unarmed stopper ends the walk
// (SetUnarmedStopper). In single-process mode nothing routes, and a walk that
// is live on no replica this one can reach reports false.
//
// A run no live loop holds anywhere — its replica is recorded dead, or a
// crash left its row — has nothing to stop: its row is finished as cancelled
// (finishUnheldRun), and the slot is free.
func (s *Server) CancelScheduledRun(ctx context.Context, runID, reason string) (bool, error) {
	if e, ok := s.walks.get(runID); ok {
		e.cancel(cancel.CauseWithReason(strings.TrimSpace(reason)))
		return true, nil
	}
	if s.store == nil {
		return false, fmt.Errorf("cancel scheduled run: no store configured")
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		var nf *store.ErrNotFound
		if errors.As(err, &nf) {
			// Nothing left to stop.
			return true, nil
		}
		return false, err
	}
	if store.IsTerminalRunStatus(run.Status) {
		return true, nil
	}
	var routeErr error
	if strings.HasPrefix(run.AgentID, teamWalkAgentPrefix) {
		// A live walk on another replica: route the cancel to its owner.
		if s.turnCancelReg != nil {
			fired, err := s.turnCancelReg.Cancel(ctx, runID, strings.TrimSpace(reason))
			if fired {
				return true, nil
			}
			routeErr = err
		}
	} else if res, ok := s.cancelReg.Cancel(run.AgentID, reason); ok && res.Cancelled {
		return true, nil
	}
	if s.finishUnheldRun(ctx, run, strings.TrimSpace(reason)) {
		return true, nil
	}
	// Not cancelled: it may have ended meanwhile, which is as good.
	if again, err := s.store.GetRun(ctx, runID); err == nil && store.IsTerminalRunStatus(again.Status) {
		return true, nil
	}
	return false, routeErr
}
