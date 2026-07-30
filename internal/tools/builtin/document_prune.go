package builtin

// document_prune.go — pruning RETIRED entity-tier chunks (RFC BL P4c/P4d).
//
// Supersede-not-delete means a corrected fact is kept, not removed, which is what
// leaves "as of June…" answerable. The cost is that retired rows accumulate
// forever, so something eventually has to reap them — and this is that something,
// driven by the RFC BM retention sweeper rather than by a bespoke loop of its own.
//
// The method lives HERE, on the Document tool, so the chunk cascade stays in one
// place. A prune that reimplemented the delete would drift from delete_chunk, and
// the drift would show up as orphaned edges or a body left behind in the Memory
// plane — invisible, because nothing reads an orphan.

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// EvidentialClass is exempt from pruning. `derived` material was distilled from
// something else and can be re-derived; `evidential` IS the something else, so
// ageing it out loses what everything else was derived FROM.
//
// Mirrors the pinned-session exemption in the chats family: one marker the operator
// (or the writer) sets to mean "this one survives the policy".
const EvidentialClass = "evidential"

// PruneRetiredChunks deletes entity-tier chunks that were retired before cutoff.
//
// A chunk is eligible when it has an end-timestamp (invalid_at or expired_at) older
// than cutoff AND its class is not evidential. Chunks with no sidecar row are never
// eligible — an ordinary document chunk has no retirement to age from, and a prune
// that swept them would delete live documents.
//
// dryRun counts without deleting, so an operator can size the change before
// enabling a destructive mode.
//
// Returns the number of chunks pruned (or, under dryRun, that would be).
func (d *Document) PruneRetiredChunks(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, cutoff int64, dryRun bool) (int, error) {
	if d.Store == nil || d.SqlMem == nil {
		return 0, fmt.Errorf("document prune: not configured")
	}
	// A scope that never used the entity tier has no sidecar table until
	// ensureSchema runs. Creating it here would provision a table for every scope
	// the sweeper walks past, so the query is attempted and a missing-table error is
	// reported as "nothing to prune" rather than as a fault.
	res, err := d.query(ctx, key,
		`SELECT chunk_id FROM chunk_memory_meta
		  WHERE (invalid_at IS NOT NULL OR expired_at IS NOT NULL)
		    AND COALESCE(expired_at, invalid_at) < ?
		    AND (class IS NULL OR class <> ?)`,
		cutoff, EvidentialClass)
	if err != nil {
		return 0, nil
	}
	if len(res.Rows) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		if id := asStr(r[0]); id != "" {
			ids = append(ids, id)
		}
	}
	if dryRun {
		return len(ids), nil
	}

	pruned := 0
	for _, id := range ids {
		// One transaction per chunk rather than one for the batch: a fault partway
		// through leaves the chunks already pruned consistently gone, instead of
		// rolling back work the next sweep would repeat. The operation is idempotent,
		// so a retry costs nothing.
		if err := d.withSqlTxn(ctx, key, func(txnID string) error {
			for _, stmt := range []string{
				`DELETE FROM chunk_edges WHERE from_id = ? OR to_id = ?`,
				`DELETE FROM chunk_assets WHERE chunk_id = ?`,
				`DELETE FROM chunk_memory_meta WHERE chunk_id = ?`,
				`DELETE FROM chunks WHERE id = ?`,
			} {
				args := []any{id}
				if stmt == `DELETE FROM chunk_edges WHERE from_id = ? OR to_id = ?` {
					args = []any{id, id}
				}
				if err := d.execTxn(ctx, txnID, stmt, args...); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return pruned, err
		}
		// The BODY lives in the Memory k/v plane, outside the SQL transaction. Deleted
		// AFTER the structure commits, so a failure here leaves an unreferenced body
		// rather than a chunk whose text has vanished — the same ordering the rest of
		// this tool uses, and the safer of the two asymmetries.
		//
		// The tenant comes from the SCOPE KEY, never from ctx. Every other write path
		// here uses direntTenant(ctx), which is right for a tool call inside a run —
		// but this method is called by the retention sweeper, which has no RunIdentity
		// on its context at all. Deriving the tenant from ctx there yields "" while the
		// bodies sit under the run's tenant, so every body would be left orphaned:
		// invisible, since no read returns it and no sweeper reaps it. A test on the
		// real path caught it; a unit test on the SQL half alone would not have.
		for _, tenant := range bodyTenantsFor(key.Tenant) {
			if removed, _ := d.Store.MemoryDelete(ctx, tenant, mscope, key.ScopeID, chunkBodyKey(id)); removed {
				break
			}
		}
		pruned++
	}
	return pruned, nil
}

// bodyTenantsFor returns the tenant value(s) a chunk body may be stored under, given
// the SQL scope key's tenant.
//
// The two planes canonicalize differently, which is documented at every site that
// touches both: SQL Memory rejects an empty tenant and maps "" → "default", while
// the k/v plane and the Path tree key on the RAW tenant and leave it "". So a
// deployment in open mode has SQL "default" and bodies under "".
//
// Returning BOTH candidates for "default" is deliberate. Deleting an absent key is a
// no-op, so trying both costs nothing and covers open mode; the alternative — an
// inverse mapping — cannot distinguish open mode from a tenant literally named
// "default", an ambiguity this canonicalization already carries everywhere else.
func bodyTenantsFor(sqlTenant string) []string {
	if sqlTenant == "default" {
		return []string{"default", ""}
	}
	return []string{sqlTenant}
}
