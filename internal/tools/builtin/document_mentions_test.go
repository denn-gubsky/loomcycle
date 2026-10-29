package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// RFC FN P3b — prose mentions a subject; a fact states a relation between two.

// subjectNode files a subject the way the memory pass does: a document whose root
// is the entity node (type + subject + entity key).
func subjectNode(t *testing.T, d *Document, ctx context.Context, name string) string {
	t.Helper()
	slug := strings.ToLower(name)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q,"path":"/facts/%s",
		"type":"person","subject":%q,"natural_key":"person:%s"}`, name, slug, name, slug))
	if r.IsError {
		t.Fatalf("subject %s: %s", name, r.Text)
	}
	return out["root_chunk_id"].(string)
}

func TestNameLinks_ALinkToASubjectIsAMention(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		ada := subjectNode(t, d, ctx, "Ada")
		notes, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		other, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Runbook"}`)
		if r.IsError {
			t.Fatal(r.Text)
		}
		p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Meeting",
			"body":"Met [[Ada]] about the [[Runbook]]."}`, notes["document_id"]))
		if r.IsError {
			t.Fatal(r.Text)
		}
		passage := p["id"].(string)
		kinds := map[string]map[string]any{}
		for _, e := range edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"get_edges","scope":"user","document_id":%q,"direction":"out"}`, notes["document_id"])) {
			kinds[e["to_id"].(string)] = e
		}
		m := kinds[ada]
		if m == nil || m["kind"] != "mentions" || m["auto"] != true {
			t.Fatalf("the link to the subject = %v", m)
		}
		ev, _ := m["evidence"].([]any)
		if len(ev) != 1 || ev[0].(map[string]any)["quote"] != "[[Ada]]" || ev[0].(map[string]any)["char_start"] != float64(4) {
			t.Fatalf("mention evidence = %v", m["evidence"])
		}
		if e := kinds[other["root_chunk_id"].(string)]; e == nil || e["kind"] != "references" {
			t.Fatalf("a link to an ordinary document = %v", e)
		}
		// The subject's dossier says where it is mentioned.
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"list_facts","scope":"user","about":%q}`, ada))
		if r.IsError {
			t.Fatal(r.Text)
		}
		mi, _ := out["mentioned_in"].([]any)
		if len(mi) != 1 || mi[0].(map[string]any)["chunk_id"] != passage {
			t.Fatalf("mentioned_in = %v", out["mentioned_in"])
		}
	})
}

// A [[link]] to a subject written as `references` before mentions existed is
// re-kinded on the scope's next provisioning.
func TestMigrateMentionKinds_RekindsOldLinksToSubjects(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ada := subjectNode(t, d, ctx, "Ada")
	notes, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes"}`)
	p, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Old","body":"plain"}`, notes["document_id"]))
	key, _, _ := d.resolveScope(ctx, "user")
	if err := d.exec(ctx, key, `INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, auto, method) VALUES (?, ?, ?, 'references', ?, 1, 'wikilink')`,
		newDocID(), p["id"], ada, time.Now().UnixNano()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := d.migrateMentionKinds(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	res, err := d.query(ctx, key, `SELECT kind FROM chunk_edges WHERE from_id = ? AND to_id = ?`, p["id"], ada)
	if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) != "mentions" {
		t.Fatalf("after the pass: %v %v", res, err)
	}
}

// A fact's relation between its two subjects becomes a statement whose evidence is
// the fact; a reserved kind, or a single subject, records none; a second relation
// of the kind from the same subject to another is reported, not retired.
func TestUpsertChunk_RelationBecomesAStatement(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		dave := subjectNode(t, d, ctx, "Dave")
		shop := subjectNode(t, d, ctx, "Shop")
		cafe := subjectNode(t, d, ctx, "Cafe")
		facts, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts"}`)
		upsertRel := func(key, body, rel string, about ...string) map[string]any {
			ids := `"` + strings.Join(about, `","`) + `"`
			out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":%q,"title":%q,"body":%q,
				"about_ids":[%s],"relation":%q,"confidence":0.8}`, facts["document_id"], key, body, body, ids, rel))
			if r.IsError {
				t.Fatalf("upsert %s: %s", key, r.Text)
			}
			return out
		}
		out := upsertRel("dave|works_at|shop", "Dave works at the shop.", "works_at", dave, shop)
		rel, _ := out["relation"].(map[string]any)
		if rel == nil || rel["status"] != "asserted" {
			t.Fatalf("relation = %v", out)
		}
		st, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, rel["statement_id"]))
		if r.IsError {
			t.Fatal(r.Text)
		}
		s := st["statement"].(map[string]any)
		ev := s["evidence"].([]any)[0].(map[string]any)
		if s["from_id"] != dave || s["to_id"] != shop || s["method"] != "consolidator" || s["confidence"] != 0.8 ||
			ev["chunk_id"] != out["id"] || ev["quote"] != "Dave works at the shop." {
			t.Fatalf("statement = %v", s)
		}
		second := upsertRel("dave|works_at|cafe", "Dave works at the cafe.", "works_at", dave, cafe)
		if second["relation_conflicts"] != float64(1) {
			t.Errorf("a second works_at from Dave was not reported: %v", second)
		}
		if out := upsertRel("dave|x|shop", "Dave replaced the shop.", "supersedes", dave, shop); out["relation"] != nil || out["relation_skipped"] == nil {
			t.Errorf("a reserved relation kind was recorded: %v", out)
		}
		if out := upsertRel("dave|alone", "Dave is tall.", "is", dave); out["relation"] != nil || out["relation_skipped"] == nil {
			t.Errorf("a one-subject fact recorded a relation: %v", out)
		}
	})
}
