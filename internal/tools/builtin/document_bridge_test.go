package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FN P3a — a fact taken from a document points at the passage it came from.

// bridgeDoc makes a document with one passage and a second document for facts.
func bridgeDoc(t *testing.T, d *Document, ctx context.Context) (passage, factsDoc, passageDoc string) {
	t.Helper()
	src, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Policy"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	passageDoc = src["document_id"].(string)
	p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Leave","body":"Staff accrue 25 days of annual leave each year."}`, passageDoc))
	if r.IsError {
		t.Fatal(r.Text)
	}
	f, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Facts"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	return p["id"].(string), f["document_id"].(string), passageDoc
}

func TestUpsertChunk_SourceChunkRecordsWhereTheFactCameFrom(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		passage, facts, _ := bridgeDoc(t, d, ctx)
		// A quote that is not in the passage refuses the whole fact.
		_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/leave-30","title":"30 days","body":"Staff get 30 days.",
			"source_chunk_id":%q,"source_quote":"30 days of annual leave"}`, facts, passage))
		if !r.IsError || !strings.Contains(r.Text, "evidence_quote_not_found") {
			t.Fatalf("a quote not in the source: %s", r.Text)
		}
		if _, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","natural_key":"memory/leave-30"}`); !r.IsError {
			key, _, _ := d.resolveScope(ctx, "user")
			if id, _ := d.chunkIDByNaturalKey(ctx, key, "memory/leave-30"); id != "" {
				t.Fatalf("a refused fact was written")
			}
		}
		// A span alone selects the quote.
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/leave-25","title":"25 days","body":"Staff get 25 days of leave.",
			"source_chunk_id":%q,"char_start":13,"char_end":41}`, facts, passage))
		if r.IsError {
			t.Fatalf("upsert with a span: %s", r.Text)
		}
		df, _ := out["derived_from"].(map[string]any)
		if df == nil || df["chunk_id"] != passage {
			t.Fatalf("derived_from = %v", out)
		}
		st, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_edge","scope":"user","id":%q}`, df["statement_id"]))
		if r.IsError {
			t.Fatal(r.Text)
		}
		s := st["statement"].(map[string]any)
		ev := s["evidence"].([]any)[0].(map[string]any)
		if s["kind"] != "derived_from" || s["method"] != "runtime" || ev["quote"] != "25 days of annual leave each" {
			t.Fatalf("statement = %v", s)
		}
		// Recall's reach-through names the passage beside the span.
		key, mscope, _ := d.resolveScope(ctx, "user")
		spans := SourceSpansFor(ctx, d.SqlMem, "tnt", mscope, docMemScopeID(mscope, key), []string{"memory/leave-25"})
		if sp := spans["memory/leave-25"]; sp.SourceChunkID != passage || sp.Span != "25 days of annual leave each" || sp.SourceDocumentID == "" {
			t.Fatalf("source span = %+v", sp)
		}
	})
}

// about_ids: the runtime writes the fact's `about` edges; a subject that is not a
// chunk of this scope is skipped and counted, never fatal.
func TestUpsertChunk_AboutIDsWriteTheEdgesThemselves(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, facts, _ := bridgeDoc(t, d, ctx)
	subj := upsertIn(t, d, ctx, facts, "person:ada", "Ada")
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"ada|wrote|program","title":"Ada wrote the first program","body":"Ada wrote the first program.",
		"about_ids":[%q,"no-such-chunk"]}`, facts, subj))
	if r.IsError {
		t.Fatal(r.Text)
	}
	if out["about_written"] != float64(1) || out["about_skipped"] != float64(1) {
		t.Fatalf("about = %v", out)
	}
	for _, e := range edgesOf(t, d, ctx, fmt.Sprintf(`{"op":"backlinks","scope":"user","id":%q}`, subj)) {
		if e["kind"] == "about" && e["from_id"] == out["id"] && e["method"] == "runtime" {
			return
		}
	}
	t.Fatalf("no runtime about edge to the subject")
}

// remember links the remembered fact to its subject's node, when the scope has one.
func TestRemember_LinksToTheSubjectNode(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, facts, _ := bridgeDoc(t, d, ctx)
	key, _, _ := d.resolveScope(ctx, "user")
	subj := upsertIn(t, d, ctx, facts, "person:ada", "Ada")
	if err := d.exec(ctx, key, `UPDATE chunk_memory_meta SET subject = 'Ada' WHERE chunk_id = ?`, subj); err != nil {
		t.Fatal(err)
	}
	out, r := docExec(t, d, ctx, `{"op":"remember","scope":"user","text":"Ada prefers tea.","subject":"ada","type":"person"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if out["about_written"] != float64(1) {
		t.Fatalf("remember wrote no about edge: %v", out)
	}
}

