package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
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
