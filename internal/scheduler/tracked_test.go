package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// storeRunner does to the runs table what the real runner does: it creates
// the run row (refusing a duplicate idempotency key), registers the run, and
// finishes the row with a status. So the scheduler reads a real outcome
// rather than RunOnce's return, which is nil however a registered run ended.
type storeRunner struct {
	st store.Store
	// hold, when set, keeps every registered run going until it is closed.
	hold chan struct{}
	// status and errMsg are how each run ends (default completed).
	status store.RunStatus
	errMsg string
	// leave returns without finishing the run row: the replica died, and the
	// row still reads running.
	leave bool

	mu     sync.Mutex
	runIDs []string
	ctxs   []context.Context
}

func (r *storeRunner) RunOnce(ctx context.Context, in runner.RunInput, cb runner.RunCallbacks) error {
	sess, err := r.st.CreateSession(ctx, in.TenantID, in.Agent, in.UserID)
	if err != nil {
		return err
	}
	run, err := r.st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a-" + in.Agent, IdempotencyKey: in.IdempotencyKey})
	if err != nil {
		return err // ErrDuplicateIdempotencyKey, before anything ran
	}
	r.mu.Lock()
	r.runIDs = append(r.runIDs, run.ID)
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
	cb.OnRegistered("a-"+in.Agent, run.ID, sess.ID, "")
	if r.hold != nil {
		<-r.hold
	}
	if r.leave {
		return nil
	}
	status := r.status
	if status == "" {
		status = store.RunCompleted
	}
	return r.st.FinishRun(context.WithoutCancel(ctx), run.ID, status, "end_turn", store.Usage{}, r.errMsg)
}

func (r *storeRunner) started() ([]string, []context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.runIDs...), append([]context.Context(nil), r.ctxs...)
}

// trackedFixture is schedulerFixture with a storeRunner and an on_complete
// hook publishing to a global channel, so a test can count dispatches.
func trackedFixture(t *testing.T, def scheduleDef) (*Scheduler, *storeRunner, string, store.Store) {
	t.Helper()
	sched, _, _, defID, st := schedulerFixture(t, def, time.Now().Add(-time.Minute))
	sr := &storeRunner{st: st}
	sched.runner = sr
	sched.SetChannelScope(func(context.Context, string, string) (DeclaredChannel, bool) {
		return DeclaredChannel{Scope: "global"}, true
	})
	return sched, sr, defID, st
}

func hookCount(t *testing.T, st store.Store) int {
	t.Helper()
	return peekScopeCount(t, st, "done-hook", store.MemoryScopeGlobal, "")
}

func runState(t *testing.T, st store.Store, defID string) store.ScheduleRunStateRow {
	t.Helper()
	got, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	return got
}

