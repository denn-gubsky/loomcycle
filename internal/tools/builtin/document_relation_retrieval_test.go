package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC FN P4 — retrieval that follows relations by kind.

// walkDoc is a small regulation: a clause that cites an article and is defined by
// a glossary entry, plus a note nobody relates to. Returns the ids by name.
func walkDoc(t *testing.T, d *Document, ctx context.Context) map[string]string {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Act"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	docID := out["document_id"].(string)
	ids := map[string]string{"doc": docID, "root": out["root_chunk_id"].(string)}
	for _, c := range []struct{ name, body string }{
		{"clause", "The licensee shall report under Article 12 savepoint rules."},
		{"article", "Article 12 requires a quarterly report nesting rules."},
		{"term", "Licensee means the holder of a permit."},
		{"note", "Unrelated note."},
	} {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":%q,"body":%q}`, docID, c.name, c.body))
		if r.IsError {
			t.Fatal(r.Text)
		}
		ids[c.name] = o["id"].(string)
	}
	link := func(from, to, kind, extra string) {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":%q%s}`, ids[from], ids[to], kind, extra)); r.IsError {
			t.Fatalf("link %s: %s", kind, r.Text)
		}
	}
	link("clause", "article", "cites", fmt.Sprintf(`,"evidence":[{"chunk_id":%q,"quote":"report under Article 12"}]`, ids["clause"]))
	link("clause", "term", "defined_by", "")
	return ids
}

// recallRows returns graph_recall's rows keyed by chunk id.
func recallRows(t *testing.T, d *Document, ctx context.Context, body string) (map[string]map[string]any, map[string]any) {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	rows := map[string]map[string]any{}
	for _, c := range out["chunks"].([]any) {
		m := c.(map[string]any)
		rows[m["id"].(string)] = m
	}
	return rows, out
}

// kinds narrows the walk; direction picks a side; the relation it came by is
// reported with its first quote.
func TestGraphRecall_FollowsOnlyTheNamedKinds(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	rows, out := recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"kinds":["cites"]}`, ids["clause"]))
	if _, ok := rows[ids["article"]]; !ok {
		t.Fatalf("cites neighbour missing: %v", rows)
	}
	if _, ok := rows[ids["term"]]; ok {
		t.Fatalf("a defined_by neighbour was walked under kinds=[cites]")
	}
	if out["walk"] != "kinds" {
		t.Errorf("walk = %v, want kinds", out["walk"])
	}
	a := rows[ids["article"]]
	if a["via_kind"] != "cites" || a["via_quote"] != "report under Article 12" {
		t.Errorf("via = %v / %v", a["via_kind"], a["via_quote"])
	}
	// From the article, the citation arrives: walking only outgoing finds nothing.
	rows, _ = recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"direction":"out"}`, ids["article"]))
	if _, ok := rows[ids["clause"]]; ok {
		t.Errorf("direction=out walked an incoming relation")
	}
	rows, _ = recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"direction":"in"}`, ids["article"]))
	if _, ok := rows[ids["clause"]]; !ok {
		t.Errorf("direction=in missed the incoming relation")
	}
}

// With no vocabulary, walk=declared has nothing declared and follows every kind —
// and a call that sets nothing is answered exactly as before (no walk field).
func TestGraphRecall_NoRelationsDeclaredWalksEverything(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	rows, out := recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q]}`, ids["clause"]))
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want the seed and both neighbours", len(rows))
	}
	if _, ok := out["walk"]; ok {
		t.Errorf("a default call reports a walk field: %v", out["walk"])
	}
	_, out = recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"walk":"declared"}`, ids["clause"]))
	if out["walk"] != "all" {
		t.Errorf("walk=declared with nothing declared reported %v, want all", out["walk"])
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"walk":"sideways"}`, ids["clause"])); !r.IsError {
		t.Errorf("an unknown walk was accepted")
	}
}

