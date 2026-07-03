package usage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// newTestStore opens a fresh sqlite store for the sweeper tests. Sweeper
// logic is store-agnostic; using sqlite keeps the tests fast and doesn't
// require a Postgres fixture. (The rollup-and-prune SQL itself is
// exercised against both backends by the store contract suite.)
func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSweeperOnce_PrunesOldDetail exercises sweepOnce against a real
// store without spinning the Run loop. Two token_usage rows older than
// the retention window are folded + deleted; a recent row is left in the
// detail table. The clock is pinned via Config.Now so the cutoff lands
// deterministically between the old and recent rows — no dependence on
// wall-clock elapsed time.
func TestSweeperOnce_PrunesOldDetail(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Pinned "now"; cutoff = now - retention window.
	now := time.Now()
	const retention = 24 * time.Hour

	mk := func(runID string, in int, ts time.Time) store.TokenUsageRow {
		return store.TokenUsageRow{
			RunID: runID, TenantID: "A", Provider: "anthropic", Model: "m",
			CredentialSource: "operator", InputTokens: in, Cost: 1.0, CostCurrency: "USD", TS: ts,
		}
	}
	// Two OLD rows (well past the window) + one RECENT row (inside it).
	seed := []store.TokenUsageRow{
		mk("old", 100, now.Add(-48*time.Hour)),
		mk("old", 200, now.Add(-48*time.Hour)),
		mk("recent", 50, now.Add(-1*time.Hour)),
	}
	for _, r := range seed {
		if err := st.RecordCallUsage(ctx, r); err != nil {
			t.Fatalf("RecordCallUsage: %v", err)
		}
	}

	sw := New(st, Config{
		Interval:        1 * time.Hour, // unused — we drive sweepOnce directly
		DetailRetention: retention,
		Logger:          func(format string, args ...any) {}, // silence
		Now:             func() time.Time { return now },     // cutoff == now-24h
	})

	n, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatalf("sweepOnce: %v", err)
	}
	if n != 2 {
		t.Errorf("sweepOnce pruned %d, want 2 (the two old rows)", n)
	}

	// The recent detail row survives; the old detail is gone.
	if rows, _ := st.TokenUsageForRun(ctx, "recent"); len(rows) != 1 {
		t.Errorf("recent run detail = %d rows, want 1 (should be spared)", len(rows))
	}
	if rows, _ := st.TokenUsageForRun(ctx, "old"); len(rows) != 0 {
		t.Errorf("old run detail = %d rows, want 0 (should be pruned)", len(rows))
	}

	// Idempotent: a second sweep with the same clock prunes nothing.
	if n2, err := sw.sweepOnce(ctx); err != nil || n2 != 0 {
		t.Errorf("second sweepOnce = (%d, %v), want (0, nil) — should be idempotent", n2, err)
	}
}