// A fact taken from a passage cannot be judged supported once the passage no
// longer says it.
func TestJudgeFact_ChangedSourceCannotSupportTheFact(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	passage, facts, _ := bridgeDoc(t, d, ctx)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/leave","title":"25 days","body":"Staff get 25 days.",
		"source_chunk_id":%q,"source_quote":"25 days of annual leave"}`, facts, passage))
	if r.IsError {
		t.Fatal(r.Text)
	}
	fact := out["id"].(string)
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"supported","reason":"the policy says so"}`, fact)); r.IsError {
		t.Fatalf("judge while the source holds: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":1,"body":"Staff accrue 30 days of annual leave each year."}`, passage)); r.IsError {
		t.Fatal(r.Text)
	}
	_, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"supported","reason":"still true"}`, fact))
	if !r.IsError || !strings.Contains(r.Text, "no longer occurs") {
		t.Fatalf("supported over a changed source: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"unclear","reason":"the policy changed"}`, fact)); r.IsError {
		t.Fatalf("unclear over a changed source: %s", r.Text)
	}
}

// Deleting the document a fact came from keeps the fact's quote and marks it.
func TestDeleteDocument_FactKeepsItsQuoteWhenTheSourceGoes(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		passage, facts, passageDoc := bridgeDoc(t, d, ctx)
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/leave","title":"25 days","body":"Staff get 25 days.",
			"source_chunk_id":%q,"char_start":13,"char_end":41}`, facts, passage))
		if r.IsError {
			t.Fatal(r.Text)
		}
		fact := out["id"].(string)
		key, _, _ := d.resolveScope(ctx, "user")
		// Staged as a fact written before the quote was copied onto it: the snapshot
		// is what keeps it.
		if err := d.exec(ctx, key, `UPDATE chunk_memory_meta SET source_quote = NULL WHERE chunk_id = ?`, fact); err != nil {
			t.Fatal(err)
		}
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_document","scope":"user","id":%q}`, passageDoc)); r.IsError {
			t.Fatal(r.Text)
		}
		res, err := d.query(ctx, key, `SELECT COALESCE(source_quote, ''), source_gone FROM chunk_memory_meta WHERE chunk_id = ?`, fact)
		if err != nil || len(res.Rows) != 1 {
			t.Fatalf("fact meta: %v %v", res, err)
		}
		if q := asStr(res.Rows[0][0]); q != "25 days of annual leave each" || asInt(res.Rows[0][1]) != 1 {
			t.Fatalf("after delete: quote %q, source_gone %v", q, res.Rows[0][1])
		}
	})
}

