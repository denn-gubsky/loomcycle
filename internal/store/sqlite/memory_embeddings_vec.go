//go:build sqlite_vec

package sqlite

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Vector Memory implementation — sqlite_vec BUILD ONLY.
//
// Built into binaries compiled with `-tags=sqlite_vec`. The driver
// swap to github.com/mattn/go-sqlite3 + the ConnectHook in
// driver_vec.go ensures the sqlite-vec C extension is loaded at
// every database open. From here, the `vec0` virtual-table API is
// available.
//
// STATUS in v0.10.2: the extension-loading mechanism is wired and
// tested (Open() succeeds against a binary built with -tags=sqlite_vec
// + LOOMCYCLE_SQLITE_VEC_PATH pointing at vec0.so/dylib). The
// MemoryEmbed* methods are still stubbed pending the full vec0
// virtual-table schema design — sqlite-vec's vec0 has dimension-fixing
// constraints that don't directly model loomcycle's per-row dimension
// posture (Postgres+pgvector supports per-row variable dimensions
// natively; vec0 needs either a single fixed dim or one virtual
// table per dimension). The design choice between those two paths is
// the load-bearing decision for v0.10.3.
//
// For now, operators selecting `-tags=sqlite_vec` get:
//   - A working CGO binary that loads the sqlite-vec extension
//   - SupportsVectors() returns true (the extension IS available)
//   - MemoryEmbed* methods return a distinct error message pointing
//     at the v0.10.3 follow-up
//
// This preserves the "I'm running the right build" signal for
// operators and unblocks the architectural decision (build-tag split)
// while deferring the schema-design decision (per-dim partitioning vs
// fixed-dim) to v0.10.3 when we have a sqlite-vec performance
// benchmark to inform it.

// SupportsVectors reports false in v0.10.2 — the sqlite-vec extension
// IS loaded at every connection-open via driver_vec.go's ConnectHook
// (visible via the boot log line in driver_vec.go), but the
// MemoryEmbed* methods themselves are still stubbed. Reporting false
// here keeps the Memory tool's "vector_unsupported" gating consistent
// with the actual capability — operators don't get false-positives in
// /v1/_memory/embed_stats or the embedder selection logic that polls
// SupportsVectors.
//
// Crucially this also makes the internal/store/storetest contract
// suite skip the vector round-trip tests on -tags=sqlite_vec builds
// (the contract gates on SupportsVectors()), keeping CI green
// regardless of build tag. When v0.10.3 wires the real implementation,
// flip this to return true and the contract tests run for real.
func (s *Store) SupportsVectors() bool {
	return false
}

// errVecImplPending is the v0.10.2 stub error for sqlite_vec-tagged
// builds. Distinct from ErrVectorUnsupported (which the default
// build returns) so operators reading logs can tell "I built with
// the tag but the methods are pending" apart from "I forgot the
// tag." The message points at the follow-up issue rather than
// re-suggesting the build tag.
var errVecImplPending = &store.MemoryError{
	Code: "sqlite_vec_impl_pending",
	Msg: "memory: sqlite-vec extension loaded but MemoryEmbed* implementation pending (v0.10.3 follow-up — " +
		"see internal/store/sqlite/memory_embeddings_vec.go docstring for the per-dim-partitioning design tradeoff)",
}

func (s *Store) MemoryEmbedSet(ctx context.Context, scope store.MemoryScope, scopeID, key string, e store.MemoryEmbedding) error {
	return errVecImplPending
}

func (s *Store) MemoryEmbedGet(ctx context.Context, scope store.MemoryScope, scopeID, key string) (store.MemoryEmbedding, error) {
	return store.MemoryEmbedding{}, errVecImplPending
}

func (s *Store) MemoryEmbedSearch(ctx context.Context, scope store.MemoryScope, scopeID, keyPrefix string, query []float32, topK int) ([]store.MemorySearchEntry, error) {
	return nil, errVecImplPending
}

func (s *Store) MemoryEmbedListByModel(ctx context.Context, scope store.MemoryScope, scopeID, currentProvider, currentModel string, limit int) ([]store.MemoryEntry, error) {
	return nil, errVecImplPending
}

func (s *Store) MemoryEmbedStats(ctx context.Context, scope store.MemoryScope) (store.MemoryEmbedStats, error) {
	return store.MemoryEmbedStats{}, errVecImplPending
}
