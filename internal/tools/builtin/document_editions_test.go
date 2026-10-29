package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
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

// A work key is a claim on the whole scope's namespace: a writer confined to part
// of the scope may not make one, by reimport or by placeholder.
func TestReimport_ConfinedWriterCannotClaimAWorkKey(t *testing.T) {
	d, base, _ := documentFixture(t)
	confined := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": edition1, "reimport": true,
		"work_key": "law:ua/10", "path": "/kb/public/act"})
	if _, r := docExec(t, d, confined, string(b)); !r.IsError {
		t.Errorf("a confined writer claimed a work key by reimport")
	}
	if _, r := docExec(t, d, confined, `{"op":"resolve_anchor","scope":"tenant","work_key":"law:ua/11","create_placeholder":true}`); !r.IsError {
		t.Errorf("a confined writer claimed a work key by placeholder")
	}
	res, _ := docExec(t, d, tenantDocCtx(base, nil), `{"op":"query_documents","scope":"tenant"}`)
	if docs, _ := res["documents"].([]any); len(docs) != 0 {
		t.Errorf("a refused claim left documents behind: %v", docs)
	}
}

// A reimport carries a superseded part's statements only where the caller may
// link both ends: a statement from a document it may not link stays as it was.
func TestReimport_LeavesStatementsItMayNotLinkAlone(t *testing.T) {
	run := func(t *testing.T, deny bool) (copied bool, status any) {
		d, base, _ := documentFixture(t)
		open := tenantDocCtx(base, nil)
		s := seedACLDocs(t, d, open)
		b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": edition1, "reimport": true,
			"work_key": "law:ua/12", "path": "/kb/public/act"})
		out, r := docExec(t, d, open, string(b))
		if r.IsError {
			t.Fatal(r.Text)
		}
		docID := out["document_id"].(string)
		art2 := resolvePartScope(t, d, open, "tenant", "law:ua/12", "article-2")
		stmt, r := docExec(t, d, open, fmt.Sprintf(`{"op":"link_chunks","scope":"tenant","from_id":%q,"to_id":%q,"kind":"implements"}`, s.secretChunk, art2))
		if r.IsError {
			t.Fatal(r.Text)
		}
		ctx := open
		if deny {
			ctx = tenantDocCtx(base, aclSet(t,
				authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
				authz.Rule{Effect: authz.Deny, Actions: []string{"link"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
		}
		b, _ = json.Marshal(map[string]any{"op": "import_md", "scope": "tenant", "markdown": edition2, "reimport": true, "document_id": docID})
		if _, r := docExec(t, d, ctx, string(b)); r.IsError {
			t.Fatal(r.Text)
		}
		for _, e := range edgesOf(t, d, open, fmt.Sprintf(`{"op":"get_edges","scope":"tenant","document_id":%q,"include_refuted":true}`, s.secretDoc)) {
			attrs, _ := e["attributes"].(map[string]any)
			if attrs["revision_of"] == stmt["id"] {
				copied = true
			}
			if e["id"] == stmt["id"] {
				status = e["status"]
			}
		}
		return copied, status
	}
	if copied, _ := run(t, false); !copied {
		t.Fatalf("control: the statement did not follow the new part")
	}
	if copied, status := run(t, true); copied || status != "asserted" {
		t.Errorf("a statement the caller may not link was changed: copied=%v status=%v", copied, status)
	}
}

func resolvePartScope(t *testing.T, d *Document, ctx context.Context, scope, wk, anchor string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"op": "resolve_anchor", "scope": scope, "work_key": wk, "anchor": anchor})
	out, r := docExec(t, d, ctx, string(b))
	if r.IsError {
		t.Fatalf("resolve_anchor: %s", r.Text)
	}
	return out["chunk_id"].(string)
}

