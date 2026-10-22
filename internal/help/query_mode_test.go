package help

import (
	"context"
	"slices"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers/embedtest"
)

// TestHelpQuery_EmbedsTheQueryInQueryMode — reconcile stores sections, the
// hybrid query compares a question against them.
func TestHelpQuery_EmbedsTheQueryInQueryMode(t *testing.T) {
	set := newHelpTestSet(&Topic{Name: "alpha", Description: "d", Content: "## Widgets\nThe frobnicator assembles widgets neatly.\n"})
	st := newHelpFakeStore()
	rec := embedtest.New(newHelpFakeEmbedder("frobnicator", "assembles", "widgets", "neatly"))
	ctx := context.Background()
	if _, err := ReconcileIndex(ctx, set, st, rec); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	res, err := QueryIndex(ctx, set, st, rec, "frobnicator widgets", 5)
	if err != nil || res.Mode != "hybrid" {
		t.Fatalf("query = mode %q, %v; want the hybrid path", res.Mode, err)
	}
	if got := rec.Queries(); !slices.Equal(got, []string{"frobnicator widgets"}) {
		t.Errorf("query-mode texts = %q, want only the help query", got)
	}
	docs := rec.Documents()
	if len(docs) == 0 || slices.Contains(docs, "frobnicator widgets") {
		t.Errorf("stored-text embeddings = %q, want the reconciled sections and not the query", docs)
	}
}
