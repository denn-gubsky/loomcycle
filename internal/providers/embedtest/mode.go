// Package embedtest holds a test double shared by the packages that embed text:
// an embedder that records which side of the query/document split each text was
// embedded on.
package embedtest

import (
	"context"
	"slices"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// ModeRecorder wraps an embedder with a query mode and records, per text,
// whether it came in through Embed (stored text) or EmbedQuery (a search
// query). Both modes return the inner embedder's Embed vectors, so retrieval
// behaves exactly as it would with the inner embedder alone and only the mode
// is under test.
//
// It deliberately has no Unwrap: it stands in for a DRIVER with a query mode,
// not for a wrapper.
type ModeRecorder struct {
	inner providers.Embedder

	mu        sync.Mutex
	documents []string
	queries   []string
}

var _ providers.QueryEmbedder = (*ModeRecorder)(nil)

// New wraps inner.
func New(inner providers.Embedder) *ModeRecorder { return &ModeRecorder{inner: inner} }

func (r *ModeRecorder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.documents = append(r.documents, texts...)
	r.mu.Unlock()
	return r.inner.Embed(ctx, texts)
}

func (r *ModeRecorder) EmbedQuery(ctx context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.queries = append(r.queries, texts...)
	r.mu.Unlock()
	return r.inner.Embed(ctx, texts)
}

func (r *ModeRecorder) Model() string    { return r.inner.Model() }
func (r *ModeRecorder) Provider() string { return r.inner.Provider() }
func (r *ModeRecorder) Dimension() int   { return r.inner.Dimension() }

// Documents returns every text embedded through Embed, in order.
func (r *ModeRecorder) Documents() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.documents)
}

// Queries returns every text embedded through EmbedQuery, in order.
func (r *ModeRecorder) Queries() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.queries)
}
