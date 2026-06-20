package sqlmem

import (
	"context"
	"os"
	"strings"
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
	if _, err := m.BeginTxn(ctx, txnID, "run1", key); err != nil {
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
	if _, err := m.RollbackTxn(txnID); err != nil {
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

// fillScope writes rows*~3KB into scope key's table (created if needed), making
// its on-disk footprint comfortably exceed a sub-megabyte budget.
func fillScope(t *testing.T, m *Manager, key ScopeKey, rows int) {
	t.Helper()
	ctx := context.Background()
	if _, err := m.Exec(ctx, key, "CREATE TABLE IF NOT EXISTS t (x TEXT)", nil, 0); err != nil {
		t.Fatalf("create %s/%s: %v", key.Scope, key.ScopeID, err)
	}
	blob := strings.Repeat("x", 3000)
	for i := 0; i < rows; i++ {
		if _, err := m.Exec(ctx, key, "INSERT INTO t VALUES (?)", []any{blob}, 0); err != nil {
			t.Fatalf("fill %s/%s: %v", key.Scope, key.ScopeID, err)
		}
	}
}

// TestGC_SweepBudgetSqlite: the size sweep drops the LARGEST idle durable scope
// to get under the aggregate budget, leaves the small scopes alone, and never
// touches the run scope even when it is large.
func TestGC_SweepBudgetSqlite(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	big := agentKey("t1", "big")
	small1 := agentKey("t1", "small1")
	small2 := agentKey("t2", "small2")
	run := ScopeKey{Scope: "run", ScopeID: "run-big"}
	for _, k := range []ScopeKey{small1, small2} {
		if _, err := m.Exec(ctx, k, "CREATE TABLE t (x TEXT)", nil, 0); err != nil {
			t.Fatalf("create %s: %v", k.ScopeID, err)
		}
	}
	fillScope(t, m, big, 500) // ~1.5 MB
	fillScope(t, m, run, 500) // large too — but a run scope is never swept

	const budget = 300 * 1024 // 300 KB: well below `big`, well above the smalls
	dropped, err := m.backend.sweepBudget(budget)
	if err != nil {
		t.Fatalf("sweepBudget: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped=%d, want 1 (only the largest durable scope)", dropped)
	}
	root := managerRoot(t, m)
	bigPath, _ := big.keyPath(root)
	if _, err := os.Stat(bigPath); !os.IsNotExist(err) {
		t.Fatal("largest scope was not dropped by the size sweep")
	}
	for _, k := range []ScopeKey{small1, small2} {
		p, _ := k.keyPath(root)
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("small scope %s dropped by size sweep: %v", k.ScopeID, err)
		}
	}
	runPath, _ := run.keyPath(root)
	if _, err := os.Stat(runPath); err != nil {
		t.Fatal("run scope dropped by size GC — run scopes must NEVER be swept")
	}
}

// TestGC_SweepBudgetSkipsInUse: the size sweep never drops an in-use scope even
// when it is over budget; it drops the largest IDLE scope instead.
func TestGC_SweepBudgetSkipsInUse(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	busy := agentKey("t1", "busy-big")
	idle := agentKey("t1", "idle-big")
	fillScope(t, m, busy, 500)
	fillScope(t, m, idle, 500)

	// Pin `busy` with an open transaction (inUse>0).
	txnID := agentTxnID("run1", "busy-big")
	if _, err := m.BeginTxn(ctx, txnID, "run1", busy); err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _, _ = m.RollbackTxn(txnID) }()

	dropped, err := m.backend.sweepBudget(300 * 1024)
	if err != nil {
		t.Fatalf("sweepBudget: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped=%d, want 1 (the idle scope; the in-use one is skipped)", dropped)
	}
	root := managerRoot(t, m)
	busyPath, _ := busy.keyPath(root)
	if _, err := os.Stat(busyPath); err != nil {
		t.Fatal("in-use scope was dropped by the size sweep")
	}
	idlePath, _ := idle.keyPath(root)
	if _, err := os.Stat(idlePath); !os.IsNotExist(err) {
		t.Fatal("idle over-budget scope was not dropped")
	}
}

// TestGC_SweepBudgetAllInUse: when the only over-budget scope is in use, the
// sweep drops nothing and terminates (no spin) — it can't get under budget this
// round and catches the scope on a later sweep once idle.
func TestGC_SweepBudgetAllInUse(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "only-big")
	fillScope(t, m, key, 500)
	txnID := agentTxnID("run1", "only-big")
	if _, err := m.BeginTxn(ctx, txnID, "run1", key); err != nil { // pin it
		t.Fatalf("begin: %v", err)
	}
	defer func() { _, _ = m.RollbackTxn(txnID) }()

	dropped, err := m.backend.sweepBudget(64 * 1024) // far below the scope; but it's pinned
	if err != nil {
		t.Fatalf("sweepBudget: %v", err)
	}
	if dropped != 0 {
		t.Fatalf("dropped=%d, want 0 (the only over-budget scope is in use)", dropped)
	}
	root := managerRoot(t, m)
	p, _ := key.keyPath(root)
	if _, err := os.Stat(p); err != nil {
		t.Fatal("in-use scope was dropped despite being pinned")
	}
}

// TestGC_SweepBudgetPostgres: the size sweep drops the largest durable scope on
// the postgres tier and leaves the small one. Gated on LOOMCYCLE_TEST_SQLMEM_PG_DSN.
func TestGC_SweepBudgetPostgres(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	big := agentKey("t1", "pg-big")
	small := agentKey("t1", "pg-small")
	if _, err := m.Exec(ctx, small, "CREATE TABLE t (x text)", nil, 0); err != nil {
		t.Fatalf("create small: %v", err)
	}
	if _, err := m.Exec(ctx, big, "CREATE TABLE t (x text)", nil, 0); err != nil {
		t.Fatalf("create big: %v", err)
	}
	// Incompressible distinct rows so pg_total_relation_size is genuinely large
	// (a repeated string would TOAST-compress to almost nothing).
	if _, err := m.Exec(ctx, big, "INSERT INTO t SELECT md5(g::text) FROM generate_series(1, 30000) g", nil, 0); err != nil {
		t.Fatalf("fill big: %v", err)
	}

	dropped, err := m.backend.sweepBudget(64 * 1024) // 64 KB: below big, above the empty small
	if err != nil {
		t.Fatalf("sweepBudget: %v", err)
	}
	if dropped != 1 {
		t.Fatalf("dropped=%d, want 1 (the largest scope)", dropped)
	}
	scopes, err := m.ListScopes(ctx)
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	got := map[ScopeKey]bool{}
	for _, s := range scopes {
		got[s] = true
	}
	if got[big] {
		t.Fatal("largest scope not dropped by the size sweep")
	}
	if !got[small] {
		t.Fatal("small scope wrongly dropped by the size sweep")
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
