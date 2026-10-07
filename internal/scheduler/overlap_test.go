package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// makeDue puts the schedule's next slot in the past, as the next cron
// crossing would while its previous run is still going.
func makeDue(t *testing.T, st store.Store, defID string) {
	t.Helper()
	if err := st.ScheduleRunStateSeed(context.Background(), defID, time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("make due: %v", err)
	}
}

// overlapFixture is a schedule whose runs are held until release() — long
// runs on a short cron.
func overlapFixture(t *testing.T, policy string) (*Scheduler, *storeRunner, string, store.Store, func()) {
	t.Helper()
	def := channelHookDef("done-hook")
	def.Schedule = "* * * * *"
	def.ConcurrencyPolicy = policy
	sched, sr, defID, st := trackedFixture(t, def)
	sr.hold = make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(sr.hold) }) }
	t.Cleanup(func() { release(); sched.runs.Wait() })
	return sched, sr, defID, st, release
}

// forbid (the default): a slot that finds the previous run still going is
// skipped — recorded as skipped_overlap, not counted toward max_fires — and
// the next slot after the run ends starts normally.
//
// Fails-before: every slot started a run, so two overlapped.
func TestScheduler_ForbidSkipsASlotWhileTheRunIsStillGoing(t *testing.T) {
	for name, policy := range map[string]string{"unset": "", "forbid": "forbid"} {
		t.Run(name, func(t *testing.T) {
			sched, sr, defID, st, release := overlapFixture(t, policy)
			ctx := context.Background()

			sched.tick(ctx)
			makeDue(t, st, defID)
			sched.tick(ctx)

			if ids, _ := sr.started(); len(ids) != 1 {
				t.Fatalf("runs started = %d, want 1 — the second slot overlapped", len(ids))
			}
			got := runState(t, st, defID)
			if got.LastStatus != "skipped_overlap" || got.FireCount != 1 {
				t.Errorf("after the overlapping slot: status=%q fire_count=%d, want skipped_overlap 1", got.LastStatus, got.FireCount)
			}
			if got.NextRunAt.Before(time.Now()) {
				t.Errorf("a skipped slot did not move next_run_at on: %v", got.NextRunAt)
			}

			release()
			sched.runs.Wait()
			makeDue(t, st, defID)
			fireT(t, sched)
			if ids, _ := sr.started(); len(ids) != 2 {
				t.Errorf("runs started after the first ended = %d, want 2", len(ids))
			}
			if got := runState(t, st, defID); got.LastStatus != "completed" || got.FireCount != 2 {
				t.Errorf("after the next run: status=%q fire_count=%d, want completed 2", got.LastStatus, got.FireCount)
			}
		})
	}
}

// allow: every slot starts a run; each is tracked and finished on its own,
// with its own hooks.
func TestScheduler_AllowStartsOverlappingRunsAndFinishesEach(t *testing.T) {
	sched, sr, defID, st, release := overlapFixture(t, "allow")
	ctx := context.Background()

	sched.tick(ctx)
	makeDue(t, st, defID)
	sched.tick(ctx)

	if ids, _ := sr.started(); len(ids) != 2 {
		t.Fatalf("runs started = %d, want 2", len(ids))
	}
	if active, _ := st.ScheduleActiveRunsList(ctx, defID); len(active) != 2 {
		t.Errorf("active runs = %d, want 2", len(active))
	}
	release()
	sched.runs.Wait()
	if n := hookCount(t, st); n != 2 {
		t.Errorf("hooks = %d, want one per run", n)
	}
	if got := runState(t, st, defID); got.FireCount != 2 || got.LastStatus != "completed" {
		t.Errorf("status=%q fire_count=%d, want completed 2", got.LastStatus, got.FireCount)
	}
}

// fakeCanceller stops a run the way the server does: its row ends cancelled.
type fakeCanceller struct {
	st      store.Store
	refuse  bool
	mu      sync.Mutex
	calls   []string
	reasons []string
}

