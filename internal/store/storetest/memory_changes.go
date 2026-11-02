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

	// The operator's all-tenant read: every tenant's rows in one seq order,
	// each naming its tenant, and the same cursor semantics.
	all, err := s.GetMemoryChangesSinceAllTenants(ctx, acme[0].Seq-1, 100)
	if err != nil {
		t.Fatalf("GetMemoryChangesSinceAllTenants: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all-tenant feed = %d rows, want 3: %+v", len(all), all)
	}
	for i, want := range []string{"acme", "acme", "globex"} {
		if all[i].TenantID != want {
			t.Errorf("all[%d].TenantID = %q, want %q", i, all[i].TenantID, want)
		}
		if i > 0 && !(all[i-1].Seq < all[i].Seq) {
			t.Errorf("all-tenant feed not in seq order: %d then %d", all[i-1].Seq, all[i].Seq)
		}
	}
	if tail, err := s.GetMemoryChangesSinceAllTenants(ctx, acme[1].Seq, 100); err != nil || len(tail) != 1 || tail[0].TenantID != "globex" {
		t.Errorf("all-tenant since=%d = %+v (err %v), want only globex's row", acme[1].Seq, tail, err)
	}
	if capped, _ := s.GetMemoryChangesSinceAllTenants(ctx, acme[0].Seq-1, 2); len(capped) != 2 {
		t.Errorf("all-tenant feed with limit 2 returned %d rows", len(capped))
	}

	// RFC FV: the document a change belongs to round-trips on both the tenant and
	// the all-tenant read, an unstamped row reads back "", and the head is the
	// tenant's own newest seq (0 for a tenant with no rows).
	must(s.AppendMemoryChange(ctx, store.MemoryChange{TenantID: "acme", Type: store.DocumentEdgesChanged, Scope: store.MemoryScopeUser, ScopeID: "u1", ChunkID: "E1", DocumentID: "DOC1"}))
	stamped, err := s.GetMemoryChangesSince(ctx, "acme", acme[1].Seq, 100)
	if err != nil || len(stamped) != 1 {
		t.Fatalf("stamped read = %+v (err %v), want one row", stamped, err)
	}
	if got := stamped[0]; got.DocumentID != "DOC1" || got.Type != store.DocumentEdgesChanged || got.ChunkID != "E1" {
		t.Errorf("stamped row = %+v, want document.edges.changed chunk E1 of DOC1", got)
	}
	if all, _ := s.GetMemoryChangesSinceAllTenants(ctx, stamped[0].Seq-1, 100); len(all) != 1 || all[0].DocumentID != "DOC1" {
		t.Errorf("all-tenant read of the stamped row = %+v, want document_id DOC1", all)
	}
	if acme[1].DocumentID != "" {
		t.Errorf("unstamped row reads document_id %q, want \"\"", acme[1].DocumentID)
	}
	if head, err := s.MemoryChangesHead(ctx, "acme"); err != nil || head != stamped[0].Seq {
		t.Errorf("acme head = %d (err %v), want its newest seq %d", head, err, stamped[0].Seq)
	}
	if head, err := s.MemoryChangesHead(ctx, "globex"); err != nil || head != glob[0].Seq {
		t.Errorf("globex head = %d (err %v), want its own newest seq %d, not acme's", head, err, glob[0].Seq)
	}
	if head, err := s.MemoryChangesHead(ctx, "nobody"); err != nil || head != 0 {
		t.Errorf("head of a tenant with no rows = %d (err %v), want 0", head, err)
	}

	// Prune removes rows older than a future cutoff (all of them).
	n, err := s.PruneMemoryChanges(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("PruneMemoryChanges: %v", err)
	}
	if n < 4 {
		t.Errorf("pruned %d, want >= 4", n)
	}
	if left, _ := s.GetMemoryChangesSince(ctx, "acme", 0, 100); len(left) != 0 {
		t.Errorf("after prune acme has %d rows, want 0", len(left))
	}

	// RFC CD Part C push — the persisted delivery cursor: 0 before any delivery,
	// then upserts.
	cur, err := s.GetChangeSubscriptionCursor(ctx, "sub-a")
	if err != nil || cur != 0 {
		t.Fatalf("initial cursor = %d (err %v), want 0", cur, err)
	}
	if err := s.SetChangeSubscriptionCursor(ctx, "sub-a", 42); err != nil {
		t.Fatalf("SetChangeSubscriptionCursor: %v", err)
	}
	if cur, err = s.GetChangeSubscriptionCursor(ctx, "sub-a"); err != nil || cur != 42 {
		t.Errorf("cursor after set = %d (err %v), want 42", cur, err)
	}
	if err := s.SetChangeSubscriptionCursor(ctx, "sub-a", 99); err != nil {
		t.Fatalf("SetChangeSubscriptionCursor(update): %v", err)
	}
	if cur, _ = s.GetChangeSubscriptionCursor(ctx, "sub-a"); cur != 99 {
		t.Errorf("cursor after update = %d, want 99", cur)
	}
	// A different subscription is independent.
	if cur, _ = s.GetChangeSubscriptionCursor(ctx, "sub-b"); cur != 0 {
		t.Errorf("independent sub-b cursor = %d, want 0", cur)
	}
}
