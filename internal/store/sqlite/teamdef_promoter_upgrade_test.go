package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMigrate_ExistingTeamActivePointerReadsAsNotCaptured is the upgrade case a
// fresh database cannot show: a team promoted before the promoter columns
// existed must come back with NO capture — which the subscription sweep runs
// fully confined — and never as a captured "unrestricted". A promote after the
// upgrade records its capture.
func TestMigrate_ExistingTeamActivePointerReadsAsNotCaptured(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_team_promoter.db")
	ctx := context.Background()

	// The pointer table as it was before the promoter columns, holding a
	// promoted team.
	{
		s, err := Open(path)
		if err != nil {
			t.Fatalf("initial open: %v", err)
		}
		for _, stmt := range []string{
			`DROP TABLE teamdef_active`,
			`CREATE TABLE teamdef_active (
				name                  TEXT    NOT NULL,
				def_id                TEXT    NOT NULL REFERENCES teamdefs(def_id),
				promoted_at           INTEGER NOT NULL,
				promoted_by_agent_id  TEXT,
				tenant_id             TEXT    NOT NULL DEFAULT '',
				PRIMARY KEY(tenant_id, name)
			)`,
			`INSERT INTO teamdefs(def_id, name, version, definition, created_at, tenant_id)
			 VALUES ('tdf_armed', 'armed-team', 1, '{"entry":"a"}', 1, 'acme')`,
			`INSERT INTO teamdef_active(name, def_id, promoted_at, promoted_by_agent_id, tenant_id)
			 VALUES ('armed-team', 'tdf_armed', 1, 'a_old', 'acme')`,
		} {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("legacy schema %q: %v", stmt[:40], err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (the migration): %v", err)
	}
	defer s.Close()

	listed := func() store.TeamDefNameSummary {
		t.Helper()
		names, err := s.TeamDefListNames(ctx)
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
	entries, err := s.SnapshotReadTeamDefActive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Promoter != nil || entries[0].PromotedByAgentID != "a_old" {
		t.Errorf("snapshot read after migrating = %+v, want the one pointer, undisturbed, with no capture", entries)
	}

	confined := store.TeamDefPromoter{OperatorKeyRestricted: true, Isolated: true}
	if err := s.TeamDefSetActive(ctx, "acme", "armed-team", "tdf_armed", "a_new", confined); err != nil {
		t.Fatalf("re-promote after migrating: %v", err)
	}
	if n := listed(); n.ActivePromoter == nil || *n.ActivePromoter != confined {
		t.Errorf("after re-promoting: promoter %+v, want %+v", n.ActivePromoter, confined)
	}
}
