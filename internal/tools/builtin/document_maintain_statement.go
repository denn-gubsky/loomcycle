package builtin

// document_maintain_statement.go — the upkeep of statements by the code that
// keeps them.
//
// A pass that re-checks the store's statements needs to retire one whose end is
// gone, send one back to review, fold a duplicate into its twin, and keep each
// evidence row pointing at text that still says it. The ops an agent had could
// only propose, judge (which re-scores) or hard-delete (unlink_chunks, under
// link). maintain_statement is the narrow op between them: it never asserts —
// acceptance stays an operator's ruling — and never deletes a statement or an
// evidence row.
//
// Who may call it: a code agent holding a statement_methods grant (its writes
// carry the method it stamps), and the operator plane. A statement a person
// wrote (method manual) takes only verify and flag from anyone: what a person
// asserted is not a pass's to undo.
//
// Every write is compare-and-set on the statement as it was read — its status
// and that it is still live (and, for flag, its attributes) — so of two
// maintainers acting at once the second is refused with statement_changed
// rather than applied over the first's change.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const maintainOp = "maintain_statement"

// maintainActions is every action, and whether it applies to a statement a
// person wrote.
var maintainActions = map[string]bool{
	"verify":            true,
	"flag":              true,
	"expire":            false,
	"demote":            false,
	"merge":             false,
	"stale_evidence":    false,
	"relocate_evidence": false,
}

// errMaintainChanged rolls back a merge whose statement (or survivor) changed
// after it was read.
var errMaintainChanged = errors.New("statement changed")

// statementLiveCAS is the condition every maintenance write holds the statement
// to: the status it was read with, and still not retired.
const statementLiveCAS = `id = ? AND status = ? AND expired_at IS NULL`

func maintainChanged() tools.Result {
	return errBusiness(maintainOp+": statement_changed: the statement was changed or retired while this was applied",
		"Reload it (op=get_edge) and decide again.")
}

// maintainStatement is Document op=maintain_statement.
func (d *Document) maintainStatement(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if !authorIsOperatorPlane(ctx) && !HasStatementMethodGrant(ctx) {
		return errPermission(maintainOp+": permission: only a code agent granted statement_methods, or an operator, maintains statements",
			"Propose a correction instead (op=propose_statement with id)."), nil
	}
	if in.ID == "" {
		return errValidation(maintainOp+": missing required field: id", "Pass the statement id from op=list_statements or op=get_edges."), nil
	}
	if _, ok := maintainActions[in.Action]; !ok {
		return errValidation(fmt.Sprintf("%s: unknown action %q", maintainOp, in.Action),
			"Pass action: verify, expire, demote, merge, stale_evidence, relocate_evidence or flag."), nil
	}
	s, err := d.statementByID(ctx, key, in.ID)
	if err != nil {
		return errFrom(maintainOp+": "+err.Error(), err), nil
	}
	if s == nil {
		return errNotFound(maintainOp+": no such statement: "+in.ID, "List statements with op=list_statements or op=get_edges."), nil
	}
	return d.applyMaintenance(ctx, key, mscope, in, s)
}

