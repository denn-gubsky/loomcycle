package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0147_WorkspaceToolchainsUpDownRoundTrips: migrating to 0147 adds
// the workspaces.toolchains column with NULL for a row written before it
// (unknown: save_template falls back to the worker's list, RFC HH §5), down
// drops it, and up again adds it back.
func TestMigrate0147_WorkspaceToolchainsUpDownRoundTrips(t *testing.T) {
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
	hasColumn := func() bool {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'workspaces' AND column_name = 'toolchains'`, schema).Scan(&n); err != nil {
			t.Fatalf("look for the column: %v", err)
		}
		return n == 1
	}

	migrateTo(146)
	if hasColumn() {
		t.Fatal("at 146: workspaces.toolchains exists already")
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.workspaces (id, tenant_id, user_id, name, worker_id, owner_run_id, state, created_at, updated_at)
		VALUES ('ws_old', 'acme', 'alice', 'work', 'w1', 'run-1', 'ready', now(), now())`); err != nil {
		t.Fatalf("write a row before 0147: %v", err)
	}
	migrateTo(147)
	if !hasColumn() {
		t.Fatal("at 147: no workspaces.toolchains column")
	}
	var tools *string
	if err := admin.QueryRow(ctx, `SELECT toolchains FROM `+schema+`.workspaces WHERE id = 'ws_old'`).Scan(&tools); err != nil || tools != nil {
		t.Fatalf("a row from before 0147 has toolchains %v, %v; want NULL", tools, err)
	}
	migrateTo(146)
	if hasColumn() {
		t.Fatal("down to 146: workspaces.toolchains is left")
	}
	migrateTo(147)
	if !hasColumn() {
		t.Fatal("up again: no workspaces.toolchains column")
	}
}
