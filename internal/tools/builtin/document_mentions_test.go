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

// A fact corrected under its key states what it states NOW: the relation and the
// `about` edges it no longer makes stop being live, so the old relation is not
// reported as a contradiction of the new one, and naming the old subject again
// brings its statement back.
func TestUpsertChunk_ACorrectedFactRetiresItsOldRelationAndAbout(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		dave := subjectNode(t, d, ctx, "Dave")
		shop := subjectNode(t, d, ctx, "Shop")
		cafe := subjectNode(t, d, ctx, "Cafe")
		facts, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts"}`)
		up := func(body, to string) map[string]any {
			out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/fact/dave-work","title":%q,"body":%q,
				"about_ids":[%q,%q],"relation":"works_at"}`, facts["document_id"], body, body, dave, to))
			if r.IsError {
				t.Fatal(r.Text)
			}
			return out
		}
		first := up("Dave works at the shop.", shop)
		fact := first["id"].(string)
		second := up("Dave works at the cafe.", cafe)
		if second["relation_conflicts"] != nil {
			t.Errorf("the fact's own earlier relation was reported as a contradiction: %v", second)
		}
		if got := liveEdgeTargets(t, d, ctx, "user", dave, "works_at"); len(got) != 1 || !got[cafe] {
			t.Errorf("live works_at from Dave = %v, want only the cafe %s", got, cafe)
		}
		if got := liveEdgeTargets(t, d, ctx, "user", fact, "about"); len(got) != 2 || !got[dave] || !got[cafe] {
			t.Errorf("live about edges = %v, want Dave and the cafe", got)
		}
		up("Dave works at the shop.", shop)
		if got := liveEdgeTargets(t, d, ctx, "user", dave, "works_at"); len(got) != 1 || !got[shop] {
			t.Errorf("after the second correction works_at = %v, want only the shop %s", got, shop)
		}
		if got := liveEdgeTargets(t, d, ctx, "user", fact, "about"); len(got) != 2 || !got[dave] || !got[shop] {
			t.Errorf("after the second correction about = %v, want Dave and the shop", got)
		}
		// The statement's evidence is the fact as it reads now.
		key, _, _ := d.resolveScope(ctx, "user")
		res, err := d.query(ctx, key, `SELECT ev.quote FROM chunk_edges e JOIN chunk_edge_evidence ev ON ev.edge_id = e.id
			WHERE e.from_id = ? AND e.to_id = ? AND e.kind = 'works_at' AND ev.chunk_id = ?`, dave, shop, fact)
		if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) != "Dave works at the shop." {
			t.Errorf("evidence from the fact = %v %v", res, err)
		}
	})
}

// A relation recorded from a fact stops being live when the fact is retired or
// deleted — unless another live fact still states it.
func TestUpsertChunk_ARelationEndsWithTheLastFactThatStatesIt(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		dave := subjectNode(t, d, ctx, "Dave")
		shop := subjectNode(t, d, ctx, "Shop")
		cafe := subjectNode(t, d, ctx, "Cafe")
		facts, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts"}`)
		up := func(key, body, rel string, about ...string) string {
			extra := ""
			if len(about) > 0 {
				extra = fmt.Sprintf(`,"about_ids":["%s"],"relation":%q`, strings.Join(about, `","`), rel)
			}
			out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":%q,"title":%q,"body":%q%s}`,
				facts["document_id"], key, body, body, extra))
			if r.IsError {
				t.Fatal(r.Text)
			}
			return out["id"].(string)
		}
		a := up("memory/fact/a", "Dave works at the shop.", "works_at", dave, shop)
		b := up("memory/fact/b", "Dave is employed by the shop.", "works_at", dave, shop)
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_chunk","scope":"user","id":%q}`, a)); r.IsError {
			t.Fatal(r.Text)
		}
		if got := liveEdgeTargets(t, d, ctx, "user", dave, "works_at"); !got[shop] {
			t.Errorf("deleting one of two facts ended a relation the other still states: %v", got)
		}
		c := up("memory/fact/c", "Dave left the shop.", "")
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"supersede_chunk","scope":"user","id":%q,"supersedes_id":%q}`, c, b)); r.IsError {
			t.Fatal(r.Text)
		}
		if got := liveEdgeTargets(t, d, ctx, "user", dave, "works_at"); got[shop] {
			t.Errorf("a relation outlived the retirement of the last fact stating it: %v", got)
		}
		k := up("memory/fact/k", "Dave knows the cafe.", "knows", dave, cafe)
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_chunk","scope":"user","id":%q}`, k)); r.IsError {
			t.Fatal(r.Text)
		}
		if got := liveEdgeTargets(t, d, ctx, "user", dave, "knows"); got[cafe] {
			t.Errorf("a relation outlived the deletion of the only fact stating it: %v", got)
		}
	})
}

