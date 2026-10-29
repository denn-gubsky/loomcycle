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

// expandACLFixture links the readable public chunk to the secret one, so a search
// that ranks the public chunk can only reach the secret chunk by expansion.
func expandACLFixture(t *testing.T) (*Document, context.Context, context.Context, aclDocs) {
	t.Helper()
	d, _, base := mermaidDocFixture(t, "savepoint", "nesting", "secret", "public")
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	denied := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	return d, open, denied, s
}

// expanded and expand_truncated describe only the rows the caller is shown: an
// unreadable neighbour neither counts nor spends the budget, so neither moves when
// a readable hit gains a relation to something the caller cannot read.
func TestSearch_ExpandCountsOnlyReadableRows(t *testing.T) {
	d, open, denied, s := expandACLFixture(t)
	// The default budget, and budget_chars 1, where any row that reaches the budget
	// truncates the expansion.
	qs := []string{
		`{"op":"search","scope":"tenant","query":"savepoint public","limit":1,"expand":{"kinds":["cites"]}}`,
		`{"op":"search","scope":"tenant","query":"savepoint public","limit":1,"expand":{"kinds":["cites"],"budget_chars":1}}`,
	}
	before := make([]map[string]any, len(qs))
	for i, q := range qs {
		out, r := docExec(t, d, denied, q)
		if r.IsError {
			t.Fatal(r.Text)
		}
		before[i] = out
	}
	if _, r := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.secretChunk+`","kind":"cites"}`); r.IsError {
		t.Fatal(r.Text)
	}
	for i, q := range qs {
		after, r := docExec(t, d, denied, q)
		if r.IsError {
			t.Fatal(r.Text)
		}
		if strings.Contains(r.Text, s.secretChunk) {
			t.Errorf("%s: the unreadable chunk reached the answer: %s", q, r.Text)
		}
		for _, k := range []string{"expanded", "expand_truncated"} {
			if fmt.Sprint(before[i][k]) != fmt.Sprint(after[k]) {
				t.Errorf("%s: %s moved %v -> %v while the visible rows did not", q, k, before[i][k], after[k])
			}
		}
	}
}

// An expanded row obeys the search's own filter: exclude_under_path and
// under_path keep out a neighbour the ranked hits could never have been.
func TestSearch_ExpandObeysTheSearchFilter(t *testing.T) {
	d, open, _, s := expandACLFixture(t)
	if _, r := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.secretChunk+`","kind":"cites"}`); r.IsError {
		t.Fatal(r.Text)
	}
	base := `{"op":"search","scope":"tenant","query":"savepoint public","expand":{"kinds":["cites"]}%s}`
	if _, r := docExec(t, d, open, fmt.Sprintf(base, "")); r.IsError || !strings.Contains(r.Text, s.secretChunk) {
		t.Fatalf("control: an unfiltered expansion did not reach the secret chunk: %s", r.Text)
	}
	// A filter that keeps both ends still expands: the filter narrows, it does not blank.
	if _, r := docExec(t, d, open, fmt.Sprintf(base, `,"under_path":"/kb"`)); r.IsError || !strings.Contains(r.Text, s.secretChunk) {
		t.Fatalf("control: a filter covering both ends dropped the expansion: %s", r.Text)
	}
	for _, f := range []string{`,"exclude_under_path":"/kb/secret"`, `,"under_path":"/kb/public"`} {
		out, r := docExec(t, d, open, fmt.Sprintf(base, f))
		if r.IsError {
			t.Fatalf("%s: %s", f, r.Text)
		}
		if strings.Contains(r.Text, s.secretChunk) {
			t.Errorf("%s: a filtered-out chunk came back by expansion: %s", f, r.Text)
		}
		if out["expanded"] != float64(0) {
			t.Errorf("%s: expanded = %v, want 0", f, out["expanded"])
		}
	}
}

