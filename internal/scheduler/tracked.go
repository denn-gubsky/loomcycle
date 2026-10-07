package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// A scheduled run outlives the tick that starts it (RFC DZ).
//
// The tick used to call RunOnce and wait for the run to END, under a
// 10-minute cap: a long job was cancelled at the cap, and every other
// schedule waited for the slowest fire of the batch. Now the tick waits only
// for the run to be ADMITTED, and the run is finished when it ends:
//
//   - When the run registers, its schedule_active_runs row is written before
//     its loop starts (ScheduleActiveRunStart), and the schedule reads
//     "running".
//   - When it ends, the goroutine that ran it finishes it (finishTracked):
//     deleting the row records the outcome, and only the caller whose delete
//     removed it dispatches on_complete and retires a spent max_fires.
//   - If this replica dies first, the stale-run sweeper fails the run and the
//     reconciler (any replica) finishes it through the same delete. A run
//     that completed but whose replica died before finishing it still gets
//     its hooks, once.
//
// No wall clock bounds a scheduled run any more. It stops the way an HTTP or
// webhook run does: its iteration cap, the token budgets, or a cancel.

// reconcileBatch bounds how many ended runs one reconcile pass finishes.
// More wait for the next tick.
const reconcileBatch = 100

// startedRun is what OnRegistered hands back to the fire.
type startedRun struct {
	agentID string
	runID   string
	// tracked is false when the active row could not be written. The run
	// still runs; the goroutine that ran it records the outcome directly,
	// and no reconciler can find it.
	tracked bool
}

// fireRun starts the slot's run and returns once the run is admitted — or
// refused before it started, which is recorded here exactly as a fire that
// ran inside the tick used to be.
//
// The run is an ordinary RunOnce on a ctx that does not end with the tick
// (context.WithoutCancel), not RunInput.Detached: a Detached RunOnce releases
// its admission slots when it returns, which here would be at once, leaving
// the run outside the per-user gate. This way it holds its slots until it
// ends, like a detached spawn batch's children (spawnDetached).
func (s *Scheduler) fireRun(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, in runner.RunInput, now time.Time) {
	runCtx, abort := context.WithCancel(context.WithoutCancel(ctx))
	var (
		mu         sync.Mutex
		registered bool // set first, so a stopping scheduler never withdraws a registered run
		started    *startedRun
		admitted   = make(chan struct{})
		refused    = make(chan error, 1)
	)
	cb := runner.RunCallbacks{
		OnRegistered: func(agentID, runID, _, _ string) {
			mu.Lock()
			registered = true
			mu.Unlock()
			// Tracked before the loop starts: from here on a dead replica
			// leaves a row the reconciler finishes.
			tracked := s.trackRun(runCtx, row, runID, now)
			mu.Lock()
			started = &startedRun{agentID: agentID, runID: runID, tracked: tracked}
			mu.Unlock()
			close(admitted)
		},
	}
	s.runs.Add(1)
	go func() {
		defer s.runs.Done()
		defer abort()
		err := s.runOnceRecovered(runCtx, row, in, cb)
		mu.Lock()
		st := started
		mu.Unlock()
		if st == nil {
			refused <- err
			return
		}
		s.finishLocal(runCtx, row, def, *st, err, now)
	}()

	done := ctx.Done()
	for {
		select {
		case <-admitted:
			return
		case err := <-refused:
			s.recordRefusal(ctx, row, def, now, err)
			return
		case <-done:
			// The scheduler is stopping. A run still waiting for admission is
			// withdrawn: nobody would be there to see it start. One that has
			// registered carries on and is finished when it ends.
			mu.Lock()
			if !registered {
				abort()
			}
			mu.Unlock()
			done = nil
		}
	}
}

// runOnceRecovered is RunOnce on the run's own goroutine, where the tick's
// recover does not reach: a panic there would take the process down. It is
// reported as the run's error instead — a refusal if the run had not started,
// otherwise a run the reconciler finishes once its row says it ended.
func (s *Scheduler) runOnceRecovered(ctx context.Context, row store.ScheduleDueRow, in runner.RunInput, cb runner.RunCallbacks) (err error) {
	defer func() {
		if r := recover(); r != nil {
			s.logf("scheduler: PANIC in the run of schedule %q (def_id=%s): %v", row.Name, row.DefID, r)
			err = fmt.Errorf("panic in run: %v", r)
		}
	}()
	return s.runner.RunOnce(ctx, in, cb)
}

// trackRun writes the run's schedule_active_runs row and marks the schedule
// running. false means the row is not there: the caller records the run's
// outcome itself when it ends.
func (s *Scheduler) trackRun(ctx context.Context, row store.ScheduleDueRow, runID string, now time.Time) bool {
	if err := s.store.ScheduleActiveRunStart(ctx, store.ScheduleActiveRun{
		DefID:     row.DefID,
		RunID:     runID,
		SlotAt:    row.NextRunAt,
		CatchUp:   !row.CatchUpUntil.IsZero(),
		StartedAt: now,
		ClaimedBy: s.cfg.ReplicaID,
	}); err != nil {
		s.logf("scheduler: schedule %q run %s: track: %v — recording its outcome when it ends", row.Name, runID, err)
		return false
	}
	return true
}