// TestArchiveSessionsOnce covers the RFC AV Phase 2b2 aged-SESSION archiver in
// both modes: "prune" cascade-deletes an aged all-terminal session (its runs +
// events); "export+prune" first writes the session JSON to the export dir, then
// deletes. A session with a still-running run is NEVER pruned — the fix-4
// regression: pruning per-run would corrupt a continued session's whole-session
// transcript replay. The clock is pinned into the future so a just-completed
// session lands past the cutoff.
func TestArchiveSessionsOnce(t *testing.T) {
	ctx := context.Background()
	// Pin now well past completed_at so cutoff = now-24h > completed_at.
	future := func() time.Time { return time.Now().Add(48 * time.Hour) }

	// seedCompletedSession makes a session with one completed run (with events).
	seedCompletedSession := func(st *sqlite.Store, agentID string) (sessID, runID string) {
		sess, err := st.CreateSession(ctx, "t", "default", "")
		if err != nil {
			t.Fatal(err)
		}
		run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: agentID})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AppendEvent(ctx, run.ID, "text", []byte(`{"t":"hi"}`)); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishRun(ctx, run.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
			t.Fatal(err)
		}
		return sess.ID, run.ID
	}

	t.Run("prune", func(t *testing.T) {
		st := newTestStore(t)
		sessID, runID := seedCompletedSession(st, "prune-a")
		sw := New(st, Config{
			RunRetention:     24 * time.Hour,
			RunRetentionMode: "prune",
			Logger:           func(string, ...any) {},
			Now:              future,
		})
		if !sw.runArchivalEnabled() {
			t.Fatal("prune mode should be enabled")
		}
		n, err := sw.archiveSessionsOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("archiveSessionsOnce = (%d, %v), want (1, nil)", n, err)
		}
		if _, err := st.GetRun(ctx, runID); err == nil {
			t.Errorf("run survived prune")
		}
		if _, err := st.GetSession(ctx, sessID); err == nil {
			t.Errorf("session survived prune")
		}
	})

	t.Run("export+prune", func(t *testing.T) {
		st := newTestStore(t)
		exportDir := t.TempDir()
		sessID, runID := seedCompletedSession(st, "export-a")
		sw := New(st, Config{
			RunRetention:     24 * time.Hour,
			RunRetentionMode: "export+prune",
			ExportDir:        exportDir,
			Logger:           func(string, ...any) {},
			Now:              future,
		})
		n, err := sw.archiveSessionsOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("archiveSessionsOnce = (%d, %v), want (1, nil)", n, err)
		}
		if _, err := st.GetSession(ctx, sessID); err == nil {
			t.Errorf("session survived export+prune")
		}
		// A JSON export exists under a per-day subdir named for the session and
		// carries session_id + runs + events.
		matches, _ := filepath.Glob(filepath.Join(exportDir, "*", sessID+".json"))
		if len(matches) != 1 {
			t.Fatalf("export file glob = %v, want exactly one %s.json", matches, sessID)
		}
		blob, err := os.ReadFile(matches[0])
		if err != nil ||
			!bytes.Contains(blob, []byte(sessID)) ||
			!bytes.Contains(blob, []byte(runID)) ||
			!bytes.Contains(blob, []byte(`"runs"`)) ||
			!bytes.Contains(blob, []byte(`"events"`)) {
			t.Errorf("export file missing session_id / run id / runs / events: err=%v", err)
		}
	})

	// fix-4 regression: a session with a still-running run must NOT be pruned,
	// even if it also holds an aged completed run. Pruning per-run would leave
	// the running run's continuation replay reading a broken transcript.
	t.Run("session with running run is not pruned", func(t *testing.T) {
		st := newTestStore(t)
		sess, _ := st.CreateSession(ctx, "t", "default", "")
		done, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "mixed-done"})
		running, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "mixed-running"}) // stays running
		if err := st.FinishRun(ctx, done.ID, store.RunCompleted, "end_turn", store.Usage{Model: "m", Provider: "p"}, ""); err != nil {
			t.Fatal(err)
		}
		sw := New(st, Config{
			RunRetention:     24 * time.Hour,
			RunRetentionMode: "prune",
			Logger:           func(string, ...any) {},
			Now:              future,
		})
		n, err := sw.archiveSessionsOnce(ctx)
		if err != nil || n != 0 {
			t.Fatalf("archiveSessionsOnce = (%d, %v), want (0, nil) — session has a running run", n, err)
		}
		if _, err := st.GetRun(ctx, done.ID); err != nil {
			t.Errorf("completed run in a still-active session was pruned: %v", err)
		}
		if _, err := st.GetRun(ctx, running.ID); err != nil {
			t.Errorf("running run was pruned: %v", err)
		}
	})

	t.Run("export+prune without dir is disabled", func(t *testing.T) {
		st := newTestStore(t)
		sw := New(st, Config{
			RunRetention:     24 * time.Hour,
			RunRetentionMode: "export+prune", // no ExportDir
			Logger:           func(string, ...any) {},
			Now:              future,
		})
		if sw.runArchivalEnabled() {
			t.Error("export+prune with no ExportDir must be disabled (never delete un-exported)")
		}
	})
}

// TestSweeperRun_StopsOnContextDone asserts the goroutine exits cleanly
// when its context is cancelled, so shutdown doesn't leak the sweeper
// goroutine past the Store's close.
func TestSweeperRun_StopsOnContextDone(t *testing.T) {
	st := newTestStore(t)
	sw := New(st, Config{
		Interval:        10 * time.Millisecond,
		DetailRetention: 1 * time.Hour,
		Logger:          func(format string, args ...any) {},
	})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(done)
	}()

	// Let the sweeper run a few ticks, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// goroutine exited cleanly
	case <-time.After(2 * time.Second):
		t.Fatal("sweeper did not exit within 2s of ctx cancellation")
	}
}

// TestSweeperRun_NilStoreNoOp asserts Run returns immediately when the
// Store is nil — relevant for callers that construct the Sweeper
// unconditionally and let nil flow through.
func TestSweeperRun_NilStoreNoOp(t *testing.T) {
	sw := New(nil, Config{
		Interval:        1 * time.Millisecond,
		DetailRetention: 1 * time.Millisecond,
		Logger:          func(format string, args ...any) {},
	})
	done := make(chan struct{})
	go func() {
		sw.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
		// returned immediately
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run with nil store should be a no-op; instead it blocked")
	}
}
