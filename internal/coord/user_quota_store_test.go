package coord

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// freshUserQuotasPool returns a pgxpool that the test owns and that
// is guaranteed to have the `user_quotas` table (migrations applied).
// Each test seeds and tears down its own user_id rows; we never
// TRUNCATE because other test packages may share the same database.
func freshUserQuotasPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial pg: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func uniqueUserID(t *testing.T) string {
	t.Helper()
	return "test-uq-" + t.Name() + "-" + time.Now().Format("150405.000000")
}

func TestUserQuotaStore_AcquireReleaseLifecycle(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	s := NewUserQuotaStore(pool)
	user := uniqueUserID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	ctx := context.Background()

	ok, err := s.TryAcquire(ctx, "", user, 2)
	if err != nil || !ok {
		t.Fatalf("first TryAcquire: ok=%v err=%v", ok, err)
	}
	ok, err = s.TryAcquire(ctx, "", user, 2)
	if err != nil || !ok {
		t.Fatalf("second TryAcquire: ok=%v err=%v", ok, err)
	}

	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := snap[user]; got != 2 {
		t.Errorf("snapshot[%s] = %d, want 2", user, got)
	}

	s.Release(ctx, "", user)
	s.Release(ctx, "", user)

	snap2, _ := s.Snapshot(ctx)
	if _, present := snap2[user]; present {
		t.Errorf("snapshot still shows %s after full release: %v", user, snap2)
	}
}

func TestUserQuotaStore_AtCapRejects(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	s := NewUserQuotaStore(pool)
	user := uniqueUserID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	ctx := context.Background()

	// Fill the cap (cap=3).
	for i := 0; i < 3; i++ {
		ok, err := s.TryAcquire(ctx, "", user, 3)
		if err != nil || !ok {
			t.Fatalf("acquire #%d: ok=%v err=%v", i, ok, err)
		}
	}

	// At-cap: ok=false, err=nil (caller maps to ErrPerUserQuotaExhausted).
	ok, err := s.TryAcquire(ctx, "", user, 3)
	if err != nil {
		t.Errorf("at-cap should return nil error, got %v", err)
	}
	if ok {
		t.Errorf("at-cap should return ok=false, got true")
	}
}

func TestUserQuotaStore_FirstAcquireCreatesRow(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	s := NewUserQuotaStore(pool)
	user := uniqueUserID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	ctx := context.Background()

	// Snapshot pre-acquire: user should not be present.
	snap, _ := s.Snapshot(ctx)
	if _, present := snap[user]; present {
		t.Fatalf("user %s present in snapshot before any acquire: %v", user, snap)
	}

	ok, err := s.TryAcquire(ctx, "", user, 5)
	if err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	snap, _ = s.Snapshot(ctx)
	if got := snap[user]; got != 1 {
		t.Errorf("after first acquire: count=%d, want 1", got)
	}
}

func TestUserQuotaStore_ReleaseUnderflowIsNoop(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	s := NewUserQuotaStore(pool)
	user := uniqueUserID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	ctx := context.Background()

	ok, err := s.TryAcquire(ctx, "", user, 1)
	if err != nil || !ok {
		t.Fatalf("acquire: ok=%v err=%v", ok, err)
	}
	s.Release(ctx, "", user)
	// Second Release should be a silent no-op (logs a warning).
	s.Release(ctx, "", user)

	// Check the row went to 0 and stays there (no negative count, no
	// CHECK constraint violation).
	snap, _ := s.Snapshot(ctx)
	if _, present := snap[user]; present {
		t.Errorf("user %s should be absent from snapshot at count=0, got %v", user, snap[user])
	}
}

func TestUserQuotaStore_SnapshotExcludesZeroCounts(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	s := NewUserQuotaStore(pool)
	user := uniqueUserID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	ctx := context.Background()

	// Acquire-and-release leaves the row at count=0; snapshot must exclude.
	_, _ = s.TryAcquire(ctx, "", user, 5)
	s.Release(ctx, "", user)

	snap, _ := s.Snapshot(ctx)
	if _, present := snap[user]; present {
		t.Errorf("user %s with active_count=0 should be excluded from Snapshot, got %v", user, snap)
	}
}

