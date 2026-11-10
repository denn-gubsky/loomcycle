package sqlmem

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/safego"
)

// txnID for an agent scope under a given run.
func agentTxnID(run, agent string) string { return BuildTxnID(run, "agent", agent) }

func rowCount(t *testing.T, m *Manager, key ScopeKey) int64 {
	t.Helper()
	res, err := m.Query(context.Background(), key, "SELECT count(*) FROM t", nil)
	if err != nil {
		t.Fatalf("count query: %v", err)
	}
	switch v := res.Rows[0][0].(type) {
	case int64:
		return v
	case string:
		var n int64
		for _, c := range v {
			n = n*10 + int64(c-'0')
		}
		return n
	default:
		t.Fatalf("unexpected count type %T", v)
		return 0
	}
}

// TestTxn_CommitPersists: begin → insert → commit makes the row durable.
func TestTxn_CommitPersists(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "txn-commit")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "txn-commit")
	if _, err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if !m.InTxn(id) {
		t.Fatal("InTxn=false after begin")
	}
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("exec in txn: %v", err)
	}
	if _, err := m.CommitTxn(id); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if m.InTxn(id) {
		t.Fatal("InTxn=true after commit")
	}
	if n := rowCount(t, m, key); n != 1 {
		t.Fatalf("after commit count=%d, want 1", n)
	}
}

// TestTxn_RollbackDiscards: a rollback undoes writes made in the transaction.
func TestTxn_RollbackDiscards(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "txn-rb")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := m.Exec(ctx, key, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	id := agentTxnID("run1", "txn-rb")
	if _, err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (2)", nil, 0); err != nil {
		t.Fatalf("exec in txn: %v", err)
	}
	// Inside the txn the second row is visible (same connection).
	res, err := m.QueryTxn(ctx, id, "SELECT count(*) FROM t", nil)
	if err != nil {
		t.Fatalf("query in txn: %v", err)
	}
	if got, _ := res.Rows[0][0].(int64); got != 2 {
		t.Fatalf("in-txn count=%v, want 2", res.Rows[0][0])
	}
	if _, err := m.RollbackTxn(id); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if n := rowCount(t, m, key); n != 1 {
		t.Fatalf("after rollback count=%d, want 1 (insert discarded)", n)
	}
}

// TestTxn_Guards: commit/rollback with none open error; a second begin NESTS
// (Phase 3b — was an error in 3a) and the depths are reported correctly.
func TestTxn_Guards(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "txn-g")
	id := agentTxnID("run1", "txn-g")
	if _, err := m.CommitTxn(id); err == nil {
		t.Fatal("commit with no open txn was allowed")
	}
	if _, err := m.RollbackTxn(id); err == nil {
		t.Fatal("rollback with no open txn was allowed")
	}
	if d, err := m.BeginTxn(ctx, id, "run1", key); err != nil || d != 1 {
		t.Fatalf("begin: depth=%d err=%v, want depth 1", d, err)
	}
	// A second begin opens a nested SAVEPOINT level (depth 2), not an error.
	if d, err := m.BeginTxn(ctx, id, "run1", key); err != nil || d != 2 {
		t.Fatalf("nested begin: depth=%d err=%v, want depth 2", d, err)
	}
	// First rollback pops the nested level (back to depth 1, txn still open).
	if d, err := m.RollbackTxn(id); err != nil || d != 1 {
		t.Fatalf("nested rollback: depth=%d err=%v, want depth 1", d, err)
	}
	// Second rollback ends the whole transaction (depth 0).
	if d, err := m.RollbackTxn(id); err != nil || d != 0 {
		t.Fatalf("root rollback: depth=%d err=%v, want depth 0", d, err)
	}
}

