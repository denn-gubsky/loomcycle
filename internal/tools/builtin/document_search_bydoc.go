package builtin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Filtered search by DOCUMENT id.
//
// A structural filter resolves to the chunk ids a search may return, and those ids
// are bound into every retrieval leg (document_search_filter.go). A filter that names
// only documents — paths and document ids — selects whole documents, so the same set
// can be sent as a few hundred document ids instead of tens of thousands of chunk
// ids, IF every document row of the scope carries its document id on its embedding
// (MemoryEmbedding.DocumentID). Where any row lacks one (a row embedded before the
// column existed, or by a writer that did not know its document), the search falls
// back to the chunk-id list: a missing value is slow, never wrong.
//
// THE COLUMN IS A COPY, so its writers are the risk. A chunk's document changes in
// exactly one place — fact homing (document_fact_home.go), which re-files a subject
// and its facts into the subject's own document; move_chunk refuses a parent in
// another document. Homing clears the copy before it moves the rows and writes it
// after, so a failure between the two leaves NULLs (the fallback), never a stale
// document id. Every embedding writer of a document row derives the id from the
// chunks table at write time (rowDocumentID), so nothing trusts a caller for it.

// documentRowPrefixes are the two key namespaces that hold Document material in the
// memory keyspace: chunk bodies and their derived search units.
var documentRowPrefixes = []string{chunkBodyKeyPrefix, memrank.DocumentUnitKeyPrefix}

// rowDocumentID is the document a Document memory row belongs to, read from the
// chunks table — "" for a key that is not a document row, a chunk that is gone, or a
// read that failed. The empty answer stores nothing, which the search reads as
// "fall back", so a fault here costs speed and never correctness.
func (d *Document) rowDocumentID(ctx context.Context, key sqlmem.ScopeKey, rowKey string) string {
	chunkID := ChunkIDFromBodyKey(rowKey)
	if chunkID == "" {
		id, isUnit := memrank.UnitChunkID(rowKey)
		if !isUnit {
			return ""
		}
		chunkID = id
	}
	if d.SqlMem == nil {
		return ""
	}
	res, err := d.query(ctx, key, `SELECT document_id FROM chunks WHERE id = ?`, chunkID)
	if err != nil || len(res.Rows) == 0 {
		return ""
	}
	return asStr(res.Rows[0][0])
}

