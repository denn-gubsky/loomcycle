package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC FN P2 — a new edition of a work reconciles onto the held one part by part.

const edition1 = `# Act

## Article 1

Scope text.

## Article 2

Duties text A.

### Paragraph 1

Para text.

## Article 3

Penalties text.

## Annex

Old annex.
`

// Article 1 and its paragraph unchanged; Article 2 changed; Article 3 renumbered to
// 4 with the same text; the Annex removed; a Schedule added.
const edition2 = `# Act

## Article 1

Scope text.

## Article 2

Duties text B.

### Paragraph 1

Para text.

## Article 4

Penalties text.

## Schedule

New schedule.
`

func reimport(t *testing.T, d *Document, ctx context.Context, md, workKey string, edition int64) map[string]any {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md, "reimport": true, "work_key": workKey, "edition_date": edition})
	out, r := docExec(t, d, ctx, string(b))
	if r.IsError {
		t.Fatalf("reimport: %s", r.Text)
	}
	return out
}

func resolvePart(t *testing.T, d *Document, ctx context.Context, wk, anchor string, asOf int64) string {
	t.Helper()
	q := map[string]any{"op": "resolve_anchor", "scope": "user", "work_key": wk, "anchor": anchor}
	if asOf != 0 {
		q["as_of"] = asOf
	}
	b, _ := json.Marshal(q)
	out, r := docExec(t, d, ctx, string(b))
	if r.IsError {
		t.Fatalf("resolve_anchor %s: %s", anchor, r.Text)
	}
	id, _ := out["chunk_id"].(string)
	return id
}

type partRow struct{ id, parent, anchor, stable string }

func currentPartsOf(t *testing.T, d *Document, ctx context.Context, docID string, at int64) map[string]partRow {
	t.Helper()
	key, _, _ := d.resolveScope(ctx, "user")
	res, err := d.query(ctx, key, `SELECT c.id, COALESCE(c.parent_id, ''), COALESCE(c.anchor, ''), COALESCE(c.stable_id, '')
		FROM chunks c JOIN documents dd ON dd.id = c.document_id
		WHERE c.document_id = ? AND c.id <> dd.root_chunk_id AND (c.invalid_at IS NULL OR c.invalid_at > ?)`, docID, at)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]partRow{}
	for _, r := range res.Rows {
		p := partRow{asStr(r[0]), asStr(r[1]), asStr(r[2]), asStr(r[3])}
		if _, dup := out[p.anchor]; dup {
			t.Fatalf("two parts in force share the anchor %q", p.anchor)
		}
		out[p.anchor] = p
	}
	return out
}

