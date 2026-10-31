package builtin

// document_subject_merge.go — an operator folds one subject into another
// (RFC FR P2).
//
// Two subject documents that name one entity (person:ada-lovelace and
// person:ada-king, adopted from two proposals) split the tenant's facts and
// statements about it. The merge says once, in one SQL transaction, what that
// means for the store:
//
//   - the duplicate's facts (every chunk under its root) move under the
//     survivor's root, natural keys unchanged;
//   - every live statement with an end at the duplicate's root is copied with
//     the survivor in its place (attributes.revision_of naming the original),
//     and the original is expired with judge_reason "merged" — never deleted,
//     so an as-of read before the merge still answers;
//   - the duplicate's root stays, keeps its natural key and its path, and
//     records where the subject went: fields.merged_into and status `merged`.
//     A reference by natural key from another scope still resolves, to a root
//     that names the survivor.
//
// It is NOT a Document tool op. Which of two subjects survives is an operator
// decision, and the operator plane (HTTP, gRPC, the MCP `ontology` tool) is the
// only caller.
//
// ⚠️ fields.merged_into IS WRITTEN BEFORE THE TRANSACTION COMMITS. The links
// agent acts on a `merged` event only when the duplicate's own root records
// merged_into = the survivor, so the field must exist by the time the status
// flip commits. Fields live in the Memory k/v plane, which cannot join the SQL
// transaction; a failed transaction restores the previous fields. "Already
// merged" is judged on the SQL status, the transaction's commit point, so an
// interrupted attempt can be re-run.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	// subjectMergedStatus is the duplicate root's status once merged.
	subjectMergedStatus = "merged"
	// subjectMergedIntoField is the duplicate root's field naming the
	// survivor's document; the links agent confirms a merge against it.
	subjectMergedIntoField = "merged_into"
	// mergeSubjectsOp names the operation in access-control refusals.
	mergeSubjectsOp = "merge_subjects"
)

// mergeSubject is one side of a merge as read before the transaction.
type mergeSubject struct {
	connector.SubjectRef
	typ, status string
	revision    int
}

// mergePlan is what the transaction applies to one statement.
type mergePlan struct {
	s        statementRow
	from, to string
	copy     bool // false: expire only
}

