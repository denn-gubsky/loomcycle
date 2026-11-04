package builtin

// document_move.go — RFC FT P1: move_documents, a set of documents moved into
// another scope on the server, every id kept.
//
// A MOVE IS A COPY THAT KEEPS IDS, THEN A DELETE. The target is written by the
// copy path in move mode (document_copy.go): the same rows under the same ids,
// each document's rows in one target transaction with a marker row in
// document_moves. The source is removed only after the target is checked, and
// the target is named only after the source is gone, so a Path name never
// resolves in both scopes.
//
// THE ORDER, and what an interruption leaves:
//  1. target rows + marker, bodies, the statements between members;
//  2. check each document's target against its source;
//  3. remove the source (rows, bodies, units, names) — as delete_document does,
//     but keeping the change feed, which belongs to the document's id;
//  4. name the target, clear the marker, announce `moved`.
//
// The same call again resumes from any step. A target holding this move's own
// marker is its unfinished work, not an id collision: rows are kept, bodies are
// rewritten, statements already written are skipped. A marker whose source
// document is gone is past step 3, and only step 4 remains; every move call
// between the same two scopes finishes those first, whatever set it names,
// because once the source names are gone an under_path can no longer find them.
//
// WHY A MARKER TABLE, not a field on the root chunk (the RFC's wording): a
// listing filters on it in SQL, and a reader never sees it.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/audit"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// documentMovesDDL holds the documents a move into this scope has not finished:
// where each came from, and the Path names it takes when it does.
const documentMovesDDL = `CREATE TABLE IF NOT EXISTS document_moves (
		document_id TEXT PRIMARY KEY, from_scope TEXT NOT NULL, from_scope_id TEXT NOT NULL,
		names TEXT NOT NULL, started_at BIGINT NOT NULL)`

// notMoving keeps a document whose move into this scope is unfinished out of a
// listing: until the source is gone it would be listed in both scopes.
const notMoving = "id NOT IN (SELECT document_id FROM document_moves)"

// move refusal codes, besides the copy's.
const (
	moveVerifyFailed     = "verify_failed"
	moveSourceNotRemoved = "source_not_removed"
)

// moveFinish is a document whose target is complete and whose source is gone:
// it only needs its names, its marker cleared and its announcement.
type moveFinish struct {
	id    string
	names []string
}

