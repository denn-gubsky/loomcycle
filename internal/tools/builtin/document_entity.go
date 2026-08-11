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
		return d.writeBody(ctx, mscope, key, chunkID, "", body, fields)
	}
	return nil
}

// chunkMetaRow is a sidecar row as stored. Nullable columns are pointers so
// "absent" survives a read/write round trip — writing 0 where NULL was is the
// difference between "never retired" and "retired at the unix epoch".
type chunkMetaRow struct {
	ValidAt    *int64
	InvalidAt  *int64
	CreatedAt  *int64
	ExpiredAt  *int64
	Class      string
	Origin     string
	Confidence *float64
	SessionID  string
	RunID      string
	EventSeq   *int64
	NaturalKey string
}

// readChunkMeta returns the chunk's sidecar row, or found=false when it has none.
func (d *Document) readChunkMeta(ctx context.Context, key sqlmem.ScopeKey, chunkID string) (row chunkMetaRow, found bool, err error) {
	res, err := d.query(ctx, key,
		`SELECT valid_at, invalid_at, created_at, expired_at, class, origin, confidence, session_id, run_id, event_seq, natural_key
		   FROM chunk_memory_meta WHERE chunk_id = ?`, chunkID)
	if err != nil || len(res.Rows) == 0 {
		return chunkMetaRow{}, false, err
	}
	r := res.Rows[0]
	return chunkMetaRow{
		ValidAt: asInt64Ptr(r[0]), InvalidAt: asInt64Ptr(r[1]),
		CreatedAt: asInt64Ptr(r[2]), ExpiredAt: asInt64Ptr(r[3]),
		Class: asStr(r[4]), Origin: asStr(r[5]), Confidence: asFloat64Ptr(r[6]),
		SessionID: asStr(r[7]), RunID: asStr(r[8]), EventSeq: asInt64Ptr(r[9]),
		NaturalKey: asStr(r[10]),
	}, true, nil
}

// chunkMetaToJSON renders a fact's sidecar row as the get_chunk / list_facts
// `entity` block. Timestamps stay raw unix-nanos int64 (lossless — the UI
// formats them), matching graph_recall's valid_at/invalid_at output. Nil/empty
// fields are omitted (as graphChunk omits nil timestamps), EXCEPT `retired`,
// which is always present so a reader never has to infer it from an absent key.
//
// `retired` keys on expired_at (SYSTEM time), not invalid_at: the supersede path
// closes expired_at and the retired predicate keys on it (see writeChunkMeta). A
// future world-time invalid_at is a still-current fact with a known end, so
// keying `retired` on invalid_at would report such a fact as retired.
func chunkMetaToJSON(m chunkMetaRow) map[string]any {
	out := map[string]any{"retired": m.ExpiredAt != nil}
	if m.ValidAt != nil {
		out["valid_at"] = *m.ValidAt
	}
	if m.InvalidAt != nil {
		out["invalid_at"] = *m.InvalidAt
	}
	if m.CreatedAt != nil {
		out["created_at"] = *m.CreatedAt
	}
	if m.ExpiredAt != nil {
		out["expired_at"] = *m.ExpiredAt
	}
	if m.Class != "" {
		out["class"] = m.Class
	}
	if m.Origin != "" {
		out["origin"] = m.Origin
	}
	if m.NaturalKey != "" {
		out["natural_key"] = m.NaturalKey
	}
	if m.Confidence != nil {
		out["confidence"] = *m.Confidence
	}
	if m.SessionID != "" {
		out["session_id"] = m.SessionID
	}
	if m.RunID != "" {
		out["run_id"] = m.RunID
	}
	if m.EventSeq != nil {
		out["event_seq"] = *m.EventSeq
	}
	return out
}

