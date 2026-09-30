package snapshot

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The memory_pending section (RFC DP §4.7): the consolidation queue's
// UNDRAINED rows — adds that were accepted but not yet consolidated. Without
// it a restore silently loses every queued conversation: memory carries only
// what was already consolidated.
//
// "Undrained" is the store's own definition: drained_at IS NULL. A
// consolidation pass reads rows (MemoryPendingDrain, which does not mark
// them) and acks the ones it folded into memory (MemoryPendingAck stamps
// drained_at). A drained row stays in the table as history until a scope
// erasure deletes it; it is not work, so it never travels.
//
// A row holds no claim of its own. What makes a target's queue busy is the
// per-target lease in memory_cursors (with the target's watermark), which is
// owned by a replica of the SOURCE, so it is never carried. A restored row is
// therefore claimable on the target the moment it lands: the first pass there
// takes a fresh lease and drains it.
//
// AT-LEAST-ONCE, BY CAPTURE ORDER. The section is always captured, whether or
// not the runtime is paused, and it is read BEFORE memory. The two reads are
// not one transaction, and a pass can ack a row between them:
//   - pending first (this order): the row is captured as undrained AND the
//     facts it produced are in the memory read. The target consolidates it
//     again — a duplicate.
//   - memory first: the facts are not in the memory read yet, and the row is
//     acked before the pending read — in neither. A loss.
//
// A duplicate is the queue's own contract (drain is at-least-once: a pass
// that dies before its ack re-delivers the batch), and it is usually but not
// always harmless. Consolidation writes through the similarity merge band
// (a fact close enough to a stored one rewrites that row in place) and files
// entity facts by natural key, so a repeat of the same turns mostly collapses
// onto what the first pass wrote. But extraction is model-driven: a second
// pass is not guaranteed to produce the same facts or keys, so a restored
// duplicate can leave a near-duplicate fact behind. A capture taken while
// paused has no pass in flight and is exact.
//
// The live row stands: a row already on the id is left alone, including one
// the target has since drained — so re-restoring a snapshot does not queue
// the same work twice.

// captureMemoryPending reads every undrained row, every tenant's and target's.
func captureMemoryPending(ctx context.Context, s store.Store, out *MemoryPendingSection) error {
	out.Version = SectionVersion
	rows, err := s.SnapshotReadMemoryPending(ctx)
	if err != nil {
		return fmt.Errorf("snapshot memory_pending: %w", err)
	}
	out.Entries = make([]MemoryPendingEntry, 0, len(rows))
	for _, r := range rows {
		out.Entries = append(out.Entries, MemoryPendingEntry{
			ID:              r.ID,
			TenantID:        r.TenantID,
			Scope:           string(r.Scope),
			ScopeID:         r.ScopeID,
			Payload:         r.Payload,
			Origin:          r.Origin,
			SourceSessionID: r.SourceSessionID,
			SourceRunID:     r.SourceRunID,
			CreatedAt:       r.CreatedAt.UTC(),
		})
	}
	return nil
}

// pendingScopes are the scopes a queue row can be enqueued under: the Memory
// tool's add and the compaction bank write only these. A row naming anything
// else did not come from this runtime and has no consolidator to drain it.
var pendingScopes = map[store.MemoryScope]bool{
	store.MemoryScopeAgent:  true,
	store.MemoryScopeUser:   true,
	store.MemoryScopeTenant: true,
}

// restoreMemoryPending writes each row un-drained under its own tenant and
// target, keeping its id, origin, source ids and created_at, so it drains on
// the target in the order it would have on the source.
func restoreMemoryPending(ctx context.Context, s store.Store, sec *MemoryPendingSection, result *RestoreResult) {
	for _, e := range sec.Entries {
		where := fmt.Sprintf("memory_pending %s (%s/%s/%s)", e.ID, e.TenantID, e.Scope, e.ScopeID)
		if e.ID == "" {
			result.Warnings = append(result.Warnings, where+": not restored: the row has no id")
			continue
		}
		if !pendingScopes[store.MemoryScope(e.Scope)] {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %q is not a scope memory is queued under", where, e.Scope))
			continue
		}
		inserted, err := s.SnapshotRestoreMemoryPending(ctx, store.MemoryPendingRow{
			ID:              e.ID,
			TenantID:        e.TenantID,
			Scope:           store.MemoryScope(e.Scope),
			ScopeID:         e.ScopeID,
			Payload:         e.Payload,
			Origin:          e.Origin,
			SourceSessionID: e.SourceSessionID,
			SourceRunID:     e.SourceRunID,
			CreatedAt:       e.CreatedAt,
		})
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: not restored: %v", where, err))
			continue
		}
		if inserted {
			result.MemoryPendingRestored++
		}
	}
}
