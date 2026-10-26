package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestMigrate0113_MovesTenantDocumentRowsAndIsIdempotent runs the SQL migration
// itself (the boot sweep's Go twin is in the store contract): tenant document
// rows at the old SQL Memory scope id move to "" with their embeddings, a
// conflict keeps the later write, a stray stays, and a re-run changes nothing.
// With and without pgvector — memory_embeddings exists only with it.
func TestMigrate0113_MovesTenantDocumentRowsAndIsIdempotent(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	for _, vectors := range []bool{false, true} {
		name := "without pgvector"
		if vectors {
			name = "with pgvector"
		}
		t.Run(name, func(t *testing.T) {
			if vectors && !pgvectorInstallable(t, dsn) {
				t.Skip("test Postgres cannot install pgvector")
			}
			fix := freshSchemaWithVectors(t, dsn, vectors)
			defer fix.cleanup()
			s := fix.store
			ctx := context.Background()
			ts := store.MemoryScopeTenant
			set := func(tenant, scopeID, key, val string) {
				t.Helper()
				if err := s.MemorySet(ctx, tenant, ts, scopeID, key, []byte(`"`+val+`"`), 0); err != nil {
					t.Fatal(err)
				}
				time.Sleep(3 * time.Millisecond)
			}
			embed := func(tenant, scopeID, key string) {
				t.Helper()
				if !s.SupportsVectors() {
					return
				}
				if err := s.MemoryEmbedSet(ctx, tenant, ts, scopeID, key, store.MemoryEmbedding{
					Provider: "p", Model: "m", Dimension: 3, Vector: []float32{1, 2, 3}, EmbedText: key,
				}); err != nil {
					t.Fatal(err)
				}
			}
			// The old scope id IS sqlmem.ScopeTenant — the rule the SQL restates.
			set("tnt", sqlmem.ScopeTenant("tnt"), "doc.chunk:a", "A")
			embed("tnt", sqlmem.ScopeTenant("tnt"), "doc.chunk:a")
			set("", sqlmem.ScopeTenant(""), "doc.unit:d:summary:0", "D")
			set("tnt", "", "doc.chunk:c", "OLD-dest")
			set("tnt", "tnt", "doc.chunk:c", "NEW-legacy")
			embed("tnt", "tnt", "doc.chunk:c")
			set("tnt", "other", "doc.chunk:s", "STRAY")
			// An old replica's blank write-back, newer than the moved body.
			if err := s.MemorySet(ctx, "tnt", ts, "", "doc.chunk:b", []byte(`{"body":"kept"}`), 0); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * time.Millisecond)
			if err := s.MemorySet(ctx, "tnt", ts, "tnt", "doc.chunk:b", []byte(`{"body":"","fields":{"k":"v"}}`), 0); err != nil {
				t.Fatal(err)
			}

			for run := 1; run <= 2; run++ {
				forceMigrationVersion(t, fix.storeDSN, 112)
				if err := MigrateUp(fix.storeDSN); err != nil {
					t.Fatalf("run %d: MigrateUp re-running 0113: %v", run, err)
				}
				for _, w := range []struct{ tenant, key, val string }{
					{"tnt", "doc.chunk:a", `"A"`}, {"", "doc.unit:d:summary:0", `"D"`}, {"tnt", "doc.chunk:c", `"NEW-legacy"`},
				} {
					e, err := s.MemoryGet(ctx, w.tenant, ts, "", w.key)
					if err != nil || string(e.Value) != w.val {
						t.Errorf("run %d: %q/%s at \"\" = %s (%v), want %s", run, w.tenant, w.key, e.Value, err, w.val)
					}
				}
				for _, g := range []struct{ tenant, scopeID, key string }{
					{"tnt", "tnt", "doc.chunk:a"}, {"", "default", "doc.unit:d:summary:0"}, {"tnt", "tnt", "doc.chunk:c"},
				} {
					if _, err := s.MemoryGet(ctx, g.tenant, ts, g.scopeID, g.key); err == nil {
						t.Errorf("run %d: %s still at the old scope id %q", run, g.key, g.scopeID)
					}
				}
				if e, err := s.MemoryGet(ctx, "tnt", ts, "", "doc.chunk:b"); err != nil || !strings.Contains(string(e.Value), "kept") {
					t.Errorf("run %d: a newer BLANK old body replaced the destination: %s (%v)", run, e.Value, err)
				}
				if _, err := s.MemoryGet(ctx, "tnt", ts, "tnt", "doc.chunk:b"); err == nil {
					t.Errorf("run %d: the blank old row was left behind", run)
				}
				if _, err := s.MemoryGet(ctx, "tnt", ts, "other", "doc.chunk:s"); err != nil {
					t.Errorf("run %d: the stray moved: %v", run, err)
				}
				if s.SupportsVectors() {
					for _, k := range []string{"doc.chunk:a", "doc.chunk:c"} {
						if _, err := s.MemoryEmbedGet(ctx, "tnt", ts, "", k); err != nil {
							t.Errorf("run %d: embedding of %s not at \"\": %v", run, k, err)
						}
					}
				}
			}
		})
	}
}