// setDocumentAttribution writes docID ("" clears it) on the stored embeddings of
// these chunks and of their derived units. A store without a vector index has no
// embeddings to mark, which is not a failure.
func (d *Document) setDocumentAttribution(ctx context.Context, tenant string, mscope store.MemoryScope, scopeID string, chunkIDs []string, docID string) error {
	const batch = 5000
	for start := 0; start < len(chunkIDs); start += batch {
		ids := chunkIDs[start:min(start+batch, len(chunkIDs))]
		for _, prefix := range documentRowPrefixes {
			if _, err := d.Store.MemoryEmbedSetDocumentID(ctx, tenant, mscope, scopeID, prefix, ids, docID); err != nil {
				if errors.Is(err, store.ErrVectorUnsupported) {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

// attributeDocument writes docID on the embeddings of every chunk the document now
// holds — read from the chunks table, so whatever the move carried along is covered.
func (d *Document) attributeDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, docID string) error {
	ids, _, err := d.chunkIDsOfDocuments(ctx, key, []string{docID})
	if err != nil {
		return err
	}
	return d.setDocumentAttribution(ctx, direntTenant(ctx), mscope, key.ScopeID, ids, docID)
}

// resolveStoreFilter is resolveSearchFilter for the two ops that hand the set to the
// main store (search, related): it sends document ids where the filter and the
// scope allow, and the chunk-id list otherwise.
func (d *Document) resolveStoreFilter(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op string, in docInput) (*searchFilterSet, *tools.Result, error) {
	set, refusal, ok, err := d.resolveByDocument(ctx, key, mscope, op, in)
	if err != nil || refusal != nil || ok {
		return set, refusal, err
	}
	return d.resolveSearchFilter(ctx, key, op, in)
}

// resolveByDocument resolves a filter to document-id lists. ok is false when the
// filter names anything below the document (type, status, tags, validity) or when
// some document row of the scope has no stored document id; the caller then takes
// the chunk-id path. The echo's counts are the chunk-id path's: the same filter
// reports the same matches whichever list travels.
func (d *Document) resolveByDocument(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op string, in docInput) (*searchFilterSet, *tools.Result, bool, error) {
	ap, refusal := normaliseSearchFilter(op, in)
	if refusal != nil {
		return nil, refusal, false, nil
	}
	// Document-level only. The chunk predicate is the one the chunk-id path builds,
	// so a predicate added there later disqualifies this path without a second list.
	var scratch searchFilterEcho
	if where, _ := d.chunkFilterPredicate(ctx, ap, &scratch); where != "" {
		return nil, nil, false, nil
	}
	if d.Store == nil {
		return nil, nil, false, nil
	}
	missing, err := d.Store.MemoryEmbedMissingDocumentID(ctx, direntTenant(ctx), mscope, key.ScopeID, documentRowPrefixes)
	if err != nil {
		// The chunk-id path needs no column, so a probe fault only costs the shortcut.
		slog.WarnContext(ctx, "document: search filter could not check document ids; using chunk ids", "err", err)
		return nil, nil, false, nil
	}
	if missing {
		return nil, nil, false, nil
	}

	set := &searchFilterSet{echo: searchFilterEcho{Applied: ap}}
	include, exclude, err := d.documentLevelSets(ctx, key, ap)
	if err != nil {
		return nil, nil, false, err
	}
	if include != nil && len(include) == 0 {
		return set, nil, true, nil
	}
	// ACCESS CONTROL, as on the chunk-id path: for a caller an access-control set
	// limits, the list is narrowed to readable documents and always sent as an
	// allowlist, so no id of a denied document reaches the store and no count moves
	// with one.
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return nil, nil, false, err
	}
	maxIDs := d.searchFilterMaxIDs()
	if include == nil && !limited {
		// Exclusion only: a denylist, counted against the scope like the chunk path.
		scopeChunks, scopeDocs, err := d.scopeChunkCounts(ctx, key)
		if err != nil {
			return nil, nil, false, err
		}
		deny := docIDsOf(exclude)
		chunks, docs, err := d.documentChunkCounts(ctx, key, deny)
		if err != nil {
			return nil, nil, false, err
		}
		set.echo.MatchedChunks, set.echo.MatchedDocuments = scopeChunks-chunks, scopeDocs-docs
		if set.echo.MatchedChunks <= 0 {
			set.echo.MatchedChunks, set.echo.MatchedDocuments = 0, 0
			return set, nil, true, nil
		}
		if len(deny) > maxIDs {
			return nil, filterTooWideDocuments(op, len(deny), maxIDs), false, nil
		}
		set.excludeDocumentIDs = deny
		return set, nil, true, nil
	}
	var candidates []string
	if include != nil {
		candidates = docIDsOf(include)
	} else if candidates, err = d.allDocumentIDs(ctx, key); err != nil {
		return nil, nil, false, err
	}
	allow := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if exclude[id] {
			continue
		}
		if limited {
			ok, err := acl.readable(ctx, id)
			if err != nil {
				return nil, nil, false, err
			}
			if !ok {
				continue
			}
		}
		allow = append(allow, id)
	}
	chunks, docs, err := d.documentChunkCounts(ctx, key, allow)
	if err != nil {
		return nil, nil, false, err
	}
	set.echo.MatchedChunks, set.echo.MatchedDocuments = chunks, docs
	if chunks == 0 {
		return set, nil, true, nil
	}
	if len(allow) > maxIDs {
		return nil, filterTooWideDocuments(op, len(allow), maxIDs), false, nil
	}
	set.documentIDs = allow
	return set, nil, true, nil
}

// documentLevelSets resolves the path and document-id arguments to an include set
// (nil = every document; empty = nothing) and an exclude set, with the exclusions
// already taken out of the include set. The same resolution the chunk-id path runs.
func (d *Document) documentLevelSets(ctx context.Context, key sqlmem.ScopeKey, ap searchFilterApplied) (include, exclude map[string]bool, err error) {
	if len(ap.UnderPaths) > 0 || len(ap.DocumentIDs) > 0 {
		include = map[string]bool{}
		for _, p := range ap.UnderPaths {
			ids, err := d.documentsAtOrUnder(ctx, key, p)
			if err != nil {
				return nil, nil, err
			}
			for _, id := range ids {
				include[id] = true
			}
		}
		for _, id := range ap.DocumentIDs {
			include[id] = true
		}
	}
	exclude = map[string]bool{}
	for _, p := range ap.ExcludeUnderPaths {
		ids, err := d.documentsAtOrUnder(ctx, key, p)
		if err != nil {
			return nil, nil, err
		}
		for _, id := range ids {
			exclude[id] = true
		}
	}
	for _, id := range ap.ExcludeDocumentIDs {
		exclude[id] = true
	}
	for id := range exclude {
		delete(include, id)
	}
	return include, exclude, nil
}

// documentChunkCounts is how many chunks these documents hold, and how many of the
// documents hold any — an unknown or foreign id simply counts nothing.
func (d *Document) documentChunkCounts(ctx context.Context, key sqlmem.ScopeKey, docIDs []string) (chunks, docs int, err error) {
	for start := 0; start < len(docIDs); start += searchFilterBatch {
		ph, args := inPlaceholders(docIDs[start:min(start+searchFilterBatch, len(docIDs))])
		res, err := d.query(ctx, key, `SELECT count(*), count(DISTINCT document_id) FROM chunks WHERE document_id IN (`+ph+`)`, args...)
		if err != nil {
			return 0, 0, err
		}
		if len(res.Rows) > 0 && len(res.Rows[0]) >= 2 {
			chunks += asInt(res.Rows[0][0])
			docs += asInt(res.Rows[0][1])
		}
	}
	return chunks, docs, nil
}

// allDocumentIDs lists the scope's documents that hold chunks, keyset-paged so the
// operator's row cap never truncates it silently.
func (d *Document) allDocumentIDs(ctx context.Context, key sqlmem.ScopeKey) ([]string, error) {
	var out []string
	after := ""
	for {
		res, err := d.query(ctx, key, `SELECT DISTINCT document_id FROM chunks WHERE document_id > ? ORDER BY document_id LIMIT ?`, after, searchFilterPage)
		if err != nil {
			return nil, err
		}
		for _, r := range res.Rows {
			out = append(out, asStr(r[0]))
		}
		if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < searchFilterPage) {
			sort.Strings(out)
			return out, nil
		}
		after = asStr(res.Rows[len(res.Rows)-1][0])
	}
}

// filterTooWideDocuments refuses a document list over the cap. The cap is the one
// the chunk-id list obeys; a document list reaching it means tens of thousands of
// documents.
func filterTooWideDocuments(op string, listed, maxIDs int) *tools.Result {
	r := errBusiness(fmt.Sprintf("%s: filter_too_wide: the filter would send the search a list of %d document ids, over the limit of %d (max_ids=%d)",
		op, listed, maxIDs, maxIDs),
		"Narrow the filter with type, status or a tag, or search a smaller subtree.")
	return &r
}
