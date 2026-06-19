package sqlmem

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestGC_SweepStaleSqlite: a durable scope whose .db mtime is older than the
// cutoff is dropped; a fresh durable scope survives; the run scope is never
// swept even when stale.
func TestGC_SweepStaleSqlite(t *testing.T) {
	m := newTestManager(t, Config{ScopeTTLMS: 3600_000}) // GC on; the 30-min tick won't fire in this test
	ctx := context.Background()
	stale := agentKey("t1", "stale-agent")
	fresh := agentKey("t1", "fresh-agent")
	run := ScopeKey{Scope: "run", ScopeID: "run-keep"}
	for _, k := range []ScopeKey{stale, fresh, run} {
		if _, err := m.Exec(ctx, k, "CREATE TABLE t (x INT)", nil, 0); err != nil {
			t.Fatalf("create %s/%s: %v", k.Scope, k.ScopeID, err)
		}
	}
	root := managerRoot(t, m)
	old := time.Now().Add(-2 * time.Hour)
	stalePath, _ := stale.keyPath(root)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}
	runPath, _ := run.keyPath(root)
	_ = os.Chtimes(runPath, old, old) // backdate run too — it must STILL survive

	dropped, err := m.backend.sweepStale(time.Now().Add(-1 * time.Hour))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped=%d, want 1 (only the stale durable scope)", dropped)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatal("stale scope .db still exists after GC")
	}
	freshPath, _ := fresh.keyPath(root)
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh scope .db was removed: %v", err)
	}
	if _, err := os.Stat(runPath); err != nil {
		t.Fatal("run scope .db was removed — run scopes must NOT be GC'd")
	}
}

// TestGC_SkipsInUseScope regresses the live-scope race: a durable scope with an
// OPEN transaction is pinned (inUse>0) and must NOT be swept even when stale;
// once the transaction finishes the scope is reclaimable.
func TestGC_SkipsInUseScope(t *testing.T) {
	m := newTestManager(t, Config{ScopeTTLMS: 3600_000})
	ctx := context.Background()
	key := agentKey("t1", "busy")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	root := managerRoot(t, m)
	path, _ := key.keyPath(root)
	old := time.Now().Add(-2 * time.Hour)

	// Open a transaction → pins the scope handle (inUse>0); backdate AFTER begin
	// (BeginTxn touches the scope forward).
	txnID := agentTxnID("run1", "busy")
	if err := m.BeginTxn(ctx, txnID, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	_ = os.Chtimes(path, old, old)
	if dropped, err := m.backend.sweepStale(time.Now().Add(-time.Hour)); err != nil || dropped != 0 {
		t.Fatalf("sweep with open txn: dropped=%d err=%v, want 0/nil (scope in use)", dropped, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("in-use scope .db was removed by GC")
	}

	// Finish the txn → inUse drops → now reclaimable.
	if err := m.RollbackTxn(txnID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	_ = os.Chtimes(path, old, old)
	if dropped, err := m.backend.sweepStale(time.Now().Add(-time.Hour)); err != nil || dropped != 1 {
		t.Fatalf("sweep after txn: dropped=%d err=%v, want 1/nil", dropped, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("idle stale scope .db still exists after GC")
	}
}

// TestGC_TouchUpdatesMtime: a Query touches the durable scope's .db mtime
// forward (so a read-only-but-live scope isn't GC'd).
func TestGC_TouchUpdatesMtime(t *testing.T) {
	m := newTestManager(t, Config{ScopeTTLMS: 3600_000})
	ctx := context.Background()
	key := agentKey("t1", "touch")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	root := managerRoot(t, m)
	path, _ := key.keyPath(root)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(path, old, old)
	// Clear the debounce so the next op's touch actually fires.
	m.touchMu.Lock()
	m.lastTouch = map[string]time.Time{}
	m.touchMu.Unlock()

	if _, err := m.Query(ctx, key, "SELECT count(*) FROM t", nil); err != nil {
		t.Fatalf("query: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.ModTime().Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("read did not touch the scope mtime forward (still %v)", info.ModTime())
	}
}

// TestGC_OffNoTouch: with GC off (ScopeTTLMS=0), touch is a no-op.
func TestGC_OffNoTouch(t *testing.T) {
	m := newTestManager(t, Config{}) // GC off
	ctx := context.Background()
	key := agentKey("t1", "noop")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	root := managerRoot(t, m)
	path, _ := key.keyPath(root)
	old := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(path, old, old)
	m.touch(key) // no-op when GC is off
	info, _ := os.Stat(path)
	if info.ModTime().After(old.Add(time.Minute)) {
		t.Fatal("touch updated the mtime with GC off")
	}
}
