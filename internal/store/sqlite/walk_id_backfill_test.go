package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestMigrate_BackfillsWalkIDOnRunsWrittenBeforeTheColumn: a walk member
// written before runs.walk_id existed carries its walk only inside
// parent_context. After the upgrade it is listed with its walk like a member
// written today — and a parent_context that is not JSON does not stop the
// database from opening.
func TestMigrate_BackfillsWalkIDOnRunsWrittenBeforeTheColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pre-walk-id.db")

	// Build a database, then take the column (and its index) back out, so the
	// runs table has exactly the shape an older binary left on disk.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP INDEX runs_by_walk`,
		`ALTER TABLE runs DROP COLUMN walk_id`,
		`INSERT INTO sessions(id, tenant_id, agent, created_at) VALUES ('s_1', 't', 'team:w', 1)`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_walk', 's_1', 'completed', 10, 't', NULL)`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_member', 's_1', 'completed', 20, 't', '{"walk_id":"r_walk","wave_index":0,"state":"a","state_visit":1}')`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_bystander', 's_1', 'completed', 30, 't', '{"root_agent_run_id":"r_walk","wave_index":0}')`,
		`INSERT INTO runs(id, session_id, status, started_at, tenant_id, parent_context)
		 VALUES ('r_garbled', 's_1', 'completed', 40, 't', '{"walk_id":"r_walk"')`,
	} {
		if _, err := seed.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("Open after the upgrade: %v", err)
	}
	defer st.Close()
	got, next, err := st.ListRunsByWalk(context.Background(), "t", "r_walk", 10, "")
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
}
