package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Facet counts on op=search (RFC GL P3): beside the hits, how many matching
// DOCUMENTS each type, folder and concept has, so an agent answering "what do we
// hold on X" can see how wide the material is before it reads any of it.
//
// WHAT COUNTS AS A MATCH. A search ranks everything, so "matching" has to be a
// bounded window: the facet_window chunks nearest the query by meaning, under the
// same filter and access narrowing the search itself ran with, together with the
// passages the search returned (a passage found by its words alone may sit
// outside the nearest-by-meaning window). Counts are DISTINCT DOCUMENTS of those
// chunks, never chunks, and only documents the caller may read.
//
// WHY A SECOND, KEYS-ONLY QUERY. The search's own candidate pool is capped at 51
// rows by the store and is reranked, deduplicated and trimmed before the tool
// sees it, so there is no larger ranked set to reuse. One extra nearest-neighbour
// statement selecting keys alone (store.MemoryEmbedSearchKeys) is the cheapest
// window: no value, no vector, no access-count bump. It runs only when facets are
// asked for; a call without them takes exactly the path it took before.

const (
	// facetWindowDefault is the window when the caller names none. 100 chunks is
	// ten pages of hits at the default limit: wide enough that a topic spread over
	// a few dozen documents shows its spread (the widest overview question measured
	// for RFC GL matched 15 document and folder summaries), small enough that the
	// extra keys query and the two or three metadata lookups stay cheap.
	facetWindowDefault = 100
	// facetValuesCap bounds each facet's list. A folder facet over 500 chunks can
	// name hundreds of folders; the long tail of single-document values says little
	// about spread, and the list is model-read.
	facetValuesCap = 50
	// facetDocBatch bounds one IN list of document ids in the facet lookups, kept
	// small so a scope with many tags or statements per document stays well under
	// the operator's row cap per statement.
	facetDocBatch = 100
)

// The facets search can count, in the order a response lists them.
var searchFacetKinds = []string{"type", "folder", "concept"}

// facetValue is one value of a facet and how many documents in the window have it.
type facetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// facetRequest is a call's normalised facets argument; nil when none was asked.
type facetRequest struct {
	kinds  []string
	window int
}

// normaliseFacets checks facets / facet_window before any work. An empty list is
// the same as none, so the response stays byte-identical.
func normaliseFacets(in docInput) (*facetRequest, *tools.Result) {
	seen := map[string]bool{}
	for _, f := range in.Facets {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		known := false
		for _, k := range searchFacetKinds {
			known = known || k == f
		}
		if !known {
			r := errValidation(fmt.Sprintf("search: unknown facet %q", f), `Pass facets as any of "type", "folder", "concept".`)
			return nil, &r
		}
		seen[f] = true
	}
	if len(seen) == 0 {
		if in.FacetWindow != 0 {
			r := errValidation("search: facet_window is taken with facets", `Add facets, e.g. ["type","folder"], or drop facet_window.`)
			return nil, &r
		}
		return nil, nil
	}
	if in.FacetWindow < 0 {
		r := errValidation("search: facet_window must be positive", fmt.Sprintf("Omit it for %d, or pass at most %d.", facetWindowDefault, store.MemoryEmbedKeysMax))
		return nil, &r
	}
	req := &facetRequest{window: facetWindowDefault}
	if in.FacetWindow > 0 {
		req.window = min(in.FacetWindow, store.MemoryEmbedKeysMax)
	}
	for _, k := range searchFacetKinds {
		if seen[k] {
			req.kinds = append(req.kinds, k)
		}
	}
	return req, nil
}

// empty is the facets block of a search that matched nothing: every asked facet
// with no values. Nothing is looked up.
func (fr *facetRequest) empty() map[string]any {
	out := map[string]any{"facet_window": fr.window, "chunks_counted": 0, "documents_counted": 0}
	for _, k := range fr.kinds {
		out[k] = []facetValue{}
	}
	return out
}