// The hierarchy is a relation when asked for: up to the parent, down to the parts.
func TestGraphRecall_IncludeHierarchyStepsAlongTheTree(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	rows, _ := recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"include_hierarchy":"up","kinds":["none"]}`, ids["note"]))
	root, ok := rows[ids["root"]]
	if !ok || root["via_kind"] != "part_of" {
		t.Fatalf("up did not reach the parent as part_of: %v", rows)
	}
	rows, _ = recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"include_hierarchy":"down","kinds":["none"]}`, ids["root"]))
	if n, ok := rows[ids["note"]]; !ok || n["via_kind"] != "has_part" {
		t.Fatalf("down did not reach the parts as has_part: %v", rows)
	}
}

// A statement's own validity bounds the walk: one that ended is not followed now,
// and is followed as of a moment inside it.
func TestGraphRecall_StatementValidityBoundsTheWalk(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	past := time.Now().Add(-time.Hour).UnixNano()
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"amended_by","valid_at":1000,"invalid_at":%d}`,
		ids["clause"], ids["note"], past)); r.IsError {
		t.Fatal(r.Text)
	}
	rows, _ := recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q]}`, ids["clause"]))
	if _, ok := rows[ids["note"]]; ok {
		t.Errorf("a relation that ended was walked now")
	}
	rows, _ = recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"as_of":5000}`, ids["clause"]))
	if _, ok := rows[ids["note"]]; !ok {
		t.Errorf("as_of inside the relation's validity did not walk it")
	}
}

// include_proposed walks a proposal and says so on the row.
func TestGraphRecall_IncludeProposedMarksTheRow(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"related"}`, ids["clause"], ids["note"])); r.IsError {
		t.Fatal(r.Text)
	}
	rows, _ := recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"include_proposed":true}`, ids["clause"]))
	n, ok := rows[ids["note"]]
	if !ok || n["via_status"] != "proposed" {
		t.Fatalf("proposal not walked or not marked: %v", n)
	}
	if a := rows[ids["article"]]; a["via_status"] != nil {
		t.Errorf("an asserted relation carries via_status: %v", a["via_status"])
	}
}

// decay caps each hop's share of the budget: a far hop cannot take what a near
// one did not use.
func TestGraphRecall_DecayCapsFarHops(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	// clause → article → (chain of four more), each title ~40 characters.
	prev := ids["article"]
	for i := 0; i < 4; i++ {
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"far chunk number %d with a long title","body":"x"}`, ids["doc"], i))
		if r.IsError {
			t.Fatal(r.Text)
		}
		id := o["id"].(string)
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"link_chunks","scope":"user","from_id":%q,"to_id":%q,"kind":"cites"}`, prev, id)); r.IsError {
			t.Fatal(r.Text)
		}
		prev = id
	}
	q := `{"op":"graph_recall","scope":"user","seed_ids":[%q],"kinds":["cites"],"hops":5,"budget_chars":400,"body_chars":0%s}`
	full, _ := recallRows(t, d, ctx, fmt.Sprintf(q, ids["clause"], ""))
	decayed, out := recallRows(t, d, ctx, fmt.Sprintf(q, ids["clause"], `,"decay":0.1`))
	if len(decayed) >= len(full) {
		t.Fatalf("decay 0.1 kept %d rows, no decay %d — the far hops were not capped", len(decayed), len(full))
	}
	if out["decay"] != 0.1 {
		t.Errorf("decay not reported: %v", out["decay"])
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(q, ids["clause"], `,"decay":0`)); !r.IsError {
		t.Errorf("decay 0 was accepted")
	}
}

// related_to answers by relation, with no embedder at all.
func TestRelated_RelatedToAnswersByRelation(t *testing.T) {
	d, ctx, _ := documentFixture(t) // no embedder
	ids := walkDoc(t, d, ctx)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"related","scope":"user","related_to":%q}`, ids["clause"]))
	if r.IsError {
		t.Fatalf("related_to: %s", r.Text)
	}
	got := map[string]string{}
	for _, l := range out["linked"].([]any) {
		m := l.(map[string]any)
		got[m["chunk_id"].(string)] = m["via_kind"].(string)
	}
	if got[ids["article"]] != "cites" || got[ids["term"]] != "defined_by" || len(got) != 2 {
		t.Fatalf("linked = %v", got)
	}
}

