package providers_test

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// modeEmbedder counts calls per mode.
type modeEmbedder struct{ embeds, queries int }

func (m *modeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	m.embeds++
	return make([][]float32, len(texts)), nil
}
func (m *modeEmbedder) EmbedQuery(_ context.Context, texts []string) ([][]float32, error) {
	m.queries++
	return make([][]float32, len(texts)), nil
}
func (m *modeEmbedder) Model() string    { return "m" }
func (m *modeEmbedder) Provider() string { return "p" }
func (m *modeEmbedder) Dimension() int   { return 0 }

// docOnlyEmbedder has no query mode.
type docOnlyEmbedder struct{ embeds int }

func (d *docOnlyEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	d.embeds++
	return make([][]float32, len(texts)), nil
}
func (d *docOnlyEmbedder) Model() string    { return "m" }
func (d *docOnlyEmbedder) Provider() string { return "p" }
func (d *docOnlyEmbedder) Dimension() int   { return 0 }

// opaqueWrapper unwraps to its inner embedder but forwards only Embed — the
// shape of a wrapper that forgot EmbedQuery.
type opaqueWrapper struct{ inner providers.Embedder }

func (w *opaqueWrapper) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return w.inner.Embed(ctx, texts)
}
func (w *opaqueWrapper) Model() string              { return w.inner.Model() }
func (w *opaqueWrapper) Provider() string           { return w.inner.Provider() }
func (w *opaqueWrapper) Dimension() int             { return w.inner.Dimension() }
func (w *opaqueWrapper) Unwrap() providers.Embedder { return w.inner }

func TestEmbedQuery_UsesTheQueryModeWhenTheValueHasOne(t *testing.T) {
	m := &modeEmbedder{}
	if _, err := providers.EmbedQuery(context.Background(), m, []string{"q"}); err != nil {
		t.Fatal(err)
	}
	if m.queries != 1 || m.embeds != 0 {
		t.Fatalf("queries=%d embeds=%d, want the query mode once and Embed never", m.queries, m.embeds)
	}
}

func TestEmbedQuery_FallsBackToEmbedWithoutAQueryMode(t *testing.T) {
	d := &docOnlyEmbedder{}
	vecs, err := providers.EmbedQuery(context.Background(), d, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if d.embeds != 1 || len(vecs) != 2 {
		t.Fatalf("embeds=%d vectors=%d, want Embed once with both vectors back", d.embeds, len(vecs))
	}
}

// TestEmbedQuery_AsksOnlyTheValueItIsGiven pins the design: the helper does not
// walk Unwrap to the driver (that would skip the lane and the health counters
// of the wrappers in between), so a wrapper without EmbedQuery ends in Embed.
// The wrapper census is what keeps the real wrappers from being this one.
func TestEmbedQuery_AsksOnlyTheValueItIsGiven(t *testing.T) {
	m := &modeEmbedder{}
	if _, err := providers.EmbedQuery(context.Background(), &opaqueWrapper{inner: m}, []string{"q"}); err != nil {
		t.Fatal(err)
	}
	if m.queries != 0 || m.embeds != 1 {
		t.Fatalf("queries=%d embeds=%d, want Embed through the wrapper and no reach past it", m.queries, m.embeds)
	}
}
