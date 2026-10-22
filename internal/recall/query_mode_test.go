package recall

import (
	"context"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/providers/embedtest"
)

// TestIndex_SearchEmbedsTheQueryInQueryMode — Harvest stores turns, Search
// compares a query against them.
func TestIndex_SearchEmbedsTheQueryInQueryMode(t *testing.T) {
	rec := embedtest.New(bagEmbedder{dim: 64})
	ix := NewIndex(rec, 0)
	ix.Harvest(context.Background(), []providers.Message{userMsg("the deployment token is zephyr-quartz-42")})
	if _, err := ix.Search(context.Background(), "what is the deployment token", 3); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := rec.Queries(); !slices.Equal(got, []string{"what is the deployment token"}) {
		t.Errorf("query-mode texts = %q, want only the search query", got)
	}
	docs := rec.Documents()
	if len(docs) == 0 || slices.Contains(docs, "what is the deployment token") {
		t.Errorf("stored-text embeddings = %q, want the harvested turn and not the query", docs)
	}
}
