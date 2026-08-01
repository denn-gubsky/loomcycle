package builtin

// document_deadlink.go — the authoritative dead-link reconciliation for one scope.
//
// A chunk is referenced from five places: chunk_edges (both endpoints),
// chunk_assets, chunk_memory_meta, its own row, and its BODY in the Memory k/v
// plane. delete_chunk and delete_document clean all of them, and the read-time
// guard hides a hit whose chunk or body has gone. Neither is complete, which is
// why this exists:
//
//   - The body delete runs AFTER the transaction commits, deliberately (it is a
//     different store, so it cannot join the txn). A crash in that window leaves a
//     body whose chunk is gone.
//   - An out-of-band delete — a repair script, a restore, an operator with psql —
//     bypasses the tool entirely and leaves every reference behind.
//   - The read-time guard makes an orphan invisible, which is the right behaviour
//     for a read and is precisely why nothing ever notices the orphan is there.
//
// So this is the only layer that actually collects them, and it is always-on
// integrity rather than opt-in policy: everything it deletes is UNREACHABLE by
// definition — a body no chunk points at, an edge to a chunk that does not exist.
// That is the line between this and the retention sweeper, which deletes reachable
// data because an operator asked it to, and is off by default for that reason.
//
// It has its OWN advisory key. LockKeyMemorySweeper belongs to the SQL-Memory
// scope GC; sharing it would make one subsystem's tick silently starve the other.

import (
	"context"
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// DeadLinkReport is one scope's reconciliation outcome.
//
// The reaped classes are separated from the reported ones because they carry
// different authority. A dangling edge is unreachable and deleting it restores an
// invariant; a chunk whose DOCUMENT has gone is a different thing — it may be a
// tree an operator can still recover, and destroying it to tidy a count is not
// this sweeper's call. Reported, so it is visible, and left alone.
type DeadLinkReport struct {
	Scope sqlmem.ScopeKey

	// Reaped (or, in a dry run, reapable).
	Bodies   int // doc.chunk:<id> in Memory with no chunks row
	Sidecars int // chunk_memory_meta rows with no chunks row
	Edges    int // chunk_edges with a missing endpoint
	Assets   int // chunk_assets rows with no chunks row

	// Reported only, never deleted.
	OrphanChunks int // chunks whose document_id has no documents row

	// Skipped explains a scope this pass declined to touch. Non-empty means
	// NOTHING was deleted for it.
	Skipped string
}

// Total is the reaped count across every class.
func (r DeadLinkReport) Total() int { return r.Bodies + r.Sidecars + r.Edges + r.Assets }

// Empty reports whether there is nothing worth logging.
func (r DeadLinkReport) Empty() bool {
	return r.Total() == 0 && r.OrphanChunks == 0 && r.Skipped == ""
}

func (r DeadLinkReport) String() string {
	if r.Skipped != "" {
		return fmt.Sprintf("%s/%s/%s: skipped (%s)", r.Scope.Tenant, r.Scope.Scope, r.Scope.ScopeID, r.Skipped)
	}
	s := fmt.Sprintf("%s/%s/%s: %d body, %d sidecar, %d edge, %d asset",
		r.Scope.Tenant, r.Scope.Scope, r.Scope.ScopeID, r.Bodies, r.Sidecars, r.Edges, r.Assets)
	if r.OrphanChunks > 0 {
		s += fmt.Sprintf(" (+%d chunk(s) whose document is gone — reported, not deleted)", r.OrphanChunks)
	}
	return s
}

// ReconcileDeadLinks collects one scope's unreachable chunk references.
//
// dryRun counts without deleting, which is how an operator sizes the problem
// before arming the sweeper — and how the sweeper itself reports when configured
// to observe only.
func (d *Document) ReconcileDeadLinks(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, dryRun bool) (DeadLinkReport, error) {
	rep := DeadLinkReport{Scope: key}

	// THE LIVE CHUNK-ID SET IS THE WHOLE BASIS OF EVERY DECISION BELOW, so a fault
	// reading it aborts the scope rather than proceeding.
	//
	// This is the guard that matters most here. If the read failed and were treated
	// as "no chunks exist", every body in the scope would look orphaned and the
	// sweeper would delete the lot. A transient SQL error, a scope resolved with the
	// wrong id, a schema not yet provisioned — this subsystem has produced all three
	// — would each turn an integrity pass into data loss.
	live, err := d.liveChunkIDs(ctx, key)
	if err != nil {
		return rep, err
	}

	// And a second guard on the same failure shape, for the case where the read
	// SUCCEEDS but against the wrong place: a scope holding chunk bodies but zero
	// chunk rows is either mid-provisioning or a mis-resolved scope key, not a scope
	// whose every chunk was deleted. Refusing costs one skipped tick; being wrong
	// costs every body in the scope.
	bodies, err := d.chunkBodyKeys(ctx, key, mscope)
	if err != nil {
		return rep, err
	}
	if len(live) == 0 && len(bodies) > 0 {
		rep.Skipped = fmt.Sprintf("%d chunk bodies but no chunk rows — refusing to treat that as a fully-deleted scope", len(bodies))
		return rep, nil
	}

	// --- the SQL-side anti-joins, all within one database so a subquery works ---
	//
	// NOT IN is safe against NULLs here because chunks.id is the primary key and
	// can never be null; a NULL on the right of NOT IN would otherwise make the
	// whole predicate unknown and silently match nothing.
	for _, c := range []struct {
		into  *int
		table string
		where string
	}{
		{&rep.Sidecars, "chunk_memory_meta", "chunk_id NOT IN (SELECT id FROM chunks)"},
		{&rep.Assets, "chunk_assets", "chunk_id NOT IN (SELECT id FROM chunks)"},
		{&rep.Edges, "chunk_edges", "from_id NOT IN (SELECT id FROM chunks) OR to_id NOT IN (SELECT id FROM chunks)"},
	} {
		n, err := d.countWhere(ctx, key, c.table, c.where)
		if err != nil {
			return rep, err
		}
		*c.into = n
		if n > 0 && !dryRun {
			if err := d.exec(ctx, key, `DELETE FROM `+c.table+` WHERE `+c.where); err != nil {
				return rep, err
			}
		}
	}

	// Reported, not reaped — see DeadLinkReport.
	if n, err := d.countWhere(ctx, key, "chunks", "document_id NOT IN (SELECT id FROM documents)"); err == nil {
		rep.OrphanChunks = n
	} else {
		return rep, err
	}

	// --- the cross-plane pass: bodies in Memory, chunks in SQL Memory ---
	//
	// Not expressible as a join: the two live in different stores. Which is also
	// why this class exists at all, since a cross-store delete cannot be
	// transactional.
	for _, b := range bodies {
		if live[b.chunkID] {
			continue
		}
		rep.Bodies++
		if dryRun {
			continue
		}
		// bodyTenantsFor mirrors the prune's two-try lookup: sqlmem needs a
		// non-empty tenant so the single-tenant default is "default", while the k/v
		// plane wrote those same rows under "". Try both; the first hit wins.
		for _, tenant := range bodyTenantsFor(key.Tenant) {
			if removed, _ := d.Store.MemoryDelete(ctx, tenant, mscope, key.ScopeID, b.key); removed {
				break
			}
		}
	}
	return rep, nil
}

// liveChunkIDs reads the scope's chunk ids as a set.
func (d *Document) liveChunkIDs(ctx context.Context, key sqlmem.ScopeKey) (map[string]bool, error) {
	res, err := d.query(ctx, key, `SELECT id FROM chunks`)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(res.Rows))
	for _, r := range res.Rows {
		if id := asStr(r[0]); id != "" {
			out[id] = true
		}
	}
	return out, nil
}

