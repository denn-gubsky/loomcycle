package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// legacyMemoryDDL is the `memory` table as it was created BEFORE any of the columns that
// arrive by ALTER — no tenant_id, no source_session_id, no observed_at, no valid_at.
//
// This is what a long-lived deployment actually has on disk: CREATE TABLE IF NOT EXISTS is
// a no-op against an existing table, so the table keeps the shape it was first created
// with and every later column comes from the ALTER block.
const legacyMemoryDDL = `CREATE TABLE memory (
	scope      TEXT NOT NULL,
	scope_id   TEXT NOT NULL,
	key        TEXT NOT NULL,
	value      TEXT NOT NULL,
	expires_at INTEGER,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (scope, scope_id, key)
)`

// TestMigrate_OpensADBWhoseMemoryTablePredatesEveryAddedColumn is the regression test for
// an upgrade that could not start at all.
//
// The bug: three partial indexes on `memory` sat in the CREATE-TABLE block, ahead of the
// ALTERs that add the columns they name. On a fresh DB the columns come from CREATE TABLE,
// so everything passed — including every test, because every test started from an empty
// file. On an UPGRADED DB the table already existed without those columns, the index
// creation failed with "no such column: observed_at", index errors are fatal, and the ALTER
// that would have added the column was never reached. Any sqlite deployment whose `memory`
// table predated RFC CL refused to open.
//
// Starting from the legacy shape is the whole point: a test that starts empty cannot see
// this class of bug.
func TestMigrate_OpensADBWhoseMemoryTablePredatesEveryAddedColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	// Lay down the old table with a row in it, then close, so Open() faces a real
	// pre-existing database rather than one this process is already holding open.
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := seed.Exec(legacyMemoryDDL); err != nil {
		t.Fatalf("create legacy memory table: %v", err)
	}
	if _, err := seed.Exec(
		`INSERT INTO memory (scope, scope_id, key, value, created_at, updated_at)
		 VALUES ('user', 'alice', 'pref/editor', '"vim"', 1, 1)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy DB must migrate it, got: %v", err)
	}
	defer func() { _ = st.Close() }()

	ctx := context.Background()

	// The ALTERs ran: every column the indexes name now exists.
	cols := map[string]bool{}
	rows, err := st.db.QueryContext(ctx, `SELECT name FROM pragma_table_info('memory')`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cols[n] = true
	}
	_ = rows.Close()
	for _, want := range []string{"tenant_id", "source_session_id", "observed_at", "valid_at", "invalid_at"} {
		if !cols[want] {
			t.Errorf("column %q was not added to the legacy table", want)
		}
	}

	// And the indexes were actually created, rather than the migration "succeeding" by
	// skipping them — which would leave the time predicates unindexed and silent.
	for _, want := range []string{"memory_by_source_session", "memory_by_observed_at", "memory_by_valid_at"} {
		var name string
		err := st.db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='index' AND name = ?`, want).Scan(&name)
		if err != nil {
			t.Errorf("index %q missing after migrate: %v", want, err)
		}
	}

	// The pre-existing row survived. A migration that fixed the schema by dropping data
	// would pass every check above.
	var value string
	if err := st.db.QueryRowContext(ctx,
		`SELECT value FROM memory WHERE scope='user' AND scope_id='alice' AND key='pref/editor'`,
	).Scan(&value); err != nil {
		t.Fatalf("the legacy row did not survive the migration: %v", err)
	}
	if value != `"vim"` {
		t.Errorf("legacy row value = %q, want %q", value, `"vim"`)
	}

	// Idempotent: opening again must be a clean no-op, which is what every boot after
	// the upgrade does.
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open on the migrated DB failed: %v", err)
	}
	_ = st2.Close()
}
