package builtin

// The entity-tier WRITE path (RFC BL P4c PR 2): idempotent upsert keyed by a
// natural key, and supersede-not-delete.
//
// WHY THERE IS NO SEPARATE "CURATOR" GATE HERE, though the phase plan called for
// one. Document.Execute resolves the scope CENTRALLY, before it dispatches, so
// every op below already passes through the check P4b added: reaching `tenant`
// scope requires the operator to have listed `tenant` in BOTH memory_scopes (the
// chunk bodies) and sql_scopes (the structure). An agent holding those grants IS
// the operator's designated curator for that tenant's shared memory.
//
// Adding a second, entity-specific gate on top would duplicate that decision and
// leave two places to get it wrong — the argument made against exactly this in
// P4b, and it holds in the other direction too. What PR 2 owes instead is proof
// that the new ops cannot BYPASS the existing gate, which is asserted rather than
// assumed (see TestEntity_TenantWritesRequireBothGrants).
//
// An agent writing entities in its OWN agent scope needs no curator: that graph is
// private to it, and there is nothing shared to poison.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// entityClasses is the closed set for chunk_memory_meta.class (Locked decision
// 10). `evidential` is EXEMPT from the retention sweeper's memory-content family,
// so it is a privilege a caller can claim — validated against this set, and worth
// knowing that a model could mark its own writes evidential to make them
// un-prunable. That is tolerated because the alternative is worse: the server has
// no signal from which to infer the distinction, and dropping it would lose the
// exemption that keeps source-of-truth material from being aged out.
var entityClasses = map[string]bool{"derived": true, "evidential": true}

// upsertChunk is create-or-update keyed by natural_key within the scope — the
// idempotency primitive the entity tier is built on. Calling it twice with the
// same key yields ONE chunk, which is what stops an entity accumulating a row per
// mention.
//
// It deliberately does NOT take a `revision`, unlike update_chunk. Optimistic
// concurrency assumes the caller read the row and knows its version; an upsert's
// whole premise is that the caller knows only the natural KEY. Last-writer-wins is
// the correct semantics when the key is the identity, and pretending otherwise
// would make every consolidation pass guess a revision it has no way to hold.
func (d *Document) upsertChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if strings.TrimSpace(in.NaturalKey) == "" {
		return errResult("upsert_chunk: missing required field: natural_key (the idempotency handle — without it this is create_chunk)"), nil
	}
	if in.Class != "" && !entityClasses[in.Class] {
		return errResult(fmt.Sprintf("upsert_chunk: unknown class %q (want derived or evidential)", in.Class)), nil
	}

	existing, err := d.chunkIDByNaturalKey(ctx, key, in.NaturalKey)
	if err != nil {
		return errResult("upsert_chunk: lookup: " + err.Error()), nil
	}

	if existing == "" {
		// Delegate the INSERT to create_chunk rather than writing a second insert
		// path. It owns the parent/position/document-exists validation, and a
		// duplicate would drift from it silently.
		res, cerr := d.createChunk(ctx, key, mscope, in)
		if cerr != nil || res.IsError {
			return res, cerr
		}
		var created struct {
			ID string `json:"id"`
		}
		if uerr := json.Unmarshal([]byte(res.Text), &created); uerr != nil || created.ID == "" {
			return errResult("upsert_chunk: created the chunk but could not read its id back"), nil
		}
		if werr := d.writeChunkMeta(ctx, key, created.ID, in); werr != nil {
			return errResult("upsert_chunk: chunk created but its metadata write failed: " + werr.Error()), nil
		}
		return okJSON(map[string]any{
			"id": created.ID, "natural_key": in.NaturalKey, "created": true,
		})
	}

	// Update in place. Only the fields the caller actually supplied move, so an
	// upsert that carries a new body does not blank a title it never mentioned.
	if uerr := d.updateChunkForUpsert(ctx, key, mscope, existing, in); uerr != nil {
		return errResult("upsert_chunk: " + uerr.Error()), nil
	}
	if werr := d.writeChunkMeta(ctx, key, existing, in); werr != nil {
		return errResult("upsert_chunk: metadata write failed: " + werr.Error()), nil
	}
	return okJSON(map[string]any{
		"id": existing, "natural_key": in.NaturalKey, "created": false,
	})
}

