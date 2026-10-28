package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMigrate0117_ExistingTokenReadsFrozenAndDownUpRoundTrips: a token minted
// at schema version 116 has no authority column. After migrating up it reads
// back frozen (""), never inherit — an existing token keeps the ceiling it was
// minted with. A token minted inherit after the upgrade reads back inherit,
// and the down migration drops the column cleanly (the row survives; up again
// reads it frozen, since the mode went with the column).
func TestMigrate0117_ExistingTokenReadsFrozenAndDownUpRoundTrips(t *testing.T) {
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

	m, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(116); err != nil {
		closeMigrator(m)
		t.Fatalf("migrate to 116: %v", err)
	}
	closeMigrator(m)

	seed, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(ctx, `INSERT INTO operator_token_defs
		(def_id, name, tenant_id, subject, token_hash, allowed_scopes, created_at)
		VALUES ('def_old', 'u-alice', 'acme', 'alice', 'h-old', '["substrate:user"]', now())`); err != nil {
		seed.Close()
		t.Fatalf("seed a pre-0117 token: %v", err)
	}
	seed.Close()

	st, err := Open(ctx, Config{DSN: storeDSN, MaxOpenConns: 4, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (migrating up from 116): %v", err)
	}
	defer st.Close()

	authority := func(hash string) string {
		t.Helper()
		row, err := st.OperatorTokenDefGetByTokenHash(ctx, hash)
		if err != nil {
			t.Fatalf("lookup %s: %v", hash, err)
		}
		return row.Authority
	}
	if got := authority("h-old"); got != "" {
		t.Errorf("pre-0117 token authority after migrating = %q, want frozen (\"\")", got)
	}
	if _, err := st.OperatorTokenDefCreate(ctx, store.OperatorTokenDefRow{
		DefID: "def_new", Name: "u-alice", TenantID: "acme", Subject: "alice", TokenHash: "h-new",
		AllowedScopes: []string{"substrate:user"}, Authority: "inherit",
	}); err != nil {
		t.Fatalf("mint inherit after migrating: %v", err)
	}
	if got := authority("h-new"); got != "inherit" {
		t.Errorf("inherit token authority = %q, want inherit", got)
	}

	down, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMigrator(down)
	if err := down.Migrate(116); err != nil {
		t.Fatalf("migrate down to 116: %v", err)
	}
	var cols int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'operator_token_defs' AND column_name = 'authority'`, schema).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatalf("authority column still present after the down migration")
	}
	if err := down.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if got := authority("h-new"); got != "" {
		t.Errorf("after down + up: authority = %q, want frozen (the mode went with the column)", got)
	}
}
