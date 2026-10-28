package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0122_ADatabaseThatSkipped0118GetsTheDeviceGateTables: 0121
// merged to main before 0118, so a database that migrated on the commit
// between them sits at version 121 without the device-gate tables, and
// golang-migrate never runs 0118 for it. 0122 must create them there; on a
// database that did run 0118 it must change nothing; and stepping down past
// 122 must not drop tables 0118 owns.
func TestMigrate0122_ADatabaseThatSkipped0118GetsTheDeviceGateTables(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("dial postgres: %v", err)
	}
	defer admin.Close()

	newSchema := func() (string, string) {
		t.Helper()
		schema := uniqueSchemaName(t)
		if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
			t.Fatalf("create schema: %v", err)
		}
		t.Cleanup(func() { _, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
		return schema, appendOption(dsn, "search_path", schema+",public")
	}
	tables := func(schema string) int {
		t.Helper()
		var n int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_schema = $1 AND table_name IN ('device_authorizations', 'tenant_auth_policies')`, schema).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The skipped database: at 117, then 0121's change applied and the
	// version recorded as 121 — what main's history left on such a database.
	skipped, skippedDSN := newSchema()
	m, err := newMigrator(skippedDSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(117); err != nil {
		closeMigrator(m)
		t.Fatalf("migrate to 117: %v", err)
	}
	if _, err := admin.Exec(ctx, `ALTER TABLE `+skipped+`.teamdef_active ADD COLUMN promoter_acl_owner TEXT`); err != nil {
		closeMigrator(m)
		t.Fatalf("apply 0121's change: %v", err)
	}
	if err := m.Force(121); err != nil {
		closeMigrator(m)
		t.Fatalf("record version 121: %v", err)
	}
	closeMigrator(m)
	if n := tables(skipped); n != 0 {
		t.Fatalf("setup: the skipped database already has %d device-gate tables", n)
	}
	st, err := Open(ctx, Config{DSN: skippedDSN, MaxOpenConns: 2, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (migrating up from 121): %v", err)
	}
	st.Close()
	if n := tables(skipped); n != 2 {
		t.Errorf("after migrating up from 121: %d of 2 device-gate tables, want both", n)
	}

	// A database that ran 0118: 0122 is a no-op, and down past 122 keeps
	// the tables 0118 owns.
	full, fullDSN := newSchema()
	st, err = Open(ctx, Config{DSN: fullDSN, MaxOpenConns: 2, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (fresh): %v", err)
	}
	st.Close()
	m, err = newMigrator(fullDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMigrator(m)
	if err := m.Migrate(121); err != nil {
		t.Fatalf("migrate down to 121: %v", err)
	}
	if n := tables(full); n != 2 {
		t.Errorf("after stepping down past 122: %d of 2 device-gate tables, want both kept", n)
	}
}
