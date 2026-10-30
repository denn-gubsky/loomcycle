package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// confidence is 0..1 on every write path.
func TestLinkChunks_RefusesAConfidenceOutsideZeroToOne(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, a, b, _ := stmtDoc(t, d, ctx, "range")
	for _, c := range []string{"7", "-0.1"} {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","confidence":%s}`, a, b, c)); !r.IsError {
			t.Errorf("link_chunks stored confidence %s", c)
		}
	}
}

// A writer's own confidence is recorded and never withholds; a judge's verdict
// is stored apart from it, so judging does not erase what the writer said.
func TestStatementVisibility_WritersConfidenceNeverWithholds(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		docID, a, b, c := stmtDoc(t, d, ctx, "unsure")
		low, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","confidence":0.1}`, a, b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		judged, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","confidence":0.9,
			"evidence":[{"chunk_id":%q,"quote":"Article 12"}]}`, a, c, a))
		if r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docExec(t, d, inRunCtx(ctx), fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"unsupported","reason":"wrong article"}`, judged["id"])); r.IsError {
			t.Fatal(r.Text)
		}
		edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q}`, docID))
		if len(edges) != 1 || edges[0]["id"] != low["id"] {
			t.Errorf("get_edges = %v, want only the unsure-but-unjudged link", edges)
		}
		s := integrityEdge(t, d, ctx, "user", judged["id"].(string))
		if s["confidence"] != 0.9 || s["verdict_confidence"] != confidenceUnsupported {
			t.Errorf("the verdict overwrote the writer's confidence: %v", s)
		}
	})
}

// A scope already in the statement shape gains verdict_confidence, and a verdict
// stored in `confidence` before the split moves there — so it still withholds.
func TestMigrateEdgeStatements_MovesAStoredVerdictToItsOwnColumn(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key, _, err := d.resolveScope(ctx, "user")
		if err != nil {
			t.Fatal(err)
		}
		cols := strings.Replace(chunkEdgesColumns, ", verdict_confidence DOUBLE PRECISION", "", 1)
		if cols == chunkEdgesColumns {
			t.Fatalf("staging: chunkEdgesColumns no longer names verdict_confidence")
		}
		for _, s := range []string{
			`CREATE TABLE chunk_edges (` + cols + `)`,
			`INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, confidence, verified_at) VALUES ('j', 'a', 'b', 'cites', 1, 0.1, 2)`,
			`INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, confidence) VALUES ('w', 'a', 'c', 'cites', 1, 0.1)`,
		} {
			if err := d.exec(ctx, key, s); err != nil {
				t.Fatalf("stage: %v", err)
			}
		}
		d.SqlMem.ForgetEnsured(key)
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatalf("ensureSchema: %v", err)
		}
		res, err := d.query(ctx, key, `SELECT e.id FROM chunk_edges e WHERE `+edgeLiveClause+` ORDER BY e.id`)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 1 || asStr(res.Rows[0][0]) != "w" {
			t.Errorf("live after the split = %v, want only the unjudged w (j's verdict withholds it)", res.Rows)
		}
	})
}