// search expand adds what the top hits are related to, after them, marked.
func TestSearch_ExpandAddsWhatTheHitsAreRelatedTo(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "savepoint", "nesting")
	ids := walkDoc(t, d, ctx)
	plain, r := docExec(t, d, ctx, `{"op":"search","scope":"user","query":"savepoint","limit":1}`)
	if r.IsError {
		t.Fatalf("search: %s", r.Text)
	}
	if hits := plain["chunks"].([]any); len(hits) != 1 || hits[0].(map[string]any)["chunk_id"] != ids["clause"] {
		t.Fatalf("staging: the top hit is not the clause: %v", plain["chunks"])
	}
	out, r := docExec(t, d, ctx, `{"op":"search","scope":"user","query":"savepoint","limit":1,"expand":{"kinds":["cites"]}}`)
	if r.IsError {
		t.Fatalf("search expand: %s", r.Text)
	}
	chunks := out["chunks"].([]any)
	if len(chunks) != 2 || out["expanded"] != float64(1) {
		t.Fatalf("expanded = %v, chunks = %v", out["expanded"], chunks)
	}
	e := chunks[1].(map[string]any)
	via, _ := e["expanded_via"].(map[string]any)
	if e["chunk_id"] != ids["article"] || via["kind"] != "cites" || via["from_chunk_id"] != ids["clause"] || via["quote"] != "report under Article 12" {
		b, _ := json.Marshal(e)
		t.Fatalf("expanded row = %s", b)
	}
	if _, r := docExec(t, d, ctx, `{"op":"search","scope":"user","query":"savepoint","expand":{"hops":2}}`); !r.IsError || !strings.Contains(r.Text, "one hop") {
		t.Errorf("expand.hops=2: want a refusal, got %s", r.Text)
	}
}

// Every walk branch at once — both relation directions, a kind filter, statement
// validity, and both hierarchy steps — on both SQL tiers: the union mixes literal
// columns with typed ones, which postgres resolves differently from sqlite.
func TestGraphRecall_EveryBranchOnBothTiers(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		ids := walkDoc(t, d, ctx)
		rows, _ := recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],
			"kinds":["cites","defined_by"],"include_hierarchy":"both","as_of":%d}`, ids["clause"], time.Now().UnixNano()))
		for _, want := range []string{"article", "term", "root"} {
			if _, ok := rows[ids[want]]; !ok {
				t.Errorf("%s missing from %v", want, rows)
			}
		}
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"related","scope":"user","related_to":%q}`, ids["article"]))
		if r.IsError || len(out["linked"].([]any)) != 1 {
			t.Errorf("related_to: %v %s", out, r.Text)
		}
	})
}

// search expand starts only from hits the caller may read: an unreadable hit is
// dropped from the answer by the listing filter, and expanding from it would put
// its chunk id and its relations into the answer anyway.
func TestSearch_ExpandNeverStartsFromAnUnreadableHit(t *testing.T) {
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	o, r := docExec(t, d, open, `{"op":"create_chunk","scope":"tenant","document_id":"`+s.publicDoc+`","title":"Target","body":"other words"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	target := o["id"].(string)
	if _, r := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.secretChunk+`","to_id":"`+target+`","kind":"cites"}`); r.IsError {
		t.Fatal(r.Text)
	}
	// limit 1: the secret chunk is the only ranked hit, so the target can only
	// arrive by expansion.
	q := `{"op":"search","scope":"tenant","query":"savepoint secret","limit":1,"expand":{"kinds":["cites"]}}`
	_, r = docExec(t, d, open, q)
	if r.IsError || !strings.Contains(r.Text, `"expanded_via"`) || !strings.Contains(r.Text, target) {
		t.Fatalf("control: expansion from the secret hit did not reach the target: %s", r.Text)
	}
	denied := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	_, r = docExec(t, d, denied, q)
	if r.IsError {
		t.Fatalf("search: %s", r.Text)
	}
	if strings.Contains(r.Text, s.secretChunk) || strings.Contains(r.Text, `"expanded_via"`) {
		t.Errorf("an expansion from an unreadable hit reached the answer: %s", r.Text)
	}
}