// TestTxn_MaxOpen: the process-wide cap refuses a begin past the limit.
func TestTxn_MaxOpen(t *testing.T) {
	m := newTestManager(t, Config{MaxOpenTxns: 1})
	ctx := context.Background()
	k1, k2 := agentKey("t1", "a1"), agentKey("t1", "a2")
	id1, id2 := agentTxnID("run1", "a1"), agentTxnID("run1", "a2")
	if _, err := m.BeginTxn(ctx, id1, "run1", k1); err != nil {
		t.Fatalf("begin 1: %v", err)
	}
	if _, err := m.BeginTxn(ctx, id2, "run1", k2); err == nil {
		t.Fatal("begin past MaxOpenTxns was allowed")
	}
	if _, err := m.RollbackTxn(id1); err != nil {
		t.Fatalf("rollback 1: %v", err)
	}
	if _, err := m.BeginTxn(ctx, id2, "run1", k2); err != nil {
		t.Fatalf("begin 2 after freeing a slot: %v", err)
	}
	_, _ = m.RollbackTxn(id2)
}

// TestTxn_OneTenantFillingTheCapLeavesASlotForAnother: with only the global
// cap set, a tenant's share defaults to a quarter of it, so one tenant opening
// transactions until refused still leaves room for a second tenant.
func TestTxn_OneTenantFillingTheCapLeavesASlotForAnother(t *testing.T) {
	m := newTestManager(t, Config{MaxOpenTxns: 4})
	ctx := context.Background()
	opened := 0
	for i := 0; i < 4; i++ {
		agent := fmt.Sprintf("a%d", i)
		if _, err := m.BeginTxn(ctx, agentTxnID("runA", agent), "runA", agentKey("tenantA", agent)); err != nil {
			if !strings.Contains(err.Error(), "too many open transactions") {
				t.Fatalf("tenant A begin %d: refusal is %q, want the cap error", i, err)
			}
			break
		}
		opened++
	}
	if opened != 1 {
		t.Fatalf("tenant A opened %d of 4 slots, want its share of 1", opened)
	}
	if _, err := m.BeginTxn(ctx, agentTxnID("runB", "b"), "runB", agentKey("tenantB", "b")); err != nil {
		t.Fatalf("tenant B begin after tenant A filled its share: %v", err)
	}
	m.RollbackRunTxns("runA")
	m.RollbackRunTxns("runB")
}

// TestTxn_PerTenantShareIsConfigurable: an explicit per-tenant cap replaces the
// default share, and finishing a transaction frees that tenant's slot.
func TestTxn_PerTenantShareIsConfigurable(t *testing.T) {
	m := newTestManager(t, Config{MaxOpenTxns: 8, MaxOpenTxnsPerTenant: 3})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		agent := fmt.Sprintf("a%d", i)
		if _, err := m.BeginTxn(ctx, agentTxnID("runA", agent), "runA", agentKey("tenantA", agent)); err != nil {
			t.Fatalf("tenant A begin %d: %v", i, err)
		}
	}
	if _, err := m.BeginTxn(ctx, agentTxnID("runA", "a3"), "runA", agentKey("tenantA", "a3")); err == nil {
		t.Fatal("tenant A opened a 4th transaction past its share of 3")
	}
	if _, err := m.RollbackTxn(agentTxnID("runA", "a0")); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := m.BeginTxn(ctx, agentTxnID("runA", "a3"), "runA", agentKey("tenantA", "a3")); err != nil {
		t.Fatalf("tenant A begin after freeing a slot: %v", err)
	}
	m.RollbackRunTxns("runA")
}

// TestTxn_DefaultTenantIsNotHeldToTheShare: the runtime's "" tenant (stored as
// ScopeTenant("")) is the operator's own, the one a single-tenant install runs
// in, so it keeps the full global cap.
func TestTxn_DefaultTenantIsNotHeldToTheShare(t *testing.T) {
	m := newTestManager(t, Config{MaxOpenTxns: 4})
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		agent := fmt.Sprintf("a%d", i)
		if _, err := m.BeginTxn(ctx, agentTxnID("run0", agent), "run0", agentKey(ScopeTenant(""), agent)); err != nil {
			t.Fatalf("default tenant begin %d of 4: %v", i, err)
		}
	}
	if _, err := m.BeginTxn(ctx, agentTxnID("run0", "a4"), "run0", agentKey(ScopeTenant(""), "a4")); err == nil {
		t.Fatal("default tenant opened a 5th transaction past the global cap of 4")
	}
	m.RollbackRunTxns("run0")
}

