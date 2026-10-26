package builtin

import (
	"context"
	"fmt"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The Document tool resolves the document filters on a Memory search or recall
// (memrank.DocumentFilterResolver). It owns the structure the filter names —
// the Path tree and the chunk tables — so the Memory tool asks it rather than
// reaching into SQL Memory itself. The resolution is document_search_filter.go's,
// called with only the document-level lists set, so a path or a document id means
// exactly what it means on Document op=search, access-control narrowing included.

var _ memrank.DocumentFilterResolver = (*Document)(nil)

// documentFilterRefusal is a refusal the caller can act on — a malformed path,
// a filter too wide to send, documents not configured — carried across the
// resolver interface with its failure category, so the Memory tool renders it
// exactly as the Document tool would have.
type documentFilterRefusal struct{ res tools.Result }

func (e *documentFilterRefusal) Error() string { return e.res.Text }

// ResolveDocumentFilter resolves f in the memory scope (scope, scopeID) of the
// run on ctx. Resolving READS SQL Memory — the chunk tables — and the echo
// discloses counts from it, so it goes through the Document tool's own scope gate
// (resolveScope: the grants and the isolated-member confinement) exactly as
// Document op=search does, and it never provisions anything: a scope whose SQL
// Memory or document tables do not exist yet holds no documents, so it matches
// nothing. Store, SqlMem and the rest are read here, at use, because boot hands
// this tool to the Memory tool before binding them.
func (d *Document) ResolveDocumentFilter(ctx context.Context, op string, scope store.MemoryScope, scopeID string, f memrank.DocumentFilter) (memrank.DocumentFilterResult, error) {
	if d == nil || d.Store == nil || d.SqlMem == nil {
		return memrank.DocumentFilterResult{}, &documentFilterRefusal{errBusiness(
			op+": documents not configured on this server, so under_paths, exclude_under_paths, document_ids and exclude_document_ids cannot be applied",
			"Search without the document filter.")}
	}
	// The scope gate first, so a refusal says what it would say on Document
	// op=search, in the same category: a missing grant or an isolated member's
	// confinement is a permission refusal an operator can lift, not a malformed
	// call.
	key, mscope, err := d.resolveScope(ctx, string(scope))
	if err != nil {
		return memrank.DocumentFilterResult{}, &documentFilterRefusal{docFail(op+": document filter: ", err)}
	}
	if mscope != scope || docMemScopeID(mscope, key) != scopeID {
		// Both tools derive the scope id from the same run identity; a mismatch
		// means the two would read different trees, so nothing is resolved.
		return memrank.DocumentFilterResult{}, fmt.Errorf("document filter: memory scope %s/%q is not document scope %s/%q", scope, scopeID, mscope, docMemScopeID(mscope, key))
	}
	empty, err := d.documentTablesAbsent(ctx, key)
	if err != nil {
		return memrank.DocumentFilterResult{}, err
	}
	in := docInput{
		UnderPaths:         f.UnderPaths,
		ExcludeUnderPaths:  f.ExcludeUnderPaths,
		DocumentIDs:        f.DocumentIDs,
		ExcludeDocumentIDs: f.ExcludeDocumentIDs,
	}
	if empty {
		ap, refusal := normaliseSearchFilter(op, in)
		if refusal != nil {
			return memrank.DocumentFilterResult{}, &documentFilterRefusal{*refusal}
		}
		return memrank.DocumentFilterResult{Applied: appliedDocumentFilter(ap)}, nil
	}
	set, refusal, err := d.resolveSearchFilter(ctx, key, op, in)
	if err != nil {
		return memrank.DocumentFilterResult{}, err
	}
	if refusal != nil {
		return memrank.DocumentFilterResult{}, &documentFilterRefusal{*refusal}
	}
	return memrank.DocumentFilterResult{
		Applied:          appliedDocumentFilter(set.echo.Applied),
		IDs:              set.ids,
		ExcludeIDs:       set.excludeIDs,
		MatchedDocuments: set.echo.MatchedDocuments,
		MatchedChunks:    set.echo.MatchedChunks,
		Inverted:         set.echo.Inverted,
	}, nil
}

func appliedDocumentFilter(ap searchFilterApplied) memrank.DocumentFilter {
	return memrank.DocumentFilter{
		UnderPaths:         ap.UnderPaths,
		ExcludeUnderPaths:  ap.ExcludeUnderPaths,
		DocumentIDs:        ap.DocumentIDs,
		ExcludeDocumentIDs: ap.ExcludeDocumentIDs,
	}
}

// documentTablesAbsent reports whether this scope has no document tables yet —
// no SQL Memory scope at all, or one created by other SQL ops without them. It
// creates nothing: ScopeExists never provisions, and the probe goes to the
// Manager directly, not through query(), whose missing-table retry re-provisions.
func (d *Document) documentTablesAbsent(ctx context.Context, key sqlmem.ScopeKey) (bool, error) {
	ok, err := d.SqlMem.ScopeExists(ctx, key)
	if err != nil || !ok {
		return !ok, err
	}
	_, err = d.SqlMem.Query(ctx, key, "SELECT count(*) FROM chunks WHERE 1=0", nil)
	if schemaMissing(err) {
		return true, nil
	}
	return false, err
}

var _ documentHitReader = (*Document)(nil)

// ReadableDocumentChunks answers the Memory tool's access-control question for
// the document hits of a search or recall (memory_document_acl.go): which of
// chunkIDs, in the memory scope (scope, scopeID), the caller on ctx may read.
// The answer is Document op=search's listing filter: a chunk is readable when it
// belongs to a document and the caller may read that document at every name it
// is filed under. A chunk with no document row is not, as on Document op=search.
//
// The scope goes through resolveScope, as every Document read does, and nothing
// is provisioned: a scope with no document tables holds no readable chunk.
func (d *Document) ReadableDocumentChunks(ctx context.Context, scope store.MemoryScope, scopeID string, chunkIDs []string) (map[string]bool, error) {
	out := map[string]bool{}
	if d == nil || d.Store == nil || d.SqlMem == nil || len(chunkIDs) == 0 {
		return out, nil
	}
	key, mscope, err := d.resolveScope(ctx, string(scope))
	if err != nil {
		return nil, err
	}
	if mscope != scope || docMemScopeID(mscope, key) != scopeID {
		return nil, fmt.Errorf("document access check: memory scope %s/%q is not document scope %s/%q", scope, scopeID, mscope, docMemScopeID(mscope, key))
	}
	empty, err := d.documentTablesAbsent(ctx, key)
	if err != nil || empty {
		return out, err
	}
	meta, err := d.chunkMetaByIDs(ctx, key, chunkIDs)
	if err != nil {
		return nil, err
	}
	acl := d.newDocACL(ctx, key)
	readable := map[string]bool{}
	for _, id := range chunkIDs {
		docID := meta[id].documentID
		if docID == "" {
			continue
		}
		ok, seen := readable[docID]
		if !seen {
			ok = true
			if acl != nil {
				if ok, err = acl.readable(ctx, docID); err != nil {
					return nil, err
				}
			}
			readable[docID] = ok
		}
		if ok {
			out[id] = true
		}
	}
	return out, nil
}

// ResolveDocumentsScope answers the Memory tool's question for a search or
// recall that also reads the documents of ANOTHER scope (documents_scope): may the
// run on ctx search the documents of scope, and at which memory scope id do they
// live. It is Document op=search's own scope gate — resolveScope: the grants
// (for tenant, memory_scopes AND sql_scopes), the isolated-member confinement,
// the user id a user scope needs — so the Memory tool can never reach a scope's
// documents that Document op=search would refuse. It provisions nothing. A
// refusal comes back as a *documentFilterRefusal carrying its category.
func (d *Document) ResolveDocumentsScope(ctx context.Context, op string, scope store.MemoryScope) (string, error) {
	if d == nil || d.Store == nil || d.SqlMem == nil {
		return "", &documentFilterRefusal{errBusiness(op+": documents are not configured on this server, so documents_scope cannot be searched",
			"Search without documents_scope.")}
	}
	key, mscope, err := d.resolveScope(ctx, string(scope))
	if err != nil {
		return "", &documentFilterRefusal{docFail(op+": documents_scope: ", err)}
	}
	if mscope != scope {
		return "", fmt.Errorf("documents_scope: memory scope %s is not document scope %s", scope, mscope)
	}
	return docMemScopeID(mscope, key), nil
}
