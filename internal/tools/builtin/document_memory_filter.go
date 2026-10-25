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
	// A TENANT document's bodies are not in the keyspace a tenant-scope Memory
	// call reads: they are written under the SQL Memory scope id (the tenant),
	// while the Memory tool's tenant scope id is empty (see resolveScope's tenant
	// branch and writeBodyAs). A filter there could never match a document the
	// search can return, so it is refused rather than resolved.
	if scope == store.MemoryScopeTenant {
		return memrank.DocumentFilterResult{}, &documentFilterRefusal{errValidation(
			op+": document filters are not available on a tenant-scope Memory call — tenant documents are not searchable through this scope",
			"Drop the document filter, or search the tenant's documents with the Document tool's search (scope tenant), which takes the same filters.")}
	}
	key, mscope, err := d.resolveScope(ctx, string(scope))
	if err != nil {
		return memrank.DocumentFilterResult{}, &documentFilterRefusal{errValidation(
			op+": document filter: "+err.Error(), "Drop the document filter, or search a scope you may read documents in.")}
	}
	if mscope != scope || key.ScopeID != scopeID {
		// Both tools derive the scope id from the same run identity; a mismatch
		// means the two would read different trees, so nothing is resolved.
		return memrank.DocumentFilterResult{}, fmt.Errorf("document filter: memory scope %s/%q is not document scope %s/%q", scope, scopeID, mscope, key.ScopeID)
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
