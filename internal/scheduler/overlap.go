package scheduler

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// What a slot does when the schedule's previous run is still going (RFC DZ,
// `concurrency_policy`). The values mirror config.ScheduleConcurrency*; this
// package takes no dependency on internal/config.
const (
	policyForbid  = "forbid"
	policyAllow   = "allow"
	policyReplace = "replace"
)

// replacedReason is the cancel reason a run replaced by the next slot ends
// with.
const replacedReason = "replaced by the next slot"

// RunCanceller stops one scheduled run by its run id, on whichever replica
// runs it, for a `replace` slot. stopped=false with a nil error: the run could
// not be reached, and nothing may start over it. Satisfied by
// (*http.Server).CancelScheduledRun; main.go wires it.
type RunCanceller interface {
	CancelScheduledRun(ctx context.Context, runID, reason string) (stopped bool, err error)
}

// SetRunCanceller wires what a `replace` slot cancels the running run through.
// Without one, replace skips the slot as forbid does. Must be called before
// Start.
func (s *Scheduler) SetRunCanceller(c RunCanceller) {
	s.canceller = c
}

// admitOverlap applies the def's concurrency_policy to a slot that is about to
// start a run or a walk, and reports whether it may start.
//
//   - forbid (the default, "" too): a slot that finds any of the schedule's
//     runs still going is skipped, recorded as skipped_overlap. A skip is not
//     a fire, so it does not count toward max_fires.
//   - allow: always starts. Each run is tracked and finished on its own.
//   - replace: cancels the schedule's running runs, then starts. A cancelled
//     run is finished as cancelled, with no hooks. A run or a walk is stopped
//     on whichever replica runs it; if one cannot be stopped (its replica is
//     unreachable, a store fault), the slot is skipped as forbid would:
//     replace never starts a run over one it did not stop.
//
// Whether anything is running is read from schedule_active_runs, which every
// replica shares, so the policy holds across replicas.
func (s *Scheduler) admitOverlap(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time) bool {
	policy := def.ConcurrencyPolicy
	if policy == policyAllow {
		return true
	}
	active, err := s.store.ScheduleActiveRunsList(ctx, row.DefID)
	if err != nil {
		// Unknown is not "nothing running": starting could overlap.
		s.logf("scheduler: schedule %q: list its running runs: %v — skipping this slot", row.Name, err)
		s.recordSkip(ctx, row.DefID, "skipped_overlap", now)
		return false
	}
	if len(active) == 0 {
		return true
	}
	if policy == policyReplace && s.replaceActive(ctx, row, active) {
		return true
	}
	s.logf("scheduler: schedule %q: its run %s is still going — skipping this slot (concurrency_policy %s)",
		row.Name, active[len(active)-1].RunID, policyOrDefault(policy))
	s.recordSkip(ctx, row.DefID, "skipped_overlap", now)
	return false
}

// replaceActive cancels each of the schedule's running runs. It reports
// whether every one was stopped.
func (s *Scheduler) replaceActive(ctx context.Context, row store.ScheduleDueRow, active []store.ScheduleActiveRun) bool {
	if s.canceller == nil {
		s.logf("scheduler: schedule %q: concurrency_policy replace has no canceller wired", row.Name)
		return false
	}
	for _, a := range active {
		stopped, err := s.canceller.CancelScheduledRun(ctx, a.RunID, replacedReason)
		if err != nil {
			s.logf("scheduler: schedule %q: replace run %s: %v", row.Name, a.RunID, err)
			return false
		}
		if !stopped {
			s.logf("scheduler: schedule %q: replace: run %s could not be stopped (its replica did not answer)", row.Name, a.RunID)
			return false
		}
	}
	return true
}

func policyOrDefault(p string) string {
	if p == "" {
		return policyForbid
	}
	return p
}
