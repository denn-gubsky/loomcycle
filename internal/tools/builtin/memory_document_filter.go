package builtin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/memory/backends/inprocess"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Document filters on Memory op=search and op=recall.
//
// Chunk bodies share the memory keyspace, so a Memory search returns them beside
// facts and notes; a caller that must keep one part of the scope's documents out
// (the gates' own reports, say) needs the same path and document-id filters
// Document op=search takes. The structure they name lives in SQL Memory, which this
// tool does not reach, so the Document tool resolves them (DocumentFilters) to chunk
// ids, and those travel on the SearchQuery exactly as a Document search's do —
// applied in every leg before its LIMIT, never as a post-filter.
//
// They narrow DOCUMENTS ONLY. In a search that also returns facts and notes, those
// pass the filter untouched (IDsPrefixes confines the lists to document rows); a
// search that cannot return documents at all is refused rather than run with a
// filter it would silently ignore.

// documentFilter is the call's document-level filter.
func (in memoryInput) documentFilter() memrank.DocumentFilter {
	return memrank.DocumentFilter{
		UnderPaths:         in.UnderPaths,
		ExcludeUnderPaths:  in.ExcludeUnderPaths,
		DocumentIDs:        in.DocumentIDs,
		ExcludeDocumentIDs: in.ExcludeDocumentIDs,
	}
}

// MemoryDocFilterEcho is the `filter` block of a filtered search or recall — the
// same shape Document op=search reports, minus the chunk-level fields this tool
// does not take. Present only when a filter was given. Exported for the off-run
// POST /v1/_memory/search, which reports the same block.
type MemoryDocFilterEcho struct {
	Applied          memrank.DocumentFilter `json:"applied"`
	MatchedDocuments int                    `json:"matched_documents"`
	MatchedChunks    int                    `json:"matched_chunks"`
	Inverted         bool                   `json:"inverted"`
}

// MemoryDocFilterPlan is a resolved filter, ready to put on a query.
type MemoryDocFilterPlan struct {
	echo            MemoryDocFilterEcho
	ids, excludeIDs []string
	// noDocuments: the filter matched no chunk. An empty id list means "every
	// row", so this cannot travel as a list — the documents are dropped instead.
	noDocuments bool
	// documentsOnly: the query returns nothing BUT documents, so with none matched
	// there is nothing to search for.
	documentsOnly bool
}

// Echo is the response's `filter` block.
func (p *MemoryDocFilterPlan) Echo() MemoryDocFilterEcho { return p.echo }

// NothingToSearch reports that the filter matched no document and the query
// can return nothing else: answer empty, without embedding or a store call.
func (p *MemoryDocFilterPlan) NothingToSearch() bool { return p.noDocuments && p.documentsOnly }

// filterSelectors names the document selectors a call used, for a refusal.
func filterSelectors(f memrank.DocumentFilter) string {
	var used []string
	for _, s := range []struct {
		name string
		vals []string
	}{
		{"under_paths", f.UnderPaths}, {"exclude_under_paths", f.ExcludeUnderPaths},
		{"document_ids", f.DocumentIDs}, {"exclude_document_ids", f.ExcludeDocumentIDs},
	} {
		if !(memrank.DocumentFilter{UnderPaths: s.vals}).IsZero() {
			used = append(used, s.name)
		}
	}
	return strings.Join(used, ", ")
}

// planDocumentFilter resolves f for a query that will run as q. why describes q's
// selector for the refusal when q cannot return documents. A refusal comes back as
// the *tools.Result.
func (m *Memory) planDocumentFilter(ctx context.Context, op string, scope store.MemoryScope, scopeID string,
	f memrank.DocumentFilter, q memrank.SearchQuery, why string) (*MemoryDocFilterPlan, *tools.Result) {
	// Read HERE, at use: boot assigns the resolver before it is fully bound.
	resolver := m.DocumentFilters
	// The documents a filter names are this server's. A memory backend that serves
	// the search from elsewhere would receive an id list it cannot interpret and
	// drop it — so the filter is refused there rather than silently not applied.
	// Asked after the two refusals PlanMemoryDocumentFilter makes first, so a call
	// that would be refused for its selector or a missing resolver still is.
	if q.CanReturnDocuments() && resolver != nil {
		if _, local := m.backend(ctx).(*inprocess.Backend); !local {
			sel := filterSelectors(f)
			r := errBusiness(op+": "+sel+" applies only to this server's own memory, and this agent's memory is served by another backend",
				"Search without "+sel+", or use the Document tool's search for documents on this server.")
			return nil, &r
		}
	}
	return PlanMemoryDocumentFilter(ctx, resolver, op, scope, scopeID, f, q, why)
}

