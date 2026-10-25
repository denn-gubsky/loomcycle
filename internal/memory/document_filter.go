package memory

import (
	"context"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// Document filters on a Memory search or recall.
//
// Chunk bodies share the memory keyspace, so a Memory search can return them;
// the structure that says WHICH document a chunk belongs to — its path, its
// document id — lives in SQL Memory, which this package does not reach and must
// not grow a dependency on. So the resolution happens where that structure is
// owned (the Document tool) and crosses over through this interface as the same
// chunk-id lists SearchQuery.IDs / ExcludeIDs already carry.
//
// It lives here, beside SearchQuery, because its output is exactly SearchQuery's
// input: the resolver hands back id segments and the caller assigns them, with
// nothing translated in between.

// DocumentFilterResolver turns document-level selectors into the id segments a
// SearchQuery restricts on. Implemented by the Document tool. A caller that
// holds none refuses any document filter rather than ignoring it.
type DocumentFilterResolver interface {
	// ResolveDocumentFilter resolves f in the given memory scope, as the run on
	// ctx sees it. op names the calling op in a refusal. A refusal the caller can
	// act on (a malformed path, a filter too wide to send, documents not
	// configured) is returned as the implementation's own error type; any other
	// error is a fault.
	ResolveDocumentFilter(ctx context.Context, op string, scope store.MemoryScope, scopeID string, f DocumentFilter) (DocumentFilterResult, error)
}

// DocumentFilter is the document-level half of a structural filter: paths and
// document ids. The chunk-level predicates (type, status, tags, validity)
// describe chunks, and stay on Document op=search.
type DocumentFilter struct {
	UnderPaths         []string `json:"under_paths,omitempty"`
	ExcludeUnderPaths  []string `json:"exclude_under_paths,omitempty"`
	DocumentIDs        []string `json:"document_ids,omitempty"`
	ExcludeDocumentIDs []string `json:"exclude_document_ids,omitempty"`
}

// IsZero reports whether the filter names nothing. Blank entries do not count:
// a list of empty strings is no filter, and must take the unfiltered path.
func (f DocumentFilter) IsZero() bool {
	for _, l := range [][]string{f.UnderPaths, f.ExcludeUnderPaths, f.DocumentIDs, f.ExcludeDocumentIDs} {
		for _, v := range l {
			if strings.TrimSpace(v) != "" {
				return false
			}
		}
	}
	return true
}

// DocumentFilterResult is a resolved filter. At most one of IDs / ExcludeIDs is
// set; neither is set either when the filter matched nothing (MatchedChunks 0)
// or when it matched the whole scope.
type DocumentFilterResult struct {
	// Applied is the filter as resolved: paths canonicalised, duplicates dropped.
	Applied DocumentFilter
	// IDs / ExcludeIDs are chunk ids, for SearchQuery.IDs / ExcludeIDs.
	IDs, ExcludeIDs []string
	// MatchedDocuments / MatchedChunks are what the filter resolved to.
	MatchedDocuments, MatchedChunks int
	// Inverted reports that the allow set was sent as its complement.
	Inverted bool
}