// chunkIDByNaturalKey resolves the scope's chunk for a natural key, or "" when
// absent. The lookup is an indexed point read on chunk_memory_meta — the reason
// the key lives in SQL rather than in the chunk's `fields` blob.
func (d *Document) chunkIDByNaturalKey(ctx context.Context, key sqlmem.ScopeKey, naturalKey string) (string, error) {
	res, err := d.query(ctx, key, `SELECT chunk_id FROM chunk_memory_meta WHERE natural_key = ?`, naturalKey)
	if err != nil {
		return "", err
	}
	if len(res.Rows) == 0 {
		return "", nil
	}
	return asStr(res.Rows[0][0]), nil
}

// updateChunkForUpsert applies the supplied fields to an existing chunk without a
// revision check (see upsertChunk). Absent fields are left alone.
func (d *Document) updateChunkForUpsert(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, chunkID string, in docInput) error {
	sets := []string{"updated_at = ?", "revision = revision + 1"}
	args := []any{time.Now().UnixNano()}
	if in.Title != "" {
		sets = append(sets, "title = ?")
		args = append(args, in.Title)
	}
	if in.Type != "" {
		sets = append(sets, "type = ?")
		args = append(args, in.Type)
	}
	if in.Status != "" {
		sets = append(sets, "status = ?")
		args = append(args, in.Status)
	}
	args = append(args, chunkID)
	if err := d.exec(ctx, key, `UPDATE chunks SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		return err
	}
	// Body/fields live in the Memory k/v plane. Only rewrite when the caller sent
	// something: writeBody replaces BOTH halves, so an unconditional write would
	// erase fields on a body-only upsert — the failure that emptied this store once.
	if in.Body != "" || len(in.Fields) > 0 {
		body, fields := in.Body, in.Fields
		if in.Body == "" || len(in.Fields) == 0 {
			cur, rerr := d.readBody(ctx, mscope, key.ScopeID, chunkID)
			if rerr != nil {
				return rerr
			}
			if in.Body == "" {
				body = cur.Body
			}
			if len(in.Fields) == 0 {
				fields = cur.Fields
			}
		}
		return d.writeBody(ctx, mscope, key.ScopeID, chunkID, body, fields)
	}
	return nil
}

// writeChunkMeta upserts the sidecar row.
//
// Delete-then-insert rather than ON CONFLICT, matching the two existing upserts in
// this file — one portable statement beats a per-dialect split.
//
// `origin` and the provenance triple are SERVER-STAMPED and deliberately not
// readable from the input. A forgeable origin would let any agent label its own
// writes as machine-distilled, and the column has to stay a trustworthy filter for
// the ones that really are — the same rule the consolidation queue's origin
// follows.
func (d *Document) writeChunkMeta(ctx context.Context, key sqlmem.ScopeKey, chunkID string, in docInput) error {
	now := time.Now().UnixNano()
	validAt := now
	if in.ValidAt != nil {
		validAt = *in.ValidAt
	}
	var invalidAt any
	if in.InvalidAt != nil {
		invalidAt = *in.InvalidAt
	}
	class := in.Class
	if class == "" {
		class = "derived"
	}
	var confidence any
	if in.Confidence != nil {
		confidence = *in.Confidence
	}
	if err := d.exec(ctx, key, `DELETE FROM chunk_memory_meta WHERE chunk_id = ?`, chunkID); err != nil {
		return err
	}
	return d.exec(ctx, key,
		`INSERT INTO chunk_memory_meta
		   (chunk_id, valid_at, invalid_at, created_at, expired_at, class, origin, confidence, session_id, run_id, event_seq, natural_key)
		 VALUES (?, ?, ?, ?, NULL, ?, ?, ?, ?, ?, NULL, ?)`,
		chunkID, validAt, invalidAt, now, class,
		originForEntityWrite(ctx), confidence,
		// session_id stays NULL: it is not on the run ctx, only the run id is. The
		// column is kept because the consolidation path can fill it when it relays a
		// drained pending row, and a column that is sometimes populated is more useful
		// than one removed because one writer cannot fill it.
		nil, nullIfEmpty(tools.RunID(ctx)), nullIfEmpty(in.NaturalKey))
}

// originForEntityWrite decides the provenance label from the RUN, never the input.
// A write arriving with no run id is an operator acting by hand (the MCP/off-run
// plane); one inside a run is an agent.
func originForEntityWrite(ctx context.Context) string {
	if tools.RunID(ctx) == "" {
		return "operator"
	}
	return "agent_explicit"
}

// supersedeChunk retires a chunk without deleting it: it closes BOTH of the old
// row's end-timestamps and links the replacement to it.
//
// Closing invalid_at AND expired_at together is the correction case — the fact
// stopped being true in the world at the same moment the system learned it had.
// (A fact that merely stopped being true, with no replacement, closes invalid_at
// alone via upsert_chunk.)
//
// The old chunk stays live and queryable, which is what keeps "as of June…"
// answerable. It becomes a "retired entity": eligible for the retention sweeper's
// memory-content family past an age, gated by its class.
func (d *Document) supersedeChunk(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.ID == "" {
		return errResult("supersede_chunk: missing required field: id (the REPLACEMENT chunk)"), nil
	}
	if in.SupersedesID == "" {
		return errResult("supersede_chunk: missing required field: supersedes_id (the chunk being retired)"), nil
	}
	if in.ID == in.SupersedesID {
		return errResult("supersede_chunk: a chunk cannot supersede itself"), nil
	}

	// Both chunks must exist, checked BEFORE anything is written: a supersede that
	// closed a real row and then failed to link a missing one would retire a fact
	// with nothing replacing it, which reads as "we forgot this" rather than "this
	// was corrected".
	for _, id := range []string{in.ID, in.SupersedesID} {
		res, err := d.query(ctx, key, `SELECT id FROM chunks WHERE id = ?`, id)
		if err != nil {
			return errResult("supersede_chunk: lookup: " + err.Error()), nil
		}
		if len(res.Rows) == 0 {
			return errResult(fmt.Sprintf("supersede_chunk: chunk %q not found in this scope", id)), nil
		}
	}

	now := time.Now().UnixNano()
	txErr := d.withSqlTxn(ctx, key, func(txnID string) error {
		// The retired row may have no sidecar yet (it predates the entity tier, or
		// was written by plain create_chunk), so seed one before closing it —
		// otherwise the UPDATE silently affects zero rows and the fact stays current.
		res, err := d.queryTxn(ctx, txnID, `SELECT chunk_id FROM chunk_memory_meta WHERE chunk_id = ?`, in.SupersedesID)
		if err != nil {
			return err
		}
		if len(res.Rows) == 0 {
			if err := d.execTxn(ctx, txnID,
				`INSERT INTO chunk_memory_meta (chunk_id, valid_at, created_at, class, origin) VALUES (?, ?, ?, ?, ?)`,
				in.SupersedesID, now, now, "derived", originForEntityWrite(ctx)); err != nil {
				return err
			}
		}
		if err := d.execTxn(ctx, txnID,
			`UPDATE chunk_memory_meta SET invalid_at = ?, expired_at = ? WHERE chunk_id = ?`,
			now, now, in.SupersedesID); err != nil {
			return err
		}
		return d.execTxn(ctx, txnID,
			`INSERT INTO chunk_edges (from_id, to_id, kind, created_at) VALUES (?, ?, ?, ?)`,
			in.ID, in.SupersedesID, "supersedes", now)
	})
	if txErr != nil {
		return errResult("supersede_chunk: " + txErr.Error()), nil
	}
	return okJSON(map[string]any{
		"id": in.ID, "supersedes": in.SupersedesID, "retired_at": now,
	})
}