// The tick returns once its run is admitted and the run carries on after it:
// no deadline on the run, nothing cancels it when the scheduler's ctx ends,
// and its outcome and hooks are recorded when it finishes. (RFC DZ S1 + S2.)
//
// Fails-before: the tick waited for the run, so it never returned while the
// run was held, and the run ran under a FireTimeout deadline.
func TestScheduler_TickReturnsWhileItsRunRunsAndTheRunHasNoDeadline(t *testing.T) {
	sched, sr, defID, st := trackedFixture(t, channelHookDef("done-hook"))
	sr.hold = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticked := make(chan struct{})
	go func() {
		sched.tick(ctx)
		close(ticked)
	}()
	select {
	case <-ticked:
	case <-time.After(5 * time.Second):
		close(sr.hold)
		t.Fatal("the tick did not return while its run ran — it must wait for admission only")
	}

	ids, ctxs := sr.started()
	if len(ids) != 1 {
		t.Fatalf("runs started = %d, want 1", len(ids))
	}
	if _, has := ctxs[0].Deadline(); has {
		t.Errorf("the scheduled run has a deadline — a scheduled run has no wall clock")
	}
	got := runState(t, st, defID)
	if got.LastStatus != "running" || got.LastRunID != ids[0] || got.FireCount != 1 {
		t.Errorf("while running: status=%q run=%q fire_count=%d, want running %s 1", got.LastStatus, got.LastRunID, got.FireCount, ids[0])
	}
	if active, _ := st.ScheduleActiveRunsList(context.Background(), defID); len(active) != 1 || active[0].RunID != ids[0] {
		t.Errorf("active runs = %+v, want the run tracked", active)
	}

	// The scheduler stopping does not stop the run.
	cancel()
	if err := ctxs[0].Err(); err != nil {
		t.Errorf("the run's ctx ended with the scheduler's (%v) — only a cancel may stop it", err)
	}
	if n := hookCount(t, st); n != 0 {
		t.Errorf("hooks dispatched before the run ended: %d", n)
	}

	close(sr.hold)
	sched.runs.Wait()
	got = runState(t, st, defID)
	if got.LastStatus != "completed" || got.LastRunID != ids[0] || got.FinishedAt.IsZero() {
		t.Errorf("after the run: status=%q run=%q finished_at=%v, want completed with a finish time", got.LastStatus, got.LastRunID, got.FinishedAt)
	}
	if active, _ := st.ScheduleActiveRunsList(context.Background(), defID); len(active) != 0 {
		t.Errorf("active runs after it finished = %+v, want none", active)
	}
	if n := hookCount(t, st); n != 1 {
		t.Errorf("on_complete dispatched %d time(s), want 1", n)
	}
}

// A run that fails is recorded as failed, with its error, and fires no hooks.
//
// Fails-before: RunOnce returns nil once a run has registered, however the
// loop ended, so a failed run was recorded "completed" and its hooks fired.
func TestScheduler_ARunThatFailsIsRecordedFailedWithoutHooks(t *testing.T) {
	sched, sr, defID, st := trackedFixture(t, channelHookDef("done-hook"))
	sr.status, sr.errMsg = store.RunFailed, "provider exploded"

	fireT(t, sched)

	got := runState(t, st, defID)
	if got.LastStatus != "failed" || got.LastError != "provider exploded" {
		t.Errorf("status=%q error=%q, want failed / provider exploded", got.LastStatus, got.LastError)
	}
	if n := hookCount(t, st); n != 0 {
		t.Errorf("a failed run dispatched %d hook(s), want 0", n)
	}
}

// A run whose replica died before finishing it is finished by the reconciler
// on a later tick: with its hooks if it completed, without if it failed — and
// either way only once.
func TestScheduler_ReconcilerFinishesARunWhoseReplicaDied(t *testing.T) {
	for _, tc := range []struct {
		name      string
		end       store.RunStatus
		errMsg    string
		wantHooks int
	}{
		{"completed before the replica died", store.RunCompleted, "", 1},
		{"failed by the stale-run sweeper", store.RunFailed, "heartbeat_timeout", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sched, sr, defID, st := trackedFixture(t, channelHookDef("done-hook"))
			sr.leave = true // the run row still reads running when RunOnce returns

			fireT(t, sched)
			ids, _ := sr.started()
			if len(ids) != 1 {
				t.Fatalf("runs started = %d, want 1", len(ids))
			}
			if got := runState(t, st, defID); got.LastStatus != "running" {
				t.Fatalf("status = %q while the run row reads running, want running", got.LastStatus)
			}

			// The run ends somewhere else: its row is finished, nobody here
			// is waiting on it.
			if err := st.FinishRun(context.Background(), ids[0], tc.end, "end_turn", store.Usage{}, tc.errMsg); err != nil {
				t.Fatalf("finish run: %v", err)
			}
			fireT(t, sched) // nothing due; the reconciler runs
			fireT(t, sched) // and again: nothing left to finish

			got := runState(t, st, defID)
			if got.LastStatus != string(tc.end) || got.LastError != tc.errMsg || got.LastRunID != ids[0] {
				t.Errorf("status=%q error=%q run=%q, want %s %q %s", got.LastStatus, got.LastError, got.LastRunID, tc.end, tc.errMsg, ids[0])
			}
			if n := hookCount(t, st); n != tc.wantHooks {
				t.Errorf("hooks = %d, want %d", n, tc.wantHooks)
			}
			if active, _ := st.ScheduleActiveRunsList(context.Background(), defID); len(active) != 0 {
				t.Errorf("active runs after reconcile = %+v, want none", active)
			}
		})
	}
}

