package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// testMemoryChangesFeed is the RFC CD Part C change-feed store contract: append,
// the tenant-scoped since-cursor read (ordering + isolation), and prune. Runs
// on both backends via storetest.Run.
func testMemoryChangesFeed(t *testing.T, s store.Store) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("AppendMemoryChange: %v", err)
		}
	}
	must(s.AppendMemoryChange(ctx, store.MemoryChange{TenantID: "acme", Type: store.MemoryChangeSet, Scope: store.MemoryScopeAgent, ScopeID: "a1", Key: "k1"}))
	must(s.AppendMemoryChange(ctx, store.MemoryChange{TenantID: "acme", Type: store.DocumentChangeUpdated, Scope: store.MemoryScopeAgent, ScopeID: "a1", ChunkID: "CID"}))
	must(s.AppendMemoryChange(ctx, store.MemoryChange{TenantID: "globex", Type: store.MemoryChangeSet, Scope: store.MemoryScopeUser, ScopeID: "u1", Key: "z"}))

	acme, err := s.GetMemoryChangesSince(ctx, "acme", 0, 100)
	if err != nil {
		t.Fatalf("GetMemoryChangesSince(acme): %v", err)
	}
	if len(acme) != 2 {
		t.Fatalf("acme changes = %d, want 2: %+v", len(acme), acme)
	}
	if acme[0].Type != store.MemoryChangeSet || acme[0].Key != "k1" || acme[0].ChunkID != "" {
		t.Errorf("acme[0] = %+v, want memory.set key=k1", acme[0])
	}
	if acme[1].Type != store.DocumentChangeUpdated || acme[1].ChunkID != "CID" || acme[1].Key != "" {
		t.Errorf("acme[1] = %+v, want document.chunk.updated chunk_id=CID", acme[1])
	}
	if !(acme[0].Seq < acme[1].Seq) {
		t.Errorf("seq not ascending: %d then %d", acme[0].Seq, acme[1].Seq)
	}
	if acme[0].At.IsZero() {
		t.Errorf("At not stamped")
	}

	// Tenant isolation: a globex reader never sees acme's rows.
	glob, err := s.GetMemoryChangesSince(ctx, "globex", 0, 100)
	if err != nil {
		t.Fatalf("GetMemoryChangesSince(globex): %v", err)
	}
	if len(glob) != 1 || glob[0].TenantID != "globex" {
		t.Errorf("globex feed = %+v, want exactly its own 1 change", glob)
	}

	// Cursor: reading since the first acme seq returns only the second.
	rest, err := s.GetMemoryChangesSince(ctx, "acme", acme[0].Seq, 100)
	if err != nil {
		t.Fatalf("cursor read: %v", err)
	}
	if len(rest) != 1 || rest[0].Seq != acme[1].Seq {
		t.Errorf("since=%d returned %+v, want only the second row", acme[0].Seq, rest)
	}

	// Prune removes rows older than a future cutoff (all of them).
	n, err := s.PruneMemoryChanges(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneMemoryChanges: %v", err)
	}
	if n < 3 {
		t.Errorf("pruned %d, want >= 3", n)
	}
	if left, _ := s.GetMemoryChangesSince(ctx, "acme", 0, 100); len(left) != 0 {
		t.Errorf("after prune acme has %d rows, want 0", len(left))
	}
}