// applyMaintenance applies in.Action to s, the statement as the caller read it.
// Split from maintainStatement so the compare-and-set can be exercised against
// a read that another writer has since overtaken.
func (d *Document) applyMaintenance(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, s *statementRow) (tools.Result, error) {
	switch {
	case s.Status == edgeStatusRejected:
		return errBusiness(maintainOp+": statement_rejected: an operator rejected this statement; it is a tombstone, not something to maintain", ""), nil
	case s.ExpiredAt != nil:
		return errBusiness(maintainOp+": statement_expired: the statement is already retired", "Leave it; it stays readable with include_retired."), nil
	case s.Method == edgeMethodManual && !maintainActions[in.Action]:
		return errBusiness(maintainOp+": statement_manual: a person wrote this statement, so only verify and flag apply to it",
			"Flag it for review (action flag) and say why in reason."), nil
	}
	reason := strings.TrimSpace(in.Reason)
	judge := in.edgeMethod
	if judge == "" {
		judge = judgedByForVerdict(ctx)
	}
	now := time.Now().UnixNano()
	out := map[string]any{"id": s.ID, "action": in.Action, "ok": true}
	published := s.Status
	var n int64
	var err error
	switch in.Action {
	case "verify":
		n, err = d.execCount(ctx, key, `UPDATE chunk_edges SET verified_at = ? WHERE `+statementLiveCAS, now, s.ID, s.Status)
	case "expire":
		if reason == "" {
			return errValidation(maintainOp+": expire: reason is required", "Say why the statement no longer holds; a reader of the retired statement sees it."), nil
		}
		n, err = d.execCount(ctx, key, `UPDATE chunk_edges SET expired_at = ?, judged_at = ?, judge_reason = ?, judged_by = ? WHERE `+statementLiveCAS,
			now, now, reason, judge, s.ID, s.Status)
		published = "expired"
	case "demote":
		if s.Status != edgeStatusAsserted {
			return errBusiness(maintainOp+": statement_not_asserted: the statement is "+s.Status+", so there is nothing to demote", ""), nil
		}
		if reason == "" {
			return errValidation(maintainOp+": demote: reason is required", "Say why the statement goes back to review; the reviewer reads it."), nil
		}
		n, err = d.execCount(ctx, key, `UPDATE chunk_edges SET status = ?, judged_at = ?, judge_reason = ?, judged_by = ? WHERE `+statementLiveCAS,
			edgeStatusProposed, now, reason, judge, s.ID, s.Status)
		published = edgeStatusProposed
	case "flag":
		var refused *tools.Result
		n, refused, err = d.flagStatement(ctx, key, s, reason)
		if refused != nil {
			return *refused, nil
		}
	case "stale_evidence":
		if in.EvidenceID == "" {
			return errValidation(maintainOp+": stale_evidence: evidence_id is required", "Pass the evidence row's id from op=get_edge."), nil
		}
		refused, rerr := d.statementEvidenceRow(ctx, key, s.ID, in.EvidenceID)
		if rerr != nil {
			return errFrom(maintainOp+": "+rerr.Error(), rerr), nil
		}
		if refused != nil {
			return *refused, nil
		}
		n, err = d.execCount(ctx, key, `UPDATE chunk_edge_evidence SET stale = 1 WHERE id = ? AND edge_id = ?
			AND EXISTS (SELECT 1 FROM chunk_edges WHERE `+statementLiveCAS+`)`, in.EvidenceID, s.ID, s.ID, s.Status)
	case "relocate_evidence":
		var refused *tools.Result
		n, refused, err = d.relocateEvidence(ctx, key, mscope, s, in)
		if refused != nil {
			return *refused, nil
		}
	case "merge":
		sv, counts, refused, merr := d.mergeStatement(ctx, key, s, in, judge, now)
		if errors.Is(merr, errMaintainChanged) {
			return maintainChanged(), nil
		}
		if merr != nil {
			return errFrom(maintainOp+": "+merr.Error(), merr), nil
		}
		if refused != nil {
			return *refused, nil
		}
		out["survivor_id"], out["evidence_moved"], out["evidence_duplicate"] = sv.ID, counts.moved, counts.duplicate
		if counts.left > 0 {
			out["evidence_left"] = counts.left
		}
		d.publishEdgeChange(ctx, key, mscope, maintainOp, s.ID, s.FromID, s.ToID, s.Kind, "expired")
		d.publishEdgeChange(ctx, key, mscope, maintainOp, sv.ID, sv.FromID, sv.ToID, sv.Kind, sv.Status)
		return jsonResult(out)
	}
	if err != nil {
		return errFrom(maintainOp+": "+err.Error(), err), nil
	}
	if n == 0 {
		return maintainChanged(), nil
	}
	d.publishEdgeChange(ctx, key, mscope, maintainOp, s.ID, s.FromID, s.ToID, s.Kind, published)
	return jsonResult(out)
}