type chunkBodyRow struct {
	key     string
	chunkID string
}

// chunkBodyKeys lists the scope's chunk-body keys from the Memory plane.
func (d *Document) chunkBodyKeys(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope) ([]chunkBodyRow, error) {
	const prefix = "doc.chunk:"
	var out []chunkBodyRow
	for _, tenant := range bodyTenantsFor(key.Tenant) {
		entries, _, err := d.Store.MemoryList(ctx, tenant, mscope, key.ScopeID, prefix, 0)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if id := strings.TrimPrefix(e.Key, prefix); id != e.Key && id != "" {
				out = append(out, chunkBodyRow{key: e.Key, chunkID: id})
			}
		}
		if len(out) > 0 {
			// The bodies live under ONE of the candidate tenants, never split across
			// them. Stopping at the first non-empty answer keeps a scope from being
			// counted twice on the tier where both spellings resolve.
			break
		}
	}
	return out, nil
}

// countWhere counts rows matching a predicate. The predicate is a compile-time
// constant from the table above, never caller input.
func (d *Document) countWhere(ctx context.Context, key sqlmem.ScopeKey, table, where string) (int, error) {
	res, err := d.query(ctx, key, `SELECT count(*) FROM `+table+` WHERE `+where)
	if err != nil {
		return 0, err
	}
	if len(res.Rows) == 0 || len(res.Rows[0]) == 0 {
		return 0, nil
	}
	n, _ := asInt64(res.Rows[0][0])
	return int(n), nil
}
