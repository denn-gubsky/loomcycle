package builtin

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/lanegate"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// laneMarkEmbedder records, per embedding, whether it was asked for as
// background work.
type laneMarkEmbedder struct {
	providers.Embedder
	mu         sync.Mutex
	background []bool
}

func (e *laneMarkEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.background = append(e.background, lanegate.IsBackground(ctx))
	e.mu.Unlock()
	return e.Embedder.Embed(ctx, texts)
}

func (e *laneMarkEmbedder) counts() (background, foreground int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, bg := range e.background {
		if bg {
			background++
		} else {
			foreground++
		}
	}
	return
}

// TestReindex_TheDetachedPassEmbedsAsBackgroundWork — an edit's own embedding
// is the caller's: it is made while the op is still answering. The re-index of
// a large subtree that goes on after the op has returned is nobody's request
// any more, and its embeddings must queue in the embedder's lane behind the
// calls of live runs, not ahead of them by the hundred.
func TestReindex_TheDetachedPassEmbedsAsBackgroundWork(t *testing.T) {
	d, _, ctx := mermaidDocFixture(t, "section")
	marks := &laneMarkEmbedder{Embedder: d.Embedder}
	d.Embedder = marks
	out := docOp(t, d, ctx, map[string]any{"op": "create_document", "title": "Old name"})
	docID, rootID := out["document_id"].(string), out["root_chunk_id"].(string)
	const chunks = reindexSyncMax + 8
	for i := 0; i < chunks; i++ {
		docOp(t, d, ctx, map[string]any{"op": "create_chunk", "document_id": docID,
			"title": fmt.Sprintf("Section %d", i), "body": "section text"})
	}
	d.waitReindex()
	if bg, fg := marks.counts(); bg != 0 || fg < chunks {
		t.Fatalf("creating %d chunks: %d background and %d foreground embeddings, want none background — these are the caller's own writes", chunks, bg, fg)
	}

	docOp(t, d, ctx, map[string]any{"op": "update_chunk", "id": rootID,
		"revision": chunkRevision(t, d, ctx, rootID), "title": "New name"})
	d.waitReindex()
	if bg, _ := marks.counts(); bg < chunks {
		t.Fatalf("the detached re-index of %d chunks made %d background embeddings: the pass queued as live work", chunks, bg)
	}
}