// flagStatement sets attributes.needs_review (and needs_review_reason) for a
// person to look at. Compare-and-set on the attributes as well, so two flags
// at once cannot drop each other's other attributes.
func (d *Document) flagStatement(ctx context.Context, key sqlmem.ScopeKey, s *statementRow, reason string) (int64, *tools.Result, error) {
	res, err := d.query(ctx, key, `SELECT COALESCE(attributes, '') FROM chunk_edges WHERE id = ?`, s.ID)
	if err != nil || len(res.Rows) == 0 {
		return 0, nil, err
	}
	raw := asStr(res.Rows[0][0])
	attrs := map[string]any{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &attrs); err != nil {
			return 0, nil, err
		}
	}
	attrs["needs_review"] = true
	if reason != "" {
		attrs["needs_review_reason"] = reason
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return 0, nil, err
	}
	if len(b) > edgeAttributesMaxBytes {
		r := errValidation(fmt.Sprintf("%s: flag: the statement's attributes would pass %d bytes", maintainOp, edgeAttributesMaxBytes), "Shorten reason.")
		return 0, &r, nil
	}
	n, err := d.execCount(ctx, key, `UPDATE chunk_edges SET attributes = ? WHERE `+statementLiveCAS+` AND COALESCE(attributes, '') = ?`,
		string(b), s.ID, s.Status, raw)
	return n, nil, err
}

// statementEvidenceRow refuses an evidence id that is not a row of the
// statement: a statement's maintenance reaches only its own evidence.
func (d *Document) statementEvidenceRow(ctx context.Context, key sqlmem.ScopeKey, edgeID, evidenceID string) (*tools.Result, error) {
	res, err := d.query(ctx, key, `SELECT 1 FROM chunk_edge_evidence WHERE id = ? AND edge_id = ?`, evidenceID, edgeID)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) == 0 {
		r := errNotFound(maintainOp+": no such evidence row on that statement: "+evidenceID, "Read the statement's evidence ids with op=get_edge.")
		return &r, nil
	}
	return nil, nil
}

// relocateEvidence re-points a chunk citation to chunk_id after checking its
// quote occurs there, recomputing the span (characters) and clearing stale.
func (d *Document) relocateEvidence(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, s *statementRow, in docInput) (int64, *tools.Result, error) {
	refuse := func(r tools.Result) (int64, *tools.Result, error) { return 0, &r, nil }
	if in.EvidenceID == "" || in.ChunkID == "" {
		return refuse(errValidation(maintainOp+": relocate_evidence: evidence_id and chunk_id are required",
			"Pass the evidence row's id (op=get_edge) and the chunk its quote is in now."))
	}
	res, err := d.query(ctx, key, `SELECT COALESCE(chunk_id, ''), quote FROM chunk_edge_evidence WHERE id = ? AND edge_id = ?`, in.EvidenceID, s.ID)
	if err != nil {
		return 0, nil, err
	}
	if len(res.Rows) == 0 {
		return refuse(errNotFound(maintainOp+": no such evidence row on that statement: "+in.EvidenceID, "Read the statement's evidence ids with op=get_edge."))
	}
	from, quote := asStr(res.Rows[0][0]), asStr(res.Rows[0][1])
	if from == "" {
		return refuse(errValidation(maintainOp+": relocate_evidence moves a chunk citation, and this evidence cites a chat turn or another scope", ""))
	}
	// A chunk the caller may not read answers like a missing one, as checkEvidence
	// does: checking the quote against it would say whether that text is there.
	readable, err := d.chunkReadable(ctx, key, in.ChunkID)
	if err != nil {
		return 0, nil, err
	}
	if !readable {
		return refuse(errNotFound(maintainOp+": no such chunk: "+in.ChunkID, "Pass a chunk of this scope."))
	}
	cb, err := d.readBody(ctx, mscope, key, in.ChunkID)
	if err != nil {
		return 0, nil, err
	}
	start, end, found := quoteSpan(cb.Body, quote, nil, nil)
	if !found {
		return refuse(errValidation(maintainOp+": evidence_quote_not_found — the evidence's quote does not occur in chunk "+in.ChunkID,
			"Relocate it to the chunk whose text holds the quote, or mark it stale (action stale_evidence)."))
	}
	n, err := d.execCount(ctx, key, `UPDATE chunk_edge_evidence SET chunk_id = ?, char_start = ?, char_end = ?, stale = 0
		WHERE id = ? AND edge_id = ? AND chunk_id = ? AND EXISTS (SELECT 1 FROM chunk_edges WHERE `+statementLiveCAS+`)`,
		in.ChunkID, start, end, in.EvidenceID, s.ID, from, s.ID, s.Status)
	return n, nil, err
}

// mergeCounts is where a merged statement's evidence rows went.
type mergeCounts struct {
	moved, duplicate, left int
}

