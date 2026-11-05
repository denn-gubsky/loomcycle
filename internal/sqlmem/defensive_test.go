package sqlmem

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// A scope file holds the agent's own tables and the Document plane's together,
// so a statement that rewrites the file's pages, rather than naming a table,
// reaches both. Defensive mode makes the engine refuse those writes on every
// connection the tier opens; these checks run on the scope's own pool, below
// the Memory tool and its guard.
func TestSqlmem_DefensiveModeIsOnEveryScopeConnection(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	b := m.backend.(*sqliteBackend)

	keys := []ScopeKey{
		{Tenant: "t1", Scope: "agent", ScopeID: "a1"},
		{Tenant: "t1", Scope: "user", ScopeID: "u1"},
		{Tenant: "t1", Scope: "tenant", ScopeID: "t1"},
		{Scope: runScope, ScopeID: "run-1"},
	}

	// refusesPageWrites proves the vtab is there (the read works, so a failed
	// write is not "no such table") and that the engine refuses the write.
	refusesPageWrites := func(t *testing.T, label string, q interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	}) {
		t.Helper()
		var pages int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_dbpage`).Scan(&pages); err != nil || pages == 0 {
			t.Fatalf("%s: read sqlite_dbpage = %d, %v; want the pages (the vtab must exist for this check to mean anything)", label, pages, err)
		}
		_, err := q.ExecContext(ctx, `UPDATE sqlite_dbpage SET data = data WHERE pgno = 1`)
		if err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s: write to sqlite_dbpage: err = %v; want the engine to refuse it (defensive mode)", label, err)
		}
		if _, err := q.ExecContext(ctx, `PRAGMA writable_schema = ON`); err != nil {
			t.Fatalf("%s: PRAGMA writable_schema = ON: %v", label, err)
		}
		var writable int
		if err := q.QueryRowContext(ctx, `PRAGMA writable_schema`).Scan(&writable); err != nil || writable != 0 {
			t.Fatalf("%s: writable_schema after setting it ON = %d, %v; want 0 (ignored in defensive mode)", label, writable, err)
		}
	}

	for _, key := range keys {
		if _, err := m.Exec(ctx, key, `CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`, nil, 0); err != nil {
			t.Fatalf("%s: create a table: %v", key.Scope, err)
		}
		path, err := key.keyPath(b.cfg.Root)
		if err != nil {
			t.Fatal(err)
		}

		db, err := b.acquire(path)
		if err != nil {
			t.Fatal(err)
		}
		refusesPageWrites(t, key.Scope+" pool", db)
		b.release(path)

		// The Document plane is the same file on this tier and reaches it
		// through the same pool.
		if _, err := m.DocPlane().Exec(ctx, key, `UPDATE sqlite_dbpage SET data = data WHERE pgno = 1`, nil, 0); err == nil || !strings.Contains(err.Error(), "read-only") {
			t.Fatalf("%s: page write through the document plane: err = %v; want it refused", key.Scope, err)
		}

		// An explicit transaction holds the scope's connection.
		tx, done, err := b.beginTx(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		refusesPageWrites(t, key.Scope+" txn", tx)
		_ = tx.Rollback()
		done()

		// A handle closed by eviction reopens with the same posture.
		b.mu.Lock()
		b.evictPathLocked(path)
		b.mu.Unlock()
		db, err = b.acquire(path)
		if err != nil {
			t.Fatal(err)
		}
		refusesPageWrites(t, key.Scope+" reopened", db)
		b.release(path)
	}
}

// Defensive mode refuses the file-corrupting paths only; the operations the
// runtime and an agent rely on still work on a defensive connection.
func TestSqlmem_DefensiveModeKeepsOrdinaryOperations(t *testing.T) {
	ctx := context.Background()
	m := newTestManager(t, Config{})
	b := m.backend.(*sqliteBackend)
	key := ScopeKey{Tenant: "t1", Scope: "user", ScopeID: "u1"}
	for _, s := range []string{
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT)`,
		`CREATE INDEX notes_body ON notes (body)`,
		`INSERT INTO notes (body) VALUES ('a'), ('b')`,
		`ALTER TABLE notes ADD COLUMN kind TEXT`,
		`ALTER TABLE notes RENAME TO notes2`,
		`CREATE VIEW v AS SELECT * FROM notes2`,
		`DROP VIEW v`,
	} {
		if _, err := m.Exec(ctx, key, s, nil, 0); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	path, err := key.keyPath(b.cfg.Root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.release(path)
	// The runtime's own maintenance statements, run on the scope pool.
	for _, s := range []string{`VACUUM`, `PRAGMA wal_checkpoint(TRUNCATE)`, `ANALYZE`} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s on a defensive connection: %v", s, err)
		}
	}
	var mode string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", mode, err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM notes2`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows after VACUUM = %d, %v; want 2", n, err)
	}
}