// MergeSubjects folds the duplicate subject into the survivor in scope. Each is
// named by its document id or its natural key. Errors wrap the
// connector.ErrSubjectMerge* sentinels.
func (d *Document) MergeSubjects(ctx context.Context, scope, duplicate, survivor string) (connector.MergeSubjectsResult, error) {
	var out connector.MergeSubjectsResult
	duplicate, survivor = strings.TrimSpace(duplicate), strings.TrimSpace(survivor)
	if duplicate == "" || survivor == "" {
		return out, connector.WithMessage(connector.ErrSubjectMergeInvalid,
			"duplicate and survivor are both required: each a subject's document id or natural key")
	}
	if d.SqlMem == nil || d.Store == nil {
		return out, connector.WithMessage(connector.ErrSubjectMergeUnavailable, "merging subjects needs SQL Memory; none is configured")
	}
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return out, connector.WithMessage(connector.ErrSubjectMergeUnavailable, "merge_subjects: "+err.Error())
	}
	dup, err := d.mergeSubjectRef(ctx, key, duplicate)
	if err != nil {
		return out, err
	}
	surv, err := d.mergeSubjectRef(ctx, key, survivor)
	if err != nil {
		return out, err
	}
	out.Duplicate, out.Survivor = dup.SubjectRef, surv.SubjectRef
	if dup.DocumentID == surv.DocumentID {
		return out, connector.WithMessage(connector.ErrSubjectMergeSame,
			"duplicate and survivor name the same subject ("+dup.NaturalKey+")")
	}
	for _, s := range []mergeSubject{dup, surv} {
		if s.status == subjectMergedStatus {
			return out, connector.WithMessage(connector.ErrSubjectMergeAlreadyMerged,
				s.NaturalKey+" was already merged into another subject")
		}
	}
	acl := d.newDocACL(ctx, key)
	if acl != nil {
		for _, docID := range []string{dup.DocumentID, surv.DocumentID} {
			refused, err := acl.document(ctx, mergeSubjectsOp, "write", docID)
			if err != nil {
				return out, err
			}
			if refused != nil {
				return out, connector.WithMessage(connector.ErrSubjectMergeForbidden, refused.Text)
			}
		}
	}

	plans, left, err := d.planMergeStatements(ctx, key, acl, dup.RootChunkID, surv.RootChunkID)
	if err != nil {
		return out, err
	}
	out.StatementsLeft = left

	// merged_into before the transaction: see the file note.
	prior, err := d.readBody(ctx, mscope, key, dup.RootChunkID)
	if err != nil {
		return out, err
	}
	fields := map[string]any{}
	if len(prior.Fields) > 0 {
		_ = json.Unmarshal(prior.Fields, &fields)
	}
	fields[subjectMergedIntoField] = surv.DocumentID
	fb, _ := json.Marshal(fields)
	if err := d.writeBody(ctx, mscope, key, dup.RootChunkID, dup.typ, prior.Body, fb); err != nil {
		return out, err
	}

	moved, top, err := d.applySubjectMerge(ctx, key, dup, surv, plans)
	if err != nil {
		if rerr := d.writeBody(ctx, mscope, key, dup.RootChunkID, dup.typ, prior.Body, prior.Fields); rerr != nil {
			err = fmt.Errorf("%w (and restoring the duplicate's fields failed: %v)", err, rerr)
		}
		return out, err
	}
	out.FactsMoved = moved
	for _, p := range plans {
		if p.copy {
			out.StatementsRepointed++
		} else {
			out.StatementsRetired++
		}
	}

	scopeID := docMemScopeID(mscope, key)
	for _, id := range top {
		d.reindexSubtree(ctx, key, mscope, surv.DocumentID, id)
	}
	d.publishChange(ctx, mscope, scopeID, dup.DocumentID, mergeSubjectsOp, dup.RootChunkID)
	d.publishChange(ctx, mscope, scopeID, surv.DocumentID, mergeSubjectsOp, surv.RootChunkID)
	out.NotAnnounced = d.publishSubjectEvent(ctx, key, mscope, subjectEvent{
		Op: "merged", NaturalKey: dup.NaturalKey, DocumentID: dup.DocumentID, RootChunkID: dup.RootChunkID,
		SurvivingKey: surv.NaturalKey, SurvivingDocumentID: surv.DocumentID,
	})
	return out, nil
}

