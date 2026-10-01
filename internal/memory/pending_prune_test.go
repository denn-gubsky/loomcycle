package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// pendingPruneFixture opens a real sqlite store holding `drained` acked rows and
// one undrained row, all in the user queue of alice in tenant acme.
func pendingPruneFixture(t *testing.T, drained int) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "prune.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	var ids []string
	for i := 0; i <= drained; i++ {
		id := fmt.Sprintf("mp_%d", i)
		if err := st.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{ID: id, TenantID: "acme",
			Scope: store.MemoryScopeUser, ScopeID: "alice", Payload: json.RawMessage(`{"m":1}`)}); err != nil {
			t.Fatalf("enqueue %s: %v", id, err)
		}
		if i < drained {
			ids = append(ids, id)
		}
	}
	if err := st.MemoryPendingAck(ctx, "acme", store.MemoryScopeUser, "alice", ids); err != nil {
		t.Fatalf("ack: %v", err)
	}
	return st
}

// queued counts the fixture rows still stored, drained or not: MemoryPendingGet
// does not filter on drained_at, so it sees a drained row until it is deleted.
func queued(t *testing.T, st *sqlite.Store, total int) int {
	t.Helper()
	left := 0
	for i := 0; i < total; i++ {
		if _, err := st.MemoryPendingGet(context.Background(), "acme", store.MemoryScopeUser, "alice",
			fmt.Sprintf("mp_%d", i)); err == nil {
			left++
		}
	}
	return left
}

func TestPruneDrainedPending_DeletesRowsDrainedLongerThanTheTTL(t *testing.T) {
	st := pendingPruneFixture(t, 5)
	// Eight days from now, every ack is more than a week old. batch=2 makes the
	// five drained rows take three statements, so a sweep that ran one batch
	// and stopped would leave rows behind.
	n, err := PruneDrainedPending(context.Background(), st, 7*24*time.Hour, time.Now().Add(8*24*time.Hour), 2)
	if err != nil {
		t.Fatalf("PruneDrainedPending: %v", err)
	}
	if n != 5 {
		t.Errorf("pruned %d, want 5", n)
	}
	if left := queued(t, st, 6); left != 1 {
		t.Errorf("%d rows left, want 1 (the undrained row)", left)
	}
}

func TestPruneDrainedPending_KeepsRowsDrainedWithinTheTTL(t *testing.T) {
	st := pendingPruneFixture(t, 3)
	// Six days from now every ack is inside the week; the sibling test sits
	// eight days out, so the pair straddles the TTL.
	n, err := PruneDrainedPending(context.Background(), st, 7*24*time.Hour, time.Now().Add(6*24*time.Hour), 2)
	if err != nil {
		t.Fatalf("PruneDrainedPending: %v", err)
	}
	if n != 0 {
		t.Errorf("pruned %d rows drained six days ago under a 7-day TTL, want 0", n)
	}
	if left := queued(t, st, 4); left != 4 {
		t.Errorf("%d rows left, want 4", left)
	}
}

func TestPruneDrainedPending_NonPositiveTTLPrunesNothing(t *testing.T) {
	st := pendingPruneFixture(t, 3)
	for _, ttl := range []time.Duration{0, -time.Hour} {
		n, err := PruneDrainedPending(context.Background(), st, ttl, time.Now().Add(365*24*time.Hour), 2)
		if err != nil || n != 0 {
			t.Errorf("ttl=%s: PruneDrainedPending = (%d, %v), want (0, nil): a non-positive TTL disables the prune", ttl, n, err)
		}
	}
	if left := queued(t, st, 4); left != 4 {
		t.Errorf("%d rows left, want 4", left)
	}
}