// mergeStatement folds s into survivor_id, a live statement of the same from,
// to and kind: s is expired (judge_reason "merged") and its evidence re-pointed
// to the survivor, except a row the survivor already holds (same source, quote
// and span) and any past the survivor's evidence cap — those stay on s, so
// nothing is deleted. One transaction: a merge whose statement or survivor
// changed after it was read leaves both as they were (errMaintainChanged).
func (d *Document) mergeStatement(ctx context.Context, key sqlmem.ScopeKey, s *statementRow, in docInput, judge string, now int64) (*statementRow, mergeCounts, *tools.Result, error) {
	var counts mergeCounts
	refuse := func(r tools.Result) (*statementRow, mergeCounts, *tools.Result, error) { return nil, counts, &r, nil }
	if in.SurvivorID == "" {
		return refuse(errValidation(maintainOp+": merge: survivor_id is required", "Pass the statement that keeps the evidence."))
	}
	sv, err := d.statementByID(ctx, key, in.SurvivorID)
	if err != nil {
		return nil, counts, nil, err
	}
	switch {
	case sv == nil:
		return refuse(errNotFound(maintainOp+": no such statement: "+in.SurvivorID, "Pass survivor_id from op=list_statements or op=get_edges."))
	case sv.ID == s.ID || sv.FromID != s.FromID || sv.ToID != s.ToID || sv.Kind != s.Kind ||
		sv.ExpiredAt != nil || sv.Status == edgeStatusRejected:
		return refuse(errValidation(maintainOp+": survivor_mismatch: a merge keeps another live statement of the same from_id, to_id and kind",
			"Pass a live statement of the same relation as survivor_id, or expire this one instead."))
	}
	err = d.withSqlTxn(ctx, key, func(tx string) error {
		res, err := d.SqlMem.ExecTxn(ctx, tx, d.SqlMem.Rebind(`UPDATE chunk_edges SET expired_at = ?, judged_at = ?, judge_reason = ?, judged_by = ? WHERE `+statementLiveCAS),
			[]any{now, now, "merged", judge, s.ID, s.Status}, 0)
		if err != nil {
			return err
		}
		if res.RowsAffected == 0 {
			return errMaintainChanged
		}
		live, err := d.queryTxn(ctx, tx, `SELECT COUNT(*) FROM chunk_edges WHERE `+statementLiveCAS, sv.ID, sv.Status)
		if err != nil {
			return err
		}
		if len(live.Rows) == 0 || asInt(live.Rows[0][0]) == 0 {
			return errMaintainChanged
		}
		const cols = `id, COALESCE(chunk_id, ''), COALESCE(session_id, ''), COALESCE(event_seq, -1), COALESCE(ref_scope, ''),
			COALESCE(ref_natural_key, ''), quote, COALESCE(char_start, -1), COALESCE(char_end, -1)`
		// rowKey is a row's source, quote and span: what makes two rows the same
		// citation.
		rowKey := func(r []any) string {
			return strings.Join([]string{asStr(r[1]), asStr(r[2]), strconv.Itoa(asInt(r[3])), asStr(r[4]), asStr(r[5]),
				asStr(r[6]), strconv.Itoa(asInt(r[7])), strconv.Itoa(asInt(r[8]))}, "\x00")
		}
		held, err := d.queryTxn(ctx, tx, `SELECT `+cols+` FROM chunk_edge_evidence WHERE edge_id = ?`, sv.ID)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for _, r := range held.Rows {
			have[rowKey(r)] = true
		}
		mine, err := d.queryTxn(ctx, tx, `SELECT `+cols+` FROM chunk_edge_evidence WHERE edge_id = ? ORDER BY created_at, id`, s.ID)
		if err != nil {
			return err
		}
		n, max := len(held.Rows), edgeEvidenceMax()
		for _, r := range mine.Rows {
			k := rowKey(r)
			switch {
			case have[k]:
				counts.duplicate++
				continue
			case n >= max:
				counts.left++
				continue
			}
			if err := d.execTxn(ctx, tx, `UPDATE chunk_edge_evidence SET edge_id = ? WHERE id = ? AND edge_id = ?`, sv.ID, asStr(r[0]), s.ID); err != nil {
				return err
			}
			have[k] = true
			n++
			counts.moved++
		}
		return nil
	})
	if err != nil {
		return nil, mergeCounts{}, nil, err
	}
	return sv, counts, nil, nil
}
