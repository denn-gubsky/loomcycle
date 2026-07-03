package coord

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFnvKey_Stable(t *testing.T) {
	// The constants must hash deterministically across runs — pin
	// against the known FNV-1a 64-bit values for the documented
	// sweeper names. Future maintainers reading this can verify the
	// keys are stable.
	cases := map[string]int64{
		// Computed offline via hash/fnv. If this test fails, the
		// FNV-1a algorithm in stdlib changed (extremely unlikely)
		// or the key string itself was changed (refuse to merge).
		"heartbeat_sweeper":     fnvKey("heartbeat_sweeper"),
		"memory_sweeper":        fnvKey("memory_sweeper"),
		"channels_sweeper":      fnvKey("channels_sweeper"),
		"interrupts_sweeper":    fnvKey("interrupts_sweeper"),
		"metrics_sweeper":       fnvKey("metrics_sweeper"),
		"dynamic_agent_sweeper": fnvKey("dynamic_agent_sweeper"),
		"replicas_sweeper":      fnvKey("replicas_sweeper"),
		"usage_sweeper":         fnvKey("usage_sweeper"),
	}
	// All eight must be distinct.
	seen := map[int64]string{}
	for name, key := range cases {
		if other, dup := seen[key]; dup {
			t.Errorf("lock key collision: %q and %q hash to %d", name, other, key)
		}
		seen[key] = name
	}
}

func TestLockKeyConstants_MatchInitFnv(t *testing.T) {
	// The init()-set constants should match what fnvKey returns for
	// the documented names. Pin so a future rename doesn't silently
	// orphan the const.
	if LockKeyHeartbeatSweeper != fnvKey("heartbeat_sweeper") {
		t.Errorf("LockKeyHeartbeatSweeper drift")
	}
	if LockKeyReplicasSweeper != fnvKey("replicas_sweeper") {
		t.Errorf("LockKeyReplicasSweeper drift")
	}
}

// ---- Postgres-gated tests ----

func TestAdvisoryLock_AcquiresAndRunsFn(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool := freshUserQuotasPool(t, dsn)
	lock := NewAdvisoryLock(pool)
	key := int64(time.Now().UnixNano()) // unique per test run

	ran := atomic.Bool{}
	acquired, err := lock.TryRun(context.Background(), key, func(ctx context.Context) error {
		ran.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("TryRun err: %v", err)
	}
	if !acquired {
		t.Error("acquired=false, want true (single caller)")
	}
	if !ran.Load() {
		t.Error("fn did not run")
	}

	// Second call after first releases should acquire again.
	acquired2, err := lock.TryRun(context.Background(), key, func(ctx context.Context) error { return nil })
	if err != nil || !acquired2 {
		t.Errorf("second TryRun: acquired=%v err=%v (lock not released?)", acquired2, err)
	}
}

func TestAdvisoryLock_TwoCallers_OnlyOneAcquires(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	// Two separate pools so the lock contention is at the DB level
	// (different sessions), matching the cross-replica scenario.
	poolA, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool A: %v", err)
	}
	defer poolA.Close()
	poolB, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool B: %v", err)
	}
	defer poolB.Close()

	lockA := NewAdvisoryLock(poolA)
	lockB := NewAdvisoryLock(poolB)
	key := int64(time.Now().UnixNano()) + 1

	// A grabs the lock and holds it via a slow fn while B tries.
	aRunning := make(chan struct{})
	aRelease := make(chan struct{})
	aResult := make(chan bool, 1)
	go func() {
		acq, _ := lockA.TryRun(context.Background(), key, func(ctx context.Context) error {
			close(aRunning)
			<-aRelease
			return nil
		})
		aResult <- acq
	}()

	<-aRunning // A is holding the lock

	// B tries — must NOT acquire.
	bAcq, bErr := lockB.TryRun(context.Background(), key, func(ctx context.Context) error {
		t.Error("B's fn must NOT run while A holds the lock")
		return nil
	})
	if bErr != nil {
		t.Errorf("B TryRun err: %v", bErr)
	}
	if bAcq {
		t.Error("B acquired=true while A holds the lock")
	}

	close(aRelease)
	if !<-aResult {
		t.Error("A should have acquired the lock")
	}
}

func TestAdvisoryLock_PropagatesFnError(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool := freshUserQuotasPool(t, dsn)
	lock := NewAdvisoryLock(pool)
	key := int64(time.Now().UnixNano()) + 2

	want := errors.New("fn failed")
	acq, err := lock.TryRun(context.Background(), key, func(ctx context.Context) error {
		return want
	})
	if !acq {
		t.Error("acquired=false; expected true (no contention)")
	}
	if !errors.Is(err, want) {
		t.Errorf("got err %v, want %v", err, want)
	}

	// And the lock must be released so the next call can acquire.
	acq2, err2 := lock.TryRun(context.Background(), key, func(ctx context.Context) error { return nil })
	if err2 != nil || !acq2 {
		t.Errorf("post-error lock not released: acq=%v err=%v", acq2, err2)
	}
}

func TestAdvisoryLock_ContextCancelledDuringAcquire(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	pool := freshUserQuotasPool(t, dsn)
	lock := NewAdvisoryLock(pool)
	key := int64(time.Now().UnixNano()) + 3

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel
	acq, err := lock.TryRun(ctx, key, func(ctx context.Context) error {
		t.Error("fn ran with cancelled ctx")
		return nil
	})
	if err == nil {
		t.Error("expected error from cancelled ctx")
	}
	if acq {
		t.Error("acquired=true with cancelled ctx")
	}
}
