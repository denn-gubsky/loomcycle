package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The Postgres tier writes this query differently from SQLite — COUNT(t.key)
// rather than SUM(CASE …), `= ANY($3)` rather than an expanded placeholder list,
// and a $3 that exists only when a filter was requested. That is exactly the
// kind of per-tier divergence that drifts silently, so the sqlite assertions in
// internal/store/sqlite/memory_orphan_test.go are mirrored here.
func TestMemoryOrphan_PostgresParity(t *testing.T) {
	dsn := pgDSNFromEnv(t)
	fx := freshSchema(t, dsn)
	defer fx.cleanup()
	s := fx.store
	ctx := context.Background()

	seed := func(tenant string, scope store.MemoryScope, scopeID, key, val string) {
		t.Helper()
		if err := s.MemorySet(ctx, tenant, scope, scopeID, key, json.RawMessage(`"`+val+`"`), 0); err != nil {
			t.Fatalf("seed %s/%s/%s: %v", tenant, scopeID, key, err)
		}
	}

	seed("", store.MemoryScopeUser, "u1", "doc.chunk:a", "LEGACY")
	seed("", store.MemoryScopeUser, "u1", "doc.chunk:free", "MOVES")
	seed("", store.MemoryScopeUser, "other", "k", "OTHER")
	seed("", store.MemoryScopeGlobal, "__help__", "topic#x", "H")
	seed("tnt", store.MemoryScopeUser, "u1", "doc.chunk:a", "TARGET")

	// Scan: 3 non-global orphans, 1 of them colliding, global counted apart.
	rep, err := s.MemoryOrphanScan(ctx, "tnt")
	if err != nil {
		t.Fatalf("MemoryOrphanScan: %v", err)
	}
	if rep.Orphaned != 3 || rep.Collisions != 1 || rep.SkippedGlobal != 1 {
		t.Errorf("scan = orphaned %d / collisions %d / global %d, want 3/1/1",
			rep.Orphaned, rep.Collisions, rep.SkippedGlobal)
	}
	if rep.Applied {
		t.Error("Applied = true on a read-only scan")
	}

	// Filtered repair exercises the conditional $3 bind.
	rep, err = s.MemoryOrphanRepair(ctx, "tnt", []string{"u1"})
	if err != nil {
		t.Fatalf("MemoryOrphanRepair(filtered): %v", err)
	}
	if rep.Moved != 1 {
		t.Errorf("Moved = %d, want 1 (doc.chunk:free only — :a collides, `other` filtered out)", rep.Moved)
	}
	if got, err := s.MemoryGet(ctx, "tnt", store.MemoryScopeUser, "u1", "doc.chunk:a"); err != nil {
		t.Errorf("target row lost: %v", err)
	} else if string(got.Value) != `"TARGET"` {
		t.Errorf("target value = %s, want \"TARGET\" (a collision overwrote live data)", got.Value)
	}
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "other", "k"); err != nil {
		t.Errorf("filtered-out row was moved: %v", err)
	}
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeGlobal, "__help__", "topic#x"); err != nil {
		t.Errorf("global row was moved off the legacy partition: %v", err)
	}

	// Unfiltered repair (no $3 bound at all) picks up the remainder.
	rep, err = s.MemoryOrphanRepair(ctx, "tnt", nil)
	if err != nil {
		t.Fatalf("MemoryOrphanRepair(all): %v", err)
	}
	if rep.Moved != 1 {
		t.Errorf("Moved = %d, want 1 (`other`)", rep.Moved)
	}

	// Idempotent, and the collision is still the only thing left behind.
	rep, err = s.MemoryOrphanRepair(ctx, "tnt", nil)
	if err != nil {
		t.Fatalf("MemoryOrphanRepair(again): %v", err)
	}
	if rep.Moved != 0 || rep.Orphaned != 1 || rep.Collisions != 1 {
		t.Errorf("final = moved %d / orphaned %d / collisions %d, want 0/1/1",
			rep.Moved, rep.Orphaned, rep.Collisions)
	}

	if _, err := s.MemoryOrphanRepair(ctx, "", nil); err == nil {
		t.Error("MemoryOrphanRepair(\"\") succeeded; want a refusal")
	}
}