func TestReimport_TwoEditionsOfOneAct(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const wk = "law:ua/1"
		const ed1, ed2 = int64(1_000_000), int64(2_000_000)
		first := reimport(t, d, ctx, edition1, wk, ed1)
		docID := first["document_id"].(string)
		v1 := currentPartsOf(t, d, ctx, docID, ed1+1)
		if len(v1) != 5 || v1["article-2/paragraph-1"].parent != v1["article-2"].id {
			t.Fatalf("edition 1 anchors = %+v", v1)
		}

		// Three statements touching Article 2:
		//  - a manual citation from Article 1 (follows the new Article 2 unconditionally),
		//  - a rule-made one whose quote survives into edition 2 (follows it),
		//  - a rule-made one whose quote does not (stays, proposed, evidence stale).
		key, mscope, _ := d.resolveScope(ctx, "user")
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites",
			"evidence":[{"chunk_id":%q,"quote":"Scope text"}]}`, v1["article-1"].id, v1["article-2"].id, v1["article-1"].id)); r.IsError {
			t.Fatal(r.Text)
		}
		rule := func(to, quote string) string {
			res, refuse, err := d.writeStatement(ctx, key, mscope, edgeWrite{
				from: v1["article-2"].id, to: to, kind: "implements", method: "rule:test", status: edgeStatusAsserted,
				evidence: []edgeEvidenceIn{{ChunkID: v1["article-2"].id, Quote: quote}},
			})
			if err != nil || refuse != nil {
				t.Fatalf("rule statement: %v %v", err, refuse)
			}
			return res.ID
		}
		survives := rule(v1["article-3"].id, "Duties text")
		dropped := rule(v1["annex"].id, "Duties text A")

		second := reimport(t, d, ctx, edition2, wk, ed2)
		rep := second["reimport"].(map[string]any)
		want := map[string]float64{"unchanged": 2, "superseded": 1, "moved": 1, "closed": 1, "created": 1, "statements_copied": 2, "statements_proposed": 1}
		for k, v := range want {
			if rep[k] != v {
				t.Errorf("%s = %v, want %v (report %v)", k, rep[k], v, rep)
			}
		}
		if second["document_id"] != docID {
			t.Fatalf("the edition landed in another document")
		}

		v2 := currentPartsOf(t, d, ctx, docID, ed2+1)
		if len(v2) != 5 {
			t.Fatalf("parts in force after edition 2 = %d: %+v", len(v2), v2)
		}
		// Unchanged parts keep their ids; the paragraph now hangs off the new Article 2.
		if v2["article-1"].id != v1["article-1"].id || v2["article-2/paragraph-1"].id != v1["article-2/paragraph-1"].id {
			t.Errorf("an unchanged part was recreated")
		}
		if v2["article-2/paragraph-1"].parent != v2["article-2"].id {
			t.Errorf("the paragraph did not follow its superseded parent")
		}
		// The changed part: a new chunk with the old one's stable id; the old one is
		// what the act said before the edition.
		if v2["article-2"].id == v1["article-2"].id || v2["article-2"].stable != v1["article-2"].stable {
			t.Errorf("Article 2 = %+v, edition 1 = %+v", v2["article-2"], v1["article-2"])
		}
		if got := resolvePart(t, d, ctx, wk, "article-2", ed1+1); got != v1["article-2"].id {
			t.Errorf("article-2 as of edition 1 = %s, want the old part", got)
		}
		if got := resolvePart(t, d, ctx, wk, "article-2", 0); got != v2["article-2"].id {
			t.Errorf("article-2 now = %s, want the new part", got)
		}
		// The renumbered part kept its id and is still found by its first anchor.
		if v2["article-4"].id != v1["article-3"].id {
			t.Errorf("Article 3 → 4 was not recognised as a move")
		}
		if got := resolvePart(t, d, ctx, wk, "article-3", 0); got != v1["article-3"].id {
			t.Errorf("the old anchor no longer resolves through stable_id: %s", got)
		}
		if _, ok := v2["annex"]; ok {
			t.Errorf("the removed Annex is still in force")
		}

		// Statements: the citation and the surviving rule followed the new Article 2;
		// the dropped one stayed on the old part, proposed, its evidence stale.
		edges := edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q,"include_refuted":true}`, docID))
		var cites, copiedRule bool
		for _, e := range edges {
			attrs, _ := e["attributes"].(map[string]any)
			switch {
			case e["kind"] == "cites" && e["to_id"] == v2["article-2"].id:
				cites = true
			case e["kind"] == "implements" && e["from_id"] == v2["article-2"].id && attrs["revision_of"] == survives:
				copiedRule = true
			case e["id"] == dropped:
				if e["status"] != "proposed" {
					t.Errorf("the dropped statement is %v, want proposed", e["status"])
				}
			}
		}
		if !cites || !copiedRule {
			t.Errorf("statements did not follow the new part: cites=%v rule=%v (%v)", cites, copiedRule, edges)
		}
		res, err := d.query(ctx, key, `SELECT stale FROM chunk_edge_evidence WHERE edge_id = ?`, dropped)
		if err != nil || len(res.Rows) != 1 || asInt(res.Rows[0][0]) != 1 {
			t.Errorf("the dropped statement's evidence is not marked stale: %v %v", res, err)
		}

		// Idempotent: the same edition again changes nothing.
		again := reimport(t, d, ctx, edition2, wk, ed2)["reimport"].(map[string]any)
		if again["unchanged"] != float64(5) || again["created"] != float64(0) || again["superseded"] != float64(0) {
			t.Errorf("a repeated reimport changed something: %v", again)
		}
		if n := len(currentPartsOf(t, d, ctx, docID, ed2+1)); n != 5 {
			t.Errorf("a repeated reimport duplicated parts: %d", n)
		}
	})
}

