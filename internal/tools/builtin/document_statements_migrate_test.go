package builtin

import (
	"context"
	"testing"
)

// A rebuild is the destructive path: it runs only on a positive answer that the
// table predates statements, never over a table that already holds part of the
// statement shape.
func TestMigrateEdgeStatements_RefusesAPartlyStatementShapedTable(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, err := d.resolveScope(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{
			`CREATE TABLE chunk_edges (from_id TEXT NOT NULL, to_id TEXT NOT NULL, kind TEXT NOT NULL, created_at BIGINT NOT NULL, auto INTEGER NOT NULL DEFAULT 0, status TEXT, PRIMARY KEY (from_id, to_id, kind))`,
			`INSERT INTO chunk_edges (from_id, to_id, kind, created_at, auto, status) VALUES ('a', 'b', 'cites', 1, 0, 'rejected')`,
		} {
			if err := d.exec(ctx, key, s); err != nil {
				t.Fatalf("stage: %v", err)
			}
		}
		d.SqlMem.ForgetEnsured(key)
		if err := d.migrateEdgeStatements(ctx, key); err == nil {
			t.Errorf("the rebuild ran over a table that already has a statement column")
		}
		res, err := d.query(ctx, key, `SELECT status FROM chunk_edges`)
		if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) != "rejected" {
			t.Errorf("the staged row was rewritten: %v %v", res, err)
		}
	})
}

// edgeTableNeedsRebuild decides from a positive column listing only.
func TestEdgeTableNeedsRebuild_OnlyOnAPreStatementShape(t *testing.T) {
	set := func(cols ...string) map[string]bool {
		m := map[string]bool{}
		for _, c := range cols {
			m[c] = true
		}
		return m
	}
	old := []string{"from_id", "to_id", "kind", "created_at", "auto"}
	if need, err := edgeTableNeedsRebuild(set(old...)); err != nil || !need {
		t.Errorf("pre-statement table: need=%v err=%v", need, err)
	}
	if need, err := edgeTableNeedsRebuild(set(append(old, "id", "discriminator", "status")...)); err != nil || need {
		t.Errorf("statement table: need=%v err=%v", need, err)
	}
	if _, err := edgeTableNeedsRebuild(set(append(old, "judged_by")...)); err == nil {
		t.Errorf("a partly statement-shaped table was not refused")
	}
	if _, err := edgeTableNeedsRebuild(set()); err == nil {
		t.Errorf("an empty column listing was read as a pre-statement table")
	}
}
