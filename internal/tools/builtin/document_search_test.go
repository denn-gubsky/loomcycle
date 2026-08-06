package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDocumentSearch_FindsChunksByFreeTextWithoutAKeyFormat is the reason this op
// exists (RFC BU §6). Without it the way into a document is
// `memory op=search prefix="doc.chunk:"`, which requires knowing that chunk bodies
// live in the memory keyspace, the exact spelling of a reserved prefix, and that the
// chunk id must be cut back off the key. A live transcript showed an agent failing at
// exactly that: it did not know a reserved string, guessed, and concluded nothing was
// remembered.
func TestDocumentSearch_FindsChunksByFreeTextWithoutAKeyFormat(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "savepoint", "nesting", "lifo", "deploy", "runbook")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Ops"}`))
	docID := resultField(res, "document_id")
	mk := func(title, body string) string {
		b, _ := json.Marshal(map[string]any{
			"op": "create_chunk", "document_id": docID, "title": title, "body": body,
		})
		r, err := d.Execute(ctx, b)
		if err != nil || r.IsError {
			t.Fatalf("create_chunk %q: %v %s", title, err, r.Text)
		}
		return resultField(r, "id")
	}
	wantID := mk("Transactions", "SAVEPOINT nesting is LIFO")
	mk("Deploys", "the deploy runbook")

	out, err := d.Execute(ctx, json.RawMessage(`{"op":"search","query":"savepoint nesting"}`))
	if err != nil || out.IsError {
		t.Fatalf("search: %v %s", err, out.Text)
	}
	var got struct {
		Chunks []struct {
			ChunkID    string  `json:"chunk_id"`
			Score      float64 `json:"score"`
			Title      string  `json:"title"`
			DocumentID string  `json:"document_id"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(out.Text), &got); err != nil {
		t.Fatalf("unmarshal: %v — %s", err, out.Text)
	}
	if len(got.Chunks) == 0 {
		t.Fatal("no chunks returned")
	}
	if got.Chunks[0].ChunkID != wantID {
		t.Errorf("best match = %s, want %s (the SAVEPOINT chunk)", got.Chunks[0].ChunkID, wantID)
	}
	// ENRICHED, which is the other half of the value: a hit is navigable without a
	// second round trip, and no caller has to parse a key.
	if got.Chunks[0].Title != "Transactions" {
		t.Errorf("hit carries no title (%q) — a caller would need another call to know "+
			"what it found", got.Chunks[0].Title)
	}
	if got.Chunks[0].DocumentID != docID {
		t.Errorf("hit carries document_id %q, want %q", got.Chunks[0].DocumentID, docID)
	}
	// And the raw key never leaks: the caller asked for chunks, not memory rows.
	if strings.Contains(out.Text, "doc.chunk:") {
		t.Errorf("the reserved key prefix leaked into the result: %s", out.Text)
	}
}

// TestDocumentSearch_RefusesWithoutAQueryOrAnEmbedder — both refusals name what is
// missing. A semantic search with no embedder is a deployment state, not a bug, and
// saying so beats returning an empty list that reads as "nothing matches".
func TestDocumentSearch_RefusesWithoutAQueryOrAnEmbedder(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "anything")

	res, err := d.Execute(ctx, json.RawMessage(`{"op":"search"}`))
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "query") {
		t.Errorf("a search with no query should refuse and name the field, got: %s", res.Text)
	}

	d.Embedder = nil
	res, err = d.Execute(ctx, json.RawMessage(`{"op":"search","query":"anything"}`))
	if err != nil {
		t.Fatalf("hard error: %v", err)
	}
	if !res.IsError || !strings.Contains(res.Text, "embedder") {
		t.Errorf("a search with no embedder should refuse and say so rather than return "+
			"an empty set, got: %s", res.Text)
	}
}

// TestDocumentSearch_OrderIsByScore guards the enrichment loop: metadata is fetched in
// one batch keyed by id, so rebuilding results from that map would return them in map
// order. A caller reading the first element must get the best match.
func TestDocumentSearch_OrderIsByScore(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "alpha", "beta", "gamma", "delta", "epsilon")

	res, _ := d.Execute(ctx, json.RawMessage(`{"op":"create_document","title":"Words"}`))
	docID := resultField(res, "document_id")
	for _, w := range []string{"alpha", "beta", "gamma", "delta", "epsilon"} {
		b, _ := json.Marshal(map[string]any{
			"op": "create_chunk", "document_id": docID, "title": w, "body": w,
		})
		if r, err := d.Execute(ctx, b); err != nil || r.IsError {
			t.Fatalf("create_chunk: %v %s", err, r.Text)
		}
	}
	out, err := d.Execute(ctx, json.RawMessage(`{"op":"search","query":"gamma"}`))
	if err != nil || out.IsError {
		t.Fatalf("search: %v %s", err, out.Text)
	}
	var got struct {
		Chunks []struct {
			Score float64 `json:"score"`
			Title string  `json:"title"`
		} `json:"chunks"`
	}
	_ = json.Unmarshal([]byte(out.Text), &got)
	if len(got.Chunks) < 2 {
		t.Skipf("need at least 2 hits to check ordering, got %d", len(got.Chunks))
	}
	for i := 1; i < len(got.Chunks); i++ {
		if got.Chunks[i].Score > got.Chunks[i-1].Score {
			t.Errorf("results are not score-ordered: %v at %d beats %v at %d",
				got.Chunks[i].Score, i, got.Chunks[i-1].Score, i-1)
		}
	}
}
