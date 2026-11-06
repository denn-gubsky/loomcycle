package builtin

// document_mark_source.go — RFC GB §5: Document op=mark_source, the marker an
// imported document carries when its source is gone.
//
// A sync of a source that keeps changing sees an item removed or trashed at the
// source. The document stays — its text, its statements and its history are the
// knowledge base's, not the source's — and says so: the provenance's
// source_status (present, removed, trashed) and the document tag
// source/removed, which a reader filters on. The op never deletes or rewrites a
// body: it revises the root's provenance alone, as a revision of the root, so
// the earlier status stays in history.
//
// Who may call it: the operator, and a run granted statement_methods (rule or
// extractor) — the grant maintain_statement already asks of the code that keeps
// a corpus in step. A sync pass is that code; a second grant for one marker
// would be one more thing an operator must know to give, for no narrower door.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const markSourceOp = "mark_source"

// sourceStatuses are mark_source's statuses; sourceRemovedTag is the document
// tag a gone source adds.
var sourceStatuses = []string{"present", "removed", "trashed"}

const sourceRemovedTag = "source/removed"

// markSource is Document op=mark_source.
func (d *Document) markSource(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	if !callerDecidesIn(ctx, key) && !HasStatementMethodGrant(ctx) {
		return errPermission(markSourceOp+": permission: only a code agent granted statement_methods, or an operator, marks a document's source",
			"Ask an operator to mark it, or to grant this agent statement_methods."), nil
	}
	status := strings.TrimSpace(in.Status)
	if !slices.Contains(sourceStatuses, status) {
		return errValidation(fmt.Sprintf("%s: status %q is not one of %s", markSourceOp, status, strings.Join(sourceStatuses, ", ")),
			"Pass status: present, removed or trashed."), nil
	}
	docID, err := d.docIDFromInput(ctx, key, docInput{ID: in.ID, DocumentID: in.DocumentID, Path: in.Path})
	if err != nil {
		return errFrom(markSourceOp+": "+err.Error(), err), nil
	}
	// The editions lock: a re-import revises the same provenance.
	unlock, err := d.editions.lock(ctx, key, "document", docID)
	if err != nil {
		return errFrom(markSourceOp+": "+err.Error(), err), nil
	}
	defer unlock()
	res, err := d.query(ctx, key, `SELECT root_chunk_id FROM documents WHERE id = ?`, docID)
	if err != nil {
		return errFrom(markSourceOp+": "+err.Error(), err), nil
	}
	if len(res.Rows) == 0 {
		return errNotFound(markSourceOp+": no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	rootID := asStr(res.Rows[0][0])
	cb, err := d.readBody(ctx, mscope, key, rootID)
	if err != nil {
		return errFrom(markSourceOp+": "+err.Error(), err), nil
	}
	prov := d.importedProvenance(ctx, mscope, key, rootID, cb)
	if prov == nil {
		return errBusiness(markSourceOp+": not_imported: the document was not imported, so it has no source to mark",
			"Only a document made by File op=import, or by import_md with a source, has one."), nil
	}
	prov[importFieldSourceStatus] = status
	changed, err := d.reviseProvenance(ctx, key, mscope, rootID, prov)
	if err != nil {
		return errFrom(markSourceOp+": "+err.Error(), err), nil
	}
	if status == "present" {
		err = d.removeTagRows(ctx, key, "document_tags", "document_id", docID, []string{sourceRemovedTag})
	} else {
		err = d.addTagRows(ctx, key, "document_tags", "document_id", docID, []string{sourceRemovedTag})
	}
	if err != nil {
		return errFrom(markSourceOp+": tags: "+err.Error(), err), nil
	}
	tags, err := d.listDocumentTags(ctx, key, docID)
	if err != nil {
		return errFrom(markSourceOp+": "+err.Error(), err), nil
	}
	return jsonResult(map[string]any{"document_id": docID, "source_status": status, "changed": changed, "tags": tags})
}
