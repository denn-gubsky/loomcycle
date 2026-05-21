package sqlite

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Vector Memory refusal stubs.
//
// v0.9.0 ships Postgres-only vector support. The SQLite implementation
// here refuses every MemoryEmbed* call with ErrVectorUnsupported and
// reports SupportsVectors() == false unconditionally.
//
// The sqlite-vec story (a build-tag swap to mattn/go-sqlite3 + cgo +
// the loadable extension) lands in v0.9.1. modernc.org/sqlite (the
// pure-Go driver this binary links against today) can't load C
// extensions, so without the swap there's no path to a vector index
// on SQLite. The refusal-with-clear-message is the documented v0.9.0
// behaviour per the RFC at rfcs/semantic-memory.md.

// SupportsVectors reports false for SQLite in v0.9.0.
func (s *Store) SupportsVectors() bool {
	return false
}

func (s *Store) MemoryEmbedSet(ctx context.Context, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error {
	return store.ErrVectorUnsupported
}

func (s *Store) MemoryEmbedGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEmbedding, error) {
	return store.MemoryEmbedding{}, store.ErrVectorUnsupported
}

func (s *Store) MemoryEmbedSearch(ctx context.Context, scope store.MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	return nil, store.ErrVectorUnsupported
}

func (s *Store) MemoryEmbedListByModel(ctx context.Context, scope store.MemoryScope, scopeID, currentProvider, currentModel string, limit int) ([]store.MemoryEntry, error) {
	return nil, store.ErrVectorUnsupported
}

func (s *Store) MemoryEmbedStats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	return store.MemoryEmbedStats{}, store.ErrVectorUnsupported
}
