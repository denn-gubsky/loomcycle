package inprocess_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	memory "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/providers/embedtest"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestInProcess_SearchEmbedsTheQueryInQueryMode — RFC EV §5.3: the text a
// Search compares against stored vectors is a query; the text Set stores is
// not. (This Search also serves the consolidation merge pass, which is why
// memory-calibrate measures the same query-against-document comparison.)
func TestInProcess_SearchEmbedsTheQueryInQueryMode(t *testing.T) {
	s, cleanup := newStore(t)
	defer cleanup()
	rec := embedtest.New(newFakeEmbedder("alice", "bob", "go", "rust", "python"))
	b := inprocess.New(newVectorStore(s), rec)
	ctx := context.Background()

	if r, err := b.Set(ctx, store.MemoryScopeAgent, "a1", "rec1", json.RawMessage(`1`), memory.SetOptions{Embed: true, EmbedText: "alice go rust"}); err != nil || !r.Embedded {
		t.Fatalf("set: r=%+v err=%v", r, err)
	}
	if _, err := b.Search(ctx, store.MemoryScopeAgent, "a1", memory.SearchQuery{QueryText: "go rust", TopK: 5}, memory.DefaultRankConfig(), memory.DedupConfig{}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if got := rec.Queries(); !slices.Equal(got, []string{"go rust"}) {
		t.Errorf("query-mode texts = %q, want only the search query", got)
	}
	if got := rec.Documents(); !slices.Equal(got, []string{"alice go rust"}) {
		t.Errorf("stored-text embeddings = %q, want only what Set stored", got)
	}
}
