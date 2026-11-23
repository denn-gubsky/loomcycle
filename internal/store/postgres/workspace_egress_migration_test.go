package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0140_WorkspaceEgressUpDownRoundTrips: migrating to 0140 adds
// the workspaces.egress column with '[]' for a row written before it, down
// drops it, and up again adds it back — so a rollback leaves no column
// behind and every row from before has no way out.
func TestMigrate0140_WorkspaceEgressUpDownRoundTrips(t *testing.T) {
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
			WHERE table_schema = $1 AND table_name = 'workspaces' AND column_name = 'egress'`, schema).Scan(&n); err != nil {
			t.Fatalf("look for the column: %v", err)
		}
		return n == 1
	}

	migrateTo(136)
	if hasColumn() {
		t.Fatal("at 136: workspaces.egress exists already")
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.workspaces (id, tenant_id, user_id, name, worker_id, owner_run_id, state, created_at, updated_at)
		VALUES ('ws_old', 'acme', 'alice', 'work', 'w1', 'run-1', 'ready', now(), now())`); err != nil {
		t.Fatalf("write a row before 0140: %v", err)
	}
	migrateTo(140)
	if !hasColumn() {
		t.Fatal("at 140: no workspaces.egress column")
	}
	var egress string
	if err := admin.QueryRow(ctx, `SELECT egress FROM `+schema+`.workspaces WHERE id = 'ws_old'`).Scan(&egress); err != nil || egress != "[]" {
		t.Fatalf("a row from before 0140 has egress %q, %v; want []", egress, err)
	}
	migrateTo(136)
	if hasColumn() {
		t.Fatal("down to 136: workspaces.egress is left")
	}
	migrateTo(140)
	if !hasColumn() {
		t.Fatal("up again: no workspaces.egress column")
	}
}
