package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMigrate_LegacyAgentDefReadsAsNotOperatorAuthored is the upgrade case, and
// it is the one a fresh-DB test cannot see.
//
// A deployment that upgrades has rows written before the column existed. They
// must read as NOT operator-authored — the safe direction, because the flag is
// what gates the widened prompt-expansion families: a row that defaulted the
// other way would hand every pre-existing agent-authored def an authority it
// was never granted.
//
// Builds the pre-column schema on disk, writes a row into it, then reopens so
// the ALTER runs against real legacy data.
func TestMigrate_LegacyAgentDefReadsAsNotOperatorAuthored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy_authorship.db")
	ctx := context.Background()

	// 1. Stand up the schema as it was BEFORE operator_authored, and put a row
	//    in it the way a pre-upgrade deployment would have.
	{
		s, err := Open(path)
		if err != nil {
			t.Fatalf("initial open: %v", err)
		}
		for _, stmt := range []string{
			`DROP TABLE agent_def_active`,
			`DROP TABLE agent_defs`,
			`CREATE TABLE agent_defs (
				def_id                    TEXT    PRIMARY KEY,
				name                      TEXT    NOT NULL,
				version                   INTEGER NOT NULL,
				parent_def_id             TEXT    REFERENCES agent_defs(def_id),
				definition                TEXT    NOT NULL,
				description               TEXT,
				created_at                INTEGER NOT NULL,
				created_by_agent_id       TEXT,
				created_by_run_id         TEXT,
				retired                   INTEGER NOT NULL DEFAULT 0,
				bootstrapped_from_static  INTEGER NOT NULL DEFAULT 0,
				content_sha256            TEXT,
				tenant_id                 TEXT    NOT NULL DEFAULT '',
				UNIQUE(tenant_id, name, version)
			)`,
			`CREATE TABLE agent_def_active (
				tenant_id             TEXT    NOT NULL DEFAULT '',
				name                  TEXT    NOT NULL,
				def_id                TEXT    NOT NULL REFERENCES agent_defs(def_id),
				promoted_at           INTEGER NOT NULL,
				promoted_by_agent_id  TEXT,
				PRIMARY KEY (tenant_id, name)
			)`,
			`INSERT INTO agent_defs(def_id, name, version, definition, created_at, tenant_id)
			 VALUES ('adf_legacy', 'legacy-agent', 1, '{"tier":"middle"}', 1, '')`,
		} {
			if _, err := s.db.ExecContext(ctx, stmt); err != nil {
				t.Fatalf("legacy schema %q: %v", stmt[:40], err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}

	// 2. Reopen — the ALTER runs against a table that already holds a row.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (the migration): %v", err)
	}
	defer s.Close()

	row, err := s.AgentDefGet(ctx, "adf_legacy")
	if err != nil {
		t.Fatalf("read the legacy row after migrating: %v", err)
	}
	if row.OperatorAuthored {
		t.Error("a PRE-MIGRATION row reads as operator-authored — nothing may gain authority " +
			"by having predated the column")
	}
	if row.Name != "legacy-agent" {
		t.Errorf("the migration disturbed the row: name = %q", row.Name)
	}

	// 3. And a row written AFTER the migration still records authorship, so the
	//    ALTER did not leave the column write-only.
	created, err := s.AgentDefCreate(ctx, store.AgentDefRow{
		DefID: "adf_new", Name: "new-agent", Definition: json.RawMessage(`{"tier":"middle"}`),
		OperatorAuthored: true,
	})
	if err != nil {
		t.Fatalf("create after migrating: %v", err)
	}
	back, err := s.AgentDefGet(ctx, created.DefID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !back.OperatorAuthored {
		t.Error("a row written after the migration lost its authorship — the column is write-only")
	}
}