// mergeSubjectRef resolves a reference — a document id, else a natural key — to
// a subject's document in this scope.
func (d *Document) mergeSubjectRef(ctx context.Context, key sqlmem.ScopeKey, ref string) (mergeSubject, error) {
	var docID, rootID string
	res, err := d.query(ctx, key, `SELECT id, root_chunk_id FROM documents WHERE id = ?`, ref)
	if err != nil {
		return mergeSubject{}, err
	}
	if len(res.Rows) > 0 {
		docID, rootID = asStr(res.Rows[0][0]), asStr(res.Rows[0][1])
	} else {
		chunkID, err := d.chunkIDByNaturalKey(ctx, key, ref)
		if err != nil {
			return mergeSubject{}, err
		}
		if chunkID == "" {
			return mergeSubject{}, connector.WithMessage(connector.ErrSubjectMergeNotInScope,
				fmt.Sprintf("no subject %q in the %s scope: name each subject by its document id or natural key, and both must be in this scope", ref, key.Scope))
		}
		row, ok, err := d.getChunkRow(ctx, key, chunkID)
		if err != nil {
			return mergeSubject{}, err
		}
		if !ok {
			return mergeSubject{}, connector.WithMessage(connector.ErrSubjectMergeNotInScope, fmt.Sprintf("no subject %q in the %s scope", ref, key.Scope))
		}
		docID = row.DocumentID
		if rootID, err = d.documentRootChunk(ctx, key, docID); err != nil {
			return mergeSubject{}, err
		}
		if rootID != chunkID {
			return mergeSubject{}, connector.WithMessage(connector.ErrSubjectMergeNotSubject,
				fmt.Sprintf("%q is the key of a fact inside a document, not of a subject", ref))
		}
	}
	root, ok, err := d.getChunkRow(ctx, key, rootID)
	if err != nil {
		return mergeSubject{}, err
	}
	if !ok || !d.subjectNodes(ctx, key, []string{rootID})[rootID] {
		return mergeSubject{}, connector.WithMessage(connector.ErrSubjectMergeNotSubject,
			fmt.Sprintf("document %s is not a subject: its root carries no entity natural key", docID))
	}
	meta, err := d.query(ctx, key, `SELECT natural_key FROM chunk_memory_meta WHERE chunk_id = ?`, rootID)
	if err != nil {
		return mergeSubject{}, err
	}
	nk := ""
	if len(meta.Rows) > 0 {
		nk = asStr(meta.Rows[0][0])
	}
	return mergeSubject{
		SubjectRef: connector.SubjectRef{DocumentID: docID, RootChunkID: rootID, NaturalKey: nk},
		typ:        root.Type, status: root.Status, revision: root.Revision,
	}, nil
}

// planMergeStatements reads the live statements with an end at the duplicate's
// root and decides each one: copy onto the survivor and expire, expire only, or
// leave. Returns the plans and how many are left.
//
// Mirrors reimport's moveStatements: an inline [[name]] link (auto) belongs to
// the text that makes it and is never copied, and a statement whose other end
// is in a document the caller may not link is left as it is — re-pointing it
// changes that document's relations, which a write on these two does not
// authorise. Unlike moveStatements, a person's or an operator's statement is
// re-pointed too: the merge is itself an operator's decision.
func (d *Document) planMergeStatements(ctx context.Context, key sqlmem.ScopeKey, acl *docACL, dupRoot, survRoot string) ([]mergePlan, int, error) {
	res, err := d.query(ctx, key, `SELECT `+statementCols+` FROM chunk_edges e
		WHERE (e.from_id = ? OR e.to_id = ?) AND e.kind <> 'supersedes' AND e.expired_at IS NULL
		ORDER BY e.created_at, e.id`, dupRoot, dupRoot)
	if err != nil {
		return nil, 0, err
	}
	var plans []mergePlan
	left := 0
	for _, r := range res.Rows {
		s := scanStatement(r)
		if s.Auto {
			continue
		}
		if acl != nil {
			denied := false
			for _, end := range []string{s.FromID, s.ToID} {
				if end == dupRoot {
					continue
				}
				refused, err := acl.chunk(ctx, mergeSubjectsOp, "link", end)
				if err != nil {
					return nil, 0, err
				}
				denied = denied || refused != nil
			}
			if denied {
				left++
				continue
			}
		}
		p := mergePlan{s: s, from: s.FromID, to: s.ToID}
		if p.from == dupRoot {
			p.from = survRoot
		}
		if p.to == dupRoot {
			p.to = survRoot
		}
		switch {
		case p.from == p.to:
			// A statement between the two subjects would join the survivor to
			// itself: it is expired, not copied.
		default:
			held, err := d.statementByKey(ctx, key, p.from, p.to, s.Kind, s.ValidAt, s.Discriminator)
			if err != nil {
				return nil, 0, err
			}
			switch {
			case held == nil:
				p.copy = true
			case held.ExpiredAt != nil:
				// The survivor's matching statement was retired: the unique key is
				// taken, and copying would revive what was retired. Left for the
				// links agent's pass, which reports what it cannot re-point.
				left++
				continue
			}
			// held live: the survivor already says it; the original is expired.
		}
		plans = append(plans, p)
	}
	return plans, left, nil
}