// listFacts returns the scope's facts — chunks that HAVE a chunk_memory_meta
// sidecar — as metadata only, no bodies. It is the browse surface behind the
// human-facing memory view: a viewer fetches a fact's body via get_chunk on
// click, exactly as graph_recall returns no body. The INNER JOIN is what makes
// "fact" mean "has a sidecar" rather than "any chunk".
func (d *Document) listFacts(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	if in.Class != "" && !entityClasses[in.Class] {
		return errResult("list_facts: class must be one of: derived, evidential"), nil
	}
	// Same default/cap as graph_recall — a fact list is a browse surface, so the
	// bound is a page size, not a correctness limit.
	limit := graphDefaultLimit
	if in.Limit > 0 {
		limit = in.Limit
	}
	if limit > graphMaxLimit {
		limit = graphMaxLimit
	}

	where := []string{}
	args := []any{}
	// RFC BZ subtype expansion: listing facts of type `event` must also list the
	// `incident` facts, or the taxonomy an operator built changes no answers.
	typeExpansion := d.expandTypeFilter(ctx, in.Type)
	if frag, targs := typeFilterSQL("c.type", typeExpansion); frag != "" {
		where = append(where, frag)
		args = append(args, targs...)
	}
	if in.Class != "" {
		where = append(where, "m.class = ?")
		args = append(args, in.Class)
	}
	if in.DocumentID != "" {
		where = append(where, "c.document_id = ?")
		args = append(args, in.DocumentID)
	}
	// Reuse graph_recall's temporal filter so "currently true" means the same
	// thing on both surfaces. The INNER JOIN makes m.chunk_id non-null, so the
	// clause's `m.chunk_id IS NULL OR ...` disjunct is inert here (which is
	// correct: a JOINed row always has a sidecar). Empty when include_retired.
	if temporal, targs := graphTemporalClause(in); temporal != "" {
		where = append(where, temporal)
		args = append(args, targs...)
	}

	stmt := `SELECT c.id, c.document_id, c.parent_id, c.position, c.title, c.type, c.status, c.revision,
	                m.valid_at, m.invalid_at, m.created_at, m.expired_at, m.class, m.origin, m.confidence,
	                m.session_id, m.run_id, m.event_seq, m.natural_key
	           FROM chunks c JOIN chunk_memory_meta m ON m.chunk_id = c.id`
	if len(where) > 0 {
		stmt += " WHERE " + strings.Join(where, " AND ")
	}
	// Over-fetch by one to detect truncation without a second COUNT round trip.
	stmt += " ORDER BY m.created_at DESC LIMIT ?"
	args = append(args, limit+1)

	res, err := d.query(ctx, key, stmt, args...)
	if err != nil {
		return errResult("list_facts: " + err.Error()), nil
	}
	truncated := len(res.Rows) > limit
	rows := res.Rows
	if truncated {
		rows = rows[:limit]
	}

	facts := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		// Scan the m.* columns back into a chunkMetaRow so the per-fact `entity`
		// block is byte-identical to get_chunk's — one formatter, no drift.
		meta := chunkMetaRow{
			ValidAt: asInt64Ptr(r[8]), InvalidAt: asInt64Ptr(r[9]),
			CreatedAt: asInt64Ptr(r[10]), ExpiredAt: asInt64Ptr(r[11]),
			Class: asStr(r[12]), Origin: asStr(r[13]), Confidence: asFloat64Ptr(r[14]),
			SessionID: asStr(r[15]), RunID: asStr(r[16]), EventSeq: asInt64Ptr(r[17]),
			NaturalKey: asStr(r[18]),
		}
		fact := map[string]any{
			"id":          asStr(r[0]),
			"document_id": asStr(r[1]),
			"position":    asInt(r[3]),
			"title":       asStr(r[4]),
			"revision":    asInt(r[7]),
			"entity":      chunkMetaToJSON(meta),
		}
		if pid := asStr(r[2]); pid != "" {
			fact["parent_id"] = pid
		}
		if typ := asStr(r[5]); typ != "" {
			fact["type"] = typ
		}
		if status := asStr(r[6]); status != "" {
			fact["status"] = status
		}
		facts = append(facts, fact)
	}
	out := map[string]any{"facts": facts, "count": len(facts), "truncated": truncated}
	// REPORTED for the same reason query_chunks reports it: the filter that ran is
	// wider than the one that was asked for, and a count nobody can account for reads
	// as a bug rather than as a working taxonomy.
	if len(typeExpansion) > 1 {
		out["type_expanded_to"] = typeExpansion
	}
	return okJSON(out)
}

