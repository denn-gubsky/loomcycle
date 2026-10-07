package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// backlogFixture is an hourly schedule whose last ten slots were all missed:
// next_run_at sits nine hours before the current hour.
func backlogFixture(t *testing.T, catchUpMax int, policy string) (*Scheduler, *storeRunner, string, store.Store, []time.Time) {
	t.Helper()
	def := channelHookDef("done-hook")
	def.Schedule = "0 * * * *"
	def.CatchUpMax = catchUpMax
	def.ConcurrencyPolicy = policy
	sched, sr, defID, st := trackedFixture(t, def)
	// The slots are counted up to now: a test that crosses an hour would see
	// an eleventh. Start clear of the boundary.
	if left := time.Until(time.Now().UTC().Truncate(time.Hour).Add(time.Hour)); left < 10*time.Second {
		time.Sleep(left + time.Second)
	}
	hour := time.Now().UTC().Truncate(time.Hour)
	slots := make([]time.Time, 10)
	for i := range slots {
		slots[i] = hour.Add(time.Duration(i-9) * time.Hour)
	}
	if err := st.ScheduleRunStateSeed(context.Background(), defID, slots[0]); err != nil {
		t.Fatal(err)
	}
	return sched, sr, defID, st, slots
}

func slotKeys(defID string, slots ...time.Time) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = slotRunKey(defID, s)
	}
	return out
}

func keysOf(ins []runner.RunInput) []string {
	out := make([]string, len(ins))
	for i, in := range ins {
		out[i] = in.IdempotencyKey
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// catch_up_max 0 (the default) collapses an outage into one live fire, for
// the newest missed slot — and says how many it dropped.
func TestCatchUp_ZeroCollapsesTheOutageAndCountsTheDropped(t *testing.T) {
	sched, sr, defID, st, slots := backlogFixture(t, 0, "")

	fireT(t, sched)

	ins := sr.inputs()
	if got, want := keysOf(ins), slotKeys(defID, slots[9]); !equalStrings(got, want) {
		t.Fatalf("runs = %v, want one for the newest slot %v", got, want)
	}
	if _, has := ins[0].Metadata["slot_at"]; has {
		t.Errorf("a live fire carries catch-up metadata: %v", ins[0].Metadata)
	}
	st0 := runState(t, st, defID)
	if st0.MissedSlots != 9 || !st0.NextRunAt.After(time.Now()) || !st0.CatchUpUntil.IsZero() {
		t.Errorf("state missed=%d next=%v until=%v, want 9 dropped, a future slot, no backlog", st0.MissedSlots, st0.NextRunAt, st0.CatchUpUntil)
	}
}

// forbid (the default): the newest catch_up_max missed slots run one after
// another in slot order, each carrying its slot; the older ones are counted as
// dropped; a catch-up slot WAITS for the run before it rather than being
// skipped; and the schedule then resumes its cadence. (RFC DZ acceptance 6.)
//
// Fails-before: catch_up_max was never read — one fire, for the listed slot.
func TestCatchUp_ForbidDrainsTheKeptSlotsOneAtATime(t *testing.T) {
	sched, sr, defID, st, slots := backlogFixture(t, 3, "")
	sr.gate = make(chan struct{})
	ctx := context.Background()
	release := func() {
		t.Helper()
		sr.gate <- struct{}{}
		sched.runs.Wait()
	}

	sched.tick(ctx)
	if got := runState(t, st, defID); got.MissedSlots != 7 || !got.NextRunAt.Equal(slots[8]) {
		t.Fatalf("after the first claim: missed=%d next=%v, want 7 dropped and the next kept slot %v", got.MissedSlots, got.NextRunAt, slots[8])
	}
	// The next kept slot is due, but the first catch-up run is still going:
	// it waits, unclaimed.
	sched.tick(ctx)
	if n := len(sr.inputs()); n != 1 {
		t.Fatalf("runs started while the first ran = %d, want 1 — a catch-up slot waits", n)
	}
	if got := runState(t, st, defID); !got.NextRunAt.Equal(slots[8]) || got.LastStatus == "skipped_overlap" {
		t.Fatalf("the waiting slot moved: next=%v status=%q, want still %v and not skipped", got.NextRunAt, got.LastStatus, slots[8])
	}
	release()
	sched.tick(ctx)
	release()
	// The last kept slot is the only one still due now; it must still be a
	// catch-up slot, so it waits for — rather than is skipped by — forbid.
	sched.tick(ctx)
	release()
	sched.tick(ctx) // nothing left due

	ins := sr.inputs()
	if got, want := keysOf(ins), slotKeys(defID, slots[7], slots[8], slots[9]); !equalStrings(got, want) {
		t.Fatalf("runs = %v, want the newest three slots in order %v", got, want)
	}
	for i, in := range ins {
		if in.Metadata["catch_up"] != true || in.Metadata["slot_at"] != slots[7+i].Format(time.RFC3339) {
			t.Errorf("run %d metadata = %v, want catch_up and slot_at %s", i, in.Metadata, slots[7+i].Format(time.RFC3339))
		}
	}
	got := runState(t, st, defID)
	if !got.NextRunAt.After(time.Now()) || got.FireCount != 3 || got.LastStatus != "completed" {
		t.Errorf("after the backlog: next=%v fire_count=%d status=%q, want the cadence resumed, 3 fires, completed", got.NextRunAt, got.FireCount, got.LastStatus)
	}
	if n := hookCount(t, st); n != 3 {
		t.Errorf("hooks = %d, want one per catch-up run", n)
	}
}

// allow: the kept slots start at once, as long as the schedule has fewer than
// catch_up_max running runs; with one already running when the backlog is
// found, two start and the third waits until one ends. (RFC DZ acceptance 7.)
func TestCatchUp_AllowStartsKeptSlotsAtOnceUnderTheLimit(t *testing.T) {
	sched, sr, defID, st, slots := backlogFixture(t, 3, "allow")
	sr.gate = make(chan struct{})
	ctx := context.Background()

	// A run of this schedule is already going.
	sess, err := st.CreateSession(ctx, "", "researcher", "alice")
	if err != nil {
		t.Fatal(err)
	}
	pre, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a-pre"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ScheduleActiveRunStart(ctx, store.ScheduleActiveRun{DefID: defID, RunID: pre.ID, SlotAt: slots[0], StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	sched.tick(ctx)
	if got, want := keysOf(sr.inputs()), slotKeys(defID, slots[7], slots[8]); !equalStrings(got, want) {
		t.Fatalf("runs started at once = %v, want the two that fit under catch_up_max %v", got, want)
	}
	active, _ := st.ScheduleActiveRunsList(ctx, defID)
	if len(active) != 3 || !active[1].CatchUp || !active[2].CatchUp {
		t.Fatalf("active = %+v, want 3 with the new ones marked catch-up", active)
	}
	if got := runState(t, st, defID); !got.NextRunAt.Equal(slots[9]) {
		t.Fatalf("next_run_at = %v, want the third kept slot %v waiting", got.NextRunAt, slots[9])
	}

	// One catch-up run ends; the third kept slot starts on the next tick.
	sr.gate <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if rows, _ := st.ScheduleActiveRunsList(ctx, defID); len(rows) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the released run was never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sched.tick(ctx)
	if got, want := keysOf(sr.inputs()), slotKeys(defID, slots[7], slots[8], slots[9]); !equalStrings(got, want) {
		t.Errorf("runs = %v, want the third kept slot started once one ended %v", got, want)
	}
	sr.gate <- struct{}{}
	sr.gate <- struct{}{}
	sched.runs.Wait()
}