// The relation runs between the first two subjects the CALLER named. When one of
// them cannot be linked, the relation is skipped rather than drawn between the
// next two that happened to be written.
func TestUpsertChunk_ARelationUsesTheFirstTwoGivenSubjects(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	dave := subjectNode(t, d, ctx, "Dave")
	shop := subjectNode(t, d, ctx, "Shop")
	facts, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts"}`)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/fact/x","title":"x","body":"Nobody works at the shop.",
		"about_ids":["no-such-chunk",%q,%q],"relation":"works_at"}`, facts["document_id"], dave, shop))
	if r.IsError {
		t.Fatal(r.Text)
	}
	if out["relation"] != nil || out["relation_skipped"] == nil {
		t.Errorf("relation = %v", out)
	}
	if got := liveEdgeTargets(t, d, ctx, "user", dave, "works_at"); got[shop] {
		t.Errorf("the relation was drawn between the second and third subjects: %v", got)
	}
}

// A remembered fact carries a subject and a key with no `|`, but it is a fact, not
// a subject node: a [[link]] to it is a reference, not a mention.
func TestSubjectNodes_ARememberedFactIsNotASubject(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"remember","scope":"user","text":"Ada prefers tea.","subject":"Ada","type":"person"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	key, _, _ := d.resolveScope(ctx, "user")
	if got := d.subjectNodes(ctx, key, []string{out["id"].(string)}); got[out["id"].(string)] {
		t.Errorf("a remembered fact is classified as a subject node")
	}
}

// Mention evidence quotes the link that names the subject — not a longer link that
// starts with the same name — and is written only for an edge that exists.
func TestNameLinks_MentionEvidenceQuotesTheExactLink(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		ada := subjectNode(t, d, ctx, "Ada")
		notes, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes"}`)
		mk := func(body string) string {
			p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"M","body":%q}`, notes["document_id"], body))
			if r.IsError {
				t.Fatal(r.Text)
			}
			return p["id"].(string)
		}
		key, _, _ := d.resolveScope(ctx, "user")
		for body, want := range map[string]string{
			"See [[Ada Lovelace notes]] and [[Ada]].":       "[[Ada]]",
			"See [[Ada Lovelace notes]] and [[Ada|her]].":   "[[Ada|her]]",
			"See [[Adam]], then [[Ada]] and [[Ada]] again.": "[[Ada]]",
		} {
			p := mk(body)
			res, err := d.query(ctx, key, `SELECT ev.quote, ev.char_start FROM chunk_edges e JOIN chunk_edge_evidence ev ON ev.edge_id = e.id
				WHERE e.from_id = ? AND e.to_id = ? AND e.kind = 'mentions'`, p, ada)
			if err != nil || len(res.Rows) != 1 || asStr(res.Rows[0][0]) != want ||
				asInt(res.Rows[0][1]) != len([]rune(body[:strings.Index(body, want)])) {
				t.Errorf("%q: evidence = %v %v, want %q", body, res, err, want)
			}
		}
		// A manual mention already there: the guarded insert writes no edge, so no
		// evidence row may be written for one.
		p := mk("plain")
		if err := d.exec(ctx, key, `INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, auto, method) VALUES (?, ?, ?, 'mentions', ?, 0, 'manual')`,
			newDocID(), p, ada, time.Now().UnixNano()); err != nil {
			t.Fatal(err)
		}
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":1,"body":"Met [[Ada]]."}`, p)); r.IsError {
			t.Fatal(r.Text)
		}
		res, err := d.query(ctx, key, `SELECT COUNT(*) FROM chunk_edge_evidence ev WHERE NOT EXISTS (SELECT 1 FROM chunk_edges e WHERE e.id = ev.edge_id)`)
		if err != nil || asInt(res.Rows[0][0]) != 0 {
			t.Errorf("orphan evidence rows: %v %v", res, err)
		}
	})
}
