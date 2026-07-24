package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// MemoryBumpAccessBatch must add deltas (not overwrite) and max-merge
// last_accessed_at — the real-SQL counterpart to the in-process flusher's
// additive contract. Sequential here (SQLite serialises writers); the
// concurrent-flusher additivity is covered in internal/memory.
func TestMemoryBumpAccessBatch_AdditiveAndMax(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const (
		tenant  = "t1"
		scopeID = "a1"
		key     = "k"
	)
	scope := store.MemoryScopeAgent

	if err := s.MemorySet(ctx, tenant, scope, scopeID, key, json.RawMessage(`1`), 0); err != nil {
		t.Fatalf("MemorySet: %v", err)
	}

	early := time.Unix(1_700_000_000, 0).UTC()
	late := early.Add(time.Hour)

	// First flush: +3 at the LATE ts.
	if err := s.MemoryBumpAccessBatch(ctx, []store.MemoryAccessBump{
		{TenantID: tenant, Scope: scope, ScopeID: scopeID, Key: key, CountDelta: 3, LastAccess: late},
	}); err != nil {
		t.Fatalf("bump 1: %v", err)
	}
	// Second flush: +2 at the EARLY ts — count must sum to 5, ts stay at late.
	if err := s.MemoryBumpAccessBatch(ctx, []store.MemoryAccessBump{
		{TenantID: tenant, Scope: scope, ScopeID: scopeID, Key: key, CountDelta: 2, LastAccess: early},
	}); err != nil {
		t.Fatalf("bump 2: %v", err)
	}

	count, lastNanos := readAccess(t, s, tenant, string(scope), scopeID, key)
	if count != 5 {
		t.Errorf("access_count = %d, want 5 (additive)", count)
	}
	if lastNanos != late.UnixNano() {
		t.Errorf("last_accessed_at = %d, want %d (max)", lastNanos, late.UnixNano())
	}
}

// A bump for a row that doesn't exist is a clean no-op (0 rows), not an error.
func TestMemoryBumpAccessBatch_MissingRowNoOp(t *testing.T) {
	s := newTestStore(t)
	if err := s.MemoryBumpAccessBatch(context.Background(), []store.MemoryAccessBump{
		{TenantID: "t", Scope: store.MemoryScopeUser, ScopeID: "nope", Key: "gone", CountDelta: 1, LastAccess: time.Now()},
	}); err != nil {
		t.Fatalf("bump on missing row must be a no-op, got %v", err)
	}
}

func readAccess(t *testing.T, s *Store, tenant, scope, scopeID, key string) (int64, int64) {
	t.Helper()
	var count int64
	var last sql.NullInt64
	err := s.db.QueryRowContext(context.Background(),
		`SELECT access_count, last_accessed_at FROM memory
		  WHERE tenant_id = ? AND scope = ? AND scope_id = ? AND key = ?`,
		tenant, scope, scopeID, key,
	).Scan(&count, &last)
	if err != nil {
		t.Fatalf("readAccess: %v", err)
	}
	return count, last.Int64
}
