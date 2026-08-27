package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestReleaseConsolidationLease covers the server-side half of the fix: the hook
// the terminal-run path defers.
//
// Why it matters that this runs on a REAL store rather than a double: the whole
// point is the SQL predicate (release by owner alone, across targets), and a
// double that records the call would pass whatever the predicate did.
func TestReleaseConsolidationLease(t *testing.T) {
	base, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = base.Close() })
	s := &Server{store: base}
	ctx := context.Background()

	// A pass takes a long lease and then its run dies without releasing.
	if _, ok, err := base.MemoryCursorLease(ctx, "", store.MemoryScopeUser, "alice", "r_dead", time.Now().UTC(), time.Hour); err != nil || !ok {
		t.Fatalf("lease: acquired=%v err=%v", ok, err)
	}
	s.releaseConsolidationLease("r_dead")

	// The next pass must start immediately, not in an hour. Before this hook
	// existed the only thing that freed the target was the TTL, so every pass in
	// between refused with "target busy".
	row, ok, err := base.MemoryCursorLease(ctx, "", store.MemoryScopeUser, "alice", "r_next", time.Now().UTC(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || row.LeasedBy != "r_next" {
		t.Errorf("after the dead run's lease was released: acquired=%v leasedBy=%q, want true/r_next", ok, row.LeasedBy)
	}

	// A LIVE run's lease is not collateral damage: releasing on behalf of one run
	// must not touch another's, or a sub-agent finishing would free its parent's.
	if _, ok, err := base.MemoryCursorLease(ctx, "", store.MemoryScopeUser, "bob", "r_live", time.Now().UTC(), time.Hour); err != nil || !ok {
		t.Fatalf("lease bob: acquired=%v err=%v", ok, err)
	}
	s.releaseConsolidationLease("r_someone_else")
	if _, ok, err := base.MemoryCursorLease(ctx, "", store.MemoryScopeUser, "bob", "r_thief", time.Now().UTC(), time.Hour); err != nil {
		t.Fatal(err)
	} else if ok {
		t.Error("releasing for an unrelated run freed a live lease")
	}

	// The two no-ops. Both matter: nearly every run that reaches the terminal path
	// holds no lease at all, and this hook is deferred on all of them.
	s.releaseConsolidationLease("")            // no run id
	(&Server{}).releaseConsolidationLease("x") // no store configured
}