// writeChunkMeta upserts the sidecar row, PRESERVING every field the caller did not
// supply.
//
// The preservation is the whole point and it was missing. `upsertChunk` already
// takes care not to blank a title an upsert never mentioned — then called this,
// which rebuilt the sidecar from defaults. So the two halves of one operation had
// opposite semantics, and a re-observation of an existing fact silently:
//
//   - dropped `class: evidential` back to `derived`, defeating the retention
//     exemption that keeps source-of-truth material from being aged out;
//   - reset `created_at`, so "when did we first believe this" became "when did we
//     last write it" — the bi-temporal record erased by re-observation;
//   - cleared `invalid_at` and `expired_at`, UN-RETIRING a superseded fact. The
//     stale row came back as current beside the one that replaced it, while the
//     `supersedes` edge still said it had been replaced. Two contradictory facts,
//     both live, in a store other agents read as ground truth.
//
// That last one is the failure supersede-not-delete exists to prevent, and a
// consolidator upserts by natural key on EVERY pass — so corrections were
// impermanent by construction.
//
// A retired fact is NOT revivable through this op, and that is deliberate rather
// than an omission. `invalid_at` (world time) is caller-settable, so an upsert can
// move when the fact stopped being true; `expired_at` (system time) is not, and the
// retired predicate keys on it. So pushing invalid_at into the future does not bring
// a superseded fact back — measured, not assumed.
//
// That is the correct shape for a bi-temporal store: system time is append-only.
// "We stopped believing X at T" is itself a historical fact, and erasing it would be
// rewriting history rather than recording a change of mind. The way to retract a
// correction is to record another one — write a new fact and supersede the
// superseder, which leaves the whole chain and its timestamps intact.
//
// Delete-then-insert rather than ON CONFLICT, matching the two existing upserts in
// this file — one portable statement beats a per-dialect split.
//
// `origin` and the provenance triple stay SERVER-STAMPED and deliberately not
// readable from the input. A forgeable origin would let any agent label its own
// writes as machine-distilled, and the column has to stay a trustworthy filter for
// the ones that really are — the same rule the consolidation queue's origin follows.
func (d *Document) writeChunkMeta(ctx context.Context, key sqlmem.ScopeKey, chunkID string, in docInput) error {
	now := time.Now().UnixNano()
	prev, hadPrev, err := d.readChunkMeta(ctx, key, chunkID)
	if err != nil {
		// Refuse rather than fall back to defaults. Falling back is exactly the bug
		// above: a read fault would silently un-retire a fact and strip its class.
		return err
	}

	// valid_at — caller, else what was already believed, else now.
	validAt := now
	if hadPrev && prev.ValidAt != nil {
		validAt = *prev.ValidAt
	}
	if in.ValidAt != nil {
		validAt = *in.ValidAt
	}
	// invalid_at — caller, else preserved. Preserving is what keeps a retirement
	// retired across a re-observation.
	invalidAt := int64Arg(prev.InvalidAt)
	if in.InvalidAt != nil {
		invalidAt = *in.InvalidAt
	}
	// created_at / expired_at are SYSTEM time and never caller-settable: the first
	// is when the store began believing this, the second when it stopped. Only a
	// first write sets one and only supersede sets the other.
	createdAt := now
	if hadPrev && prev.CreatedAt != nil {
		createdAt = *prev.CreatedAt
	}
	expiredAt := int64Arg(prev.ExpiredAt)

	class := prev.Class
	if in.Class != "" {
		class = in.Class
	}
	if class == "" {
		class = "derived"
	}
	confidence := float64Arg(prev.Confidence)
	if in.Confidence != nil {
		confidence = *in.Confidence
	}
	// The provenance triple is preserved when this write cannot supply it — an
	// operator editing a fact an agent recorded should not erase which run recorded
	// it. `origin` DOES re-stamp, because it describes whoever wrote the content
	// that is there now.
	runID := tools.RunID(ctx)
	if runID == "" {
		runID = prev.RunID
	}
	naturalKey := in.NaturalKey
	if naturalKey == "" {
		naturalKey = prev.NaturalKey
	}

	if err := d.exec(ctx, key, `DELETE FROM chunk_memory_meta WHERE chunk_id = ?`, chunkID); err != nil {
		return err
	}
	return d.exec(ctx, key,
		`INSERT INTO chunk_memory_meta
		   (chunk_id, valid_at, invalid_at, created_at, expired_at, class, origin, confidence, session_id, run_id, event_seq, natural_key)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		chunkID, validAt, invalidAt, createdAt, expiredAt, class,
		originForEntityWrite(ctx), confidence,
		// session_id has no writer yet: it is not on the run ctx, only the run id is.
		// Preserved rather than nulled so the consolidation path — which CAN fill it
		// when it relays a drained pending row — does not lose it to the next upsert.
		nullIfEmpty(prev.SessionID), nullIfEmpty(runID), int64Arg(prev.EventSeq),
		nullIfEmpty(naturalKey))
}

// int64Arg / float64Arg turn a nullable read back into a bind arg that round-trips
// NULL as NULL. A plain zero would assert an instant (the unix epoch) or a
// confidence of 0 where the column said "unknown".
func int64Arg(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func float64Arg(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

// asInt64Ptr / asFloat64Ptr read a nullable numeric cell as a pointer, so a NULL
// survives the round trip. asInt64Ptr reuses asInt64's presence/value split (see
// its comment: NULL and a real 0 are different claims here).
func asInt64Ptr(v any) *int64 {
	if n, ok := asInt64(v); ok {
		return &n
	}
	return nil
}

func asFloat64Ptr(v any) *float64 {
	switch t := v.(type) {
	case nil:
		return nil
	case float64:
		return &t
	case float32:
		f := float64(t)
		return &f
	case int64:
		f := float64(t)
		return &f
	case int:
		f := float64(t)
		return &f
	}
	return nil
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

	// A chunk may be retired ONCE. Without this, two different replacements can
	// each supersede the same fact — both succeed, both stay live, and a recall
	// returns two contradictory "current" answers. That defeats the entire point of
	// supersede-not-delete, which is that there is always exactly one current
	// answer and the old ones remain queryable behind it.
	//
	// A correction chain is A -> B -> C: to correct B you supersede B, not A.
	// Forking A twice is a caller error, so it is refused by NAMING the existing
	// replacement — that is the id the caller almost certainly meant to pass.
	//
	// Re-superseding with the SAME replacement is different: it is genuinely
	// idempotent, and must be a clean no-op rather than an error. These ops are
	// driven by a background consolidator that retries after a partial failure, and
	// a retry that reports failure for work already done is indistinguishable from
	// one that could not do it. Previously this surfaced the raw PK violation
	// ("UNIQUE constraint failed: chunk_edges.from_id, ..."), leaking schema
	// internals into a model-visible result.
	prior, err := d.query(ctx, key,
		`SELECT from_id FROM chunk_edges WHERE to_id = ? AND kind = 'supersedes'`, in.SupersedesID)
	if err != nil {
		return errResult("supersede_chunk: supersession lookup: " + err.Error()), nil
	}
	for _, row := range prior.Rows {
		existing := asStr(row[0])
		if existing == in.ID {
			return okJSON(map[string]any{
				"id": in.ID, "supersedes": in.SupersedesID, "already": true,
			})
		}
		return errResult(fmt.Sprintf(
			"supersede_chunk: chunk %q is already superseded by %q. To correct that newer "+
				"fact, supersede %q instead — superseding the same chunk twice would leave two "+
				"contradictory current facts.", in.SupersedesID, existing, existing)), nil
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
