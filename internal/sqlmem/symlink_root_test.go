package sqlmem

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A sqlite root that is itself a symlink is walked like any other. WalkDir
// does not descend a root that is a symlink, so ListScopes found no scope, and
// with it the snapshot dump, retention and subject erasure all skipped every
// SQL-memory scope, silently; the stale and budget sweeps dropped nothing.
// The in-use check must still hold: a scope pinned by an open transaction is
// keyed by its path, and the walk must produce the same path, or a sweep
// would remove a live scope.
func TestSQLite_ARootThatIsASymlinkIsWalked(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "sqlroot")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, Config{Root: link, ScopeTTLMS: 3600_000})
	ctx := context.Background()

	kept := agentKey("t1", "kept")
	user := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	stale := agentKey("t1", "stale")
	busy := agentKey("t1", "busy")
	for _, k := range []ScopeKey{kept, user, stale, busy} {
		if _, err := m.Exec(ctx, k, "CREATE TABLE t (v TEXT)", nil, 0); err != nil {
			t.Fatalf("create %v: %v", k, err)
		}
	}
	if _, err := m.Exec(ctx, kept, "INSERT INTO t (v) VALUES (?)", []any{"survives a snapshot"}, 0); err != nil {
		t.Fatal(err)
	}

	scopes, err := m.ListScopes(ctx)
	if err != nil {
		t.Fatalf("ListScopes: %v", err)
	}
	got := map[ScopeKey]bool{}
	for _, s := range scopes {
		got[s] = true
	}
	for _, want := range []ScopeKey{kept, user, stale, busy} {
		if !got[want] {
			t.Errorf("ListScopes = %v, missing %v", scopes, want)
		}
	}

	// The snapshot path: every listed scope exports, and restores elsewhere.
	dump, err := m.ExportScope(ctx, kept)
	if err != nil {
		t.Fatalf("ExportScope: %v", err)
	}
	dst := newTestManager(t, Config{})
	if err := dst.RestoreScope(ctx, kept, dump); err != nil {
		t.Fatalf("RestoreScope: %v", err)
	}
	res, err := dst.Query(ctx, kept, "SELECT v FROM t", nil)
	if err != nil || len(res.Rows) != 1 || res.Rows[0][0] != "survives a snapshot" {
		t.Fatalf("restored rows = %v (err %v)", res, err)
	}

	// Pin busy with an open transaction, then backdate it and stale.
	txnID := agentTxnID("run1", "busy")
	if _, err := m.BeginTxn(ctx, txnID, "run1", busy); err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _, _ = m.RollbackTxn(txnID) }()
	old := time.Now().Add(-2 * time.Hour)
	pathOf := func(k ScopeKey) string {
		p, err := k.keyPath(real)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, k := range []ScopeKey{stale, busy} {
		if err := os.Chtimes(pathOf(k), old, old); err != nil {
			t.Fatal(err)
		}
	}

	dropped, err := m.backend.sweepStale(time.Now().Add(-time.Hour))
	if err != nil || dropped != 1 {
		t.Errorf("sweepStale: dropped=%d err=%v, want 1 (the stale scope)", dropped, err)
	}
	if _, err := os.Stat(pathOf(stale)); !os.IsNotExist(err) {
		t.Error("the stale scope survived the sweep")
	}
	if _, err := os.Stat(pathOf(busy)); err != nil {
		t.Errorf("the sweep removed a scope pinned by an open transaction: %v", err)
	}

	// A budget of zero drops every idle durable scope, never the pinned one.
	dropped, err = m.backend.sweepBudget(0)
	if err != nil || dropped != 2 {
		t.Errorf("sweepBudget(0): dropped=%d err=%v, want 2 (kept and user)", dropped, err)
	}
	if _, err := os.Stat(pathOf(busy)); err != nil {
		t.Errorf("the budget sweep removed a scope pinned by an open transaction: %v", err)
	}
}
