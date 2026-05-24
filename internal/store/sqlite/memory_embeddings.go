//go:build !sqlite_vec

package sqlite

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Vector Memory refusal stubs — DEFAULT BUILD.
//
// modernc.org/sqlite (the pure-Go driver this binary links against
// without `-tags=sqlite_vec`) can't load C extensions, so there's no
// path to a vector index on the default-build SQLite. The
// refusal-with-clear-message preserves the "single static binary, no
// CGO" posture for operators who don't need vectors on SQLite.
//
// Operators who DO want vectors on SQLite build with
// `-tags=sqlite_vec` — that swaps the driver to mattn/go-sqlite3
// (CGO) and routes through memory_embeddings_vec.go, which loads
// the sqlite-vec extension via the driver.ConnectHook in
// driver_vec.go.
//
// Postgres operators set LOOMCYCLE_PGVECTOR_ENABLED=1 and use
// pgvector instead; this stub doesn't apply to them.

// SupportsVectors reports false for the default SQLite build.
// (The sqlite_vec-tagged build overrides this in
// memory_embeddings_vec.go.)
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
