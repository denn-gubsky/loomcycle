package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0148_WorkspaceExposureStateUpDownRoundTrips: migrating to 0148
// keeps a row written before it live, with no lease and the operator's
// timeout (RFC HI, Storage); a closed row no longer holds its port; down
// drops the closed rows and the columns and brings the old port key back,
// and up again adds them.
func TestMigrate0148_WorkspaceExposureStateUpDownRoundTrips(t *testing.T) {
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
	columns := func() int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'workspace_exposures'
			  AND column_name IN ('workspace_name', 'run_id', 'idle_timeout_s', 'lease_expires_at', 'closed_at', 'closed_reason')`, schema).Scan(&n); err != nil {
			t.Fatalf("look for the columns: %v", err)
		}
		return n
	}
	insert := func(id string) error {
		_, err := admin.Exec(ctx, `INSERT INTO `+schema+`.workspace_exposures (id, tenant_id, user_id, workspace_id, port, created_at, last_used_at)
			VALUES ($1, 'acme', 'alice', 'ws1', 8000, now(), now())`, id)
		return err
	}

	migrateTo(147)
	if n := columns(); n != 0 {
		t.Fatalf("at 147: %d of the new columns exist already", n)
	}
	if err := insert("ex_old"); err != nil {
		t.Fatalf("write a row before 0148: %v", err)
	}
	migrateTo(148)
	if n := columns(); n != 6 {
		t.Fatalf("at 148: %d of the 6 new columns", n)
	}
	var idle *int
	var lease, closed *time.Time
	var reason string
	if err := admin.QueryRow(ctx, `SELECT idle_timeout_s, lease_expires_at, closed_at, closed_reason FROM `+schema+`.workspace_exposures WHERE id = 'ex_old'`).
		Scan(&idle, &lease, &closed, &reason); err != nil || idle != nil || lease != nil || closed != nil || reason != "" {
		t.Fatalf("a row from before 0148 = %v %v %v %q (%v); want live, no lease, the operator's timeout", idle, lease, closed, reason, err)
	}
	if err := insert("ex_live_twin"); err == nil {
		t.Fatal("two live rows share a port")
	}
	if _, err := admin.Exec(ctx, `UPDATE `+schema+`.workspace_exposures SET closed_at = now(), closed_reason = 'unexposed' WHERE id = 'ex_old'`); err != nil {
		t.Fatal(err)
	}
	if err := insert("ex_new"); err != nil {
		t.Fatalf("a closed row still holds its port: %v", err)
	}
	migrateTo(147)
	if n := columns(); n != 0 {
		t.Fatalf("down to 147: %d of the new columns are left", n)
	}
	var left int
	if err := admin.QueryRow(ctx, `SELECT COUNT(*) FROM `+schema+`.workspace_exposures`).Scan(&left); err != nil || left != 1 {
		t.Fatalf("down to 147: %d rows (%v), want the live one only", left, err)
	}
	if err := insert("ex_twin"); err == nil {
		t.Fatal("down to 147: the port key is not back")
	}
	migrateTo(148)
	if n := columns(); n != 6 {
		t.Fatalf("up again: %d of the 6 new columns", n)
	}
}