// TestTxn_RollbackRunTxns: run-end cleanup rolls back the tree's open txns.
func TestTxn_RollbackRunTxns(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "rr")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("runX", "rr")
	if _, err := m.BeginTxn(ctx, id, "runX", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (9)", nil, 0); err != nil {
		t.Fatalf("exec: %v", err)
	}
	m.RollbackRunTxns("runX") // simulate run completion
	if m.InTxn(id) {
		t.Fatal("txn still open after run-end rollback")
	}
	if n := rowCount(t, m, key); n != 0 {
		t.Fatalf("after run-end rollback count=%d, want 0 (insert discarded)", n)
	}
}

// TestTxn_ReapStale: the reaper rolls back a transaction past its TTL.
func TestTxn_ReapStale(t *testing.T) {
	m := newTestManager(t, Config{TxnTimeoutMS: 60000})
	ctx := context.Background()
	key := agentKey("t1", "reap")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "reap")
	if _, err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("exec: %v", err)
	}
	m.reapStale(0) // TTL 0 ⇒ everything open before now is stale
	if m.InTxn(id) {
		t.Fatal("stale txn not reaped")
	}
	if n := rowCount(t, m, key); n != 0 {
		t.Fatalf("after reap count=%d, want 0 (insert rolled back)", n)
	}
}

// TestTxn_ReapStaleReleasesEveryVictimWhenOneFinishPanics: the reaper takes all
// stale victims out of the registry before finishing them, so a panic in one
// victim's finish (a driver panic in Rollback) must not strand the rest — each
// would keep its pinned connection forever, and a SQLite scope (one connection)
// would hang on its next statement.
func TestTxn_ReapStaleReleasesEveryVictimWhenOneFinishPanics(t *testing.T) {
	m := newTestManager(t, Config{TxnTimeoutMS: 60000})
	ctx := context.Background()
	key := agentKey("t1", "reap-panic")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "reap-panic")
	if _, err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("exec: %v", err)
	}
	// Victims whose finish panics: a nil *sql.Tx panics in Rollback. Two of
	// them make the outcome independent of map order — whichever is reaped
	// first, at least one panicking victim is unreleased without the fix.
	var mu sync.Mutex
	released := map[string]bool{}
	started := time.Now().Add(-time.Hour)
	for _, fid := range []string{"panic-a", "panic-b"} {
		m.txns.mu.Lock()
		m.txns.open[fid] = &openTxn{key: agentKey("t1", fid), started: started, release: func() {
			mu.Lock()
			released[fid] = true
			mu.Unlock()
		}}
		m.txns.mu.Unlock()
	}

	// The production reaper calls reapStale under safego.Run; mirror it so a
	// panic escaping reapStale fails the assertions below, not the test binary.
	safego.Run(ctx, "test.reaper", func() { m.reapStale(0) })

	mu.Lock()
	for _, fid := range []string{"panic-a", "panic-b"} {
		if !released[fid] {
			mu.Unlock()
			t.Fatalf("victim %s not released after a reap where a finish panicked", fid)
		}
	}
	mu.Unlock()
	if m.InTxn(id) {
		t.Fatal("healthy stale txn not reaped")
	}
	// The healthy victim's connection is back: the scope answers, and its
	// insert was rolled back. (Checked last: without the fix a stranded pin
	// would hang this query on the one SQLite connection.)
	if n := rowCount(t, m, key); n != 0 {
		t.Fatalf("after reap count=%d, want 0 (insert rolled back)", n)
	}
}