// searchFacets counts the facets over the window. sq is the query the search ran
// (its prefix, id lists and exclusions are the window's filter); queryVec is the
// embedding it made, nil when the backend embeds out of process or the passage
// search did not run; hitIDs are the passages it returned; asOf is the filter's
// instant, which a concept's statement must be in force at.
func (d *Document) searchFacets(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, fr *facetRequest, sq memrank.SearchQuery, queryVec []float32, hitIDs []string, asOf *int64) (map[string]any, error) {
	if len(queryVec) == 0 {
		vecs, err := providers.EmbedQuery(ctx, d.Embedder, []string{sq.QueryText})
		if err != nil {
			return nil, fmt.Errorf("embed query: %w", err)
		}
		if len(vecs) != 1 {
			return nil, fmt.Errorf("embed query: got %d vectors, want 1", len(vecs))
		}
		queryVec = vecs[0]
	}
	sf, err := sq.Filter()
	if err != nil {
		return nil, err
	}
	keys, err := d.Store.MemoryEmbedSearchKeys(ctx, direntTenant(ctx), mscope, docMemScopeID(mscope, key), sf, queryVec, fr.window)
	if err != nil {
		return nil, err
	}
	var chunkIDs []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			chunkIDs = append(chunkIDs, id)
		}
	}
	for _, k := range keys {
		add(ChunkIDFromBodyKey(k))
	}
	for _, id := range hitIDs {
		add(id)
	}
	out := map[string]any{"facet_window": fr.window, "chunks_counted": len(chunkIDs)}

	// The documents of the window, then only those the caller may read. The id
	// lists already keep a confined reader's hidden chunks out when they fit under
	// the operator's cap; when they do not, the search runs unnarrowed, so every
	// document is asked here as well. A count over a hidden document would tell the
	// caller it exists and what it is about.
	docOf, err := d.documentsOfChunks(ctx, key, chunkIDs)
	if err != nil {
		return nil, err
	}
	acl := d.newDocACL(ctx, key)
	limited, err := acl.limitsReading(ctx)
	if err != nil {
		return nil, err
	}
	var docs []string
	readable := map[string]bool{}
	for _, cid := range chunkIDs {
		doc := docOf[cid]
		if doc == "" {
			continue // a body whose structure row is gone: not a document
		}
		ok, asked := readable[doc]
		if !asked {
			ok = true
			if limited {
				if ok, err = acl.readable(ctx, doc); err != nil {
					return nil, err
				}
			}
			readable[doc] = ok
			if ok {
				docs = append(docs, doc)
			}
		}
	}
	sort.Strings(docs)
	out["documents_counted"] = len(docs)

	var truncated []string
	for _, kind := range fr.kinds {
		var byDoc map[string][]string
		var capped bool
		switch kind {
		case "type":
			byDoc, capped, err = d.facetRootTypes(ctx, key, docs)
		case "folder":
			byDoc, err = d.facetFolders(ctx, key, acl, docs)
		case "concept":
			byDoc, capped, err = d.facetConcepts(ctx, key, docs, asOf)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", kind, err)
		}
		values, cut := facetCounts(byDoc)
		out[kind] = values
		if cut || capped {
			truncated = append(truncated, kind)
		}
	}
	if len(truncated) > 0 {
		out["truncated"] = truncated
	}
	return out, nil
}