func (c *fakeCanceller) CancelScheduledRun(ctx context.Context, runID, reason string) (bool, error) {
	c.mu.Lock()
	c.calls = append(c.calls, runID)
	c.reasons = append(c.reasons, reason)
	c.mu.Unlock()
	if c.refuse {
		return false, nil
	}
	return true, c.st.FinishRun(ctx, runID, store.RunCancelled, "cancelled", store.Usage{}, reason)
}

// replace: the slot cancels the running run, then starts its own. The
// replaced run is finished as cancelled, with no hooks.
func TestScheduler_ReplaceCancelsTheRunningRunThenStarts(t *testing.T) {
	sched, sr, defID, st, release := overlapFixture(t, "replace")
	fc := &fakeCanceller{st: st}
	sched.SetRunCanceller(fc)
	ctx := context.Background()

	sched.tick(ctx)
	first, _ := sr.started()
	makeDue(t, st, defID)
	sched.tick(ctx)

	ids, _ := sr.started()
	if len(ids) != 2 {
		t.Fatalf("runs started = %d, want 2 — replace starts the new slot's run", len(ids))
	}
	if len(fc.calls) != 1 || fc.calls[0] != first[0] || fc.reasons[0] != "replaced by the next slot" {
		t.Errorf("cancels = %v reasons %v, want the first run, replaced by the next slot", fc.calls, fc.reasons)
	}
	release()
	sched.runs.Wait()
	if n := hookCount(t, st); n != 1 {
		t.Errorf("hooks = %d, want 1 — only the run that completed", n)
	}
	// Each run's own outcome. (The schedule's last_status describes whichever
	// finished last, and here both are released at once.)
	for i, want := range []store.RunStatus{store.RunCancelled, store.RunCompleted} {
		if run, err := st.GetRun(context.Background(), ids[i]); err != nil || run.Status != want {
			t.Errorf("run %d = %s (err %v), want %s", i, run.Status, err, want)
		}
	}
	if active, _ := st.ScheduleActiveRunsList(context.Background(), defID); len(active) != 0 {
		t.Errorf("active runs after both ended = %+v, want none", active)
	}
}

// replace that cannot stop the running run (a walk on another replica, an
// unreachable owner) starts nothing over it: the slot is skipped as forbid.
func TestScheduler_ReplaceThatCannotStopTheRunSkipsTheSlot(t *testing.T) {
	sched, sr, defID, st, _ := overlapFixture(t, "replace")
	sched.SetRunCanceller(&fakeCanceller{st: st, refuse: true})
	ctx := context.Background()

	sched.tick(ctx)
	makeDue(t, st, defID)
	sched.tick(ctx)

	if ids, _ := sr.started(); len(ids) != 1 {
		t.Errorf("runs started = %d, want 1 — nothing may start over a run that was not stopped", len(ids))
	}
	if got := runState(t, st, defID); got.LastStatus != "skipped_overlap" {
		t.Errorf("status = %q, want skipped_overlap", got.LastStatus)
	}
}

// A team schedule is forbid by default too: no second walk over a running
// one. (Before, a team tick started a walk over a running one.)
func TestScheduler_TeamScheduleForbidsASecondWalk(t *testing.T) {
	def := teamTick(0)
	def.Schedule = "* * * * *"
	sched, _, _, defID, st := schedulerFixture(t, def, time.Now().Add(-time.Minute))
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, "acme", "team:weekly-report", "u-42")
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	walk, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "team:weekly-report"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	teams := &countingWalkStarter{runID: walk.ID}
	sched.SetTeamWalkStarter(teams)

	fireT(t, sched)
	makeDue(t, st, defID)
	fireT(t, sched)

	if n := teams.count(); n != 1 {
		t.Errorf("walks started = %d, want 1 — the walk is still running", n)
	}
	if got := runState(t, st, defID); got.LastStatus != "skipped_overlap" {
		t.Errorf("status = %q, want skipped_overlap", got.LastStatus)
	}
}

type countingWalkStarter struct {
	runID string
	mu    sync.Mutex
	n     int
}

func (w *countingWalkStarter) StartTeamWalk(context.Context, runner.TeamWalkInput) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n++
	return w.runID, nil
}

func (w *countingWalkStarter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}