// related_to obeys the same structural filter as the semantic neighbours.
func TestRelated_RelatedToObeysTheFilter(t *testing.T) {
	d, open, _, s := expandACLFixture(t)
	if _, r := docExec(t, d, open, `{"op":"link_chunks","scope":"tenant","from_id":"`+s.publicChunk+`","to_id":"`+s.secretChunk+`","kind":"cites"}`); r.IsError {
		t.Fatal(r.Text)
	}
	base := `{"op":"related","scope":"tenant","related_to":"` + s.publicChunk + `"%s}`
	if _, r := docExec(t, d, open, fmt.Sprintf(base, "")); r.IsError || !strings.Contains(r.Text, s.secretChunk) {
		t.Fatalf("control: related_to did not reach the secret chunk: %s", r.Text)
	}
	if _, r := docExec(t, d, open, fmt.Sprintf(base, `,"under_path":"/kb"`)); r.IsError || !strings.Contains(r.Text, s.secretChunk) {
		t.Fatalf("control: a filter covering both ends dropped the linked chunk: %s", r.Text)
	}
	for _, f := range []string{`,"under_path":"/kb/public"`, `,"exclude_under_path":"/kb/secret"`} {
		out, r := docExec(t, d, open, fmt.Sprintf(base, f))
		if r.IsError {
			t.Fatalf("%s: %s", f, r.Text)
		}
		if strings.Contains(r.Text, s.secretChunk) {
			t.Errorf("%s: related_to returned a chunk outside the filter: %s", f, r.Text)
		}
		if _, ok := out["filter"]; !ok {
			t.Errorf("%s: no filter echo: %s", f, r.Text)
		}
	}
}

// related with both id and related_to on a server with no embedder still answers
// by relation, and says why the semantic half is missing.
func TestRelated_IDAndRelatedToWithoutEmbedderKeepsLinked(t *testing.T) {
	d, ctx, _ := documentFixture(t) // no embedder
	ids := walkDoc(t, d, ctx)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"related","scope":"user","id":%q,"related_to":%q}`, ids["clause"], ids["clause"]))
	if r.IsError {
		t.Fatalf("related id+related_to: %s", r.Text)
	}
	if l, _ := out["linked"].([]any); len(l) != 2 {
		t.Errorf("linked = %v", out["linked"])
	}
	if n, _ := out["note"].(string); !strings.Contains(n, "embedder") {
		t.Errorf("note = %v", out["note"])
	}
}

// A rejected statement is an operator's ruling, not a verdict on a fact:
// include_refuted brings back refuted facts and relations, never a rejected one.
func TestGraphRecall_IncludeRefutedNeverWalksARejectedStatement(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		ids := walkDoc(t, d, ctx)
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"propose_statement","scope":"user","from_id":%q,"to_id":%q,"kind":"related"}`, ids["clause"], ids["note"]))
		if r.IsError {
			t.Fatal(r.Text)
		}
		sid, _ := o["id"].(string)
		if _, r := docExec(t, d, operatorCtx(ctx), fmt.Sprintf(`{"op":"reject_statement","scope":"user","id":%q,"reason":"wrong"}`, sid)); r.IsError {
			t.Fatalf("reject_statement: %s", r.Text)
		}
		for _, extra := range []string{`,"include_refuted":true`, `,"include_refuted":true,"include_proposed":true`} {
			rows, _ := recallRows(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q]%s}`, ids["clause"], extra))
			if n, ok := rows[ids["note"]]; ok {
				t.Errorf("%s: the rejected statement was walked: %v", extra, n)
			}
			if _, ok := rows[ids["article"]]; !ok {
				t.Errorf("%s: control: the asserted relation was not walked", extra)
			}
		}
	})
}

// decay shares out budget_chars, so it is refused without one, and reported only
// when a budget made it apply.
func TestGraphRecall_DecayAppliesOnlyWithABudget(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"decay":0.5}`, ids["clause"]))
	if !r.IsError || !strings.Contains(r.Text, "budget_chars") {
		t.Errorf("decay without budget_chars: want a refusal naming budget_chars, got %s", r.Text)
	}
	// A declared walk defaults decay to 0.7; with no budget there is nothing to decay.
	f := newRelFixture(t, relationsOntologyMD)
	if _, failed, msg := f.link(t, "supervises", f.provA, f.provB, ""); failed {
		t.Fatalf("link: %s", msg)
	}
	_, out := recallRows(t, f.d, f.ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q]}`, f.provA))
	if out["walk"] != "declared" {
		t.Fatalf("staging: walk = %v, want declared", out["walk"])
	}
	if _, ok := out["decay"]; ok {
		t.Errorf("decay reported with no budget: %v", out["decay"])
	}
	_, out = recallRows(t, f.d, f.ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"budget_chars":500}`, f.provA))
	if out["decay"] != 0.7 {
		t.Errorf("declared walk with a budget: decay = %v, want 0.7", out["decay"])
	}
}

// walk is checked whether or not kinds are named: a typo is refused, not ignored.
func TestGraphRecall_InvalidWalkIsRefusedWithKinds(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := walkDoc(t, d, ctx)
	_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"graph_recall","scope":"user","seed_ids":[%q],"kinds":["cites"],"walk":"everything"}`, ids["clause"]))
	if !r.IsError || !strings.Contains(r.Text, "walk must be") {
		t.Errorf("walk=everything with kinds: want a refusal, got %s", r.Text)
	}
}