// A fact with no document source is not affected by any of this.
func TestUpsertChunk_NoSourceIsUnchanged(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, facts, _ := bridgeDoc(t, d, ctx)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"plain|fact","title":"plain","body":"plain"}`, facts))
	if r.IsError {
		t.Fatal(r.Text)
	}
	for _, k := range []string{"derived_from", "about_written", "about_skipped"} {
		if _, ok := out[k]; ok {
			t.Errorf("a plain upsert reports %s: %v", k, out)
		}
	}
	_ = store.MemoryScopeUser
}

// A judge who may not read the passage a fact came from gets no answer about it:
// refusing `supported` would reveal whether the passage still says the quote.
func TestJudgeFact_UnreadableSourceIsNotAnOracle(t *testing.T) {
	d, base, _ := documentFixture(t)
	open := tenantDocCtx(base, nil)
	s := seedACLDocs(t, d, open)
	out, r := docExec(t, d, open, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":%q,"natural_key":"memory/plan","title":"plan","body":"the plan nests savepoints",
		"source_chunk_id":%q,"source_quote":"savepoint nesting secret"}`, s.publicDoc, s.secretChunk))
	if r.IsError {
		t.Fatal(r.Text)
	}
	fact := out["id"].(string)
	if _, r := docExec(t, d, open, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"revision":1,"body":"rewritten"}`, s.secretChunk)); r.IsError {
		t.Fatal(r.Text)
	}
	denied := tenantDocCtx(base, aclSet(t,
		authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
		authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:tenant:/kb/secret/**"}}))
	_, r = docExec(t, d, denied, fmt.Sprintf(`{"op":"judge_fact","scope":"tenant","id":%q,"verdict":"supported","reason":"x"}`, fact))
	if r.IsError && strings.Contains(r.Text, "no longer occurs") {
		t.Errorf("the refusal revealed that an unreadable passage changed: %s", r.Text)
	}
	// The reader who may see the passage is still held to it.
	_, r = docExec(t, d, open, fmt.Sprintf(`{"op":"judge_fact","scope":"tenant","id":%q,"verdict":"supported","reason":"x"}`, fact))
	if !r.IsError || !strings.Contains(r.Text, "no longer occurs") {
		t.Errorf("control: a reader of the passage was not held to it: %s", r.Text)
	}
}

// liveEdgeTargets lists the live edges of kind from `from`.
func liveEdgeTargets(t *testing.T, d *Document, ctx context.Context, scope, from, kind string) map[string]bool {
	t.Helper()
	key, _, _ := d.resolveScope(ctx, scope)
	res, err := d.query(ctx, key, `SELECT e.to_id FROM chunk_edges e WHERE e.from_id = ? AND e.kind = ? AND `+edgeLiveClause, from, kind)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, r := range res.Rows {
		out[asStr(r[0])] = true
	}
	return out
}

// A fact taken again from another passage keeps ONE source, the current one: the
// judge checks it and recall points at it.
func TestUpsertChunk_ARederivedFactKeepsOnlyItsCurrentSource(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		p1, facts, pdoc := bridgeDoc(t, d, ctx)
		p2o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Leave2","body":"From 2027 staff accrue 30 days of annual leave."}`, pdoc))
		if r.IsError {
			t.Fatal(r.Text)
		}
		p2 := p2o["id"].(string)
		up := func(body, src, quote string) string {
			out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/leave","title":"leave","body":%q,
				"source_chunk_id":%q,"source_quote":%q}`, facts, body, src, quote))
			if r.IsError {
				t.Fatal(r.Text)
			}
			return out["id"].(string)
		}
		fact := up("Staff get 25 days.", p1, "25 days of annual leave")
		up("Staff get 30 days.", p2, "30 days of annual leave")
		if got := liveEdgeTargets(t, d, ctx, "user", fact, "derived_from"); len(got) != 1 || !got[p2] {
			t.Fatalf("derived_from = %v, want only the current source %s", got, p2)
		}
		// The old passage changes; the current one still holds.
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":1,"body":"Superseded."}`, p1)); r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"supported","reason":"30 days is in the current source"}`, fact)); r.IsError {
			t.Errorf("judged against the fact's old source: %s", r.Text)
		}
		// Re-derived from the SAME passage with another quote: the judge reads the new one.
		up("Staff get 30 days from 2027.", p2, "From 2027 staff")
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"user","id":%q,"revision":1,"body":"From 2027 staff accrue 31 days."}`, p2)); r.IsError {
			t.Fatal(r.Text)
		}
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"judge_fact","scope":"user","id":%q,"verdict":"supported","reason":"the current quote holds"}`, fact)); r.IsError {
			t.Errorf("judged against the fact's old quote: %s", r.Text)
		}
		key, mscope, _ := d.resolveScope(ctx, "user")
		sp := SourceSpansFor(ctx, d.SqlMem, "tnt", mscope, docMemScopeID(mscope, key), []string{"memory/leave"})["memory/leave"]
		if sp.SourceChunkID != p2 || sp.SourceDocumentID != pdoc {
			t.Errorf("recall source = %s/%s, want %s/%s", sp.SourceChunkID, sp.SourceDocumentID, p2, pdoc)
		}
	})
}

// A fact that carries two derived_from edges (written before a fact kept only one)
// is read through its NEWEST, and the chunk and its document come from that one
// edge rather than from two separate lookups.
func TestSourceSpansFor_TheNewestSourceGivesChunkAndDocumentTogether(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, ctx context.Context) {
		_, facts, _ := bridgeDoc(t, d, ctx)
		mk := func(title string) (doc, chunk string) {
			o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"user","title":%q}`, title))
			if r.IsError {
				t.Fatal(r.Text)
			}
			c, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"p","body":"text"}`, o["document_id"]))
			if r.IsError {
				t.Fatal(r.Text)
			}
			return o["document_id"].(string), c["id"].(string)
		}
		oldDoc, oldChunk := mk("Old")
		newDoc, newChunk := mk("New")
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/two","title":"t","body":"t"}`, facts))
		if r.IsError {
			t.Fatal(r.Text)
		}
		key, mscope, _ := d.resolveScope(ctx, "user")
		// The newer edge is inserted FIRST, so a lookup that does not order picks the old one.
		for _, e := range []struct {
			to string
			at int64
		}{{newChunk, 2000}, {oldChunk, 1000}} {
			id := newDocID()
			if err := d.exec(ctx, key, `INSERT INTO chunk_edges (id, from_id, to_id, kind, created_at, method) VALUES (?, ?, ?, 'derived_from', ?, 'runtime')`,
				id, out["id"], e.to, e.at); err != nil {
				t.Fatal(err)
			}
			if err := d.exec(ctx, key, `INSERT INTO chunk_edge_evidence (id, edge_id, chunk_id, quote, created_at) VALUES (?, ?, ?, 'text', ?)`,
				newDocID(), id, e.to, e.at); err != nil {
				t.Fatal(err)
			}
		}
		sp := SourceSpansFor(ctx, d.SqlMem, "tnt", mscope, docMemScopeID(mscope, key), []string{"memory/two"})["memory/two"]
		if sp.SourceChunkID != newChunk || sp.SourceDocumentID != newDoc {
			t.Errorf("source = %s/%s, want the newest %s/%s (old %s/%s)", sp.SourceChunkID, sp.SourceDocumentID, newChunk, newDoc, oldChunk, oldDoc)
		}
	})
}