// recordRefusal records a run that never started: the runner refused it at
// setup (unknown agent, backpressure, paused, a spent budget, ...). It is
// classified exactly as before, and nothing is tracked or dispatched.
func (s *Scheduler) recordRefusal(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, now time.Time, runErr error) {
	if errors.Is(runErr, store.ErrDuplicateIdempotencyKey) {
		// This slot already has its run. Whoever started it records it.
		s.logf("scheduler: schedule %q slot %s already has a run — not firing it twice", row.Name, row.NextRunAt.UTC().Format(time.RFC3339Nano))
		return
	}
	if ctx.Err() != nil {
		// Withdrawn while waiting for admission because the scheduler is
		// stopping. Nothing ran, and the claimed slot is not retried.
		s.logf("scheduler: schedule %q: the scheduler stopped before its run was admitted — slot %s not run", row.Name, row.NextRunAt.UTC().Format(time.RFC3339Nano))
		return
	}
	out := fireOutcome{Status: "failed", CountAsFire: true}
	if runErr == nil {
		// RunOnce returned without registering a run, and without saying why.
		out.Err = "run did not start"
	} else {
		class := classifyFire(runErr)
		out.Status, out.Err, out.CountAsFire = class.status(), runErr.Error(), class.countsAsFire()
		switch class {
		case fireUnknownAgent:
			s.logf("scheduler: schedule %q could not resolve agent %q in tenant %q — not counting toward max_fires; check the agent exists in this tenant (F38)",
				row.Name, def.Agent, def.TenantID)
		case firePaused:
			s.logf("scheduler: schedule %q was refused because the runtime paused after this tick began — not counting toward max_fires", row.Name)
		}
	}
	_, done := s.recordFireOutcome(ctx, row, def, now, out)
	done()
}

// finishLocal finishes a run this replica ran, once RunOnce has returned.
func (s *Scheduler) finishLocal(ctx context.Context, row store.ScheduleDueRow, def scheduleDef, st startedRun, runErr error, now time.Time) {
	status, errStr, ok := s.finalOutcome(ctx, st, runErr)
	if !ok {
		// Its row does not say it ended. The reconciler finishes it when it
		// does (or, untracked, nobody can — logged by finalOutcome).
		return
	}
	if st.tracked {
		s.finishTracked(ctx, row.DefID, row.Name, def, st.runID, st.agentID, status, errStr)
		return
	}
	recordCtx, done := s.recordFireOutcome(ctx, row, def, now, fireOutcome{
		RunID: st.runID, Status: status, Err: errStr, CountAsFire: true,
	})
	defer done()
	if status == string(store.RunCompleted) {
		s.dispatchHooks(recordCtx, row.Name, def, st.runID, st.agentID)
	}
}

// finalOutcome is how a run ended, read from its run row: once a run has
// registered, RunOnce returns nil however the loop ended, so its return says
// nothing about a failed run. ok=false: the outcome is not known yet.
func (s *Scheduler) finalOutcome(ctx context.Context, st startedRun, runErr error) (status, errStr string, ok bool) {
	run, err := s.store.GetRun(ctx, st.runID)
	if err == nil {
		if !store.IsTerminalRunStatus(run.Status) {
			if !st.tracked {
				s.logf("scheduler: run %s returned while its row reads %q and it is not tracked — its schedule's outcome is not recorded", st.runID, run.Status)
			}
			return "", "", false
		}
		return string(run.Status), run.ErrorMsg, true
	}
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) && st.tracked {
		// A store fault, not an answer. The reconciler reads it again.
		s.logf("scheduler: run %s: read its outcome: %v — the reconciler finishes it", st.runID, err)
		return "", "", false
	}
	// No run row to read: what RunOnce returned is all there is.
	if runErr != nil {
		return string(store.RunFailed), runErr.Error(), true
	}
	return string(store.RunCompleted), "", true
}

// finishTracked finishes one tracked run, from whichever caller gets there:
// the replica that ran it, or the reconciler. Only the caller whose delete
// removed the run's row dispatches its hooks and retires a spent max_fires,
// so both happen once however the two race. A failed, cancelled or rejected
// run dispatches no hooks (RFC E: on_complete is for successful runs).
func (s *Scheduler) finishTracked(ctx context.Context, defID, name string, def scheduleDef, runID, agentID, status, errStr string) {
	won, err := s.store.ScheduleActiveRunFinish(ctx, store.ScheduleRunFinish{
		DefID:      defID,
		RunID:      runID,
		Status:     status,
		Error:      errStr,
		FinishedAt: time.Now(),
	})
	if err != nil {
		s.logf("scheduler: schedule %q run %s: finish: %v — the reconciler retries", name, runID, err)
		return
	}
	if !won {
		return
	}
	if status == string(store.RunCompleted) {
		s.dispatchHooks(ctx, name, def, runID, agentID)
	}
	s.retireIfSpent(ctx, defID, name, def)
}

