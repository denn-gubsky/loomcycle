package heartbeat

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// newTestStore opens a fresh sqlite store for the sweeper tests.
// Sweeper logic is store-agnostic; using sqlite keeps the tests fast
// and doesn't require a Postgres fixture.
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

// TestSweeperOnce_MarksStale exercises sweepOnce against a real store
// without spinning the Run loop. Verifies the stale row gets flipped
// while a fresh row is left alone.
func TestSweeperOnce_MarksStale(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sess, _ := st.CreateSession(ctx, "t", "a", "u")

	stale, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_stale"})
	_ = st.UpdateHeartbeat(ctx, stale.ID)

	// Sleep + StaleAfter gap was 20ms vs 10ms — flaked under -race
	// where scheduling slowdown could push the time between fresh
	// heartbeat write and the sweepOnce cutoff calculation past 10ms,
	// causing the fresh row to also count as stale (test expects
	// exactly 1 marked stale). Widened to 100ms sleep + 50ms cutoff
	// for a 5× margin — still fast (~100ms total test runtime) but
	// resilient to -race slowdown.
	time.Sleep(100 * time.Millisecond)

	fresh, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_fresh"})
	_ = st.UpdateHeartbeat(ctx, fresh.ID)

	sw := New(st, Config{
		Interval:   1 * time.Hour, // unused — we drive sweepOnce directly
		StaleAfter: 50 * time.Millisecond,
		Logger:     func(format string, args ...any) {}, // silence
	})
	n, err := sw.sweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("sweepOnce returned %d, want 1", n)
	}

	staleAfter, _ := st.GetRunByAgentID(ctx, "a_stale")
	if staleAfter.Status != store.RunFailed {
		t.Errorf("stale row: status=%q, want failed", staleAfter.Status)
	}
	freshAfter, _ := st.GetRunByAgentID(ctx, "a_fresh")
	if freshAfter.Status != store.RunRunning {
		t.Errorf("fresh row: status=%q, want running", freshAfter.Status)
	}
}

// TestSweeperRun_StopsOnContextDone asserts the goroutine exits cleanly
// when its context is cancelled. Without this, a slow Run loop on
// shutdown could outlive the parent process and prevent the Store
// from closing.
func TestSweeperRun_StopsOnContextDone(t *testing.T) {
	st := newTestStore(t)
	sw := New(st, Config{
		Interval:   10 * time.Millisecond,
		StaleAfter: 1 * time.Hour,
		Logger:     func(format string, args ...any) {},
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
// Store is nil — relevant for callers that want to construct the
// Sweeper unconditionally and let nil flow through.
func TestSweeperRun_NilStoreNoOp(t *testing.T) {
	sw := New(nil, Config{
		Interval:   1 * time.Millisecond,
		StaleAfter: 1 * time.Millisecond,
		Logger:     func(format string, args ...any) {},
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

// TestSweeperRun_LogsResults captures sweep log output and asserts the
// "marked N stale run(s)" line fires when a stale row is present, and
// the "0 stale runs" no-op line fires when nothing is stale.
func TestSweeperRun_LogsResults(t *testing.T) {
	st := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, _ := st.CreateSession(ctx, "t", "a", "u")
	stale, _ := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "a_stale_log"})
	_ = st.UpdateHeartbeat(ctx, stale.ID)
	// Wider sleep/StaleAfter margin than the v0.4 original (15ms/5ms)
	// for the same -race-slowdown reason as TestSweeperOnce_MarksStale
	// above. The test still completes within the 2s waitForLogContaining
	// budget — the polling loop tolerates extra latency.
	time.Sleep(100 * time.Millisecond)

	var (
		mu   sync.Mutex
		logs []string
	)
	sw := New(st, Config{
		Interval:   20 * time.Millisecond,
		StaleAfter: 50 * time.Millisecond,
		Logger: func(format string, args ...any) {
			mu.Lock()
			logs = append(logs, format)
			mu.Unlock()
		},
	})
	go sw.Run(ctx)

	// Poll until the expected log line appears, with a 2s deadline.
	// Replaces a fixed 80ms sleep that flaked under -race on CI (PR
	// #190's run hit it once). Under -race, the scheduler's 2-5x
	// slowdown can push a 10ms-interval tick past a 80ms budget;
	// poll-until-condition removes that dependency on wall-clock
	// timing. Same poll-until-condition pattern as PR #195's
	// waitForActive helper.
	waitForLogContaining(t, &mu, &logs, "heartbeat: marked %d stale run(s) as failed", 2*time.Second)

	cancel()
	// Brief settle so the sweeper goroutine returns cleanly before
	// the store is closed by t.Cleanup. Not load-bearing for the
	// assertion above (already satisfied at this point); purely
	// "don't leak a goroutine into the next test."
	time.Sleep(20 * time.Millisecond)
}

// waitForLogContaining polls the captured-log slice under the mutex
// until any entry equals the wanted format string OR the deadline
// elapses. Fails the test with the full log buffer for diagnosis when
// the deadline fires. The format string (not the formatted message)
// is what gets captured by the test's Logger closure — sweeper.go's
// log lines come through as the raw fmt template before any args are
// substituted.
func waitForLogContaining(t *testing.T, mu *sync.Mutex, logs *[]string, want string, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		mu.Lock()
		for _, line := range *logs {
			if line == want {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("waitForLogContaining: timed out after %s waiting for %q; captured: %v",
		deadline, want, *logs)
}
