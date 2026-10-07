package http

import (
	"context"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A run's row says "running" for as long as nobody ends it, whether or not a
// loop is still behind it. After a crash, a replica's death, or a pause that
// was never resumed, the row outlives the loop: a cancel aimed at it finds no
// live run on this replica and none to route to, and used to leave the row as
// it was — to be resumed by a later pass, or to sit running until the stale
// sweeper failed it. Such a row is finished here instead, as cancelled with
// the canceller's reason.
//
// Finishing a row whose loop is alive would leave that loop running against a
// run recorded as over, so "nobody holds it" is decided strictly (runUnheld).

// defaultUnheldRunGrace is how long a row must have been quiet — neither
// started nor heartbeated — before it can be judged unheld. A run's row is
// written a moment before its loop is registered (a new run), and a resumed
// run's row is flipped to running and heartbeated a moment before it is; a
// cancel landing in that moment must not take the row from a loop about to
// start. It also allows for clock skew against the replica that wrote the row.
const defaultUnheldRunGrace = 10 * time.Second

func (s *Server) unheldGrace() time.Duration {
	if s.unheldRunGrace > 0 {
		return s.unheldRunGrace
	}
	return defaultUnheldRunGrace
}

// runUnheld reports whether a running row has no live loop behind it,
// anywhere. It is held when:
//   - this process holds it (pausedRunIsLive: the cancel registry or the pause
//     manager), or it is a walk in this process's walk table;
//   - its row names another replica whose heartbeat is fresh — by the
//     replicas table, never by a missing ack, so a slow owner is not a dead one;
//   - in a cluster, its row names no replica: there is no record to show its
//     owner is gone;
//   - it is a channel hook's run, which a worker holds rather than a loop;
//   - it was started or heartbeated within the grace.
//
// A failed liveness read is an error, and the caller leaves the row alone.
func (s *Server) runUnheld(ctx context.Context, run store.Run) (bool, error) {
	if run.Status != store.RunRunning || run.UserID == hookRunUser {
		return false, nil
	}
	if _, live := s.walks.get(run.ID); live {
		return false, nil
	}
	if s.replicaStore != nil && run.ReplicaID == "" {
		return false, nil
	}
	if live, err := s.pausedRunIsLive(ctx, run); live || err != nil {
		return false, err
	}
	quietSince := run.StartedAt
	if run.LastHeartbeatAt.After(quietSince) {
		quietSince = run.LastHeartbeatAt
	}
	return time.Since(quietSince) >= s.unheldGrace(), nil
}

// finishUnheldRun ends run as cancelled with reason when no live loop holds
// it, and with it every descendant in the same state, and reports whether run
// itself was ended. The caller has already established that its own caller
// may cancel run: this checks nothing about who asks.
func (s *Server) finishUnheldRun(ctx context.Context, run store.Run, reason string) bool {
	finished, _ := s.FinishRunOwnerGone(ctx, run, reason)
	return finished
}

// FinishRunOwnerGone is finishUnheldRun that also returns the agent ids of
// the descendants it ended. It is the finisher the cluster cancel coordinator
// calls for a run whose owning replica is recorded dead
// (coord.OwnerGoneFinisher; main.go wires it), so that a cancel routed
// through the cluster ends such a run exactly as one that never left this
// replica does.
func (s *Server) FinishRunOwnerGone(ctx context.Context, run store.Run, reason string) (bool, []string) {
	if s.store == nil {
		return false, nil
	}
	unheld, err := s.runUnheld(ctx, run)
	if err != nil {
		log.Printf("cancel: run %s is left as it is: %v", run.ID, err)
		return false, nil
	}
	if !unheld {
		return false, nil
	}
	if reason == "" {
		reason = "cancelled by api" // what a live run's cancel records for no reason
	}
	s.cancelOrphanedRun(run, reason)
	var cascaded []string
	s.finishUnheldDescendants(ctx, run, reason, map[string]bool{run.ID: true}, &cascaded)
	return true, cascaded
}

// finishUnheldDescendants ends the unheld runs below parent, which has ended.
// A child a live loop holds is left, and so is everything below it: its loop
// answers for them. seen bounds the walk on rows whose parent links loop;
// ended collects the agent ids of the runs ended.
func (s *Server) finishUnheldDescendants(ctx context.Context, parent store.Run, reason string, seen map[string]bool, ended *[]string) {
	// By the parent's run id, not its agent id: a team walk's members name the
	// agent that started the walk as their parent agent, and only the walk's
	// run id as their parent.
	children, err := s.store.ListRunsByParentRunID(ctx, parent.ID)
	if err != nil {
		log.Printf("cancel: list run %s's children: %v", parent.ID, err)
		return
	}
	for _, child := range children {
		if seen[child.ID] {
			continue
		}
		seen[child.ID] = true
		if !isTerminalRunStatus(child.Status) {
			unheld, err := s.runUnheld(ctx, child)
			if err != nil || !unheld {
				continue
			}
			s.cancelOrphanedRun(child, reason)
			*ended = append(*ended, child.AgentID)
		}
		s.finishUnheldDescendants(ctx, child, reason, seen, ended)
	}
}

// cancelRunWherever cancels a run by its row for a caller that has no live
// handle on it: through the cancel registry, which reaches it here or on its
// replica, and — when no live loop holds it anywhere — by finishing its row.
func (s *Server) cancelRunWherever(ctx context.Context, run store.Run, reason string) {
	if isTerminalRunStatus(run.Status) {
		return
	}
	if run.AgentID != "" && s.cancelReg != nil {
		// Found and not cancelled is an owner that did not answer, or a run
		// that ended meanwhile: either way the registry has spoken for it.
		if _, found := s.cancelReg.Cancel(run.AgentID, reason); found {
			return
		}
	}
	s.finishUnheldRun(ctx, run, reason)
}