// facetCounts turns document → values into values by document count, most first
// and then by value, capped at facetValuesCap. A document counts once per value.
func facetCounts(byDoc map[string][]string) ([]facetValue, bool) {
	counts := map[string]int{}
	for _, values := range byDoc {
		once := map[string]bool{}
		for _, v := range values {
			if v != "" && !once[v] {
				once[v] = true
				counts[v]++
			}
		}
	}
	out := make([]facetValue, 0, len(counts))
	for v, n := range counts {
		out = append(out, facetValue{Value: v, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	if len(out) > facetValuesCap {
		return out[:facetValuesCap], true
	}
	return out, false
}

// eachFacetBatch runs stmt (with one `%s` for an IN list) over docs in batches and
// hands each row to fn. capped reports a batch the operator's row cap cut short:
// the counts are then a lower bound, and the facet says so.
func (d *Document) eachFacetBatch(ctx context.Context, key sqlmem.ScopeKey, docs []string, stmt string, pre, post []any, fn func(row []any)) (capped bool, err error) {
	for start := 0; start < len(docs); start += facetDocBatch {
		ph, args := inPlaceholders(docs[start:min(start+facetDocBatch, len(docs))])
		all := append(append(append([]any{}, pre...), args...), post...)
		res, err := d.query(ctx, key, fmt.Sprintf(stmt, ph), all...)
		if err != nil {
			return false, err
		}
		capped = capped || res.Truncated
		for _, r := range res.Rows {
			fn(r)
		}
	}
	return capped, nil
}

// documentsOfChunks maps each chunk id to its document; ids with no structure row
// are absent.
func (d *Document) documentsOfChunks(ctx context.Context, key sqlmem.ScopeKey, chunkIDs []string) (map[string]string, error) {
	out := map[string]string{}
	_, err := d.eachFacetBatch(ctx, key, chunkIDs, `SELECT id, document_id FROM chunks WHERE id IN (%s)`, nil, nil, func(r []any) {
		out[asStr(r[0])] = asStr(r[1])
	})
	return out, err
}

// facetRootTypes is each document's root type (its class), from the documents
// row that mirrors the root chunk's type. An untyped document has no value.
func (d *Document) facetRootTypes(ctx context.Context, key sqlmem.ScopeKey, docs []string) (map[string][]string, bool, error) {
	out := map[string][]string{}
	capped, err := d.eachFacetBatch(ctx, key, docs, `SELECT id, type FROM documents WHERE id IN (%s)`, nil, nil, func(r []any) {
		out[asStr(r[0])] = []string{asStr(r[1])}
	})
	return out, capped, err
}

// facetFolders is the folder of each of a document's Path names: the name's
// parent directory, "/" at the root. A document filed under several names counts
// once in each folder; one filed nowhere has no folder.
func (d *Document) facetFolders(ctx context.Context, key sqlmem.ScopeKey, acl *docACL, docs []string) (map[string][]string, error) {
	names, err := d.namesOfDocuments(ctx, key, docs)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for doc, paths := range names {
		for _, p := range paths {
			if !acl.nameReadable(ctx, p) {
				continue
			}
			folder := p[:strings.LastIndex(p, "/")]
			if folder == "" {
				folder = "/"
			}
			out[doc] = append(out[doc], folder)
		}
	}
	return out, nil
}

// facetConcepts is the concepts each document is about, by the concept filter's
// own rule (resolveConceptFilters): in the tenant scope a live `subject` statement
// (or a narrower kind) from any of its chunks to a concept, in a user or agent
// scope a tag on any of its chunks at or under a concept's tag. Reported by the
// concept's tag, the most specific concept a tag lies under. A caller who may not
// read the ontology document resolves no concept, so the facet is empty.
func (d *Document) facetConcepts(ctx context.Context, key sqlmem.ScopeKey, docs []string, asOf *int64) (map[string][]string, bool, error) {
	schemes, err := d.readableSchemes(ctx)
	if err != nil || len(schemes) == 0 || len(docs) == 0 {
		return map[string][]string{}, false, err
	}
	out := map[string][]string{}
	if key.Scope == "tenant" {
		tagOf := map[string]string{}
		for _, s := range schemes {
			for _, c := range s.Concepts {
				tagOf[c.ChunkID] = c.Tag
			}
		}
		kinds, _ := d.expandKindFilter(ctx, []string{"subject"})
		kph, kargs := inPlaceholders(kinds)
		live, largs := statementVisibility{asOf: asOf}.clause()
		stmt := `SELECT DISTINCT c.document_id, e.to_id FROM chunk_edges e JOIN chunks c ON c.id = e.from_id
		          WHERE e.kind IN (` + kph + `) AND c.document_id IN (%s) AND ` + live
		capped, err := d.eachFacetBatch(ctx, key, docs, stmt, kargs, largs, func(r []any) {
			if tag := tagOf[asStr(r[1])]; tag != "" {
				out[asStr(r[0])] = append(out[asStr(r[0])], tag)
			}
		})
		return out, capped, err
	}
	var tags []string
	for _, s := range schemes {
		for _, c := range s.Concepts {
			tags = append(tags, c.Tag)
		}
	}
	capped, err := d.eachFacetBatch(ctx, key, docs,
		`SELECT DISTINCT c.document_id, t.tag FROM chunk_tags t JOIN chunks c ON c.id = t.chunk_id WHERE c.document_id IN (%s)`,
		nil, nil, func(r []any) {
			if tag := deepestConceptTag(tags, asStr(r[1])); tag != "" {
				out[asStr(r[0])] = append(out[asStr(r[0])], tag)
			}
		})
	return out, capped, err
}

// deepestConceptTag is the longest concept tag that tag equals or lies under, ""
// when it is under none: the concept a tag filter for that concept would match it
// by, at its most specific.
func deepestConceptTag(conceptTags []string, tag string) string {
	best := ""
	for _, c := range conceptTags {
		if (tag == c || strings.HasPrefix(tag, c+"/")) && len(c) > len(best) {
			best = c
		}
	}
	return best
}
