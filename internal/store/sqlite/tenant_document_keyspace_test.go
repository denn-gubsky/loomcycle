package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMigrate_MovesTenantDocumentRowsOnOpen: opening a database an older binary
// wrote moves its tenant document rows to the Memory tenant keyspace. The old rows
// are seeded at sqlmem.ScopeTenant — the function whose rule the migration's SQL
// restates — so a drift between the two fails here.
func TestMigrate_MovesTenantDocumentRowsOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyspace.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tenant := range []string{"acme", ""} {
		if err := s1.MemorySet(ctx, tenant, store.MemoryScopeTenant, sqlmem.ScopeTenant(tenant), "doc.chunk:x", []byte(`"body"`), 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path) // runs migrate()
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer s2.Close()
	for _, tenant := range []string{"acme", ""} {
		if _, err := s2.MemoryGet(ctx, tenant, store.MemoryScopeTenant, "", "doc.chunk:x"); err != nil {
			t.Errorf("tenant %q: body not moved to scope_id \"\": %v", tenant, err)
		}
		if _, err := s2.MemoryGet(ctx, tenant, store.MemoryScopeTenant, sqlmem.ScopeTenant(tenant), "doc.chunk:x"); err == nil {
			t.Errorf("tenant %q: body still at the old scope id", tenant)
		}
	}
}
