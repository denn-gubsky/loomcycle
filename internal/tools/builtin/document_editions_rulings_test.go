package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// A keyed reimport carries an operator's ruling on a statement: it neither
// demotes what an operator accepted nor drops the ruling from the copy.

// rulingEd2 rewrites Article 1 so the quote "One cites Two" is gone; "Two and
// Three" still occurs. Articles 2 and 3 are unchanged.
const (
	rulingEd1 = "# Act\n\n## Article 1\n\nOne cites Two and Three.\n\n## Article 2\n\nTwo.\n\n## Article 3\n\nThree.\n"
	rulingEd2 = "# Act\n\n## Article 1\n\nOne now refers to Two and Three.\n\n## Article 2\n\nTwo.\n\n## Article 3\n\nThree.\n"
)

// rulingFixture imports rulingEd1 and returns the parts in force.
func rulingFixture(t *testing.T, d *Document, ctx context.Context, wk string) (docID string, v1 map[string]partRow) {
	t.Helper()
	docID = reimport(t, d, ctx, rulingEd1, wk, 1_000_000)["document_id"].(string)
	return docID, currentPartsOf(t, d, ctx, docID, 1_000_001)
}

// seedFromArticle1 writes a rule-made statement from Article 1, its evidence
// quoting Article 1.
func seedFromArticle1(t *testing.T, d *Document, ctx context.Context, v1 map[string]partRow, to, kind, status, quote string) string {
	t.Helper()
	key, mscope, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
		from: v1["article-1"].id, to: to, kind: kind, method: "rule:citation", status: status,
		evidence: []edgeEvidenceIn{{ChunkID: v1["article-1"].id, Quote: quote}},
	})
	if err != nil || refuse != nil {
		t.Fatalf("seed %s: %v %v", kind, err, refuse)
	}
	return res.ID
}

func ruleOn(t *testing.T, d *Document, ctx context.Context, op, id string) {
	t.Helper()
	if _, r := docExec(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":%q,"scope":"user","id":%q,"reason":"ruled"}`, op, id)); r.IsError {
		t.Fatalf("%s: %s", op, r.Text)
	}
}

func stmtRow(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey, id string) statementRow {
	t.Helper()
	s, err := d.statementByID(ctx, key, id)
	if err != nil || s == nil {
		t.Fatalf("statement %s: %v", id, err)
	}
	return *s
}

// copiesOf returns the statements whose attributes.revision_of names id.
func copiesOf(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey, id string) []statementRow {
	t.Helper()
	res, err := d.query(ctx, key, `SELECT `+statementCols+` FROM chunk_edges e WHERE e.id <> ?`, id)
	if err != nil {
		t.Fatal(err)
	}
	var out []statementRow
	for _, r := range res.Rows {
		s := scanStatement(r)
		if s.Attributes["revision_of"] == id {
			out = append(out, s)
		}
	}
	return out
}

func evidenceStale(t *testing.T, d *Document, ctx context.Context, key sqlmem.ScopeKey, edgeID string) []bool {
	t.Helper()
	res, err := d.query(ctx, key, `SELECT stale FROM chunk_edge_evidence WHERE edge_id = ? ORDER BY id`, edgeID)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]bool, 0, len(res.Rows))
	for _, r := range res.Rows {
		out = append(out, asInt(r[0]) == 1)
	}
	return out
}

// An operator accepted the statement; the edition drops the text its evidence
// quotes. The ruling is a person's, not something a text change disproves: the
// statement is copied onto the new part carrying the ruling, and the original
// stays asserted.
func TestReimport_OperatorAcceptedStatementIsCopiedNotDemoted(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key := userKey(t, d, ctx)
		docID, v1 := rulingFixture(t, d, ctx, "law:ua/ruled-1")
		id := seedFromArticle1(t, d, ctx, v1, v1["article-2"].id, "cites", edgeStatusProposed, "One cites Two")
		ruleOn(t, d, ctx, "accept_statement", id)
		before := stmtRow(t, d, ctx, key, id)
		if before.OperatorRuledAt == nil {
			t.Fatalf("accept left no ruling: %+v", before)
		}

		rep := reimport(t, d, ctx, rulingEd2, "law:ua/ruled-1", 2_000_000)["reimport"].(map[string]any)
		if rep["statements_copied"] != float64(1) || rep["statements_proposed"] != float64(0) {
			t.Errorf("report = %v, want statements_copied 1 and statements_proposed 0", rep)
		}
		if after := stmtRow(t, d, ctx, key, id); after.Status != edgeStatusAsserted || !reflect.DeepEqual(after.OperatorRuledAt, before.OperatorRuledAt) {
			t.Errorf("the reimport undid the operator's acceptance: %+v", after)
		}
		if st := evidenceStale(t, d, ctx, key, id); len(st) != 1 || st[0] {
			t.Errorf("the accepted statement's own evidence was marked stale: %v", st)
		}
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)
		copies := copiesOf(t, d, ctx, key, id)
		if len(copies) != 1 {
			t.Fatalf("copies of the accepted statement = %d, want 1", len(copies))
		}
		c := copies[0]
		if c.FromID != v2["article-1"].id || c.ToID != v1["article-2"].id || c.Status != edgeStatusAsserted ||
			!reflect.DeepEqual(c.OperatorRuledAt, before.OperatorRuledAt) || c.JudgedBy != "operator" {
			t.Errorf("copy = %+v, want asserted on the new Article 1 carrying the ruling %v", c, *before.OperatorRuledAt)
		}
		if st := evidenceStale(t, d, ctx, key, c.ID); len(st) != 1 || !st[0] {
			t.Errorf("the copy's evidence quoting gone text is not stale: %v", st)
		}
	})
}

