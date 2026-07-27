package sqlite

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// seedLegacy writes a row directly into the legacy "" tenant partition, which is
// where every row written before RFC BL added tenant_id still sits.
func seedLegacy(t *testing.T, s *Store, scope store.MemoryScope, scopeID, key, val string) {
	t.Helper()
	if err := s.MemorySet(context.Background(), "", scope, scopeID, key, json.RawMessage(`"`+val+`"`), 0); err != nil {
		t.Fatalf("seed legacy %s/%s: %v", scopeID, key, err)
	}
}

// TestMemoryOrphanScan_CountsAndSkipsGlobal: the scan must report non-global
// legacy rows as orphans and count scope='global' separately — that partition is
// shared by design (the Context op=help index lives there) and "" is its correct
// home, so a repair that moved it would break help for every other tenant.
func TestMemoryOrphanScan_CountsAndSkipsGlobal(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedLegacy(t, s, store.MemoryScopeUser, "u1", "doc.chunk:a", "A")
	seedLegacy(t, s, store.MemoryScopeUser, "u1", "doc.chunk:b", "B")
	seedLegacy(t, s, store.MemoryScopeAgent, "ag1", "k", "C")
	seedLegacy(t, s, store.MemoryScopeGlobal, "__help__", "topic#x", "H")

	rep, err := s.MemoryOrphanScan(ctx, "tnt")
	if err != nil {
		t.Fatalf("MemoryOrphanScan: %v", err)
	}
	if rep.Orphaned != 3 {
		t.Errorf("Orphaned = %d, want 3", rep.Orphaned)
	}
	if rep.SkippedGlobal != 1 {
		t.Errorf("SkippedGlobal = %d, want 1", rep.SkippedGlobal)
	}
	if rep.Collisions != 0 {
		t.Errorf("Collisions = %d, want 0", rep.Collisions)
	}
	if rep.Applied {
		t.Error("Applied = true on a read-only scan")
	}
	// Grouping is what lets an operator distinguish a single-tenant deployment
	// from a multi-tenant one before moving anything.
	if len(rep.Groups) != 2 {
		t.Fatalf("Groups = %d, want 2 (user/u1 + agent/ag1): %+v", len(rep.Groups), rep.Groups)
	}
	if rep.Groups[0].ScopeID != "u1" || rep.Groups[0].Rows != 2 {
		t.Errorf("largest group = %+v, want user/u1 with 2 rows", rep.Groups[0])
	}
}

// TestMemoryOrphanRepair_MovesAndIsIdempotent: the repair re-stamps orphans onto
// the target tenant, leaves global alone, and a second call moves nothing.
func TestMemoryOrphanRepair_MovesAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedLegacy(t, s, store.MemoryScopeUser, "u1", "doc.chunk:a", "A")
	seedLegacy(t, s, store.MemoryScopeGlobal, "__help__", "topic#x", "H")

	rep, err := s.MemoryOrphanRepair(ctx, "tnt", nil)
	if err != nil {
		t.Fatalf("MemoryOrphanRepair: %v", err)
	}
	if !rep.Applied || rep.Moved != 1 {
		t.Errorf("Applied=%v Moved=%d, want true/1", rep.Applied, rep.Moved)
	}

	// Readable at the target tenant now ...
	if _, err := s.MemoryGet(ctx, "tnt", store.MemoryScopeUser, "u1", "doc.chunk:a"); err != nil {
		t.Errorf("row not readable at target tenant after repair: %v", err)
	}
	// ... and gone from the legacy partition.
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "u1", "doc.chunk:a"); err == nil {
		t.Error("row still present at the legacy tenant after repair")
	}
	// The global row must NOT have moved.
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeGlobal, "__help__", "topic#x"); err != nil {
		t.Errorf("global row was moved off the legacy partition: %v", err)
	}

	// Idempotent: nothing left to move.
	rep2, err := s.MemoryOrphanRepair(ctx, "tnt", nil)
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if rep2.Moved != 0 || rep2.Orphaned != 0 {
		t.Errorf("second repair moved %d of %d orphans, want 0 of 0", rep2.Moved, rep2.Orphaned)
	}
}

