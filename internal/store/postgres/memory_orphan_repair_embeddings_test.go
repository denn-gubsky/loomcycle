package postgres

import (
	"context"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMemoryOrphanRepair_MovesRowsThatCarryEmbeddings — memory_embeddings
// references memory by (tenant_id, scope, scope_id, key) with no ON UPDATE action,
// so re-stamping memory.tenant_id alone is refused by the foreign key wherever an
// orphan has an embedding: the repair failed on exactly the rows (document chunk
// bodies) whose loss it exists to fix. The embedding moves with its row.
func TestMemoryOrphanRepair_MovesRowsThatCarryEmbeddings(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	if !pgvectorInstallable(t, dsn) {
		t.Skip("test Postgres cannot install pgvector")
	}
	fix := freshSchemaWithVectors(t, dsn, true)
	defer fix.cleanup()
	s := fix.store
	ctx := context.Background()
	for _, key := range []string{"doc.chunk:a", "notes/b"} {
		if err := s.MemorySet(ctx, "", store.MemoryScopeUser, "u1", key, []byte(`"v"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, "u1", "doc.chunk:a", store.MemoryEmbedding{
		Provider: "p", Model: "m", Dimension: 3, Vector: []float32{1, 2, 3}, EmbedText: "a",
	}); err != nil {
		t.Fatal(err)
	}

	// A collision: the key already exists at the target, so the orphan and its
	// embedding stay where they are.
	if err := s.MemorySet(ctx, "", store.MemoryScopeUser, "u1", "doc.chunk:c", []byte(`"legacy"`), 0); err != nil {
		t.Fatal(err)
	}
	if err := s.MemoryEmbedSet(ctx, "", store.MemoryScopeUser, "u1", "doc.chunk:c", store.MemoryEmbedding{
		Provider: "p", Model: "m", Dimension: 3, Vector: []float32{1, 2, 3}, EmbedText: "c",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.MemorySet(ctx, "tnt", store.MemoryScopeUser, "u1", "doc.chunk:c", []byte(`"live"`), 0); err != nil {
		t.Fatal(err)
	}

	rep, err := s.MemoryOrphanRepair(ctx, "tnt", nil)
	if err != nil {
		t.Fatalf("repair with an embedded orphan: %v", err)
	}
	if rep.Moved != 2 {
		t.Errorf("moved %d rows, want 2: %+v", rep.Moved, rep)
	}
	if _, err := s.MemoryEmbedGet(ctx, "tnt", store.MemoryScopeUser, "u1", "doc.chunk:a"); err != nil {
		t.Errorf("the embedding did not follow its row to the tenant: %v", err)
	}
	if _, err := s.MemoryEmbedGet(ctx, "", store.MemoryScopeUser, "u1", "doc.chunk:a"); err == nil {
		t.Error("an embedding is left at the legacy tenant")
	}
	if _, err := s.MemoryEmbedGet(ctx, "", store.MemoryScopeUser, "u1", "doc.chunk:c"); err != nil {
		t.Errorf("the collision's embedding moved away from its row: %v", err)
	}
}
