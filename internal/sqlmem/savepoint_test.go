package sqlmem

import (
	"context"
	"testing"
)

// savepoint_test.go — RFC AA Phase 3b: nested transactions (auto-savepoint,
// LIFO). The sqlite cases run on every `go test`; the postgres case is gated on
// LOOMCYCLE_TEST_SQLMEM_PG_DSN (savepoints are standard SQL on both tiers).

// wantDepth(t, N) returns a checker for a (depth, err) txn-op result: it fails
// on a non-nil error or a depth != N. Called as wantDepth(t, 1)(m.BeginTxn(...))
// — the op's two return values are the checker's sole arguments (valid Go).
func wantDepth(t *testing.T, want int) func(int, error) {
	t.Helper()
	return func(got int, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("txn op: %v", err)
		}
		if got != want {
			t.Fatalf("depth=%d, want %d", got, want)
		}
	}
}

// TestTxn_NestedRollbackUndoesInnerOnly: a nested rollback undoes only the inner
// level's writes; the outer transaction's earlier write survives.
func TestTxn_NestedRollbackUndoesInnerOnly(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "sp-rb")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "sp-rb")
	wantDepth(t, 1)(m.BeginTxn(ctx, id, "run1", key))
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	wantDepth(t, 2)(m.BeginTxn(ctx, id, "run1", key)) // nested
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (2)", nil, 0); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	wantDepth(t, 1)(m.RollbackTxn(id)) // pop inner → B undone, txn open
	wantDepth(t, 0)(m.CommitTxn(id))   // commit root → A persists
	if n := rowCount(t, m, key); n != 1 {
		t.Fatalf("count=%d, want 1 (only the outer insert persists)", n)
	}
	res, err := m.Query(ctx, key, "SELECT x FROM t", nil)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got, _ := res.Rows[0][0].(int64); got != 1 {
		t.Fatalf("surviving row x=%d, want 1", got)
	}
}

// TestTxn_NestedCommitKeepsBoth: releasing the nested level then committing the
// root persists both writes.
func TestTxn_NestedCommitKeepsBoth(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "sp-commit")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "sp-commit")
	wantDepth(t, 1)(m.BeginTxn(ctx, id, "run1", key))
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	wantDepth(t, 2)(m.BeginTxn(ctx, id, "run1", key))
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (2)", nil, 0); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	wantDepth(t, 1)(m.CommitTxn(id)) // release nested
	wantDepth(t, 0)(m.CommitTxn(id)) // commit root
	if n := rowCount(t, m, key); n != 2 {
		t.Fatalf("count=%d, want 2 (both inserts persist)", n)
	}
}

// TestTxn_NestedDepthCap: a nested begin past MaxTxnDepth errors; the txn stays
// usable at the cap.
func TestTxn_NestedDepthCap(t *testing.T) {
	m := newTestManager(t, Config{MaxTxnDepth: 1}) // at most 1 savepoint → max depth 2
	ctx := context.Background()
	key := agentKey("t1", "sp-cap")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "sp-cap")
	wantDepth(t, 1)(m.BeginTxn(ctx, id, "run1", key))
	wantDepth(t, 2)(m.BeginTxn(ctx, id, "run1", key)) // 1 savepoint, at the cap
	if d, err := m.BeginTxn(ctx, id, "run1", key); err == nil {
		t.Fatalf("nested begin past the depth cap was allowed (depth=%d)", d)
	}
	// Still usable at the cap: a write + pop + root commit succeed.
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (9)", nil, 0); err != nil {
		t.Fatalf("exec at cap: %v", err)
	}
	wantDepth(t, 1)(m.CommitTxn(id))
	wantDepth(t, 0)(m.CommitTxn(id))
	if n := rowCount(t, m, key); n != 1 {
		t.Fatalf("count=%d, want 1", n)
	}
}

// TestTxn_NestedCleanupOnRunEnd: a run ending with a nested level open rolls the
// WHOLE transaction back (savepoints discarded with it); the scope is then free.
func TestTxn_NestedCleanupOnRunEnd(t *testing.T) {
	m := newTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "sp-cleanup")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x INT)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "sp-cleanup")
	wantDepth(t, 1)(m.BeginTxn(ctx, id, "run1", key))
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	wantDepth(t, 2)(m.BeginTxn(ctx, id, "run1", key))
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (2)", nil, 0); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	m.RollbackRunTxns("run1") // run-end cleanup: whole txn (+ its savepoints) rolled back
	if m.InTxn(id) {
		t.Fatal("InTxn=true after run-end cleanup")
	}
	if n := rowCount(t, m, key); n != 0 {
		t.Fatalf("count=%d, want 0 (whole txn discarded)", n)
	}
	// The scope is free again — a fresh auto-commit op works.
	if _, err := m.Exec(ctx, key, "INSERT INTO t VALUES (3)", nil, 0); err != nil {
		t.Fatalf("post-cleanup exec: %v", err)
	}
}

// TestTxn_NestedRollback_Postgres: the nested round trip on the postgres tier
// (savepoints via pgx). Gated on the aux DSN.
func TestTxn_NestedRollback_Postgres(t *testing.T) {
	m, _ := pgTestManager(t, Config{})
	ctx := context.Background()
	key := agentKey("t1", "sp-pg")
	if _, err := m.Exec(ctx, key, "CREATE TABLE t (x int)", nil, 0); err != nil {
		t.Fatalf("create: %v", err)
	}
	id := agentTxnID("run1", "sp-pg")
	wantDepth(t, 1)(m.BeginTxn(ctx, id, "run1", key))
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (1)", nil, 0); err != nil {
		t.Fatalf("insert A: %v", err)
	}
	wantDepth(t, 2)(m.BeginTxn(ctx, id, "run1", key))
	if _, err := m.ExecTxn(ctx, id, "INSERT INTO t VALUES (2)", nil, 0); err != nil {
		t.Fatalf("insert B: %v", err)
	}
	wantDepth(t, 1)(m.RollbackTxn(id))
	wantDepth(t, 0)(m.CommitTxn(id))
	if n := rowCount(t, m, key); n != 1 {
		t.Fatalf("count=%d, want 1 (inner insert rolled back)", n)
	}
}
