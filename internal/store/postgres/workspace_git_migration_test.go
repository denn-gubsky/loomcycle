package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0145_WorkspaceGitUpDownRoundTrips: migrating to 0145 adds the
// workspaces.git column with ” for a row written before it (no repository:
// a general workspace), down drops it, and up again adds it back.
func TestMigrate0145_WorkspaceGitUpDownRoundTrips(t *testing.T) {
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
			WHERE table_schema = $1 AND table_name = 'workspaces' AND column_name = 'git'`, schema).Scan(&n); err != nil {
			t.Fatalf("look for the column: %v", err)
		}
		return n == 1
	}

	migrateTo(144)
	if hasColumn() {
		t.Fatal("at 144: workspaces.git exists already")
	}
	if _, err := admin.Exec(ctx, `INSERT INTO `+schema+`.workspaces (id, tenant_id, user_id, name, worker_id, owner_run_id, state, created_at, updated_at)
		VALUES ('ws_old', 'acme', 'alice', 'work', 'w1', 'run-1', 'ready', now(), now())`); err != nil {
		t.Fatalf("write a row before 0145: %v", err)
	}
	migrateTo(145)
	if !hasColumn() {
		t.Fatal("at 145: no workspaces.git column")
	}
	var git string
	if err := admin.QueryRow(ctx, `SELECT git FROM `+schema+`.workspaces WHERE id = 'ws_old'`).Scan(&git); err != nil || git != "" {
		t.Fatalf("a row from before 0145 has git %q, %v; want empty", git, err)
	}
	migrateTo(144)
	if hasColumn() {
		t.Fatal("down to 144: workspaces.git is left")
	}
	migrateTo(145)
	if !hasColumn() {
		t.Fatal("up again: no workspaces.git column")
	}
}
