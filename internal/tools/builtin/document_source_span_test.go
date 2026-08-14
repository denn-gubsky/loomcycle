package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSourceSpan_RoundTripsAndSurvivesAnEdit.
//
// A fact could say WHO wrote it and never WHAT it was based on, so nothing could ask
// whether the source supported the claim. Three live failures traced to that. The span
// closes it — and it has to survive an ordinary edit, or the evidence evaporates the
// first time someone fixes a typo in the claim.
func TestSourceSpan_RoundTripsAndSurvivesAnEdit(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)

	quote := "I live in Cluj-Napoca and I work on loomcycle."
	qj, _ := json.Marshal(quote)
	up, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","natural_key":"fact:user-city","title":"The user lives in Cluj-Napoca.",`+
		`"type":"location","source_quote":`+string(qj)+`,"body":"x"}`)
	if r.IsError {
		t.Fatalf("upsert_chunk: %s", r.Text)
	}
	id, _ := up["id"].(string)

	got, r := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	if r.IsError {
		t.Fatalf("get_chunk: %s", r.Text)
	}
	entity, _ := got["entity"].(map[string]any)
	if entity == nil {
		t.Fatalf("no entity block: %v", got)
	}
	if entity["source_quote"] != quote {
		t.Errorf("source_quote = %v, want the span verbatim", entity["source_quote"])
	}

	// EDIT the claim without resupplying the span. The evidence must survive: an
	// operator correcting wording is not withdrawing the basis for the fact.
	if _, r = docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","natural_key":"fact:user-city","title":"The user resides in Cluj-Napoca.","body":"x"}`); r.IsError {
		t.Fatalf("re-upsert: %s", r.Text)
	}
	got, _ = docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	entity, _ = got["entity"].(map[string]any)
	if entity["source_quote"] != quote {
		t.Errorf("the span was lost on an edit: %v", entity["source_quote"])
	}
	// And it reaches the fact list, which is where an operator or a judge enumerates.
	facts, r := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if r.IsError {
		t.Fatalf("list_facts: %s", r.Text)
	}
	listed, _ := json.Marshal(facts)
	if !strings.Contains(string(listed), quote) {
		t.Error("list_facts does not carry the span — a judge would have nothing to check against")
	}
}

// TestSourceSpan_AbsentMeansUnverifiedNotFalse.
//
// Every fact written before this column existed has no span, and the decision is that
// they stay recallable and honestly labelled. A fact with no span must therefore write
// and read cleanly rather than erroring or defaulting to something that looks like
// evidence.
func TestSourceSpan_AbsentMeansUnverifiedNotFalse(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes","body":""}`)
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	up, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","natural_key":"fact:no-span","title":"Something recorded without a source.","body":"x"}`)
	if r.IsError {
		t.Fatalf("upsert without a span must be allowed: %s", r.Text)
	}
	id, _ := up["id"].(string)
	got, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	entity, _ := got["entity"].(map[string]any)
	if _, present := entity["source_quote"]; present {
		t.Errorf("an absent span must be absent, not empty-but-present: %v", entity)
	}
	// It is still a fact and still enumerable — unverified, not withheld.
	facts, _ := docExec(t, d, ctx, `{"op":"list_facts","scope":"user"}`)
	if n, _ := facts["count"].(float64); n != 1 {
		t.Errorf("a span-less fact must stay recallable, got count %v", facts["count"])
	}
}

// TestSourceSpan_MigratesAScopeThatPredatesTheColumn.
//
// ensureSchema's CREATE TABLE IF NOT EXISTS leaves an existing table untouched, so a
// store provisioned before this column would never gain it — the write would fail on
// every fact for every deployment that already exists. The migration is what makes this
// shippable, and dropping the column back off is the only honest way to test it.
func TestSourceSpan_MigratesAScopeThatPredatesTheColumn(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes","body":""}`)
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)

	key, _, err := d.resolveScope(ctx, "user")
	if err != nil {
		t.Fatalf("resolveScope: %v", err)
	}
	if err := d.exec(ctx, key, `ALTER TABLE chunk_memory_meta DROP COLUMN source_quote`); err != nil {
		t.Skipf("this tier cannot drop a column, so the pre-migration state is unreachable: %v", err)
	}
	if d.tableHasColumn(ctx, key, "chunk_memory_meta", "source_quote") {
		t.Fatal("the column survived the drop — the test cannot reach the state it is for")
	}

	// Any op re-runs ensureSchema, which must add it back.
	up, r := docExec(t, d, ctx, `{"op":"upsert_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","natural_key":"fact:migrated","title":"A claim.","source_quote":"the source said so","body":"x"}`)
	if r.IsError {
		t.Fatalf("the migration did not run: %s", r.Text)
	}
	id, _ := up["id"].(string)
	got, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	entity, _ := got["entity"].(map[string]any)
	if entity["source_quote"] != "the source said so" {
		t.Errorf("span not persisted after migration: %v", entity["source_quote"])
	}
}