// moveDocuments is op=move_documents.
func (d *Document) moveDocuments(ctx context.Context, src sqlmem.ScopeKey, srcMem store.MemoryScope, in docInput) (tools.Result, error) {
	const op = "move_documents"
	if strings.TrimSpace(in.ToScope) == "" {
		return errValidation(op+": missing required field: to_scope", "Pass `to_scope`: agent, user or tenant — the scope the documents go to."), nil
	}
	dst, dstMem, err := d.resolveScope(ctx, in.ToScope)
	if err != nil {
		return docFail(op+": to_scope: ", err), nil
	}
	if dst == src {
		return errValidation(op+": the documents are already in scope "+src.Scope,
			"To rename documents within one scope, use the Path tool's mv."), nil
	}
	if err := d.ensureSchema(ctx, dst); err != nil {
		return errFrom(op+": target schema init: "+err.Error(), err), nil
	}
	dangling := strings.TrimSpace(in.Dangling)
	if dangling != "" && dangling != "refuse" && dangling != "drop" {
		return errValidation(op+": dangling must be refuse or drop", "Omit it to refuse a move that would cut a relation."), nil
	}
	pathMap, perr := normalizePathMap(in.PathMap)
	if perr != nil {
		return errValidation(op+": path_map: "+perr.Error(), "Map absolute Path prefixes to absolute Path prefixes, e.g. {\"/loomcycle\": \"/kb/loomcycle\"}."), nil
	}
	dryRun := in.DryRun == nil || *in.DryRun
	if !dryRun && in.ExpectCount == nil {
		return errValidation(op+": expect_count is required with dry_run false",
			"Run the dry run first and pass its counts.documents as expect_count."), nil
	}
	// A pass that maintains the source's statements must not see its documents
	// vanish under it.
	if name, holder, err := d.heldScopeLease(ctx, src); err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	} else if name != "" {
		return errTransient(fmt.Sprintf("%s: scope %s is held by the lease %q (%s)", op, src.Scope, name, holder),
			"Retry when that pass ends; a lease expires on its own."), nil
	}
	pending, err := d.unfinishedMoves(ctx, src, dst)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	ids, refused := d.copySet(ctx, src, op, in, len(pending) > 0)
	if refused != nil {
		return *refused, nil
	}
	ids = withoutFinished(ids, pending)
	plan := &copyPlan{move: true, src: src, dst: dst, srcMem: srcMem, dstMem: dstMem, chunkDoc: map[string]string{},
		counts: map[string]int{}, chunkMap: map[string]string{}, failedDocs: map[string]string{}}
	if err := d.planCopy(ctx, plan, op, ids, pathMap); err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if err := d.planStatements(ctx, plan); err != nil {
		return errFrom(op+": statements: "+err.Error(), err), nil
	}
	var finished []string
	if !dryRun {
		if dangling != "drop" && plan.danglingCount > 0 {
			return errBusiness(fmt.Sprintf("%s: %d relations join a document of the set to one outside it, and a relation cannot span two scopes; nothing was moved",
				op, plan.danglingCount),
				"Run the dry run to list them under dangling, then add the other documents to the set, or pass dangling: \"drop\" to delete those relations with the source."), nil
		}
		if plan.relationsForbidden(dangling) {
			return errPermission(op+": the move would delete relations the caller's access-control rules do not let it remove; nothing was moved",
				"Move a set whose relations the caller may unlink, or ask an operator to move it."), nil
		}
		if *in.ExpectCount != len(plan.docs) {
			return errBusiness(fmt.Sprintf("%s: the set now holds %d documents to move, not the %d expected; nothing was moved",
				op, len(plan.docs), *in.ExpectCount), "Run the dry run again and pass its counts.documents."), nil
		}
		d.writeCopy(ctx, plan)
		finished = d.completeMove(ctx, plan, pending)
		d.auditMove(ctx, plan, finished)
	}
	out := copyResult(plan, dryRun)
	if dangling == "drop" && !dryRun {
		out["dropped"] = plan.danglingCount
	} else if dangling != "drop" && plan.danglingCount > 0 {
		out["blocked_by"] = "dangling_relations"
	} else if plan.relationsForbidden(dangling) {
		out["blocked_by"] = "forbidden_relations"
	}
	if dryRun {
		if len(pending) > 0 {
			ids := make([]string, len(pending))
			for i, f := range pending {
				ids[i] = f.id
			}
			out["to_finish"] = ids
		}
	} else {
		out["moved"] = finished
	}
	return jsonResult(out)
}

// relationsForbidden reports whether removing the sources would delete a
// relation the caller may not remove: one into a document it may not read
// (removed with the source whatever dangling says), or, under dangling:drop,
// one whose ends it may not link. It says neither which nor how many.
func (p *copyPlan) relationsForbidden(dangling string) bool {
	return p.hiddenDangling > 0 || (dangling == "drop" && p.unlinkable > 0)
}

// withoutFinished drops from ids the documents only waiting to be named: their
// source is gone, so the planner would call them not_found.
func withoutFinished(ids []string, pending []moveFinish) []string {
	if len(pending) == 0 {
		return ids
	}
	skip := map[string]bool{}
	for _, f := range pending {
		skip[f.id] = true
	}
	out := ids[:0:0]
	for _, id := range ids {
		if !skip[id] {
			out = append(out, id)
		}
	}
	return out
}

// moveTargetHolds asks whether the target already holds a document with id.
// resumed: it does, under this move's own marker (from this very source).
// collides: it does otherwise.
func (d *Document) moveTargetHolds(ctx context.Context, p *copyPlan, id string) (resumed, collides bool, err error) {
	res, err := d.query(ctx, p.dst, `SELECT 1 FROM documents WHERE id = ? LIMIT 1`, id)
	if err != nil || len(res.Rows) == 0 {
		return false, false, err
	}
	m, err := d.query(ctx, p.dst, `SELECT from_scope, from_scope_id FROM document_moves WHERE document_id = ?`, id)
	if err != nil {
		return false, false, err
	}
	if len(m.Rows) > 0 && asStr(m.Rows[0][0]) == p.src.Scope && asStr(m.Rows[0][1]) == p.src.ScopeID {
		return true, false, nil
	}
	return false, true, nil
}

