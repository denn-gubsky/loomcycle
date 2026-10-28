package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMigrate0121_ExistingTeamPromoterReadsWithAnUnrecordedACLOwner: a team
// promoted at schema version 116 has its confinement bits captured (0087) and
// no owner. After migrating up its pointer reads back with the bits as they
// were and the owner UNKNOWN — never a recorded "nobody" — and a promote after
// the upgrade records its owner. The down migration undoes it cleanly.
//
// Migrates from 116, the last version before this one on its branch, so the
// test stays valid whichever of 0117–0120 land beside it.
func TestMigrate0121_ExistingTeamPromoterReadsWithAnUnrecordedACLOwner(t *testing.T) {
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
	for _, q := range []string{
		`INSERT INTO teamdefs(def_id, name, version, definition, created_at, tenant_id)
		 VALUES ('tdf_armed', 'armed-team', 1, '{"entry":"a"}', now(), 'acme')`,
		`INSERT INTO teamdef_active(tenant_id, name, def_id, promoted_at, promoted_by_agent_id,
		                            promoter_operator_key_restricted, promoter_isolated)
		 VALUES ('acme', 'armed-team', 'tdf_armed', now(), 'a_old', TRUE, FALSE)`,
	} {
		if _, err := seed.Exec(ctx, q); err != nil {
			seed.Close()
			t.Fatalf("%s: %v", q, err)
		}
	}
	seed.Close()

	st, err := Open(ctx, Config{DSN: storeDSN, MaxOpenConns: 4, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (migrating up from 116): %v", err)
	}
	defer st.Close()

	listed := func() *store.TeamDefPromoter {
		t.Helper()
		names, err := st.TeamDefListNames(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range names {
			if n.Name == "armed-team" {
				return n.ActivePromoter
			}
		}
		t.Fatal("the promoted team is gone from the listing after migrating")
		return nil
	}
	unrecorded := func(what string) {
		t.Helper()
		if p := listed(); p == nil || !p.OperatorKeyRestricted || p.Isolated || p.ACLOwner != nil || !p.ACLOwnerUnrecorded {
			t.Errorf("%s: promoter %+v, want the captured bits with an unrecorded owner", what, p)
		}
	}
	unrecorded("after migrating")
	entries, err := st.SnapshotReadTeamDefActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Promoter == nil || !entries[0].Promoter.ACLOwnerUnrecorded {
		t.Errorf("snapshot read after migrating = %+v, want the owner unrecorded", entries)
	}

	owner := &authz.SubjectRef{TenantID: "acme", Subject: "dev", Class: "member"}
	if err := st.TeamDefSetActive(ctx, "acme", "armed-team", "tdf_armed", "a_new", store.TeamDefPromoter{OperatorKeyRestricted: true, ACLOwner: owner}); err != nil {
		t.Fatalf("re-promote after migrating: %v", err)
	}
	if p := listed(); p == nil || p.ACLOwner == nil || *p.ACLOwner != *owner || p.ACLOwnerUnrecorded {
		t.Errorf("after re-promoting: promoter %+v, want owner %+v recorded", p, *owner)
	}

	down, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMigrator(down)
	if err := down.Migrate(116); err != nil {
		t.Fatalf("migrate down to 116: %v", err)
	}
	if err := down.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	unrecorded("after down + up")
}