// A source_quote longer than evidence may be is refused before anything is
// written, rather than leaving the fact without its derived_from.
func TestUpsertChunk_AnOverlongSourceQuoteIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, facts, pdoc := bridgeDoc(t, d, ctx)
	long := strings.Repeat("leave ", edgeEvidenceQuoteMax/5)
	p, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Long","body":%q}`, pdoc, long))
	if r.IsError {
		t.Fatal(r.Text)
	}
	_, r = docExec(t, d, ctx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"user","document_id":%q,"natural_key":"memory/long","title":"long","body":"long",
		"source_chunk_id":%q,"source_quote":%q}`, facts, p["id"], strings.TrimSpace(long)))
	if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryValidation {
		t.Fatalf("an over-long source_quote: IsError=%v %s", r.IsError, r.Text)
	}
	key, _, _ := d.resolveScope(ctx, "user")
	if id, _ := d.chunkIDByNaturalKey(ctx, key, "memory/long"); id != "" {
		t.Errorf("a refused fact was written")
	}
}

// A recalled fact's source pointer is held back from a caller who may not read the
// document it points into, as the passage itself is.
func TestMemoryRecall_SourcePointerWithheldWhenItsDocumentIsUnreadable(t *testing.T) {
	m, open, limited, s := memACLCorpus(t)
	d := m.DocumentFilters.(*Document)
	out, r := docExec(t, d, open, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":%q,"natural_key":"memory/fact/plan","title":"plan","body":"savepoint nesting public plan",
		"source_chunk_id":%q,"source_quote":"savepoint nesting secret"}`, s.publicDoc, s.secretChunk))
	if r.IsError {
		t.Fatal(r.Text)
	}
	source := func(ctx context.Context) (string, bool) {
		res := memRun(t, m, ctx, `{"op":"recall","scope":"tenant","query":"savepoint nesting public plan","top_k":50}`)
		if res.IsError {
			t.Fatal(res.Text)
		}
		var body struct {
			Memories []map[string]any `json:"memories"`
		}
		if err := json.Unmarshal([]byte(res.Text), &body); err != nil {
			t.Fatal(err)
		}
		for _, mem := range body.Memories {
			if mem["id"] == "memory/fact/plan" {
				c, _ := mem["source_chunk_id"].(string)
				_, hasDoc := mem["source_document_id"]
				return c, hasDoc
			}
		}
		t.Fatalf("the fact %s was not recalled: %s", out["id"], res.Text)
		return "", false
	}
	if c, _ := source(open); c != s.secretChunk {
		t.Fatalf("control: an unrestricted caller got source %q, want %q", c, s.secretChunk)
	}
	if c, hasDoc := source(limited); c != "" || hasDoc {
		t.Errorf("a caller denied the source document got its pointer: %q (document %v)", c, hasDoc)
	}
}