func TestUserQuotaStore_TwoReplicasConcurrent(t *testing.T) {
	// Two backplane consumers backed by SEPARATE pgxpool instances,
	// simulating two replicas with distinct TCP sessions to Postgres.
	// The cross-process serialization guarantee for user_quotas comes
	// from the DB-side atomicity of `INSERT ON CONFLICT DO UPDATE
	// WHERE active_count < cap`, NOT from any client-side coordination
	// — using one pool would let pgxpool's per-connection serialization
	// mask wire-level races (review-1 finding #1).
	dsn := pgDSNFromEnv(t)
	poolA, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial poolA: %v", err)
	}
	t.Cleanup(poolA.Close)
	poolB, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("dial poolB: %v", err)
	}
	t.Cleanup(poolB.Close)

	sA := NewUserQuotaStore(poolA)
	sB := NewUserQuotaStore(poolB)
	user := uniqueUserID(t)
	t.Cleanup(func() {
		_, _ = poolA.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	const cap = 3
	const attempts = 10

	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		store := sA
		if i%2 == 0 {
			store = sB
		}
		go func() {
			defer wg.Done()
			ok, err := store.TryAcquire(context.Background(), "", user, cap)
			if err != nil {
				t.Errorf("acquire err: %v", err)
				return
			}
			if ok {
				successes.Add(1)
			}
		}()
	}
	wg.Wait()

	if int(successes.Load()) != cap {
		t.Errorf("got %d successful acquires, want exactly %d", successes.Load(), cap)
	}
	snap, _ := sA.Snapshot(context.Background())
	if got := snap[user]; got != cap {
		t.Errorf("snapshot[%s] = %d, want %d", user, got, cap)
	}
}

func TestUserQuotaStore_InvalidCapRejected(t *testing.T) {
	// Defensive: cap == 0 should never be passed (caller's perUserActive
	// gate prevents it), but if it is, TryAcquire must error rather
	// than silently underflow the upsert WHERE clause.
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	s := NewUserQuotaStore(pool)
	user := uniqueUserID(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	_, err := s.TryAcquire(context.Background(), "", user, 0)
	if err == nil {
		t.Error("TryAcquire with cap=0 should return error")
	}
	_, err = s.TryAcquire(context.Background(), "", "", 5)
	if err == nil {
		t.Error("TryAcquire with empty userID should return error")
	}
}

// TestUserQuotaStore_TenantIsolation pins the (tenant_id, user_id) keying
// (migration 0067): the same subject in two tenants holds INDEPENDENT caps,
// and a release in one tenant leaves the other tenant's counter untouched.
// Against the prior bare-user_id keying t2's acquire would have incremented
// the same row and been refused at cap — so this test fails there and passes
// now.
func TestUserQuotaStore_TenantIsolation(t *testing.T) {
	pool := freshUserQuotasPool(t, pgDSNFromEnv(t))
	s := NewUserQuotaStore(pool)
	user := uniqueUserID(t)
	// The (tenant_id, user_id) PK makes t1 and t2 distinct rows; user_id is
	// unique to this test, so deleting all its rows cleans up both tenants.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM user_quotas WHERE user_id = $1`, user)
	})
	ctx := context.Background()

	// t1's user takes its single slot (cap=1).
	ok, err := s.TryAcquire(ctx, "t1", user, 1)
	if err != nil || !ok {
		t.Fatalf("t1 first TryAcquire: ok=%v err=%v", ok, err)
	}

	// t2's user — same subject, different tenant — is independent: also true.
	ok, err = s.TryAcquire(ctx, "t2", user, 1)
	if err != nil || !ok {
		t.Fatalf("t2 TryAcquire (independent tenant): ok=%v err=%v", ok, err)
	}

	// A second t1 acquire is refused — t1's user is at cap.
	ok, err = s.TryAcquire(ctx, "t1", user, 1)
	if err != nil {
		t.Errorf("second t1 TryAcquire: unexpected err %v", err)
	}
	if ok {
		t.Errorf("second t1 TryAcquire: ok=true, want false (t1 at cap)")
	}

	// Releasing t1 frees t1's slot; a subsequent t1 acquire succeeds again.
	s.Release(ctx, "t1", user)
	ok, err = s.TryAcquire(ctx, "t1", user, 1)
	if err != nil || !ok {
		t.Fatalf("t1 re-acquire after release: ok=%v err=%v", ok, err)
	}

	// t2's counter was untouched by t1's release — still at 1.
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := snap[FairnessDisplayKey("t2", user)]; got != 1 {
		t.Errorf("t2 count = %d, want 1 (untouched by t1 release)", got)
	}
	if got := snap[FairnessDisplayKey("t1", user)]; got != 1 {
		t.Errorf("t1 count = %d, want 1 (re-acquired)", got)
	}
}