// moveMarkerInsert is the marker row written with a moved document's rows.
func moveMarkerInsert(p *copyPlan, doc *copyDoc) copyInsert {
	names, _ := json.Marshal(doc.targets)
	return copyInsert{
		stmt: `INSERT INTO document_moves (document_id, from_scope, from_scope_id, names, started_at) VALUES (?, ?, ?, ?, ?)`,
		vals: []any{doc.id, p.src.Scope, p.src.ScopeID, string(names), time.Now().UnixMilli()},
	}
}

// unfinishedMoves lists the moves from src into dst that stopped after the
// source was removed: the target is the only copy, and only needs naming.
func (d *Document) unfinishedMoves(ctx context.Context, src, dst sqlmem.ScopeKey) ([]moveFinish, error) {
	var out []moveFinish
	err := d.copyRows(ctx, dst, `SELECT document_id, names FROM document_moves WHERE from_scope = ? AND from_scope_id = ?`, "document_id",
		[]any{src.Scope, src.ScopeID}, func(_ []string, r []any) error {
			f := moveFinish{id: asStr(r[0])}
			_ = json.Unmarshal([]byte(asStr(r[1])), &f.names)
			out = append(out, f)
			return nil
		})
	if err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, f := range out {
		res, err := d.query(ctx, src, `SELECT 1 FROM documents WHERE id = ? LIMIT 1`, f.id)
		if err != nil {
			return nil, err
		}
		if len(res.Rows) == 0 {
			kept = append(kept, f)
		}
	}
	return kept, nil
}

// heldScopeLease names a live lease on the scope held by someone else, "" when
// none is. The runtime's own short subject-binding locks are not passes.
func (d *Document) heldScopeLease(ctx context.Context, key sqlmem.ScopeKey) (name, holder string, err error) {
	now := time.Now()
	res, err := d.query(ctx, key, `SELECT name, owner FROM document_leases WHERE owner <> '' AND expires_at > ? AND expires_at <= ?`,
		now.UnixNano(), now.Add(maxDocumentLeaseTTL).UnixNano())
	if err != nil {
		return "", "", err
	}
	me := documentLeaseOwner(ctx)
	for _, r := range res.Rows {
		n, o := asStr(r[0]), asStr(r[1])
		if strings.HasPrefix(n, principalLeasePrefix) || o == me {
			continue
		}
		return n, o, nil
	}
	return "", "", nil
}

// completeMove checks each written document, removes its source, and finishes
// it, then finishes the moves an earlier call left past step 3. It returns the
// ids of the documents now moved.
func (d *Document) completeMove(ctx context.Context, p *copyPlan, pending []moveFinish) []string {
	finish := append([]moveFinish{}, pending...)
	var kept []*copyDoc
	for _, doc := range p.docs {
		if why := d.moveVerified(ctx, p, doc); why != "" {
			p.unfinish(doc.id, moveVerifyFailed, why)
			continue
		}
		if _, err := d.removeDocument(ctx, p.src, p.srcMem, doc.id); err != nil {
			slog.WarnContext(ctx, "move_documents: source not removed", "document_id", doc.id)
			p.unfinish(doc.id, moveSourceNotRemoved, err.Error())
			continue
		}
		kept = append(kept, doc)
		finish = append(finish, moveFinish{id: doc.id, names: doc.targets})
	}
	p.docs = kept
	p.counts["documents"] = len(kept)
	var moved []string
	for _, f := range finish {
		d.finishMove(ctx, p, f)
		moved = append(moved, f.id)
	}
	return moved
}

