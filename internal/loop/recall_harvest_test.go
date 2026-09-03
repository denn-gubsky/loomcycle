package loop

import (
	"context"
	"hash/fnv"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/recall"
)

// recallTestEmbedder is a deterministic bag-of-words embedder so a query sharing
// the needle's tokens retrieves it — enough to prove the harvest wired the evicted
// span into the index, without a real embedding model.
type recallTestEmbedder struct{ dim int }

func (e recallTestEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, e.dim)
		for _, tok := range strings.Fields(strings.ToLower(t)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[h.Sum32()%uint32(e.dim)]++
		}
		out[i] = v
	}
	return out, nil
}

func (e recallTestEmbedder) Model() string    { return "bag" }
func (e recallTestEmbedder) Provider() string { return "test" }
func (e recallTestEmbedder) Dimension() int   { return e.dim }

// TestMaybeAutoCompact_HarvestsEvictedSpanToRecallIndex: recall-augmented
// distillation must embed the exact span a compaction drops into the run-scoped
// index, so a later free-text Recall recovers a needle buried past the boundary.
func TestMaybeAutoCompact_HarvestsEvictedSpanToRecallIndex(t *testing.T) {
	msgs := []providers.Message{
		userMsg("the task"), asstMsg("the secret token is orchid-88 keep it"),
		userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3"),
	}
	ix := recall.NewIndex(recallTestEmbedder{dim: 256}, 0)
	opts := RunOptions{
		Provider:    &steerProvider{}, // Call returns "ok" → summary succeeds
		Model:       "x",
		Compaction:  &config.Compaction{KeepLastN: cptr(2), KeepFirst: cptr(true), TargetPercentage: cptr(10)},
		RecallIndex: ix,
	}
	_, did := maybeAutoCompact(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("expected compaction to happen")
	}
	// Evicted = the span between the pinned first turn and the kept last-2 tail:
	// a1, q2, a2 — three messages, indexed per-message.
	if ix.Len() != 3 {
		t.Fatalf("index Len = %d, want 3 evicted spans harvested", ix.Len())
	}
	hits, err := ix.Search(context.Background(), "what is the secret token", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Text, "orchid-88") {
		t.Fatalf("recall did not recover the evicted needle: %+v", hits)
	}
}

// TestMaybeAutoCompact_NilRecallIndexNoOp: with recall off (nil index),
// compaction proceeds unchanged and the nil-receiver Harvest never panics.
func TestMaybeAutoCompact_NilRecallIndexNoOp(t *testing.T) {
	msgs := []providers.Message{userMsg("the task"), asstMsg("a1"), userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3")}
	opts := RunOptions{
		Provider:   &steerProvider{},
		Model:      "x",
		Compaction: &config.Compaction{KeepLastN: cptr(2), KeepFirst: cptr(true), TargetPercentage: cptr(10)},
	}
	if _, did := maybeAutoCompact(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto"); !did {
		t.Fatal("compaction should still happen with recall off")
	}
}

// TestMaybeRecap_HarvestsEvictedSpan: the recap path drops the evicted reasoning
// (or, in drop mode, everything) — exactly where recall is most valuable — so it
// must harvest the span too, not only the compaction path.
func TestMaybeRecap_HarvestsEvictedSpan(t *testing.T) {
	msgs := []providers.Message{
		userMsg("the task"), asstMsg("the api key rotates to violet-31 tomorrow"),
		userMsg("q2"), asstMsg("a2"), userMsg("q3"), asstMsg("a3"),
	}
	ix := recall.NewIndex(recallTestEmbedder{dim: 256}, 0)
	opts := RunOptions{
		Provider: &steerProvider{},
		Model:    "x",
		// reasoning=drop: no recap call, pure eviction — the harsh case.
		Context:     &config.Context{Mode: cptr(config.ContextModeRecap), KeepLastN: cptr(2), Reasoning: cptr("drop")},
		RecallIndex: ix,
	}
	_, did := maybeRecap(context.Background(), opts, msgs, 0, func(providers.Event) {}, "auto")
	if !did {
		t.Fatal("expected recap distillation to happen")
	}
	if ix.Len() == 0 {
		t.Fatal("recap dropped the span without harvesting it into the recall index")
	}
	hits, _ := ix.Search(context.Background(), "when does the api key rotate", 3)
	if len(hits) == 0 || !strings.Contains(hits[0].Text, "violet-31") {
		t.Fatalf("recall did not recover the evicted needle from the recap path: %+v", hits)
	}
}
