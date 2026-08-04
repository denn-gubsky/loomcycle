package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// recordingEmbedder captures what text was handed to the embedder, so a test can
// assert on the DECISION (embed this, skip that) without needing a vector-capable
// store. The plain :memory: sqlite the document fixture uses has no vector
// support, so MemoryEmbedSet would refuse — but whether we CALL the embedder, and
// with what, is the thing phase 1 is actually deciding.
type recordingEmbedder struct {
	mu    sync.Mutex
	texts []string
	fail  error
}

func (e *recordingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.texts = append(e.texts, texts...)
	e.mu.Unlock()
	if e.fail != nil {
		return nil, e.fail
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

func (e *recordingEmbedder) Dimension() int   { return 4 }
func (e *recordingEmbedder) Model() string    { return "recording-stub" }
func (e *recordingEmbedder) Provider() string { return "test" }

func (e *recordingEmbedder) seen() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.texts...)
}

// TestEmbedBody_ProseIsEmbeddedWithItsBodyText is the phase-1 deliverable: a prose
// chunk's body reaches the embedder, so `memory op=search` with prefix
// "doc.chunk:" can find it. Before this the searchable half of a document (the SQL
// chunks table) held no body text, while the half holding the text (the k/v plane)
// had no index — document prose was unreachable by any agent-visible search.
func TestEmbedBody_ProseIsEmbeddedWithItsBodyText(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	emb := &recordingEmbedder{}
	d.Embedder = emb

	const body = "SAVEPOINT nesting is LIFO; the same three ops work at any depth."
	bj, _ := json.Marshal(body)
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Nested transactions","body":`+string(bj)+`}`); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}

	seen := emb.seen()
	var found bool
	for _, s := range seen {
		if strings.Contains(s, "SAVEPOINT nesting is LIFO") {
			found = true
		}
	}
	if !found {
		t.Errorf("the body was never handed to the embedder, so the chunk is not "+
			"searchable; embedder saw %d text(s): %q", len(seen), seen)
	}
}

// TestEmbedBody_ImageBodiesAreSkipped — an image body is a rendered media form (a
// data URI): embedding it indexes base64, which matches nothing while consuming the
// scope's vector quota. The searchable text for an image is a generated description,
// which phase 4 owns, so it is skipped rather than indexed as garbage.
//
// Mermaid was skipped here too until phase 3 gave it deterministic label extraction
// — see document_mermaid_test.go, which now asserts the opposite for diagrams.
func TestEmbedBody_ImageBodiesAreSkipped(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	emb := &recordingEmbedder{}
	d.Embedder = emb
	bj, _ := json.Marshal("![diagram](data:image/png;base64,iVBORw0KGgoAAAANSUhEUg==)")
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"media","body":`+string(bj)+`}`); r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	if seen := emb.seen(); len(seen) != 0 {
		t.Errorf("an image body was embedded: %q — the searchable text for an image is "+
			"a generated description, not the data URI", seen)
	}
}

// TestEmbedBody_WriteSurvivesAnEmbedderFailure is the guarantee that keeps
// authoring independent of the embedder.
//
// An unembedded chunk is unsearchable; a REJECTED write is lost work the author
// has to redo. A cold embedder must therefore never fail the write — and this is
// not hypothetical: a 24.9 GB local model returning `unexpected EOF` on cold load
// is exactly the failure this session hit while running the memory eval.
func TestEmbedBody_WriteSurvivesAnEmbedderFailure(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	d.Embedder = &recordingEmbedder{fail: context.DeadlineExceeded}

	out, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Survives","body":"durable prose"}`)
	if r.IsError {
		t.Fatalf("a failing embedder rejected the write: %s", r.Text)
	}
	id := asStr(out["id"])

	// And the body is readable, so nothing was half-written.
	got, r2 := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+id+`"}`)
	if r2.IsError {
		t.Fatalf("get_chunk: %s", r2.Text)
	}
	if asStr(got["body"]) != "durable prose" {
		t.Errorf("body = %q, want it stored despite the embedder failing", asStr(got["body"]))
	}
}

// TestEmbedBody_NilEmbedderIsAClean NoOp — most internal construction sites
// (ontology provisioning, the tenant-root probe) have no embedder, and a required
// dependency would force them to fabricate one. Nil must behave exactly as before.
func TestEmbedBody_NilEmbedderIsACleanNoOp(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)
	d.Embedder = nil // explicit: this is the state most call sites are in
	out, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"No embedder","body":"still stored"}`)
	if r.IsError {
		t.Fatalf("nil embedder broke the write: %s", r.Text)
	}
	got, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+asStr(out["id"])+`"}`)
	if asStr(got["body"]) != "still stored" {
		t.Errorf("body = %q, want it stored", asStr(got["body"]))
	}
}
