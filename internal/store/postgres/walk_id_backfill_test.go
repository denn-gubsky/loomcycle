package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrate0086_BackfillsWalkIDOnRunsWrittenBeforeTheColumn: a walk member
// written at schema version 85 carries its walk only inside parent_context.
// After migrating up it is listed with its walk like a member written today —
// and a parent_context that is not JSON leaves that row alone instead of
// failing the migration.
func TestMigrate0086_BackfillsWalkIDOnRunsWrittenBeforeTheColumn(t *testing.T) {
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
	if err := m.Migrate(85); err != nil {
		closeMigrator(m)
		t.Fatalf("migrate to 85: %v", err)
	}
	closeMigrator(m)

	seed, err := pgxpool.New(ctx, storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sessions(id, tenant_id, agent, created_at) VALUES ('s_1', 't', 'team:w', now())`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_walk', 's_1', 'completed', now() - interval '4 minutes', 't', NULL)`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_member', 's_1', 'completed', now() - interval '3 minutes', 't', '{"walk_id":"r_walk","wave_index":0,"state":"a","state_visit":1}')`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_bystander', 's_1', 'completed', now() - interval '2 minutes', 't', '{"root_agent_run_id":"r_walk","wave_index":0}')`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_garbled', 's_1', 'completed', now() - interval '1 minute', 't', '{"walk_id":"r_walk"')`,
	} {
		if _, err := seed.Exec(ctx, q); err != nil {
			seed.Close()
			t.Fatalf("%s: %v", q, err)
		}
	}
	seed.Close()

	st, err := Open(ctx, Config{DSN: storeDSN, MaxOpenConns: 4, AutoMigrate: true})
	if err != nil {
		t.Fatalf("Open (migrating up from 85): %v", err)
	}
	defer st.Close()
	got, next, err := st.ListRunsByWalk(ctx, "t", "r_walk", 10, "")
	if err != nil {
		t.Fatalf("ListRunsByWalk: %v", err)
	}
	if next != "" || len(got) != 2 || got[0].ID != "r_walk" || got[1].ID != "r_member" {
		ids := make([]string, len(got))
		for i, r := range got {
			ids[i] = r.ID
		}
		t.Errorf("walk r_walk lists %v (next %q), want [r_walk r_member]", ids, next)
	}

	// The down migration undoes it cleanly, and going up again backfills again.
	down, err := newMigrator(storeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer closeMigrator(down)
	if err := down.Migrate(85); err != nil {
		t.Fatalf("migrate down to 85: %v", err)
	}
	if err := down.Up(); err != nil {
		t.Fatalf("migrate up again: %v", err)
	}
	if again, _, err := st.ListRunsByWalk(ctx, "t", "r_walk", 10, ""); err != nil || len(again) != 2 {
		t.Errorf("after down + up, walk r_walk lists %d runs (err %v), want 2", len(again), err)
	}
}
