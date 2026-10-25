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
// run on ctx. The scope was already granted by the Memory tool's own gate; this
// only narrows what that search may return, so it asks for no further grant.
// Store, SqlMem and the rest are read here, at use, because boot hands this tool
// to the Memory tool before binding them.
func (d *Document) ResolveDocumentFilter(ctx context.Context, op string, scope store.MemoryScope, scopeID string, f memrank.DocumentFilter) (memrank.DocumentFilterResult, error) {
	if d == nil || d.Store == nil || d.SqlMem == nil {
		return memrank.DocumentFilterResult{}, &documentFilterRefusal{errBusiness(
			op+": documents not configured on this server, so under_paths, exclude_under_paths, document_ids and exclude_document_ids cannot be applied",
			"Search without the document filter.")}
	}
	key, err := memoryScopeDocKey(ctx, scope, scopeID)
	if err != nil {
		return memrank.DocumentFilterResult{}, err
	}
	if err := d.ensureSchema(ctx, key); err != nil {
		return memrank.DocumentFilterResult{}, err
	}
	set, refusal, err := d.resolveSearchFilter(ctx, key, op, docInput{
		UnderPaths:         f.UnderPaths,
		ExcludeUnderPaths:  f.ExcludeUnderPaths,
		DocumentIDs:        f.DocumentIDs,
		ExcludeDocumentIDs: f.ExcludeDocumentIDs,
	})
	if err != nil {
		return memrank.DocumentFilterResult{}, err
	}
	if refusal != nil {
		return memrank.DocumentFilterResult{}, &documentFilterRefusal{*refusal}
	}
	ap := set.echo.Applied
	return memrank.DocumentFilterResult{
		Applied: memrank.DocumentFilter{
			UnderPaths:         ap.UnderPaths,
			ExcludeUnderPaths:  ap.ExcludeUnderPaths,
			DocumentIDs:        ap.DocumentIDs,
			ExcludeDocumentIDs: ap.ExcludeDocumentIDs,
		},
		IDs:              set.ids,
		ExcludeIDs:       set.excludeIDs,
		MatchedDocuments: set.echo.MatchedDocuments,
		MatchedChunks:    set.echo.MatchedChunks,
		Inverted:         set.echo.Inverted,
	}, nil
}

// memoryScopeDocKey is the SQL Memory scope that holds the documents of a memory
// scope. The two planes key the tenant scope differently (see resolveScope's
// tenant branch): memory's scope id is empty, SQL Memory's is the tenant.
func memoryScopeDocKey(ctx context.Context, scope store.MemoryScope, scopeID string) (sqlmem.ScopeKey, error) {
	tenant := sqlScopeTenant(ctx)
	switch scope {
	case store.MemoryScopeAgent, store.MemoryScopeUser:
		if scopeID == "" {
			return sqlmem.ScopeKey{}, fmt.Errorf("document filter: scope %s has no scope id", scope)
		}
		return sqlmem.ScopeKey{Tenant: tenant, Scope: string(scope), ScopeID: scopeID}, nil
	case store.MemoryScopeTenant:
		return sqlmem.ScopeKey{Tenant: tenant, Scope: "tenant", ScopeID: tenant}, nil
	default:
		return sqlmem.ScopeKey{}, fmt.Errorf("document filter: scope %q holds no documents", scope)
	}
}
