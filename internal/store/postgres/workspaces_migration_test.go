package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0135_WorkspacesUpDownRoundTrips: migrating 134 → 135 creates the
// workspaces table with its unique (owner_run_id, name) key and both indexes,
// down to 134 drops all of it, and up again recreates it — so a rollback
// leaves no half of the table behind and a re-upgrade is clean.
func TestMigrate0135_WorkspacesUpDownRoundTrips(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	ctx := context.Background()
	schema := uniqueSchemaName(t)

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer func() { _, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`) }()
	storeDSN := appendOption(dsn, "search_path", schema+",public")

	migrateTo := func(v uint) {
		t.Helper()
		m, err := newMigrator(storeDSN)
		if err != nil {
			t.Fatal(err)
		}
		defer closeMigrator(m)
		if err := m.Migrate(v); err != nil {
			t.Fatalf("migrate to %d: %v", v, err)
		}
	}
	// relations counts the table and its indexes present in the schema.
	relations := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, `SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND c.relname IN ('workspaces', 'workspaces_by_worker', 'workspaces_by_tenant', 'workspaces_owner_run_id_name_key')`,
			schema).Scan(&n); err != nil {
			t.Fatalf("count relations: %v", err)
		}
		return n
	}

	migrateTo(134)
	if n := relations(); n != 0 {
		t.Fatalf("at 134: %d workspace relations, want 0", n)
	}
	migrateTo(135)
	if n := relations(); n != 4 {
		t.Fatalf("at 135: %d workspace relations, want 4 (table, unique key, two indexes)", n)
	}
	migrateTo(134)
	if n := relations(); n != 0 {
		t.Fatalf("down to 134: %d workspace relations left, want 0", n)
	}
	migrateTo(135)
	if n := relations(); n != 4 {
		t.Fatalf("up again: %d workspace relations, want 4", n)
	}
}