// applySubjectMerge is the merge's one transaction. Returns how many chunks
// moved and the ids of those that now sit directly under the survivor's root.
func (d *Document) applySubjectMerge(ctx context.Context, key sqlmem.ScopeKey, dup, surv mergeSubject, plans []mergePlan) (int, []string, error) {
	var ids []string
	for _, p := range plans {
		if p.copy {
			ids = append(ids, p.s.ID)
		}
	}
	ev := map[string][]edgeEvidenceRow{}
	for _, batch := range batchIDs(ids, graphIDBatch) {
		if len(batch) == 0 {
			continue
		}
		got, _, err := d.evidenceFor(ctx, key, batch, 0)
		if err != nil {
			return 0, nil, err
		}
		for k, v := range got {
			ev[k] = v
		}
	}
	now := time.Now().UnixNano()
	judge := judgedByForVerdict(ctx)
	moved := 0
	var top []string
	err := d.withSqlTxn(ctx, key, func(tx string) error {
		// The commit point: the duplicate's root becomes `merged` only if nothing
		// changed it since it was read.
		r, err := d.docSQL().ExecTxn(ctx, tx, d.docSQL().Rebind(`UPDATE chunks SET status = ?, revision = revision + 1, updated_at = ?
			WHERE id = ? AND revision = ?`), []any{subjectMergedStatus, now, dup.RootChunkID, dup.revision}, 0)
		if err != nil {
			return err
		}
		if r.RowsAffected == 0 {
			return connector.WithMessage(connector.ErrSubjectMergeConflict, dup.NaturalKey+" changed while it was being merged; nothing was merged, so read it again and retry")
		}
		sv, err := d.queryTxn(ctx, tx, `SELECT coalesce(status, '') FROM chunks WHERE id = ?`, surv.RootChunkID)
		if err != nil {
			return err
		}
		if len(sv.Rows) == 0 || asStr(sv.Rows[0][0]) == subjectMergedStatus {
			return connector.WithMessage(connector.ErrSubjectMergeAlreadyMerged, surv.NaturalKey+" was merged into another subject while this merge ran")
		}

		// Facts move: every chunk of the duplicate's document but its root. The
		// ones directly under the root (or, in a malformed tree, beside it) go
		// after the survivor's own children, keeping their order.
		kids, err := d.queryTxn(ctx, tx, `SELECT id, parent_id FROM chunks WHERE document_id = ? AND id <> ?`, dup.DocumentID, dup.RootChunkID)
		if err != nil {
			return err
		}
		moved = len(kids.Rows)
		for _, k := range kids.Rows {
			if p := asStr(k[1]); p == "" || p == dup.RootChunkID {
				top = append(top, asStr(k[0]))
			}
		}
		if moved > 0 {
			last, err := d.queryTxn(ctx, tx, `SELECT coalesce(max(position), -1) FROM chunks WHERE parent_id = ?`, surv.RootChunkID)
			if err != nil {
				return err
			}
			offset := 0
			if len(last.Rows) > 0 {
				offset = asInt(last.Rows[0][0]) + 1
			}
			for _, s := range []struct {
				sql  string
				args []any
			}{
				{`UPDATE chunks SET parent_id = ?, position = position + ?, updated_at = ?
					WHERE document_id = ? AND id <> ? AND (parent_id = ? OR parent_id IS NULL)`,
					[]any{surv.RootChunkID, offset, now, dup.DocumentID, dup.RootChunkID, dup.RootChunkID}},
				{`UPDATE chunks SET document_id = ?, updated_at = ? WHERE document_id = ? AND id <> ?`,
					[]any{surv.DocumentID, now, dup.DocumentID, dup.RootChunkID}},
				// A type the moved chunks use may be defined on the duplicate's
				// document only; the survivor gets the definitions it lacks.
				{`INSERT INTO chunk_types (document_id, name, fields, created_at)
					SELECT ?, t.name, t.fields, t.created_at FROM chunk_types t
					WHERE t.document_id = ? AND NOT EXISTS (SELECT 1 FROM chunk_types h WHERE h.document_id = ? AND h.name = t.name)`,
					[]any{surv.DocumentID, dup.DocumentID, surv.DocumentID}},
			} {
				if err := d.execTxn(ctx, tx, s.sql, s.args...); err != nil {
					return err
				}
			}
		}

		for _, p := range plans {
			if p.copy {
				if err := d.copyMergedStatement(ctx, tx, p, ev[p.s.ID], now); err != nil {
					return err
				}
			}
			r, err := d.docSQL().ExecTxn(ctx, tx, d.docSQL().Rebind(`UPDATE chunk_edges SET expired_at = ?, judged_at = ?, judge_reason = ?, judged_by = ? WHERE `+statementLiveCAS),
				[]any{now, now, "merged", judge, p.s.ID, p.s.Status}, 0)
			if err != nil {
				return err
			}
			if r.RowsAffected == 0 {
				return connector.WithMessage(connector.ErrSubjectMergeConflict, "a statement of "+dup.NaturalKey+" changed while it was being merged; nothing was merged, so retry")
			}
		}
		return d.execTxn(ctx, tx, `UPDATE documents SET updated_at = ? WHERE id IN (?, ?)`, now, dup.DocumentID, surv.DocumentID)
	})
	if err != nil {
		if connector.SubjectMergeCode(err) != "" {
			return 0, nil, err
		}
		return 0, nil, fmt.Errorf("merge_subjects: %w", err)
	}
	return moved, top, nil
}