// TestMemoryOrphanRepair_SkipsCollisionWithoutClobbering: when the target tenant
// already holds a row for the same (scope, scope_id, key) — the PK — the legacy
// row must be left in place rather than merged. Which side should win depends on
// content the store cannot adjudicate, so it is reported for an operator to
// inspect. Silently overwriting here would destroy live data.
func TestMemoryOrphanRepair_SkipsCollisionWithoutClobbering(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedLegacy(t, s, store.MemoryScopeUser, "u1", "doc.chunk:a", "LEGACY")
	seedLegacy(t, s, store.MemoryScopeUser, "u1", "doc.chunk:free", "MOVES")
	if err := s.MemorySet(ctx, "tnt", store.MemoryScopeUser, "u1", "doc.chunk:a", json.RawMessage(`"TARGET"`), 0); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	rep, err := s.MemoryOrphanRepair(ctx, "tnt", nil)
	if err != nil {
		t.Fatalf("MemoryOrphanRepair: %v", err)
	}
	if rep.Collisions != 1 {
		t.Errorf("Collisions = %d, want 1", rep.Collisions)
	}
	if rep.Moved != 1 {
		t.Errorf("Moved = %d, want 1 (the non-colliding row only)", rep.Moved)
	}

	// The target's value is untouched ...
	got, err := s.MemoryGet(ctx, "tnt", store.MemoryScopeUser, "u1", "doc.chunk:a")
	if err != nil {
		t.Fatalf("target get: %v", err)
	}
	if string(got.Value) != `"TARGET"` {
		t.Errorf("target value = %s, want \"TARGET\" (the legacy row overwrote it)", got.Value)
	}
	// ... and the legacy row still exists, so nothing was lost.
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "u1", "doc.chunk:a"); err != nil {
		t.Errorf("colliding legacy row was deleted instead of skipped: %v", err)
	}
}

// TestMemoryOrphanRepair_ScopeIDFilter: a multi-tenant deployment's orphans may
// belong to different tenants, and "" records no owner — so the filter must move
// only the named scope_ids and leave the rest at the legacy partition.
func TestMemoryOrphanRepair_ScopeIDFilter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedLegacy(t, s, store.MemoryScopeUser, "mine", "k", "A")
	seedLegacy(t, s, store.MemoryScopeUser, "someone-else", "k", "B")

	rep, err := s.MemoryOrphanRepair(ctx, "tnt", []string{"mine"})
	if err != nil {
		t.Fatalf("MemoryOrphanRepair: %v", err)
	}
	if rep.Moved != 1 {
		t.Errorf("Moved = %d, want 1", rep.Moved)
	}
	if _, err := s.MemoryGet(ctx, "tnt", store.MemoryScopeUser, "mine", "k"); err != nil {
		t.Errorf("filtered-in row did not move: %v", err)
	}
	if _, err := s.MemoryGet(ctx, "", store.MemoryScopeUser, "someone-else", "k"); err != nil {
		t.Errorf("filtered-out row was moved: %v", err)
	}
}

// TestMemoryOrphan_EmptyTargetRefused: "" IS the legacy partition, so a repair
// targeting it is meaningless and would report success for a no-op. Refuse
// loudly rather than let an operator believe a repair ran.
func TestMemoryOrphan_EmptyTargetRefused(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.MemoryOrphanScan(ctx, ""); err == nil {
		t.Error("MemoryOrphanScan(\"\") succeeded; want a refusal")
	}
	if _, err := s.MemoryOrphanRepair(ctx, "", nil); err == nil {
		t.Error("MemoryOrphanRepair(\"\") succeeded; want a refusal")
	}
}