// The replica that ran a run and the reconciler can both try to finish it.
// Whoever does, the hooks fire once.
func TestScheduler_HooksFireOnceWhenFinishersRace(t *testing.T) {
	sched, sr, defID, st := trackedFixture(t, channelHookDef("done-hook"))
	sr.leave = true
	fireT(t, sched)
	ids, _ := sr.started()
	if err := st.FinishRun(context.Background(), ids[0], store.RunCompleted, "end_turn", store.Usage{}, ""); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	name, def := sched.loadDef(context.Background(), defID)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			sched.finishTracked(context.Background(), defID, name, def, ids[0], "a-researcher", "completed", "")
		}()
		go func() {
			defer wg.Done()
			sched.reconcileOnce(context.Background())
		}()
	}
	wg.Wait()
	if n := hookCount(t, st); n != 1 {
		t.Errorf("on_complete dispatched %d time(s) by racing finishers, want exactly 1", n)
	}
}

// A one-shot schedule is retired when its run FINISHES, not while it runs —
// and a slot that comes due while that run is still going starts nothing,
// because the one fire it had is spent.
func TestScheduler_OneShotRetiresWhenItsRunFinishes(t *testing.T) {
	def := channelHookDef("done-hook")
	def.MaxFires = 1
	def.Schedule = "* * * * *"
	sched, sr, defID, st := trackedFixture(t, def)
	sr.hold = make(chan struct{})
	ctx := context.Background()

	sched.tick(ctx)
	if row, _ := st.ScheduleDefGet(ctx, defID); row.Retired {
		t.Fatal("retired while its run is still running")
	}
	// The next slot comes due before the run ends.
	if err := st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("make due: %v", err)
	}
	sched.tick(ctx)
	if ids, _ := sr.started(); len(ids) != 1 {
		t.Errorf("runs started = %d, want 1 — a one-shot fires once", len(ids))
	}

	close(sr.hold)
	sched.runs.Wait()
	if row, _ := st.ScheduleDefGet(ctx, defID); !row.Retired {
		t.Error("not retired after its one run finished")
	}
	if n := hookCount(t, st); n != 1 {
		t.Errorf("hooks = %d, want 1", n)
	}
}

// A team tick's walk is tracked to its end: the schedule reads running while
// the walk runs and records how the walk ended. (RFC DZ S6.)
//
// Fails-before: the schedule recorded "completed" the moment the walk started
// and never looked again.
func TestScheduler_TeamWalkIsRecordedWhenItEnds(t *testing.T) {
	sched, _, _, defID, st := schedulerFixture(t, teamTick(0), time.Now().Add(-time.Minute))
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "acme", "team:weekly-report", "u-42")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	walk, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "team:weekly-report"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sched.SetTeamWalkStarter(walkStarter{runID: walk.ID})

	fireT(t, sched)
	if got := runState(t, st, defID); got.LastStatus != "running" || got.LastRunID != walk.ID {
		t.Fatalf("while the walk runs: status=%q run=%q, want running %s", got.LastStatus, got.LastRunID, walk.ID)
	}
	if err := st.FinishRun(ctx, walk.ID, store.RunCancelled, "cancelled", store.Usage{}, "cancelled by operator"); err != nil {
		t.Fatalf("finish walk: %v", err)
	}
	fireT(t, sched)
	if got := runState(t, st, defID); got.LastStatus != "cancelled" || got.LastError != "cancelled by operator" || got.FireCount != 1 {
		t.Errorf("after the walk: status=%q error=%q fire_count=%d, want cancelled / its reason / 1", got.LastStatus, got.LastError, got.FireCount)
	}
}

