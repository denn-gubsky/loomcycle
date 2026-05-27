package http

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// delayingHeartbeatStore wraps a real store.Store but injects a
// configurable sleep into UpdateHeartbeat — exercises the path where
// the pool acquire would block under burst saturation. Atomic int64
// for thread-safe delay updates from concurrent test goroutines.
type delayingHeartbeatStore struct {
	store.Store
	delayMS atomic.Int64 // milliseconds to sleep before delegating
}

func (s *delayingHeartbeatStore) UpdateHeartbeat(ctx context.Context, runID string) error {
	d := time.Duration(s.delayMS.Load()) * time.Millisecond
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.UpdateHeartbeat(ctx, runID)
}

// makeHeartbeatTestServer constructs a Server with a delaying store
// wrapped around an in-memory sqlite. Returns the server, the
// delay-control hook, and a cleanup. The caller must invoke heartbeat
// with a valid runID; we create one off a session for them.
func makeHeartbeatTestServer(t *testing.T) (*Server, *delayingHeartbeatStore, string, func()) {
	t.Helper()
	inner, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	ctx := context.Background()
	sess, err := inner.CreateSession(ctx, "tenant", "agent", "")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := inner.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "hb-test"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	delayed := &delayingHeartbeatStore{Store: inner}
	srv := &Server{store: delayed}
	cleanup := func() { _ = inner.Close() }
	return srv, delayed, run.ID, cleanup
}

// captureLog redirects log output to a buffer for the duration of the
// test. Returns a snapshot accessor + a restore closure.
func captureLog(t *testing.T) (func() string, func()) {
	t.Helper()
	var buf strings.Builder
	prev := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	return func() string { return buf.String() }, func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	}
}

// TestMakeHeartbeat_TolerantOfPoolSaturation — the canonical x5000
// scenario reproduction. At the launch crest, store.UpdateHeartbeat
// can spend up to several seconds waiting for a pgxpool acquire.
// The prior 1-second timeout fired and produced operator-visible
// noise on otherwise-healthy runs. The new 5-second budget rides
// out the natural acquire jitter; a 2-second store delay must NOT
// trip the deadline.
func TestMakeHeartbeat_TolerantOfPoolSaturation(t *testing.T) {
	srv, delayed, runID, cleanup := makeHeartbeatTestServer(t)
	defer cleanup()
	logOf, restore := captureLog(t)
	defer restore()

	delayed.delayMS.Store(2000) // 2 s — well under the 5 s budget

	hb := srv.makeHeartbeat(runID)
	if hb == nil {
		t.Fatal("makeHeartbeat returned nil")
	}
	start := time.Now()
	hb()
	elapsed := time.Since(start)

	if elapsed < 2*time.Second {
		t.Errorf("heartbeat returned too fast: %v — store delay should have applied", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("heartbeat took too long: %v — should complete around the 2 s delay", elapsed)
	}
	if got := logOf(); strings.Contains(got, "UpdateHeartbeat") {
		t.Errorf("unexpected log output (deadline shouldn't have fired): %q", got)
	}
}

// TestMakeHeartbeat_LogsOnDeadlineExceeded — when the store delay
// exceeds the budget (real pool starvation), the closure logs the
// failure and returns silently. The run must NOT see the failure.
func TestMakeHeartbeat_LogsOnDeadlineExceeded(t *testing.T) {
	srv, delayed, runID, cleanup := makeHeartbeatTestServer(t)
	defer cleanup()
	logOf, restore := captureLog(t)
	defer restore()

	// 7 s delay > the 5 s budget — deadline will fire.
	delayed.delayMS.Store(7000)

	hb := srv.makeHeartbeat(runID)
	start := time.Now()
	hb() // must not panic; must not propagate the error
	elapsed := time.Since(start)

	// Should return shortly after the 5 s deadline, not the 7 s store delay.
	if elapsed < 5*time.Second {
		t.Errorf("heartbeat returned too fast: %v — deadline should have fired at ~5 s", elapsed)
	}
	if elapsed > 6*time.Second {
		t.Errorf("heartbeat took too long: %v — deadline should fire near the 5 s budget", elapsed)
	}
	if got := logOf(); !strings.Contains(got, "UpdateHeartbeat") {
		t.Errorf("expected timeout log line, got: %q", got)
	}
	if got := logOf(); !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("expected deadline-exceeded reason in log, got: %q", got)
	}
}

// TestMakeHeartbeat_NoOpWhenStoreNil — the documented no-op path for
// store-less deployments.
func TestMakeHeartbeat_NoOpWhenStoreNil(t *testing.T) {
	srv := &Server{}
	if got := srv.makeHeartbeat("run-id"); got != nil {
		t.Errorf("nil-store makeHeartbeat returned non-nil closure")
	}
}

// TestMakeHeartbeat_NoOpWhenRunIDEmpty — sub-agent spawn path passes
// "" sometimes; must return nil rather than panic.
func TestMakeHeartbeat_NoOpWhenRunIDEmpty(t *testing.T) {
	_, _, _, cleanup := makeHeartbeatTestServer(t)
	defer cleanup()
	srv, _, _, _ := makeHeartbeatTestServer(t)
	if got := srv.makeHeartbeat(""); got != nil {
		t.Errorf("empty-runID makeHeartbeat returned non-nil closure")
	}
}

// TestMakeHeartbeat_PropagatesNotFoundSilently — the store returns
// store.ErrNotFound for a stale run_id (sweeper already collected
// it); the heartbeat closure logs but continues without panic. Sanity
// that the existing log-only error handling survives the timeout
// bump.
func TestMakeHeartbeat_PropagatesNotFoundSilently(t *testing.T) {
	srv, _, _, cleanup := makeHeartbeatTestServer(t)
	defer cleanup()
	logOf, restore := captureLog(t)
	defer restore()

	hb := srv.makeHeartbeat("r_does_not_exist")
	if hb == nil {
		t.Fatal("makeHeartbeat returned nil for non-empty runID")
	}
	hb() // must complete without panic
	// SQLite's UpdateHeartbeat is a no-op UPDATE for missing rows
	// (status='running' guard excludes the row from the update set
	// rather than erroring), so the log may be empty here. If a
	// future store change DOES return ErrNotFound, we still want
	// the closure to log + return.
	_ = logOf()
	_ = errors.New // keep import used
}