// PlanMemoryDocumentFilter resolves the document filter f of a memory search or
// recall that will run as q, in the memory scope (scope, scopeID) of the caller
// on ctx, through resolver (nil: documents are not configured, and the filter is
// refused). why describes q's selector for the refusal when q cannot return
// documents. A refusal comes back as the *tools.Result, already classified.
//
// It is the in-run Memory tool's step and the off-run POST /v1/_memory/search's,
// so the two accept, refuse and resolve a filter identically. The caller owns
// the identity on ctx and must have authorized the scope; the resolver applies
// the Document tool's own scope gate and access-control narrowing on top.
func PlanMemoryDocumentFilter(ctx context.Context, resolver memrank.DocumentFilterResolver, op string,
	scope store.MemoryScope, scopeID string, f memrank.DocumentFilter, q memrank.SearchQuery, why string) (*MemoryDocFilterPlan, *tools.Result) {
	sel := filterSelectors(f)
	if !q.CanReturnDocuments() {
		r := errValidation(fmt.Sprintf("%s: %s narrows the documents a search returns, and %s cannot return documents", op, sel, why),
			"Add documents to sources (a search with no sources returns them too), or drop "+sel+".")
		return nil, &r
	}
	if resolver == nil {
		r := errBusiness(op+": documents not configured on this server, so "+sel+" cannot be applied",
			"Search without "+sel+".")
		return nil, &r
	}
	res, err := resolver.ResolveDocumentFilter(ctx, op, scope, scopeID, f)
	if err != nil {
		var rf *documentFilterRefusal
		if errors.As(err, &rf) {
			return nil, &rf.res
		}
		r := errFrom(fmt.Sprintf("%s: document filter: %s", op, err), err)
		return nil, &r
	}
	plan := &MemoryDocFilterPlan{
		echo: MemoryDocFilterEcho{Applied: res.Applied, MatchedDocuments: res.MatchedDocuments,
			MatchedChunks: res.MatchedChunks, Inverted: res.Inverted},
		ids: res.IDs, excludeIDs: res.ExcludeIDs,
		noDocuments: res.MatchedChunks == 0,
	}
	plan.documentsOnly = strings.HasPrefix(q.Prefix, memrank.DocumentChunkKeyPrefix) ||
		(len(q.Sources) == 1 && q.Sources[0] == memrank.SourceDocuments)
	return plan, nil
}

// Apply puts the plan on q. With no document matched, a query that also returns
// facts and notes keeps exactly those — the one selector that excludes the
// document class and its units — rather than sending an empty list, which would
// mean "every document".
func (p *MemoryDocFilterPlan) Apply(q *memrank.SearchQuery) {
	if p.noDocuments {
		q.Sources = []memrank.Source{memrank.SourceFacts, memrank.SourceNotes}
		return
	}
	q.IDs, q.ExcludeIDs = p.ids, p.excludeIDs
}

// MemorySourcesWhy describes a search's selector for a refusal (the why of
// PlanMemoryDocumentFilter).
func MemorySourcesWhy(prefix string, sources []memrank.Source) string {
	if prefix != "" && !(memrank.SearchQuery{Prefix: prefix}).CanReturnDocuments() {
		return fmt.Sprintf("prefix %q", prefix)
	}
	names := make([]string, len(sources))
	for i, s := range sources {
		names[i] = string(s)
	}
	return "sources [" + strings.Join(names, ", ") + "]"
}