// retireIfSpent retires the def once its fire_count has reached max_fires
// (RFC S / F36). Retired defs are skipped by the due-query JOIN, so no slot
// fires after it. A retired def's runs that are still active finish normally.
func (s *Scheduler) retireIfSpent(ctx context.Context, defID, name string, def scheduleDef) {
	if def.MaxFires <= 0 {
		return
	}
	st, err := s.store.ScheduleRunStateGet(ctx, defID)
	if err != nil {
		s.logf("scheduler: max_fires read state for %q: %v", name, err)
		return
	}
	if st.FireCount < def.MaxFires {
		return
	}
	if err := s.store.ScheduleDefSetRetired(ctx, defID, true); err != nil {
		s.logf("scheduler: max_fires retire %q (def %s) after %d fires: %v", name, defID, st.FireCount, err)
		return
	}
	s.logf("scheduler: %q reached max_fires=%d — retired def %s", name, def.MaxFires, defID)
}

// spentWhileRunning reports whether a def's max_fires is already used up by
// runs that have not finished, so the slot must start nothing. Retirement
// waits for a run to finish (finishTracked); until then the def is still
// listed as due. A spent def with nothing left running — its retirement write
// was lost — is retired here.
func (s *Scheduler) spentWhileRunning(ctx context.Context, row store.ScheduleDueRow, def scheduleDef) bool {
	if def.MaxFires <= 0 {
		return false
	}
	st, err := s.store.ScheduleRunStateGet(ctx, row.DefID)
	if err != nil || st.FireCount < def.MaxFires {
		return false
	}
	if active, err := s.store.ScheduleActiveRunsList(ctx, row.DefID); err == nil && len(active) == 0 {
		s.retireIfSpent(ctx, row.DefID, row.Name, def)
	}
	return true
}

// SetReconcileCoordination wires the cluster gate for the reconcile sweep, so
// one replica per tick does it. Correctness does not depend on it — a run's
// row is deleted once, whoever tries — it only saves the other replicas the
// query. Without it (single replica) the sweep runs unguarded. Must be called
// before Start.
func (s *Scheduler) SetReconcileCoordination(lock AdvisoryLocker, key int64) {
	s.reconcileLock = lock
	s.reconcileLockKey = key
}

// reconcile finishes tracked runs that ended without being finished: their
// replica died, or a restore relaunched them somewhere else.
func (s *Scheduler) reconcile(ctx context.Context) {
	if s.reconcileLock == nil {
		s.reconcileOnce(ctx)
		return
	}
	if _, err := s.reconcileLock.TryRun(ctx, s.reconcileLockKey, func(ctx context.Context) error {
		s.reconcileOnce(ctx)
		return nil
	}); err != nil {
		s.logf("scheduler: reconcile lock: %v", err)
	}
}

func (s *Scheduler) reconcileOnce(ctx context.Context) {
	ended, err := s.store.ScheduleActiveRunsListEnded(ctx, reconcileBatch)
	if err != nil {
		s.logf("scheduler: reconcile: list ended runs: %v", err)
		return
	}
	for _, e := range ended {
		status, errStr := string(e.RunStatus), e.RunError
		if !e.RunFound {
			status, errStr = string(store.RunFailed), "run not found"
		}
		name, def := s.loadDef(ctx, e.DefID)
		s.finishTracked(ctx, e.DefID, name, def, e.RunID, e.AgentID, status, errStr)
	}
}

// loadDef reads a def for finishing one of its runs. A def that is gone or
// unreadable finishes its run with a zero def: no hooks, no retirement.
func (s *Scheduler) loadDef(ctx context.Context, defID string) (string, scheduleDef) {
	row, err := s.store.ScheduleDefGet(ctx, defID)
	if err != nil {
		s.logf("scheduler: reconcile: read def %s: %v — finishing its run without hooks", defID, err)
		return "", scheduleDef{}
	}
	def, err := unmarshalDef(row.Definition)
	if err != nil {
		s.logf("scheduler: reconcile: decode def %s: %v — finishing its run without hooks", defID, err)
		return row.Name, scheduleDef{}
	}
	if rehomed, ok := rehomeToOwningTenant(store.ScheduleDueRow{
		OwnerTenantID:          row.TenantID,
		BootstrappedFromStatic: row.BootstrappedFromStatic,
	}, def); ok {
		def = rehomed
	}
	return row.Name, def
}