// A cited work the scope does not hold is filed as a placeholder; importing the
// work later reconciles onto it and clears the mark.
func TestResolveAnchor_PlaceholderBecomesTheWork(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	const wk = "law:ua/2"
	out, r := docExec(t, d, ctx, `{"op":"resolve_anchor","scope":"user","work_key":"law:ua/2"}`)
	if r.IsError || out["found"] != false {
		t.Fatalf("unheld work: %v %s", out, r.Text)
	}
	out, r = docExec(t, d, ctx, `{"op":"resolve_anchor","scope":"user","work_key":"law:ua/2","create_placeholder":true}`)
	if r.IsError || out["placeholder"] != true {
		t.Fatalf("placeholder: %v %s", out, r.Text)
	}
	docID := out["document_id"].(string)
	again, _ := docExec(t, d, ctx, `{"op":"resolve_anchor","scope":"user","work_key":"law:ua/2","create_placeholder":true}`)
	if again["document_id"] != docID {
		t.Fatalf("a second resolve filed a second placeholder")
	}
	got := reimport(t, d, ctx, edition1, wk, 1)
	if got["document_id"] != docID {
		t.Fatalf("the import did not land on the placeholder: %v", got["document_id"])
	}
	meta, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_document","scope":"user","id":%q}`, docID))
	if meta["status"] == placeholderStatus {
		t.Errorf("the placeholder mark survived the import")
	}
	if id := resolvePart(t, d, ctx, wk, "article-1", 0); id == "" {
		t.Errorf("the imported work's parts do not resolve")
	}
}

// A work key held by another document is refused, never moved.
func TestReimport_WorkKeyHeldElsewhereIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	reimport(t, d, ctx, edition1, "law:ua/3", 1)
	other, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Other"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": edition2, "reimport": true,
		"document_id": other["document_id"], "work_key": "law:ua/3"})
	if _, r := docExec(t, d, ctx, string(b)); !r.IsError {
		t.Errorf("a second document took a work key another holds")
	}
}

func TestHeadingSegment_DropsLetterlessSegments(t *testing.T) {
	for in, want := range map[string]string{
		"Article 12":         "article-12",
		"1.2":                "",
		"Стаття 5. Загальні": "стаття-5-загальні",
		"  Notes  ":          "notes",
	} {
		if got := headingSegment(in); got != want {
			t.Errorf("headingSegment(%q) = %q, want %q", in, got, want)
		}
	}
}

// A work held by a document the caller may not read: the refusal says the key is
// taken (unavoidable) but not where.
func TestResolveAnchor_UnreadableWorkDoesNotNameItsDocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": edition1, "reimport": true,
		"work_key": "law:ua/9", "path": "/kb/secret/act"})
	if _, r := docExec(t, d, open, string(b)); r.IsError {
		t.Fatal(r.Text)
	}
	denied := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read", "write"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	_, r := docExec(t, d, denied, `{"op":"resolve_anchor","scope":"tenant","work_key":"law:ua/9","anchor":"article-1"}`)
	if !r.IsError || strings.Contains(r.Text, "/kb/secret") || strings.Contains(r.Text, "chunk_id") {
		t.Errorf("resolve_anchor over an unreadable work: %s", r.Text)
	}
	b, _ = json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": edition2, "reimport": true, "work_key": "law:ua/9"})
	_, r = docExec(t, d, denied, string(b))
	if !r.IsError || strings.Contains(r.Text, "/kb/secret") {
		t.Errorf("reimport over an unreadable work: %s", r.Text)
	}
}
