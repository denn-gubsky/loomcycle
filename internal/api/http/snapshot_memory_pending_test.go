package http

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/snapshot"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
)

// TestSnapshotRestore_RestoredQueueRowIsConsolidatedOnTheTarget drives a
// restored consolidation-queue row through a real consolidation pass on the
// target: a POST /v1/runs of the consolidator, the real Memory tool against
// the target store. Only the provider is scripted (the pass's tool sequence).
//
// What proves the row was the one restored, rather than the script acting on
// nothing: the drain's tool result carries the row's queued text; the fact the
// pass writes with from_pending picks up the row's SERVER-RECORDED provenance
// (origin + source run), which the Memory tool reads from the queue row under
// the run's own (tenant, scope, user) and silently ignores when the row is not
// there; and the ack stamps the restored row drained.
func TestSnapshotRestore_RestoredQueueRowIsConsolidatedOnTheTarget(t *testing.T) {
	ctx := context.Background()
	src, err := storesqlite.Open(filepath.Join(t.TempDir(), "source.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	const (
		pendID    = "pend_from_source"
		srcRunID  = "run-on-the-source"
		srcSessID = "sess-on-the-source"
		queued    = "I moved to Lisbon last spring"
	)
	payload, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": queued}}})
	if err := src.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{ID: pendID, Scope: store.MemoryScopeUser, ScopeID: evalUserID,
		Payload: payload, Origin: store.PendingOriginCompaction, SourceSessionID: srcSessID, SourceRunID: srcRunID}); err != nil {
		t.Fatal(err)
	}
	_, raw, err := snapshot.Capture(ctx, src, snapshot.CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	setFact, _ := json.Marshal(map[string]any{
		"op": "set", "scope": "user", "key": "fact_home_city", "value": "The user lives in Lisbon.",
		"provenance": map[string]string{"class": "fact"}, "from_pending": pendID,
	})
	ack, _ := json.Marshal(map[string]any{"op": "pending_ack", "scope": "user", "ids": []string{pendID}})
	env := newConsolidationEnv(t, [][]providers.Event{
		toolCall("tu_lease", "Memory", `{"op":"cursor_lease","scope":"user","lease_ttl_ms":600000}`),
		toolCall("tu_drain", "Memory", `{"op":"pending_drain","scope":"user","limit":50}`),
		toolCall("tu_set", "Memory", string(setFact)),
		toolCall("tu_ack", "Memory", string(ack)),
		toolCall("tu_release", "Memory", `{"op":"cursor_release","scope":"user"}`),
		finalText("consolidated 1 queued item"),
	})

	res, err := snapshot.Restore(ctx, env.store, raw, snapshot.RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.MemoryPendingRestored != 1 {
		t.Fatalf("memory_pending restored = %d, want 1 (warnings %v)", res.MemoryPendingRestored, res.Warnings)
	}

	stream := env.runConsolidation(evalUserID)

	if !strings.Contains(stream, queued) {
		t.Errorf("the pass's drain did not hand out the restored row's text; stream:\n%s", redactStream(stream))
	}
	prov, err := env.store.MemoryProvenanceGet(ctx, "", store.MemoryScopeUser, evalUserID, "fact_home_city")
	if err != nil {
		t.Fatalf("the pass wrote no fact: %v", err)
	}
	if prov.Origin != store.PendingOriginCompaction || prov.SourceRunID != srcRunID || prov.SourceSessionID != srcSessID {
		t.Errorf("fact provenance = %+v; want the restored row's origin %q and source run/session %q/%q",
			prov, store.PendingOriginCompaction, srcRunID, srcSessID)
	}
	row, err := env.store.MemoryPendingGet(ctx, "", store.MemoryScopeUser, evalUserID, pendID)
	if err != nil {
		t.Fatalf("restored row: %v", err)
	}
	if row.DrainedAt.IsZero() {
		t.Error("the pass did not ack the restored row; it would be consolidated again on every pass")
	}
}
