package scheduler

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// These drive a schedule restored from a snapshot through the real sweeper:
// the restored count is what max_fires is enforced against, and a schedule
// restored without its credentials advances without firing.

func openSQLite(t *testing.T, name string) store.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// plantFired writes an active def with a run state that has spent `fires`
// fires and is due now.
func plantFired(t *testing.T, st store.Store, defID, name string, body map[string]any, fires int) {
	t.Helper()
	ctx := context.Background()
	raw, _ := json.Marshal(body)
	if _, err := st.ScheduleDefCreate(ctx, store.ScheduleDefRow{DefID: defID, Name: name, Definition: raw}); err != nil {
		t.Fatal(err)
	}
	if err := st.ScheduleDefSetActive(ctx, "", name, defID, "test"); err != nil {
		t.Fatal(err)
	}
	due := time.Now().Add(-time.Minute)
	if err := st.ScheduleRunStateSeed(ctx, defID, due); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < fires; i++ {
		if err := st.ScheduleRunStateRecordResult(ctx, store.ScheduleRunResult{
			DefID: defID, LastStatus: "completed", LastRunAt: time.Now(), NextRunAt: due, CountAsFire: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func snapshotInto(t *testing.T, src, dst store.Store) snapshot.RestoreResult {
	t.Helper()
	_, raw, err := snapshot.Capture(context.Background(), src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	res, err := snapshot.Restore(context.Background(), dst, raw, snapshot.RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return res
}

// V1: a max_fires:5 schedule that fired 3 times on the source restores active
// with 2 fires left: a past-due next_run_at fires exactly once on the first
// sweep, and the def retires after the 5th fire in total.
func TestScheduler_RestoredScheduleFiresOnItsCarriedCountAndStopsAtMaxFires(t *testing.T) {
	src := openSQLite(t, "src.db")
	dst := openSQLite(t, "dst.db")
	ctx := context.Background()
	plantFired(t, src, "sd_digest", "digest", map[string]any{"agent": "researcher", "schedule": "0 * * * *", "max_fires": 5}, 3)
	snapshotInto(t, src, dst)

	fr := &fakeRunner{}
	sched := New(Config{TickInterval: 10 * time.Millisecond, FireTimeout: 5 * time.Second}, dst, fr, nil, &fakeMCP{}, t.Logf)

	sched.tick(ctx)
	if got := len(fr.Calls()); got != 1 {
		t.Fatalf("first sweep after restore fired %d times, want exactly 1 (past due fires once, no burst)", got)
	}
	st, _ := dst.ScheduleRunStateGet(ctx, "sd_digest")
	if st.FireCount != 4 {
		t.Fatalf("fire_count = %d, want 4 (3 carried + 1)", st.FireCount)
	}
	// Force it due again: the 5th fire in total is the last.
	for i := 0; i < 3; i++ {
		if err := dst.ScheduleRunStateSeed(ctx, "sd_digest", time.Now().Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		sched.tick(ctx)
	}
	if got := len(fr.Calls()); got != 2 {
		t.Errorf("restored schedule fired %d times in total, want 2 (max_fires 5, 3 spent on the source)", got)
	}
	if row, _ := dst.ScheduleDefGet(ctx, "sd_digest"); !row.Retired {
		t.Error("the schedule was not retired at max_fires")
	}
}

// V2: a schedule restored disabled for its stripped credentials advances on
// the sweep without firing and without spending its budget.
func TestScheduler_RestoredCredentialStrippedScheduleAdvancesWithoutFiring(t *testing.T) {
	src := openSQLite(t, "src.db")
	dst := openSQLite(t, "dst.db")
	ctx := context.Background()
	plantFired(t, src, "sd_cred", "digest", map[string]any{
		"agent": "researcher", "schedule": "0 * * * *", "max_fires": 5, "enabled": true,
		"user_credentials": map[string]string{"slack": "literal-token"},
	}, 3)
	if res := snapshotInto(t, src, dst); res.DefsDisabledForCredentials != 1 {
		t.Fatalf("defs_disabled_for_credentials = %d, want 1", res.DefsDisabledForCredentials)
	}

	fr := &fakeRunner{}
	sched := New(Config{TickInterval: 10 * time.Millisecond, FireTimeout: 5 * time.Second}, dst, fr, nil, &fakeMCP{}, t.Logf)
	sched.tick(ctx)
	if got := len(fr.Calls()); got != 0 {
		t.Fatalf("a schedule restored without its credentials fired %d times", got)
	}
	st, _ := dst.ScheduleRunStateGet(ctx, "sd_cred")
	if st.FireCount != 3 || st.LastStatus != "skipped_disabled" || st.NextRunAt.Before(time.Now()) {
		t.Errorf("state = count %d status %q next %v; want 3, skipped_disabled, advanced", st.FireCount, st.LastStatus, st.NextRunAt)
	}
}

// A consolidation queue restored from a snapshot is drained by the target's
// scheduled consolidation fan-out. The source's sessions do not travel, so on
// a fresh target the restored queue is the only sign the user exists: the
// fan-out must find the target from the queue itself and dispatch a pass
// under that user's identity, in the row's own tenant.
func TestScheduler_RestoredConsolidationQueueIsDispatchedOnAFreshTarget(t *testing.T) {
	src := openSQLite(t, "src.db")
	ctx := context.Background()
	// On the source alice has a chat AND a queued add; only the add travels.
	seedSettledSession(t, src, "", "alice")
	if err := src.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{ID: "pend_restored", Scope: store.MemoryScopeUser,
		ScopeID: "alice", Payload: json.RawMessage(`{"messages":[{"role":"user","content":"I moved to Porto"}]}`)}); err != nil {
		t.Fatal(err)
	}

	sched, fr, dst, logs := fanoutFixture(t, fanoutDef(nil), nil)
	sched.SetProviderResolver(stubProviderResolver{provider: "anthropic"})
	if res := snapshotInto(t, src, dst); res.MemoryPendingRestored != 1 {
		t.Fatalf("memory_pending restored = %d, want 1", res.MemoryPendingRestored)
	}
	if sessions, _, err := dst.ListSessions(ctx, store.SessionFilter{}, 10, 0); err != nil || len(sessions) != 0 {
		t.Fatalf("premise: the target holds sessions %+v (err %v); the case is a target with the queue alone", sessions, err)
	}

	fireT(t, sched)

	calls := fr.Calls()
	if len(calls) != 1 || calls[0].UserID != "alice" || calls[0].TenantID != "" {
		t.Fatalf("fan-out dispatched %+v, want one pass for alice in the operator layer; logs:\n%s", calls, logs.all())
	}
}
