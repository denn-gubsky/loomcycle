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

	return d.cascadeDeleteChunks(ctx, key, mscope, ids)
}

// cascadeDeleteChunks is the ONE writer of a chunk's removal, shared by the
// retired-content prune and the whole-scope reclaim. A second implementation would
// drift from this one, and the drift shows up as an orphaned edge or a body left
// in the Memory plane — invisible, because nothing reads an orphan.
func (d *Document) cascadeDeleteChunks(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, ids []string) (int, error) {
	pruned := 0
	for _, id := range ids {
		// One transaction per chunk rather than one for the batch: a fault partway
		// through leaves the chunks already pruned consistently gone, instead of
		// rolling back work the next sweep would repeat. The operation is idempotent,
		// so a retry costs nothing.
		if err := d.withSqlTxn(ctx, key, func(txnID string) error {
			// THE TABLE SET MATCHES delete_chunk's, and it has to stay matched. This
			// file's own header says a prune that reimplemented the delete would drift
			// from delete_chunk — and it had: chunk_tags, chunk_revisions and
			// chunk_layout were added to delete_chunk and never here, so pruning a
			// retired fact left its TAGS, its CANVAS POSITION and — worst — its whole
			// body-change log behind. `chunk_revisions.body` is the fact's text, so the
			// prune removed the chunk and kept the words it existed to reap, in a table
			// no read path consults and no sweeper walks.
			for _, stmt := range []string{
				`DELETE FROM chunk_edges WHERE from_id = ? OR to_id = ?`,
				`DELETE FROM chunk_assets WHERE chunk_id = ?`,
				`DELETE FROM chunk_tags WHERE chunk_id = ?`,
				`DELETE FROM chunk_memory_meta WHERE chunk_id = ?`,
				`DELETE FROM chunk_revisions WHERE chunk_id = ?`,
				`DELETE FROM chunk_layout WHERE chunk_id = ?`,
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

// PruneScopeChunks deletes EVERY chunk in one scope, bodies included.
//
// It backs the retention sweeper's whole-scope reclaim of a fully-retired agent.
// That reclaim already drops the SQL-Memory scope wholesale, which removes the
// chunks, the edges and the sidecar in one statement — but a chunk BODY is not in
// SQL Memory, it is a `doc.chunk:<hex>` row in the k/v plane, and dropping the
// scope leaves every one of them behind with nothing left to name them.
//
// ⚠️ THIS MUST RUN BEFORE THE SCOPE IS DROPPED. The chunk ids are the only route
// from a scope to its bodies, and dropping the scope destroys them. That ordering
// is the whole reason this exists as a separate call rather than as cleanup after.
//
// Unlike PruneRetiredChunks there is no cutoff and no evidential exemption: the
// agent this scope belongs to is gone, so "keep the source of what was derived"
// has nothing left to protect — the derived material is going too. A prune that
// exempted evidential content here would leave a scope that can never be emptied.
//
// ⚠️ IT DELETES THE SQL ROWS TOO, THOUGH THE SCOPE DROP WOULD TAKE THEM ANYWAY,
// and that redundancy is bought deliberately. Deleting only the bodies would be
// cheaper — one k/v delete per chunk instead of a transaction each — but it leaves
// a window where a failed DropScope has chunks whose text is gone. This tool's
// standing rule is that the safe asymmetry is an unreferenced body, never a chunk
// whose text has vanished, so the full cascade runs and a failed drop leaves the
// scope consistently empty instead of consistently broken.
func (d *Document) PruneScopeChunks(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, dryRun bool) (int, error) {
	if d.Store == nil || d.SqlMem == nil {
		return 0, fmt.Errorf("document prune: not configured")
	}
	// A scope that never used the chunk tier has no `chunks` table. Same treatment
	// as the retired-content prune: report nothing to do rather than a fault, and
	// never provision a table for a scope the sweeper is merely walking past.
	res, err := d.query(ctx, key, `SELECT id FROM chunks`)
	if err != nil {
		return 0, nil
	}
	ids := make([]string, 0, len(res.Rows))
	for _, r := range res.Rows {
		if id := asStr(r[0]); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if dryRun {
		return len(ids), nil
	}
	return d.cascadeDeleteChunks(ctx, key, mscope, ids)
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