// A work key lives in its own column: an ordinary edit of the root, which under a
// confirmed ontology re-derives the root's class identity, must not drop it — or
// the next edition lands in a new document.
func TestReimport_RootEditUnderAnOntologyKeepsTheWorkKey(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = installTenantOntology(t, d, ctx, typedOntologyMD, memrank.OntologyConfirmedStatus)
	const wk = "law:ua/20"
	first := reimport(t, d, ctx, edition1, wk, 1_000_000)
	root := first["root_chunk_id"].(string)
	g, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, root))
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"body":"A preamble.","revision":%d}`,
		root, int(g["revision"].(float64)))); r.IsError {
		t.Fatal(r.Text)
	}
	out, r := docExec(t, d, ctx, `{"op":"resolve_anchor","scope":"user","work_key":"law:ua/20"}`)
	if r.IsError || out["found"] != true || out["document_id"] != first["document_id"] {
		t.Errorf("a root edit dropped the work key: %v %s", out, r.Text)
	}
	if again := reimport(t, d, ctx, edition2, wk, 2_000_000); again["document_id"] != first["document_id"] {
		t.Errorf("the next edition landed in a new document %v (was %v)", again["document_id"], first["document_id"])
	}
}

// A scope whose work keys were claimed into identity_key, before work_key existed,
// migrates them where the claim is recognisable — a placeholder or untyped root —
// and leaves a typed root's identity alone.
func TestMigrateAnchors_CarriesClaimedWorkKeysIntoTheirColumn(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		work := reimport(t, d, ctx, edition1, "law:ua/21", 1)
		ph, r := docExec(t, d, ctx, `{"op":"resolve_anchor","scope":"user","work_key":"law:ua/22","create_placeholder":true}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		typed, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Typed"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		key, _, _ := d.resolveScope(ctx, "user")
		for _, q := range []string{
			`UPDATE chunks SET identity_key = work_key`,
			`UPDATE chunks SET identity_key = 'Policy:typed', type = 'Policy' WHERE id = '` + typed["root_chunk_id"].(string) + `'`,
			`DROP INDEX chunks_work_key`,
			`ALTER TABLE chunks DROP COLUMN work_key`,
		} {
			if err := d.exec(ctx, key, q); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		d.SqlMem.ForgetEnsured(key)
		if err := d.ensureSchema(ctx, key); err != nil {
			t.Fatal(err)
		}
		res, err := d.query(ctx, key, `SELECT id, COALESCE(work_key, '') FROM chunks WHERE id IN (?, ?, ?)`,
			work["root_chunk_id"], ph["root_chunk_id"], typed["root_chunk_id"])
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, row := range res.Rows {
			got[asStr(row[0])] = asStr(row[1])
		}
		if got[work["root_chunk_id"].(string)] != "law:ua/21" || got[ph["root_chunk_id"].(string)] != "law:ua/22" {
			t.Errorf("claimed keys were not carried: %v", got)
		}
		if got[typed["root_chunk_id"].(string)] != "" {
			t.Errorf("a typed root's class identity was taken for a work key: %v", got)
		}
	})
}

const insertEd1 = "# Act\n\n## Article 1\n\nOne.\n\n## Article 2\n\nTwo.\n\n## Article 3\n\nThree.\n"

// Article 2 inserted, the rest renumbered.
const insertEd2 = "# Act\n\n## Article 1\n\nOne.\n\n## Article 2\n\nInserted.\n\n## Article 3\n\nTwo.\n\n## Article 4\n\nThree.\n"

func partBody(t *testing.T, d *Document, ctx context.Context, id string) string {
	t.Helper()
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, id))
	if r.IsError {
		t.Fatal(r.Text)
	}
	b, _ := out["body"].(string)
	return b
}

func userKey(t *testing.T, d *Document, ctx context.Context) sqlmem.ScopeKey {
	t.Helper()
	k, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Inserting an article renumbers the ones after it: the text keeps its part (and
// everything linked to it), and only the inserted article is new.
func TestReimport_InsertedPartRenumbersTheRestAsMoves(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const wk = "law:ua/30"
		docID := reimport(t, d, ctx, insertEd1, wk, 1_000_000)["document_id"].(string)
		v1 := currentPartsOf(t, d, ctx, docID, 1_000_001)
		_, citing, _, _ := stmtDoc(t, d, ctx, "Other")
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, citing, v1["article-2"].id)); r.IsError {
			t.Fatal(r.Text)
		}
		rep := reimport(t, d, ctx, insertEd2, wk, 2_000_000)["reimport"].(map[string]any)
		for k, v := range map[string]float64{"unchanged": 1, "moved": 2, "created": 1, "superseded": 0, "closed": 0} {
			if rep[k] != v {
				t.Errorf("%s = %v, want %v (report %v)", k, rep[k], v, rep)
			}
		}
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)
		if v2["article-3"].id != v1["article-2"].id || v2["article-4"].id != v1["article-3"].id {
			t.Errorf("renumbering was not a move: v1=%+v v2=%+v", v1, v2)
		}
		if got := partBody(t, d, ctx, v2["article-2"].id); got != "Inserted." {
			t.Errorf("article-2 now = %q", got)
		}
		// stable ids follow the text: the inserted article does not take the stable id
		// "Two." has carried since the first edition.
		if v2["article-3"].stable != v1["article-2"].stable || v2["article-2"].stable == v1["article-2"].stable {
			t.Errorf("stable ids did not follow the text: v1=%+v v2=%+v", v1, v2)
		}
		res, err := d.query(ctx, userKey(t, d, ctx), `SELECT to_id FROM chunk_edges WHERE from_id = ? AND kind = 'cites'`, citing)
		if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) != v1["article-2"].id {
			t.Errorf("the citation of \"Two.\" moved: %v %v", res, err)
		}
	})
}

