package builtin

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FV §1–§2: what the Document tool tells the change feed.
//
// A chunk-body write's change row is made by the CDC store decorator, which
// knows keys, not documents. The reader feed decides access per document, so
// every write and delete of a `doc.chunk:` key stamps its document on the ctx
// it hands the store (store.WithChangeDocument), at the call. The writer census
// (internal/store/cdc/writer_census_test.go) holds the module to it.
//
// Statements live in SQL Memory, which the decorator never sees, so the
// statement paths append their own `document.edges.changed` rows — through the
// decorator's RecordDocumentChange, which only exists when capture is on.

// changeCapturer is the CDC decorator's "writes through me land in the feed".
type changeCapturer interface{ CapturesChanges() bool }

// documentChangeRecorder is the CDC decorator's append for a change no keyed
// write produces.
type documentChangeRecorder interface {
	RecordDocumentChange(ctx context.Context, ch store.MemoryChange)
}

// capturesChanges reports whether this tool's writes land in the change feed.
// Asked of the store, as the feed route asks it: the decorator is in the path
// exactly when capture is on. With it off, nothing below costs a query.
func (d *Document) capturesChanges() bool {
	c, ok := d.Store.(changeCapturer)
	return ok && c.CapturesChanges()
}

// bodyDocument is the document a chunk-body write belongs to: the one the
// caller stamped, or — when capture is on and it did not — the chunk row's.
// Every caller of putBody has inserted the chunk row before writing its body,
// so the lookup finds it; a bulk writer (import, create) stamps ctx itself and
// pays nothing. "" when the chunk cannot be placed (the row is unrecorded).
func (d *Document) bodyDocument(ctx context.Context, key sqlmem.ScopeKey, chunkID string) string {
	if id := store.ChangeDocumentFrom(ctx); id != "" {
		return id
	}
	if !d.capturesChanges() {
		return ""
	}
	return d.chunkDocumentID(ctx, key, chunkID)
}

// chunkDocumentID reads the document a chunk row belongs to, "" when the row is
// gone or cannot be read (a change row then names no document, which the reader
// feed never sends — it never names the wrong one).
func (d *Document) chunkDocumentID(ctx context.Context, key sqlmem.ScopeKey, chunkID string) string {
	res, err := d.query(ctx, key, `SELECT document_id FROM chunks WHERE id = ? LIMIT 1`, chunkID)
	if err != nil || len(res.Rows) == 0 {
		return ""
	}
	return asStr(res.Rows[0][0])
}

// deleteBody removes a chunk's body row, filing the change under docID. The one
// place a Document path deletes a body, so the stamp cannot be forgotten.
func (d *Document) deleteBody(ctx context.Context, docID, tenant string, mscope store.MemoryScope, scopeID, chunkID string) (bool, error) {
	return d.Store.MemoryDelete(store.WithChangeDocument(ctx, docID), tenant, mscope, scopeID, chunkBodyKey(chunkID))
}

// ChunkBodyChangeCtx is ctx stamped with the document a raw k/v delete of key
// touches — the Memory tool's and the memory route's delete of a consolidated
// fact's body row, the one chunk body a caller may remove by key. ctx unchanged
// when key is not a chunk body, capture is off, or the chunk cannot be placed.
// tenant and scopeID are the Memory plane's (docMemScopeID's) for scope.
func (d *Document) ChunkBodyChangeCtx(ctx context.Context, tenant string, scope store.MemoryScope, scopeID, key string) context.Context {
	chunkID, ok := strings.CutPrefix(key, chunkBodyKeyPrefix)
	if !ok || chunkID == "" || d == nil || d.Store == nil || d.SqlMem == nil || !d.capturesChanges() {
		return ctx
	}
	sqlTenant := sqlScopeTenantValue(tenant)
	skey := sqlmem.ScopeKey{Tenant: sqlTenant, Scope: string(scope), ScopeID: scopeID}
	if scope == store.MemoryScopeTenant {
		skey.ScopeID = sqlTenant
	}
	// Never provision a scope to answer this: one with no document tables holds
	// no chunk to place.
	if absent, err := d.documentTablesAbsent(ctx, skey); err != nil || absent {
		return ctx
	}
	if docID := d.chunkDocumentID(ctx, skey, chunkID); docID != "" {
		return store.WithChangeDocument(ctx, docID)
	}
	return ctx
}

// recordEdgesChanged appends one document.edges.changed row per endpoint
// document of a statement (RFC FV §2): one for a link inside a document, two
// for a link across documents, each naming that document's endpoint chunk.
// Best-effort like every change row; nothing when capture is off.
func (d *Document) recordEdgesChanged(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, from, to string) {
	rec, ok := d.Store.(documentChangeRecorder)
	if !ok {
		return
	}
	seen := map[string]bool{}
	for _, chunkID := range []string{from, to} {
		if chunkID == "" {
			continue
		}
		docID := d.chunkDocumentID(ctx, key, chunkID)
		if docID == "" || seen[docID] {
			continue
		}
		seen[docID] = true
		rec.RecordDocumentChange(ctx, store.MemoryChange{
			TenantID:   direntTenant(ctx),
			Type:       store.DocumentEdgesChanged,
			Scope:      mscope,
			ScopeID:    docMemScopeID(mscope, key),
			ChunkID:    chunkID,
			DocumentID: docID,
		})
	}
}

// announceChange appends a document.chunk.updated row for an op that changed a
// chunk or a document without rewriting a chunk body — a retitle, a new status
// or type, a move, a reorder, a new Path name. No keyed write happens, so the
// decorator sees nothing; a viewer that labels by title, colours by status and
// files by path would otherwise never hear of it. chunkID may be "" (a change
// to the document as a whole); docID "" is read from the chunk. Only after the
// op succeeded; best-effort; nothing when capture is off. An op that also
// rewrote a body announces twice, which the reader feed coalesces.
func (d *Document) announceChange(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID, chunkID string, res tools.Result, err error) {
	if err != nil || res.IsError {
		return
	}
	rec, ok := d.Store.(documentChangeRecorder)
	if !ok {
		return
	}
	if docID == "" && chunkID != "" {
		docID = d.chunkDocumentID(ctx, key, chunkID)
	}
	if docID == "" {
		return
	}
	rec.RecordDocumentChange(ctx, store.MemoryChange{
		TenantID:   direntTenant(ctx),
		Type:       store.DocumentChangeUpdated,
		Scope:      mscope,
		ScopeID:    docMemScopeID(mscope, key),
		ChunkID:    chunkID,
		DocumentID: docID,
	})
}
