package memory

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// bumpRecorderStore is a minimal store.Store that only implements
// MemoryBumpAccessBatch, accumulating deltas ADDITIVELY and max-merging the
// timestamp — the same semantics the real SQL (access_count + delta,
// GREATEST(last_accessed_at, ts)) provides. Embedding store.Store keeps it
// compiling as a full Store; every other method is unused here.
type bumpRecorderStore struct {
	store.Store
	mu    sync.Mutex
	fail  bool                                 // when true, MemoryBumpAccessBatch errors (exercises the re-merge path)
	count map[store.MemoryAccessBump]int64     // key fields only carry identity
	last  map[store.MemoryAccessBump]time.Time // (delta/ts zeroed in the map key)
}

func newBumpRecorderStore() *bumpRecorderStore {
	return &bumpRecorderStore{
		count: map[store.MemoryAccessBump]int64{},
		last:  map[store.MemoryAccessBump]time.Time{},
	}
}

func identity(b store.MemoryAccessBump) store.MemoryAccessBump {
	return store.MemoryAccessBump{TenantID: b.TenantID, Scope: b.Scope, ScopeID: b.ScopeID, Key: b.Key}
}

func (s *bumpRecorderStore) MemoryBumpAccessBatch(_ context.Context, bumps []store.MemoryAccessBump) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("injected bump-batch write failure")
	}
	for _, b := range bumps {
		id := identity(b)
		s.count[id] += b.CountDelta
		if b.LastAccess.After(s.last[id]) {
			s.last[id] = b.LastAccess
		}
	}
	return nil
}

func (s *bumpRecorderStore) get(tenant string, scope store.MemoryScope, scopeID, key string) (int64, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := store.MemoryAccessBump{TenantID: tenant, Scope: scope, ScopeID: scopeID, Key: key}
	return s.count[id], s.last[id]
}

// TestAccessCount_BatchedFlushIsAdditive: two flushers over one store flush
// concurrently; their deltas SUM per key and last_accessed_at takes the max.
// Run under -race to catch unguarded shared state in Record/drain/Flush.
func TestAccessCount_BatchedFlushIsAdditive(t *testing.T) {
	fs := newBumpRecorderStore()
	f1 := NewAccessFlusher(fs, time.Hour, 1<<30) // huge interval/batch: flush only when we call Flush
	f2 := NewAccessFlusher(fs, time.Hour, 1<<30)

	early := time.Unix(1_700_000_000, 0).UTC()
	late := early.Add(time.Hour)

	const (
		tenant  = "t1"
		scopeID = "a1"
	)
	scope := store.MemoryScopeAgent

	// f1: "hot" ×3 at the EARLY ts, plus "solo" ×1.
	for i := 0; i < 3; i++ {
		f1.Record(tenant, scope, scopeID, "hot", early)
	}
	f1.Record(tenant, scope, scopeID, "solo", early)
	// f2: "hot" ×2 at the LATE ts (overlaps f1's key).
	for i := 0; i < 2; i++ {
		f2.Record(tenant, scope, scopeID, "hot", late)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = f1.Flush(context.Background()) }()
	go func() { defer wg.Done(); _ = f2.Flush(context.Background()) }()
	wg.Wait()

	// "hot": 3 (f1) + 2 (f2) = 5, last = the LATER timestamp.
	if c, last := fs.get(tenant, scope, scopeID, "hot"); c != 5 || !last.Equal(late) {
		t.Fatalf("hot: count=%d last=%v, want count=5 last=%v", c, last, late)
	}
	// "solo": only f1 touched it, once, at early.
	if c, last := fs.get(tenant, scope, scopeID, "solo"); c != 1 || !last.Equal(early) {
		t.Fatalf("solo: count=%d last=%v, want count=1 last=%v", c, last, early)
	}
}

// A drained flusher must not re-emit already-flushed deltas (drain swaps the
// pending map), so a second Flush with no new Records is a clean no-op.
func TestAccessFlusher_FlushDrainsPending(t *testing.T) {
	fs := newBumpRecorderStore()
	f := NewAccessFlusher(fs, time.Hour, 1<<30)
	now := time.Unix(1_700_000_000, 0).UTC()
	f.Record("t", store.MemoryScopeUser, "u", "k", now)
	if err := f.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := f.Flush(context.Background()); err != nil { // no new records
		t.Fatalf("second flush: %v", err)
	}
	if c, _ := fs.get("t", store.MemoryScopeUser, "u", "k"); c != 1 {
		t.Fatalf("count = %d, want 1 (second flush must not double-apply)", c)
	}
}

// Fix #3 (RFC BL PR2 review): a transient store write error must NOT lose the
// drained window. drain() empties pending before the write, so on error the
// bumps are re-merged and a later successful flush writes the FULL summed
// total — not just whatever arrived after the failure.
func TestAccessFlusher_FailedFlushReMergesForRetry(t *testing.T) {
	fs := newBumpRecorderStore()
	fs.fail = true
	f := NewAccessFlusher(fs, time.Hour, 1<<30)
	now := time.Unix(1_700_000_000, 0).UTC()

	// Two accesses accumulate a delta of 2 before the (failing) flush.
	f.Record("t", store.MemoryScopeUser, "u", "k", now)
	f.Record("t", store.MemoryScopeUser, "u", "k", now)

	if err := f.Flush(context.Background()); err == nil {
		t.Fatal("expected an error from the failing store")
	}
	// Nothing was persisted on the failed write.
	if c, _ := fs.get("t", store.MemoryScopeUser, "u", "k"); c != 0 {
		t.Fatalf("failed flush must persist nothing, got count=%d", c)
	}

	// Let the store recover; the retry must write the full re-merged delta (2),
	// proving the drained bumps were preserved rather than discarded.
	fs.fail = false
	if err := f.Flush(context.Background()); err != nil {
		t.Fatalf("retry flush: %v", err)
	}
	if c, _ := fs.get("t", store.MemoryScopeUser, "u", "k"); c != 2 {
		t.Fatalf("count = %d, want 2 (re-merged bumps must survive the failed flush)", c)
	}
}