// A statement between two parts that both change is copied once, onto the two new
// versions — not once per end.
func TestReimport_StatementBetweenTwoChangedPartsIsCopiedOnce(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const wk = "law:ua/31"
		docID := reimport(t, d, ctx, insertEd1, wk, 1_000_000)["document_id"].(string)
		v1 := currentPartsOf(t, d, ctx, docID, 1_000_001)
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, v1["article-1"].id, v1["article-2"].id)); r.IsError {
			t.Fatal(r.Text)
		}
		ed := "# Act\n\n## Article 1\n\nOne changed.\n\n## Article 2\n\nTwo changed.\n\n## Article 3\n\nThree.\n"
		rep := reimport(t, d, ctx, ed, wk, 2_000_000)["reimport"].(map[string]any)
		if rep["statements_copied"] != float64(1) {
			t.Errorf("statements_copied = %v, want 1 (%v)", rep["statements_copied"], rep)
		}
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)
		res, err := d.query(ctx, userKey(t, d, ctx), `SELECT from_id, to_id FROM chunk_edges WHERE kind = 'cites'`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[[2]string]bool{}
		for _, row := range res.Rows {
			got[[2]string{asStr(row[0]), asStr(row[1])}] = true
		}
		want := map[[2]string]bool{{v1["article-1"].id, v1["article-2"].id}: true, {v2["article-1"].id, v2["article-2"].id}: true}
		if len(got) != len(res.Rows) || len(got) != 2 || !got[[2]string{v2["article-1"].id, v2["article-2"].id}] {
			t.Errorf("cites statements = %v, want %v", got, want)
		}
	})
}

// A [[link]] belongs to the text that makes it: an edition that drops it must not
// carry it to the new version.
func TestReimport_WikilinkTheEditionDroppedDoesNotSurvive(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		tgt, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Target","path":"/t/target"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		target := tgt["root_chunk_id"].(string)
		const wk = "law:ua/32"
		docID := reimport(t, d, ctx, "# Act\n\n## Article 1\n\nSee [[/t/target]] here.\n", wk, 1_000_000)["document_id"].(string)
		v1 := currentPartsOf(t, d, ctx, docID, 1_000_001)
		key := userKey(t, d, ctx)
		if res, err := d.query(ctx, key, `SELECT 1 FROM chunk_edges WHERE from_id = ? AND to_id = ?`, v1["article-1"].id, target); err != nil || len(res.Rows) != 1 {
			t.Fatalf("control: edition 1 does not link the target: %v %v", res, err)
		}
		reimport(t, d, ctx, "# Act\n\n## Article 1\n\nNo link any more.\n", wk, 2_000_000)
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)
		res, err := d.query(ctx, key, `SELECT auto, method FROM chunk_edges WHERE from_id = ? AND to_id = ?`, v2["article-1"].id, target)
		if err != nil || len(res.Rows) != 0 {
			t.Errorf("the new edition no longer links the target, yet carries %v (%v)", res.Rows, err)
		}
	})
}

