package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

// The precision half of graph_recall, measured on the reference deployment rather
// than inferred: 2 entity chunks among 3,071 chunks across 150 documents, in the
// scope where the document store lives. Correct results drowned rather than absent
// — the one failure mode a fixture corpus can never show, because every test corpus
// is a clean room and signal-to-noise is a property of the corpus.

// TestGraphRecall_DiscoveryIgnoresOrdinaryDocumentChunks. A chunk with no sidecar
// row is prose, not part of the graph. Searching it made "what do we know about X"
// answer with documentation.
func TestGraphRecall_DiscoveryIgnoresOrdinaryDocumentChunks(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)

	// An ENTITY fact about Ada.
	upsert(t, d, ctx, docID, "ada-role", "Ada leads the platform team", "Ada leads platform.", "")
	// And an ordinary prose chunk that mentions her — the shape of a doc-store chunk.
	res, _ := d.Execute(ctx, entityJSON(map[string]any{
		"op": "create_chunk", "scope": "user", "document_id": docID,
		"title": "Ada wrote the original design note", "body": "prose",
	}))
	if res.IsError {
		t.Fatalf("create_chunk: %s", res.Text)
	}

	got := graphRecall(t, d, ctx, map[string]any{"query": "Ada"})
	if len(got) != 1 {
		t.Fatalf("want 1 seed (the entity), got %d: %v", len(got), titlesOf(got))
	}
	if got[0]["title"] != "Ada leads the platform team" {
		t.Errorf("discovery returned a prose chunk: %v", titlesOf(got))
	}
}

// TestGraphRecall_NamedSeedsAreNotRestricted. seed_ids is documented as "hand in
// results you already found some other way", so restricting them would break the
// use they exist for. Same division the temporal filter draws.
func TestGraphRecall_NamedSeedsAreNotRestricted(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)
	res, _ := d.Execute(ctx, entityJSON(map[string]any{
		"op": "create_chunk", "scope": "user", "document_id": docID,
		"title": "a prose chunk", "body": "x",
	}))
	var made struct{ ID string }
	_ = json.Unmarshal([]byte(res.Text), &made)

	got := graphRecall(t, d, ctx, map[string]any{"seed_ids": []string{made.ID}})
	if len(got) != 1 {
		t.Errorf("a NAMED chunk must be returned even without a sidecar row; got %v", titlesOf(got))
	}
}

// TestGraphRecall_MatchesWholeWordsOnly is the "restating" case, verbatim: a query
// for "statin" matched a configuration paragraph reading "…WITHOUT restating the
// base matrix". A substring LIKE cannot tell those apart.
func TestGraphRecall_MatchesWholeWordsOnly(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)

	upsert(t, d, ctx, docID, "statin-fact", "takes a statin daily", "x", "")
	upsert(t, d, ctx, docID, "restating-fact", "an overlay WITHOUT restating the base matrix", "x", "")

	got := graphRecall(t, d, ctx, map[string]any{"query": "statin"})
	if len(got) != 1 {
		t.Fatalf("want 1 whole-word match, got %d: %v", len(got), titlesOf(got))
	}
	if got[0]["title"] != "takes a statin daily" {
		t.Errorf("matched a substring inside another word: %v", titlesOf(got))
	}
}

// TestGraphRecall_APunctuationQueryStillMatches: \b is undefined at a non-word
// edge, so such a query falls back to substring rather than matching nothing.
// Returning zero seeds for a legitimate-if-odd query is worse than being imprecise.
func TestGraphRecall_APunctuationQueryStillMatches(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	docID := newEntityDoc(t, d, ctx)
	upsert(t, d, ctx, docID, "paren", "uses (parens) in titles", "x", "")

	if got := graphRecall(t, d, ctx, map[string]any{"query": "(parens)"}); len(got) != 1 {
		t.Errorf("a punctuation-edged query must fall back to substring; got %v", titlesOf(got))
	}
}

// ---- helpers ----

// graphRecall drives the op and returns its chunk rows.
func graphRecall(t *testing.T, d *Document, ctx context.Context, in map[string]any) []map[string]any {
	t.Helper()
	in["op"] = "graph_recall"
	in["scope"] = "user"
	res, err := d.Execute(ctx, entityJSON(in))
	if err != nil || res.IsError {
		t.Fatalf("graph_recall: %v %s", err, res.Text)
	}
	var out struct {
		Chunks []map[string]any `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Text)
	}
	return out.Chunks
}

func titlesOf(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, asStr(r["title"]))
	}
	return out
}
