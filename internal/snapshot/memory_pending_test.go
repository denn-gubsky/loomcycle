package snapshot

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/erasure"
	"github.com/denn-gubsky/loomcycle/internal/snapshot/migrations"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// pendingPayload is a queue row's payload in the shape the Memory tool's add
// writes: the turns to consolidate.
func pendingPayload(text string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}})
	return raw
}

// plantPendingRows queues one row per (tenant, scope) shape the queue can
// hold — the operator layer and two tenants, user / agent / tenant scopes —
// plus one row that is already drained and must not travel.
func plantPendingRows(t *testing.T, s store.Store) []store.MemoryPendingRow {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 9, 2, 3, 4, 5, 0, time.UTC)
	rows := []store.MemoryPendingRow{
		{ID: "pend_op_alice", TenantID: "", Scope: store.MemoryScopeUser, ScopeID: "alice",
			Payload: pendingPayload("operator-layer alice lives in Lisbon"), Origin: store.PendingOriginAgentExplicit,
			SourceSessionID: "sess-op", SourceRunID: "run-op", CreatedAt: base},
		{ID: "pend_acme_alice", TenantID: "acme", Scope: store.MemoryScopeUser, ScopeID: "alice",
			Payload: pendingPayload("acme alice prefers tea"), Origin: store.PendingOriginCompaction,
			SourceSessionID: "sess-acme", SourceRunID: "run-acme", CreatedAt: base.Add(time.Second)},
		{ID: "pend_beta_agent", TenantID: "beta", Scope: store.MemoryScopeAgent, ScopeID: "helper",
			Payload: pendingPayload("beta helper learned a shortcut"), CreatedAt: base.Add(2 * time.Second)},
		{ID: "pend_acme_tenant", TenantID: "acme", Scope: store.MemoryScopeTenant, ScopeID: "",
			Payload: pendingPayload("acme ships on fridays"), Origin: store.PendingOriginAgentExplicit, CreatedAt: base.Add(3 * time.Second)},
	}
	for _, r := range rows {
		if err := s.MemoryPendingEnqueue(ctx, r); err != nil {
			t.Fatalf("enqueue %s: %v", r.ID, err)
		}
	}
	// Consolidated already: history, not work.
	if err := s.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{ID: "pend_drained", TenantID: "acme",
		Scope: store.MemoryScopeUser, ScopeID: "alice", Payload: pendingPayload("already consolidated"), CreatedAt: base}); err != nil {
		t.Fatalf("enqueue drained: %v", err)
	}
	if err := s.MemoryPendingAck(ctx, "acme", store.MemoryScopeUser, "alice", []string{"pend_drained"}); err != nil {
		t.Fatalf("ack drained: %v", err)
	}
	return rows
}

// pendingByID reads every undrained row on s, keyed by id.
func pendingByID(t *testing.T, s store.Store) map[string]store.MemoryPendingRow {
	t.Helper()
	rows, err := s.SnapshotReadMemoryPending(context.Background())
	if err != nil {
		t.Fatalf("SnapshotReadMemoryPending: %v", err)
	}
	out := make(map[string]store.MemoryPendingRow, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

func samePendingRow(a, b store.MemoryPendingRow) bool {
	var pa, pb any
	_ = json.Unmarshal(a.Payload, &pa)
	_ = json.Unmarshal(b.Payload, &pb)
	return a.ID == b.ID && a.TenantID == b.TenantID && a.Scope == b.Scope && a.ScopeID == b.ScopeID &&
		a.Origin == b.Origin && a.SourceSessionID == b.SourceSessionID && a.SourceRunID == b.SourceRunID &&
		a.CreatedAt.Equal(b.CreatedAt) && sameJSONValue(pa, pb)
}

func sameJSONValue(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

// TestMemoryPending_RoundTripKeepsEveryRowUnderItsOwnTarget: each undrained
// row lands on the target under the tenant, scope and scope id it was queued
// for, with its id, payload, origin, source ids and created_at, and is counted;
// the drained row stays behind.
func TestMemoryPending_RoundTripKeepsEveryRowUnderItsOwnTarget(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			src, dst := b.open(t), b.open(t)
			ctx := context.Background()
			want := plantPendingRows(t, src)

			_, raw, err := Capture(ctx, src, CaptureOptions{})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			if strings.Contains(string(raw), "already consolidated") {
				t.Error("a drained row reached the envelope; only undrained rows are work")
			}
			res, err := Restore(ctx, dst, raw, RestoreOptions{})
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if res.MemoryPendingRestored != len(want) || res.Counts()["memory_pending"] != len(want) {
				t.Errorf("memory_pending restored = %d (Counts %d), want %d", res.MemoryPendingRestored, res.Counts()["memory_pending"], len(want))
			}
			got := pendingByID(t, dst)
			if len(got) != len(want) {
				t.Errorf("target holds %d undrained rows, want %d: %+v", len(got), len(want), got)
			}
			for _, w := range want {
				if !samePendingRow(got[w.ID], w) {
					t.Errorf("%s restored as %+v, want %+v", w.ID, got[w.ID], w)
				}
			}
			if _, err := dst.MemoryPendingGet(ctx, "acme", store.MemoryScopeUser, "alice", "pend_drained"); err == nil {
				t.Error("the drained row was restored")
			}
		})
	}
}

