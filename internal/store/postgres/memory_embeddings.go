package postgres

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Vector Memory stubs — commit 1 of PR 1 adds the interface methods
// and returns ErrVectorUnsupported on every call so the Store interface
// compiles. Commit 2 replaces these with real pgvector-backed
// implementations + the boot-time `CREATE EXTENSION vector` check + the
// 0017_memory_embeddings migration.
//
// SupportsVectors() returns false here; commit 2 changes it to track
// the LOOMCYCLE_PGVECTOR_ENABLED env var + the extension probe.

// SupportsVectors reports whether vector ops are available. Stub
// implementation; replaced in commit 2.
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