type walkStarter struct{ runID string }

func (w walkStarter) StartTeamWalk(context.Context, runner.TeamWalkInput) (string, error) {
	return w.runID, nil
}

// With the claim out of the way, the slot's key alone keeps a slot to one
// run: a second fire of the same slot is refused by the runs table before it
// starts, and is neither recorded nor counted.
func TestScheduler_SlotKeyAloneKeepsASlotToOneRun(t *testing.T) {
	sched, sr, defID, st := trackedFixture(t, channelHookDef("done-hook"))
	ctx := context.Background()
	row, err := st.ScheduleRunStateGet(ctx, defID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	due := store.ScheduleDueRow{DefID: defID, Name: "sched-test", NextRunAt: row.NextRunAt}
	def := channelHookDef("done-hook")
	in := buildRunInput(def, nil, t.Logf)
	in.IdempotencyKey = slotRunKey(defID, row.NextRunAt)

	// Two fires of one slot, neither claiming it.
	sched.fireRun(ctx, due, def, in, time.Now())
	sched.fireRun(ctx, due, def, in, time.Now())
	sched.runs.Wait()

	if ids, _ := sr.started(); len(ids) != 1 {
		t.Errorf("runs started = %d, want 1", len(ids))
	}
	if got := runState(t, st, defID); got.FireCount != 1 || got.LastStatus != "completed" {
		t.Errorf("state = status %q fire_count %d, want completed 1 — the refused duplicate must not be recorded", got.LastStatus, got.FireCount)
	}
	if n := hookCount(t, st); n != 1 {
		t.Errorf("hooks = %d, want 1", n)
	}
}

// A run that cannot be tracked (its schedule's state row is gone) still runs,
// and its outcome is recorded by the goroutine that ran it.
func TestScheduler_AnUntrackedRunIsStillRecorded(t *testing.T) {
	sched, sr, defID, st := trackedFixture(t, channelHookDef("done-hook"))
	sched.store = failingStart{Store: st}

	fireT(t, sched)

	if ids, _ := sr.started(); len(ids) != 1 {
		t.Fatalf("runs started = %d, want 1", len(ids))
	}
	if got := runState(t, st, defID); got.LastStatus != "completed" || got.FireCount != 1 {
		t.Errorf("status=%q fire_count=%d, want completed 1", got.LastStatus, got.FireCount)
	}
	if n := hookCount(t, st); n != 1 {
		t.Errorf("hooks = %d, want 1", n)
	}
}

type failingStart struct{ store.Store }

func (failingStart) ScheduleActiveRunStart(context.Context, store.ScheduleActiveRun) error {
	return errors.New("store unavailable")
}

// A consolidation sweep runs off the tick, so the tick returns while it runs;
// and a slot of the same def that comes due meanwhile starts no second sweep
// on this replica.
//
// Fails-before (the sweep on the tick): the first fire blocks until the
// sweep's runs end.
func TestFanout_SweepRunsOffTheTickOnePerDef(t *testing.T) {
	sched, fr, st, logs := fanoutFixture(t, operatorFanoutDef(nil), nil)
	seedSettledSession(t, st, "acme", "u1")
	release := make(chan struct{})
	fr.onRun = func(runner.RunInput) { <-release }
	row := dueRow(t, st)

	fired := make(chan struct{})
	go func() {
		sched.fireOne(context.Background(), row, time.Now())
		close(fired)
	}()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the fire waited for the sweep — a sweep must run off the tick")
	}
	sched.fireOne(context.Background(), dueAgain(t, st, row), time.Now())
	close(release)
	sched.sweeps.Wait()

	if n := len(fr.Calls()); n != 1 {
		t.Errorf("sweep runs dispatched = %d, want 1 — the second slot must not start a second sweep; logs:\n%s", n, logs.all())
	}
}