// TestMemoryPending_RestoredRowIsClaimableOnTheTarget: the source's lease on a
// target is the source replica's claim, and it must not come back. The target
// takes its own lease on the first try and its drain hands the row out.
func TestMemoryPending_RestoredRowIsClaimableOnTheTarget(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	plantPendingRows(t, src)

	// A pass on the source holds alice's lease for an hour.
	const srcOwner = "dp6mark-source-replica-lease"
	if _, ok, err := src.MemoryCursorLease(ctx, "acme", store.MemoryScopeUser, "alice", srcOwner, time.Now(), time.Hour); err != nil || !ok {
		t.Fatalf("source lease: ok=%v err=%v", ok, err)
	}
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if strings.Contains(string(raw), srcOwner) {
		t.Error("the source replica's lease reached the envelope")
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	if _, ok, err := dst.MemoryCursorLease(ctx, "acme", store.MemoryScopeUser, "alice", "target-pass", time.Now(), time.Minute); err != nil || !ok {
		t.Fatalf("target lease: ok=%v err=%v; a restored target must be claimable", ok, err)
	}
	rows, err := dst.MemoryPendingDrain(ctx, "acme", store.MemoryScopeUser, "alice", 10)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "pend_acme_alice" || !rows[0].DrainedAt.IsZero() {
		t.Fatalf("target drain = %+v, want the one restored row, undrained", rows)
	}
}

// racingStore finishes a consolidation pass between the capture's two memory
// reads, whichever comes first: it writes the fact a pass distils from the
// row, then acks the row.
type racingStore struct {
	store.Store
	once    sync.Once
	pass    func()
	reads   []string
	readsMu sync.Mutex
}

func (r *racingStore) record(what string) {
	r.readsMu.Lock()
	r.reads = append(r.reads, what)
	r.readsMu.Unlock()
}

func (r *racingStore) SnapshotReadMemoryPending(ctx context.Context) ([]store.MemoryPendingRow, error) {
	rows, err := r.Store.SnapshotReadMemoryPending(ctx)
	r.record("memory_pending")
	r.once.Do(r.pass)
	return rows, err
}

func (r *racingStore) SnapshotReadMemory(ctx context.Context) ([]store.MemorySnapshotEntry, error) {
	rows, err := r.Store.SnapshotReadMemory(ctx)
	r.record("memory")
	r.once.Do(r.pass)
	return rows, err
}

// TestMemoryPending_ARowDrainedDuringCaptureIsDuplicatedNeverLost is the
// capture-order guarantee. A pass completes between the queue read and the
// memory read of a capture taken WITHOUT pausing: the row must be in the
// snapshot at least once — as a queued row, as the fact it produced, or both
// — and after restore the target holds the fact and re-queues the row.
//
// Fails if memory is read before memory_pending: the fact is not in the
// memory read yet and the row is acked before the queue read, so it is in
// neither.
func TestMemoryPending_ARowDrainedDuringCaptureIsDuplicatedNeverLost(t *testing.T) {
	base, baseClose := newTestStore(t)
	defer baseClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()

	if err := base.MemoryPendingEnqueue(ctx, store.MemoryPendingRow{ID: "pend_race", TenantID: "acme",
		Scope: store.MemoryScopeUser, ScopeID: "alice", Payload: pendingPayload("I moved to Porto")}); err != nil {
		t.Fatal(err)
	}
	const factKey = "fact_alice_city"
	src := &racingStore{Store: base}
	src.pass = func() {
		val, _ := json.Marshal("Alice lives in Porto.")
		if err := base.MemorySet(ctx, "acme", store.MemoryScopeUser, "alice", factKey, val, 0); err != nil {
			t.Errorf("pass: write fact: %v", err)
		}
		if err := base.MemoryPendingAck(ctx, "acme", store.MemoryScopeUser, "alice", []string{"pend_race"}); err != nil {
			t.Errorf("pass: ack: %v", err)
		}
	}

	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(src.reads) != 2 {
		t.Fatalf("capture read %v; the race needs both reads", src.reads)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	queued, fact := false, false
	for _, e := range env.Sections.MemoryPending.Entries {
		queued = queued || e.ID == "pend_race"
	}
	for _, e := range env.Sections.Memory.Entries {
		fact = fact || e.Key == factKey
	}
	if !queued && !fact {
		t.Fatalf("the row drained during the capture is in neither section (reads in order %v): it was lost", src.reads)
	}
	if !queued || !fact {
		t.Errorf("queued=%v fact=%v (reads %v); with the queue read first the row must be in both", queued, fact, src.reads)
	}

	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.MemoryPendingRestored != 1 {
		t.Errorf("memory_pending restored = %d, want 1", res.MemoryPendingRestored)
	}
	if _, err := dst.MemoryGet(ctx, "acme", store.MemoryScopeUser, "alice", factKey); err != nil {
		t.Errorf("the consolidated fact is not on the target: %v", err)
	}
	rows, err := dst.MemoryPendingDrain(ctx, "acme", store.MemoryScopeUser, "alice", 10)
	if err != nil || len(rows) != 1 || rows[0].ID != "pend_race" {
		t.Errorf("target queue = %+v (err %v); the row is re-queued for consolidation — a duplicate, not a loss", rows, err)
	}
}

// TestMemoryPending_LiveRowStandsAndAReRestoreDoesNotRequeue: once the target
// has consolidated a restored row, restoring the same snapshot again must not
// put it back in the queue — and a row already on the id is never replaced.
func TestMemoryPending_LiveRowStandsAndAReRestoreDoesNotRequeue(t *testing.T) {
	for _, b := range coverageBackends() {
		t.Run(b.name, func(t *testing.T) {
			src, dst := b.open(t), b.open(t)
			ctx := context.Background()
			plantPendingRows(t, src)
			_, raw, err := Capture(ctx, src, CaptureOptions{})
			if err != nil {
				t.Fatalf("Capture: %v", err)
			}
			if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			// The target's consolidator drains alice's row.
			if err := dst.MemoryPendingAck(ctx, "acme", store.MemoryScopeUser, "alice", []string{"pend_acme_alice"}); err != nil {
				t.Fatal(err)
			}

			res, err := Restore(ctx, dst, raw, RestoreOptions{})
			if err != nil {
				t.Fatalf("re-Restore: %v", err)
			}
			if res.MemoryPendingRestored != 0 {
				t.Errorf("re-restore wrote %d queue rows, want 0", res.MemoryPendingRestored)
			}
			for _, w := range res.Warnings {
				if strings.Contains(w, "memory_pending") {
					t.Errorf("re-restore warned about the queue: %s", w)
				}
			}
			row, err := dst.MemoryPendingGet(ctx, "acme", store.MemoryScopeUser, "alice", "pend_acme_alice")
			if err != nil {
				t.Fatal(err)
			}
			if row.DrainedAt.IsZero() {
				t.Error("a row the target consolidated was re-queued by a re-restore")
			}
		})
	}
}

// TestMemoryPending_WithoutSectionRestoresAsBefore: a snapshot taken before
// the section existed restores memory as it always did, queues nothing and
// warns about nothing.
func TestMemoryPending_WithoutSectionRestoresAsBefore(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	plantPendingRows(t, src)
	if err := src.MemorySet(ctx, "acme", store.MemoryScopeUser, "alice", "k", json.RawMessage(`"v"`), 0); err != nil {
		t.Fatal(err)
	}
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw = withoutSection(t, raw, migrations.SectionMemoryPending)
	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.MemoryPendingRestored != 0 || res.MemoryRestored != 1 || len(res.Warnings) != 0 {
		t.Fatalf("restore = %+v; want the memory row, no queue rows, no warnings", res)
	}
	if got := pendingByID(t, dst); len(got) != 0 {
		t.Errorf("queue after an old-format restore = %+v, want empty", got)
	}
}

// TestMemoryPending_RowsNoConsolidatorCanDrainAreRefused: a hand-edited entry
// with no id, or under a scope nothing queues memory in, is skipped with a
// warning; the rest of the section lands.
func TestMemoryPending_RowsNoConsolidatorCanDrainAreRefused(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	want := plantPendingRows(t, src)
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw = editPendingEntries(t, raw, func(entries []any) []any {
		return append(entries,
			map[string]any{"id": "", "scope": "user", "scope_id": "mallory", "payload": map[string]any{}, "created_at": time.Now()},
			map[string]any{"id": "pend_global", "scope": "global", "scope_id": "", "payload": map[string]any{}, "created_at": time.Now()},
		)
	})
	res, err := Restore(ctx, dst, raw, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.MemoryPendingRestored != len(want) {
		t.Errorf("restored %d, want the %d valid rows", res.MemoryPendingRestored, len(want))
	}
	joined := strings.Join(res.Warnings, "\n")
	if !strings.Contains(joined, "has no id") || !strings.Contains(joined, `"global" is not a scope memory is queued under`) {
		t.Errorf("warnings = %q, want one per refused row", joined)
	}
	if _, err := dst.MemoryPendingGet(ctx, "", store.MemoryScopeGlobal, "", "pend_global"); err == nil {
		t.Error("a row under the global scope was restored")
	}
}

// editPendingEntries rewrites the envelope's memory_pending entries, dropping
// the checksum as a hand edit would.
func editPendingEntries(t *testing.T, raw []byte, edit func([]any) []any) []byte {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	sec := env["sections"].(map[string]any)[migrations.SectionMemoryPending].(map[string]any)
	entries, _ := sec["entries"].([]any)
	sec["entries"] = edit(entries)
	delete(env, "checksum")
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestMemoryPending_ErasureAfterRestoreRemovesTheRestoredRows: a restore keeps
// each row under the subject it was queued for, so a subject erasure on the
// target reaches it — and only that subject's rows in that tenant.
func TestMemoryPending_ErasureAfterRestoreRemovesTheRestoredRows(t *testing.T) {
	src, srcClose := newTestStore(t)
	defer srcClose()
	dst, dstClose := newTestStore(t)
	defer dstClose()
	ctx := context.Background()
	plantPendingRows(t, src)
	_, raw, err := Capture(ctx, src, CaptureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, dst, raw, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if _, err := dst.MemoryPendingGet(ctx, "acme", store.MemoryScopeUser, "alice", "pend_acme_alice"); err != nil {
		t.Fatalf("premise: the restored row is not on the target: %v", err)
	}

	svc := &erasure.Service{Store: dst}
	if _, err := svc.Execute(ctx, erasure.ExecuteRequest{Tenant: "acme", Subject: "alice", Confirm: "alice"}); err != nil {
		t.Fatalf("erasure: %v", err)
	}
	_, err = dst.MemoryPendingGet(ctx, "acme", store.MemoryScopeUser, "alice", "pend_acme_alice")
	var nf *store.ErrNotFound
	if !errors.As(err, &nf) {
		t.Errorf("after erasing acme/alice her restored queue row is still there (err %v)", err)
	}
	// Another tenant's alice is a different subject.
	if _, err := dst.MemoryPendingGet(ctx, "", store.MemoryScopeUser, "alice", "pend_op_alice"); err != nil {
		t.Errorf("erasing acme/alice removed the operator layer's alice row: %v", err)
	}
}