// A reimport interrupted after a part's new version was written, but before its
// statements were carried and the old version closed, is completed by running it
// again.
func TestReimport_RerunCompletesAnInterruptedSupersession(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const wk = "law:ua/33"
		docID := reimport(t, d, ctx, insertEd1, wk, 1_000_000)["document_id"].(string)
		v1 := currentPartsOf(t, d, ctx, docID, 1_000_001)
		_, citing, _, _ := stmtDoc(t, d, ctx, "Other")
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, citing, v1["article-2"].id)); r.IsError {
			t.Fatal(r.Text)
		}
		ed := "# Act\n\n## Article 1\n\nOne.\n\n## Article 2\n\nTwo changed.\n\n## Article 3\n\nThree.\n"
		reimport(t, d, ctx, ed, wk, 2_000_000)
		key := userKey(t, d, ctx)
		// Undo the tail of the run: the copied statement, and the old version's close.
		for _, q := range []string{
			`DELETE FROM chunk_edges WHERE kind = 'cites' AND to_id <> '` + v1["article-2"].id + `'`,
			`UPDATE chunks SET invalid_at = NULL WHERE id = '` + v1["article-2"].id + `'`,
		} {
			if err := d.exec(ctx, key, q); err != nil {
				t.Fatal(err)
			}
		}
		reimport(t, d, ctx, ed, wk, 2_000_000)
		v2 := currentPartsOf(t, d, ctx, docID, 2_000_001)
		if _, still := v2["article-2"]; !still || v2["article-2"].id == v1["article-2"].id {
			t.Fatalf("article-2 in force = %+v", v2["article-2"])
		}
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edges WHERE from_id = ? AND to_id = ? AND kind = 'cites'`, citing, v2["article-2"].id)
		if err != nil || asInt(res.Rows[0][0]) != 1 {
			t.Errorf("the rerun did not carry the citation: %v %v", res, err)
		}
		if res, err = d.query(ctx, key, `SELECT COUNT(*) FROM chunks WHERE document_id = ?`, docID); err != nil || asInt(res.Rows[0][0]) != 5 {
			t.Errorf("the rerun made extra parts: %v %v", res, err)
		}
	})
}

// An edition older than the one in force is refused: reconciling it would replace
// the current text with the older one.
func TestReimport_BackdatedEditionIsRefused(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const wk = "law:ua/34"
		mk := func(b string) string { return "# Act\n\n## Article 1\n\n" + b + "\n" }
		reimport(t, d, ctx, mk("V1."), wk, 1_000_000)
		reimport(t, d, ctx, mk("V3."), wk, 3_000_000)
		b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": mk("V2."), "reimport": true, "work_key": wk, "edition_date": 2_000_000})
		_, r := docExec(t, d, ctx, string(b))
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryBusiness || !strings.Contains(r.Text, "edition_date") {
			t.Errorf("a back-dated edition was not refused: %s", r.Text)
		}
		if id := resolvePart(t, d, ctx, wk, "article-1", 0); id == "" || partBody(t, d, ctx, id) != "V3." {
			t.Errorf("the newest edition is no longer in force")
		}
		// The edition in force, imported again, is still a no-op.
		if rep := reimport(t, d, ctx, mk("V3."), wk, 3_000_000)["reimport"].(map[string]any); rep["unchanged"] != float64(1) {
			t.Errorf("re-running the current edition: %v", rep)
		}
	})
}

// Reimports of one work run one at a time: two at once each read the held parts
// before the other wrote, and left two parts in force under one anchor — or, for a
// work's first edition, a second document with no key.
func TestReimport_ConcurrentReimportsOfOneWorkSerialise(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		const n = 4
		run := func(md, wk string, edition int64) {
			var wg sync.WaitGroup
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					b, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md, "reimport": true, "work_key": wk, "edition_date": edition})
					if r, err := d.Execute(ctx, b); err != nil || r.IsError {
						t.Errorf("concurrent reimport: %v %s", err, r.Text)
					}
				}()
			}
			wg.Wait()
		}
		// Provision the scope first: concurrent first calls on a fresh scope race the
		// schema migrations, which is not what this test is about.
		if _, r := docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`); r.IsError {
			t.Fatal(r.Text)
		}
		const wk = "law:ua/35"
		run(insertEd1, wk, 1_000_000)
		key := userKey(t, d, ctx)
		res, err := d.query(ctx, key, `SELECT id FROM documents`)
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("concurrent first editions made %d documents (%v)", len(res.Rows), err)
		}
		docID := asStr(res.Rows[0][0])
		if out, r := docExec(t, d, ctx, `{"op":"resolve_anchor","scope":"user","work_key":"law:ua/35"}`); r.IsError || out["document_id"] != docID {
			t.Fatalf("the document does not hold the key: %v %s", out, r.Text)
		}
		run("# Act\n\n## Article 1\n\nOne changed.\n\n## Article 2\n\nTwo.\n\n## Article 3\n\nThree.\n", wk, 2_000_000)
		res, err = d.query(ctx, key, `SELECT anchor, COUNT(*) FROM chunks c JOIN documents dd ON dd.id = c.document_id
			WHERE c.document_id = ? AND c.id <> dd.root_chunk_id AND (c.invalid_at IS NULL OR c.invalid_at > 2000001) GROUP BY anchor`, docID)
		if err != nil || len(res.Rows) != 3 {
			t.Fatalf("anchors in force: %v %v", res, err)
		}
		for _, r := range res.Rows {
			if asInt(r[1]) != 1 {
				t.Errorf("anchor %s has %d parts in force", asStr(r[0]), asInt(r[1]))
			}
		}
	})
}
