package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// seedDue writes n active schedules, each due a minute ago.
func seedDue(t *testing.T, st store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	enabled := true
	defJSON, _ := json.Marshal(scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled})
	for i := 0; i < n; i++ {
		defID, name := fmt.Sprintf("sd-claim-%d", i), fmt.Sprintf("claim-%d", i)
		if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{DefID: defID, Name: name, Definition: defJSON}); err != nil {
			t.Fatalf("def create: %v", err)
		}
		if err := st.ScheduleDefSetActive(ctx, "", name, defID, "test"); err != nil {
			t.Fatalf("set active: %v", err)
		}
		if err := st.ScheduleRunStateSeed(ctx, defID, time.Now().Add(-time.Minute)); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

// Two replicas share one store and tick at the same moment: every due slot
// fires once. Each lists all the rows; the claim decides who fires each one.
//
// Fails-before: both replicas fire every row (the runs outlast the listing,
// and next_run_at only moved after a run), so the runner sees 2× the slots.
func TestScheduler_TwoReplicasFireEachSlotOnce(t *testing.T) {
	st, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const slots = 10
	seedDue(t, st, slots)

	// Each run takes long enough that both replicas have listed the rows
	// before either run ends.
	fr := &fakeRunner{onRun: func(runner.RunInput) { time.Sleep(50 * time.Millisecond) }}
	a := New(Config{TickInterval: time.Hour, MaxConcurrentFires: slots, ReplicaID: "replica-a"}, st, fr, nil, nil, t.Logf)
	b := New(Config{TickInterval: time.Hour, MaxConcurrentFires: slots, ReplicaID: "replica-b"}, st, fr, nil, nil, t.Logf)

	var wg sync.WaitGroup
	for _, s := range []*Scheduler{a, b} {
		wg.Add(1)
		go func(s *Scheduler) {
			defer wg.Done()
			s.tick(context.Background())
		}(s)
	}
	wg.Wait()

	calls := fr.Calls()
	if len(calls) != slots {
		t.Fatalf("runs fired = %d, want %d (one per slot across both replicas)", len(calls), slots)
	}
	keys := map[string]bool{}
	for _, c := range calls {
		if keys[c.IdempotencyKey] {
			t.Errorf("slot %q fired twice", c.IdempotencyKey)
		}
		keys[c.IdempotencyKey] = true
	}
	for i := 0; i < slots; i++ {
		got, err := st.ScheduleRunStateGet(context.Background(), fmt.Sprintf("sd-claim-%d", i))
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ClaimedBy != "replica-a" && got.ClaimedBy != "replica-b" {
			t.Errorf("slot of sd-claim-%d claimed by %q, want one of the replicas", i, got.ClaimedBy)
		}
		if got.FireCount != 1 {
			t.Errorf("sd-claim-%d fire_count = %d, want 1", i, got.FireCount)
		}
	}
}

// The claim moves next_run_at on BEFORE the run, so a run that outlasts its
// cadence does not leave its row due — and the row records which slot was
// taken, by whom.
//
// Fails-before: next_run_at only moved after the run, so during the run the
// row was still in the past.
func TestScheduler_ClaimAdvancesNextRunAtBeforeTheRun(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled}
	slot := time.Now().Add(-time.Minute)
	sched, fr, _, defID, st := schedulerFixture(t, def, slot)
	sched.cfg.ReplicaID = "replica-x"
	listed, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	var during store.ScheduleRunStateRow
	fr.onRun = func(runner.RunInput) {
		during, _ = st.ScheduleRunStateGet(context.Background(), defID)
	}
	fireT(t, sched)

	if len(fr.Calls()) != 1 {
		t.Fatalf("runs = %d, want 1", len(fr.Calls()))
	}
	if !during.NextRunAt.After(time.Now()) {
		t.Errorf("while the run ran, next_run_at = %v — still due; the claim must move it on before firing", during.NextRunAt)
	}
	if !during.SlotAt.Equal(listed.NextRunAt) || during.ClaimedBy != "replica-x" {
		t.Errorf("claim recorded slot=%v by=%q, want slot=%v by replica-x", during.SlotAt, during.ClaimedBy, listed.NextRunAt)
	}
	// And a second tick while nothing is due fires nothing.
	fireT(t, sched)
	if len(fr.Calls()) != 1 {
		t.Errorf("runs after a second tick = %d, want 1", len(fr.Calls()))
	}
}

// A slot's run carries the slot's key, and a run the runs table refuses as a
// duplicate of that key is not recorded or counted: the slot already has its
// run, and whoever started it records it.
func TestScheduler_RunCarriesItsSlotKeyAndADuplicateIsNotRecorded(t *testing.T) {
	enabled := true
	def := scheduleDef{Agent: "researcher", Schedule: "0 * * * *", Enabled: &enabled, MaxFires: 1}
	sched, fr, _, defID, st := schedulerFixture(t, def, time.Now().Add(-time.Minute))
	listed, err := st.ScheduleRunStateGet(context.Background(), defID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	fr.runErr = store.ErrDuplicateIdempotencyKey
	fireT(t, sched)

	calls := fr.Calls()
	if len(calls) != 1 {
		t.Fatalf("runs = %d, want 1", len(calls))
	}
	if want := fmt.Sprintf("sched:%s:%d", defID, listed.NextRunAt.UnixMicro()); calls[0].IdempotencyKey != want {
		t.Errorf("idempotency key = %q, want %q", calls[0].IdempotencyKey, want)
	}
	got, _ := st.ScheduleRunStateGet(context.Background(), defID)
	if got.LastStatus != "" || got.FireCount != 0 {
		t.Errorf("a duplicate was recorded: last_status=%q fire_count=%d, want nothing", got.LastStatus, got.FireCount)
	}
	if retired, _ := st.ScheduleDefGet(context.Background(), defID); retired.Retired {
		t.Errorf("a duplicate retired a max_fires:1 schedule")
	}
}