// copyMergedStatement writes p's copy and its evidence. The copy is its source
// row but for its id, its re-pointed ends, created_at and revision_of — the
// status, method, confidence, validity, ruling and verdict travel with it, as
// they do on reimport's copy, so a ruling keeps its protection and a withheld
// statement stays withheld.
func (d *Document) copyMergedStatement(ctx context.Context, tx string, p mergePlan, evidence []edgeEvidenceRow, now int64) error {
	s := p.s
	attrs := map[string]any{}
	for k, v := range s.Attributes {
		attrs[k] = v
	}
	attrs["revision_of"] = s.ID
	ab, _ := json.Marshal(attrs)
	copyID := newDocID()
	if err := d.execTxn(ctx, tx, `INSERT INTO chunk_edges
		(id, from_id, to_id, kind, created_at, auto, status, method, confidence, valid_at, invalid_at,
		 actor, run_id, ontology_version, discriminator, attributes,
		 verified_at, judged_at, judge_reason, judged_by, verdict_confidence, operator_ruled_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		copyID, p.from, p.to, s.Kind, now, s.Status, s.Method, s.Confidence, s.ValidAt, s.InvalidAt,
		nullIfEmpty(s.Actor), nullIfEmpty(s.RunID), s.OntologyVersion, s.Discriminator, string(ab),
		s.VerifiedAt, s.JudgedAt, nullIfEmpty(s.JudgeReason), nullIfEmpty(s.JudgedBy), s.VerdictConfidence, s.OperatorRuledAt); err != nil {
		return err
	}
	for _, e := range evidence {
		var start, end any
		if e.CharStart != nil {
			start = *e.CharStart
		}
		if e.CharEnd != nil {
			end = *e.CharEnd
		}
		stale := 0
		if e.Stale {
			stale = 1
		}
		if err := d.execTxn(ctx, tx, `INSERT INTO chunk_edge_evidence
			(id, edge_id, chunk_id, session_id, event_seq, ref_scope, ref_natural_key, quote, char_start, char_end, stale, created_at, run_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newDocID(), copyID, nullIfEmpty(e.ChunkID), nullIfEmpty(e.SessionID), e.EventSeq,
			nullIfEmpty(e.Scope), nullIfEmpty(e.NaturalKey), e.Quote, start, end, stale, now, nullIfEmpty(tools.RunID(ctx))); err != nil {
			return err
		}
	}
	return nil
}