// TestTxn_ConcurrentStatementsSameTxn fires many ExecTxn at the SAME open
// transaction concurrently — tool calls in one agent turn dispatch in parallel,
// so two sql_exec for the same scope reach one *sql.Tx at once. A *sql.Tx must
// not be driven concurrently (a Query's open Rows hold the connection, so a
// concurrent Exec would error "connection busy", and ordering is otherwise
// undefined); the per-txn lock serializes them. This asserts all concurrent
// inserts on one txn land — run under -race in CI.
func TestTxn_ConcurrentStatementsSameTxn(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "concurrent-txn")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "concurrent-txn")
	if _, err := m.BeginTxn(ctx, id, "run1", key); err != nil {
		t.Fatalf("begin: %v", err)
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(v int) {
			defer wg.Done()
			if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (?)", []any{v}, 0); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ExecTxn: %v", err)
	}
	if _, err := m.CommitTxn(id); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := rowCount(t, m, key); got != n {
		t.Fatalf("after concurrent inserts + commit count=%d, want %d", got, n)
	}
}

// TestTxn_AutoCommitUnchanged: with no open txn, exec persists immediately
// (the Phase 1/2 path is byte-identical).
func TestTxn_AutoCommitUnchanged(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "auto")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.InTxn(agentTxnID("run1", "auto")) {
		t.Fatal("InTxn=true with no begin")
	}
	if _, err := m.Exec(ctx, key, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if n := rowCount(t, m, key); n != 1 {
		t.Fatalf("auto-commit count=%d, want 1", n)
	}
}

// panickingBeginBackend is a backend whose beginTx panics, as a driver bug would.
type panickingBeginBackend struct{ backend }

func (panickingBeginBackend) beginTx(context.Context, ScopeKey) (*sql.Tx, func(), error) {
	panic("beginTx bug")
}

// TestTxn_APanickingBeginFreesItsReservation: a panic in the backend's begin,
// recovered higher up, must not keep the reservation — it would hold a global
// and a tenant slot for the process's life, and the txn id would answer "being
// opened — retry" forever.
func TestTxn_APanickingBeginFreesItsReservation(t *testing.T) {
	m := newTestManager(t, Config{MaxOpenTxns: 4, MaxOpenTxnsPerTenant: 1})
	real := m.backend
	m.backend = panickingBeginBackend{real}
	id := agentTxnID("runA", "a")
	func() {
		defer func() { _ = recover() }()
		_, _ = m.BeginTxn(context.Background(), id, "runA", agentKey("tenantA", "a"))
	}()
	m.backend = real
	if _, err := m.BeginTxn(context.Background(), id, "runA", agentKey("tenantA", "a")); err != nil {
		t.Fatalf("begin after a panicking begin: %v", err)
	}
	m.RollbackRunTxns("runA")
}

// TestTxn_FinishingAFinishedTxnDoesNotReleaseAgain: the reaper or run-end
// cleanup can reach a txn that finishLevel has finished but not yet removed
// from the registry. A second finish must not run release again — it is a
// refcount decrement and would drop another holder's pin on the scope.
func TestTxn_FinishingAFinishedTxnDoesNotReleaseAgain(t *testing.T) {
	m := newTestManager(t, Config{MaxOpenTxns: 4})
	id := agentTxnID("runA", "a")
	if _, err := m.BeginTxn(context.Background(), id, "runA", agentKey("tenantA", "a")); err != nil {
		t.Fatalf("begin: %v", err)
	}
	m.txns.mu.Lock()
	txn := m.txns.open[id]
	m.txns.mu.Unlock()
	releases := 0
	orig := txn.release
	txn.release = func() { releases++; orig() }
	_ = txn.finish(false)
	_ = txn.finish(false)
	if releases != 1 {
		t.Fatalf("release ran %d times over two finishes, want 1", releases)
	}
	m.RollbackRunTxns("runA")
}