// An operator rejected the statement. The rejection follows the part, so the
// relation stays refused on the new version, and it never comes back live.
func TestReimport_OperatorRejectedStatementStaysRejected(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key := userKey(t, d, ctx)
		docID, v1 := rulingFixture(t, d, ctx, "law:ua/ruled-2")
		id := seedFromArticle1(t, d, ctx, v1, v1["article-2"].id, "cites", edgeStatusProposed, "Two and Three")
		ruleOn(t, d, ctx, "reject_statement", id)
		before := stmtRow(t, d, ctx, key, id)

		reimport(t, d, ctx, rulingEd2, "law:ua/ruled-2", 2_000_000)
		if after := stmtRow(t, d, ctx, key, id); after.Status != edgeStatusRejected {
			t.Errorf("the rejected statement is now %s", after.Status)
		}
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)
		res, err := d.query(ctx, key, `SELECT `+statementCols+` FROM chunk_edges e WHERE e.from_id = ? AND e.to_id = ? AND e.kind = 'cites'`,
			v2["article-1"].id, v1["article-2"].id)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 1 {
			t.Fatalf("statements on the new Article 1 = %d, want the carried rejection", len(res.Rows))
		}
		c := scanStatement(res.Rows[0])
		if c.Status != edgeStatusRejected || !reflect.DeepEqual(c.OperatorRuledAt, before.OperatorRuledAt) || c.JudgeReason != "ruled" {
			t.Errorf("carried statement = %+v, want rejected with the operator's ruling and reason", c)
		}
	})
}

// A run that reimports cannot use the edition to escape a ruling: the copy of an
// accepted statement still refuses the run's demote, and a rejected relation
// still refuses the run's re-proposal against the new part.
func TestReimport_RunCannotLaunderARulingByReimporting(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key := userKey(t, d, ctx)
		run := statementMethodRun(ctx, "rule")
		docID, v1 := rulingFixture(t, d, ctx, "law:ua/ruled-3")
		accepted := seedFromArticle1(t, d, ctx, v1, v1["article-3"].id, "implements", edgeStatusProposed, "Two and Three")
		rejected := seedFromArticle1(t, d, ctx, v1, v1["article-2"].id, "cites", edgeStatusProposed, "One cites Two")
		ruleOn(t, d, ctx, "accept_statement", accepted)
		ruleOn(t, d, ctx, "reject_statement", rejected)

		reimport(t, d, run, rulingEd2, "law:ua/ruled-3", 2_000_000)
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)

		copies := copiesOf(t, d, ctx, key, accepted)
		if len(copies) != 1 {
			t.Fatalf("copies of the accepted statement = %d, want 1", len(copies))
		}
		_, r := docExec(t, d, run, fmt.Sprintf(`{"op":"maintain_statement","scope":"user","id":%q,"action":"demote","reason":"r","method":"rule:maintain"}`, copies[0].ID))
		wantRefusal(t, "a run's demote of the reimported copy of an accepted statement", r, "statement_operator_ruled")
		if s := stmtRow(t, d, ctx, key, copies[0].ID); s.Status != edgeStatusAsserted {
			t.Errorf("the copy of the accepted statement is %s", s.Status)
		}

		_, r = docExec(t, d, run, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"cites","method":"rule:citation"}`,
			v2["article-1"].id, v1["article-2"].id))
		wantRefusal(t, "a run's re-proposal of a rejected relation against the new part", r, "statement_rejected")
	})
}

// With no ruling the copy is the source row: every column but its id, the
// remapped end, created_at and revision_of — the judge's verdict included, or a
// withheld statement would come back visible. An unruled statement whose quote
// is gone is still demoted, not copied.
func TestReimport_CopyWithoutARulingMatchesItsSourceRow(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		key := userKey(t, d, ctx)
		_, v1 := rulingFixture(t, d, ctx, "law:ua/ruled-4")
		held := seedFromArticle1(t, d, ctx, v1, v1["article-3"].id, "implements", edgeStatusAsserted, "Two and Three")
		gone := seedFromArticle1(t, d, ctx, v1, v1["article-2"].id, "cites", edgeStatusAsserted, "One cites Two")
		attrs, _ := json.Marshal(map[string]any{"note": "kept"})
		if err := d.exec(ctx, key, `UPDATE chunk_edges SET confidence = 0.8, invalid_at = 9000000, actor = 'linker', run_id = 'run-x',
			ontology_version = 3, attributes = ?, verified_at = 111, judged_at = 222, judge_reason = 'checked', judged_by = 'rule:judge',
			verdict_confidence = 0.25 WHERE id = ?`, string(attrs), held); err != nil {
			t.Fatal(err)
		}
		src := stmtRow(t, d, ctx, key, held)

		reimport(t, d, ctx, rulingEd2, "law:ua/ruled-4", 2_000_000)

		copies := copiesOf(t, d, ctx, key, held)
		if len(copies) != 1 {
			t.Fatalf("copies of the held statement = %d, want 1", len(copies))
		}
		got := copies[0]
		delete(got.Attributes, "revision_of")
		got.ID, got.FromID, got.CreatedAt = src.ID, src.FromID, src.CreatedAt
		if !reflect.DeepEqual(got, src) {
			t.Errorf("copy differs from its source:\n got %+v\nwant %+v", got, src)
		}
		if s := stmtRow(t, d, ctx, key, gone); s.Status != edgeStatusProposed || s.OperatorRuledAt != nil || len(copiesOf(t, d, ctx, key, gone)) != 0 {
			t.Errorf("an unruled statement whose quote is gone = %+v, want demoted to proposed and not copied", s)
		}
	})
}
