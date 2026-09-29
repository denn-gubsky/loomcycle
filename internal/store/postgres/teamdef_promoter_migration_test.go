package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMigrate0087_ExistingTeamActivePointerReadsAsNotCaptured: a team promoted
// at schema version 86 has no promoter capture. After migrating up its pointer
// reads back with NONE — which the subscription sweep runs fully confined —
// never as a captured "unrestricted", and a promote after the upgrade records
// its capture. The down migration undoes it cleanly.
func TestMigrate0087_ExistingTeamActivePointerReadsAsNotCaptured(t *testing.T) {
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
	if err := m.Migrate(86); err != nil {
		closeMigrator(m)
		t.Fatalf("migrate to 86: %v", err)
	}
	closeMigrator(m)

	seed, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO teamdefs(def_id, name, version, definition, created_at, tenant_id)
		 VALUES ('tdf_armed', 'armed-team', 1, '{"entry":"a"}', now(), 'acme')`,
		`INSERT INTO teamdef_active(tenant_id, name, def_id, promoted_at, promoted_by_agent_id)
		 VALUES ('acme', 'armed-team', 'tdf_armed', now(), 'a_old')`,
	} {
		if _, err := seed.Exec(ctx, q); err != nil {
			seed.Close()
			t.Fatalf("%s: %v", q, err)
		}
	}
	seed.Close()

	st, err := Open(ctx, Config{DSN: storeDSN, MaxOpenConns: 4, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (migrating up from 86): %v", err)
	}
	defer st.Close()

	listed := func() store.TeamDefNameSummary {
		t.Helper()
		names, err := st.TeamDefListNames(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if n.Name == "armed-team" {
				return n
			}
		}
		t.Fatal("the promoted team is gone from the listing after migrating")
		return store.TeamDefNameSummary{}
	}
	if n := listed(); n.ActiveDefID != "tdf_armed" || n.ActivePromoter != nil {
		t.Errorf("after migrating: active %q promoter %+v, want tdf_armed with no capture", n.ActiveDefID, n.ActivePromoter)
	}
	entries, err := st.SnapshotReadTeamDefActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Promoter != nil || entries[0].PromotedByAgentID != "a_old" {
		t.Errorf("snapshot read after migrating = %+v, want the one pointer, undisturbed, with no capture", entries)
	}

	confined := store.TeamDefPromoter{OperatorKeyRestricted: true, Isolated: true}
	if err := st.TeamDefSetActive(ctx, "acme", "armed-team", "tdf_armed", "a_new", confined); err != nil {
		t.Fatalf("re-promote after migrating: %v", err)
	}
	if n := listed(); n.ActivePromoter == nil || *n.ActivePromoter != confined {
		t.Errorf("after re-promoting: promoter %+v, want %+v", n.ActivePromoter, confined)
	}

	down, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMigrator(down)
	if err := down.Migrate(86); err != nil {
		t.Fatalf("migrate down to 86: %v", err)
	}
	if err := down.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if n := listed(); n.ActiveDefID != "tdf_armed" || n.ActivePromoter != nil {
		t.Errorf("after down + up: active %q promoter %+v, want tdf_armed with no capture", n.ActiveDefID, n.ActivePromoter)
	}
}