// moveVerified is "" when the target holds everything of doc the source does:
// every chunk and revision row, every body (none failed this call), every
// statement between members that touches it. Otherwise it says what is short,
// and the source stays.
func (d *Document) moveVerified(ctx context.Context, p *copyPlan, doc *copyDoc) string {
	if why := p.failedDocs[doc.id]; why != "" {
		return why
	}
	for _, q := range []struct{ what, stmt string }{
		{"chunks", `SELECT count(*) FROM chunks WHERE document_id = ?`},
		{"revisions", `SELECT count(*) FROM chunk_revisions WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`},
		{"assets", `SELECT count(*) FROM chunk_assets WHERE chunk_id IN (SELECT id FROM chunks WHERE document_id = ?)`},
	} {
		a, err := d.query(ctx, p.src, q.stmt, doc.id)
		if err != nil {
			return q.what + ": " + err.Error()
		}
		b, err := d.query(ctx, p.dst, q.stmt, doc.id)
		if err != nil {
			return q.what + ": " + err.Error()
		}
		if na, nb := asInt(a.Rows[0][0]), asInt(b.Rows[0][0]); na != nb {
			return fmt.Sprintf("the target holds %d %s, the source %d", nb, q.what, na)
		}
	}
	return ""
}

// finishMove names a moved document in the target, clears its marker and
// announces it. A name that can no longer be taken is reported; the document is
// moved all the same (set_path gives it one).
func (d *Document) finishMove(ctx context.Context, p *copyPlan, f moveFinish) {
	for _, n := range f.names {
		parent, name, _ := splitPath(n)
		if row, err := d.Store.DirentGet(ctx, direntTenant(ctx), p.dst.Scope, direntScopeID(p.dst), parent, name); err == nil && direntDocumentID(row) == f.id {
			continue // named by the call this one resumes
		}
		if _, err := d.registerDocDirent(ctx, p.dst, f.id, n); err != nil {
			p.nameFailures = append(p.nameFailures, map[string]any{"document_id": f.id, "path": n, "error": err.Error()})
		}
	}
	if err := d.exec(ctx, p.dst, `DELETE FROM document_moves WHERE document_id = ?`, f.id); err != nil {
		// The document stays out of listings until the same move is run again.
		p.unfinish(f.id, moveWriteMarker, err.Error())
		return
	}
	d.publishIngested(ctx, p.dst, p.dstMem, f.id, "moved", nil)
}

// moveWriteMarker: the marker of a moved document could not be cleared.
const moveWriteMarker = "marker_not_cleared"

// direntDocumentID is the document a Path name points at, "" for any other kind.
func direntDocumentID(row store.DirentRow) string {
	if row.Kind != "document" {
		return ""
	}
	var ref struct {
		DocumentID string `json:"document_id"`
	}
	_ = json.Unmarshal(row.ResourceRef, &ref)
	return ref.DocumentID
}

// auditMove records the move: who moved which documents from which scope to
// which, and what came of it. Ids and counts only, never content.
func (d *Document) auditMove(ctx context.Context, p *copyPlan, moved []string) {
	ident := tools.RunIdentity(ctx)
	counts := map[string]int{"moved": len(moved), "unfinished": len(p.unfinished), "refused": len(p.refused),
		"statements": p.counts["statements"], "dangling": p.danglingCount}
	if d.Audit == nil {
		slog.InfoContext(ctx, "move_documents", "tenant", ident.TenantID, "subject", ident.UserID,
			"from", p.src.Scope, "to", p.dst.Scope, "moved", len(moved), "unfinished", len(p.unfinished))
		return
	}
	ev := audit.Event{
		ActorTenant: ident.TenantID, ActorSubject: ident.UserID, Action: "document_move",
		TargetTenant:  ident.TenantID,
		MoveFromScope: p.src.Scope, MoveFromScopeID: p.src.ScopeID, MoveToScope: p.dst.Scope, MoveToScopeID: p.dst.ScopeID,
		MoveDocumentIDs: moved, MoveCounts: counts,
	}
	if err := d.Audit.Record(ev); err != nil {
		slog.ErrorContext(ctx, "move_documents: audit record failed", "err", err)
	}
}
