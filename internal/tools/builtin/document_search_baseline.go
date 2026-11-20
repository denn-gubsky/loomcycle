package builtin

// document_search_baseline.go — search facets with baseline: true (RFC GM §6.3,
// plan P7): beside each facet value's count among the matching documents, its
// count in the whole readable corpus of the scope and the capped log-ratio of the
// two shares. A topic that holds 2% of the corpus and 60% of the hits names what
// the hits are about; a raw count cannot tell that topic from one that is simply
// large.
//
// THE SAME RULE ON BOTH SIDES. A value's corpus count is taken by the rule its
// facet count is taken by, so the ratio compares like with like:
//   - type: the document's own type (facetRootTypes), counted by countDocumentTypes;
//   - folder: the parent folder of a Path name the caller may read (facetFolders),
//     counted from the folder's own entries;
//   - concept: the concept a chunk of the document is about directly (facetConcepts)
//     — in the tenant scope a live subject statement to that concept, by the
//     with_counts membership rule (eachConceptMember); elsewhere a tag whose deepest
//     concept is the value. Never the closure: a facet value is the concept the
//     statement names, so its baseline is too.
//
// Assumption: the corpus side counts what is in force now. A search with as_of
// counts its facets as of that moment and its baseline as of now; on a corpus
// whose placements rarely retire the two differ by little, and the walker never
// sends as_of with facets.
//
// ACCESS (RFC GM §7). Every corpus number is over documents the caller may read
// (readableDocList), as every facet count is: a count over hidden documents would
// tell the caller they exist and what they are about. Only the values the facet
// already lists (at most facetValuesCap each) are counted, so the baseline names
// nothing the facet did not.

import (
	"context"
	"fmt"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// facetBaseline is the corpus side of one search: the readable documents of the
// scope, and the reader's list it was counted over.
type facetBaseline struct {
	list  *docReadList
	acl   *docACL
	total int
	// truncated names the facets whose corpus counts a store read cut short; their
	// corpus figures are then lower bounds.
	truncated []string
}

// newFacetBaseline counts the readable documents of the scope.
func (d *Document) newFacetBaseline(ctx context.Context, key sqlmem.ScopeKey, acl *docACL) (*facetBaseline, error) {
	list, err := d.readableDocList(ctx, acl)
	if err != nil {
		return nil, err
	}
	b := &facetBaseline{list: list, acl: acl}
	if list == nil || list.unnamedReadable {
		res, err := d.query(ctx, key, `SELECT count(*) FROM documents`)
		if err != nil {
			return nil, err
		}
		if len(res.Rows) > 0 {
			b.total = asInt(res.Rows[0][0])
		}
	}
	if list != nil && len(list.differs) > 0 {
		docs := make([]string, 0, len(list.differs))
		for id := range list.differs {
			docs = append(docs, id)
		}
		sign := 1 // the listed documents are the readable ones
		if list.unnamedReadable {
			sign = -1 // the listed documents are hidden
		}
		// Only listed documents that exist: a name can outlive its document.
		if _, err := d.eachFacetBatch(ctx, key, docs, `SELECT id FROM documents WHERE id IN (%s)`, nil, nil, func([]any) {
			b.total += sign
		}); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// apply sets the corpus figures on values, one facet's list, against the documents
// the facet counted (documentsCounted).
func (b *facetBaseline) apply(values []facetValue, corpus map[string]int, documentsCounted int) {
	for i := range values {
		v := &values[i]
		n := corpus[v.Value]
		// A hit is in the corpus: a lower corpus count (a cut read) never reads as
		// fewer than the hits that have the value.
		n = max(n, v.Count)
		share, corpusShare := 0.0, 0.0
		if documentsCounted > 0 {
			share = float64(v.Count) / float64(documentsCounted)
		}
		total := max(b.total, documentsCounted)
		if total > 0 {
			corpusShare = float64(n) / float64(total)
		}
		lr := cappedLogRatio(v.Count, documentsCounted, n, total)
		v.Corpus, v.Share, v.CorpusShare, v.LogRatio = &n, &share, &corpusShare, &lr
	}
}

// corpusCounts counts, over the readable corpus, the documents that have each of
// values in the facet kind.
func (d *Document) corpusCounts(ctx context.Context, key sqlmem.ScopeKey, b *facetBaseline, kind string,
	values []facetValue, schemes []memrank.OntologyScheme) (map[string]int, bool, error) {
	out := map[string]int{}
	if len(values) == 0 {
		return out, false, nil
	}
	switch kind {
	case "type":
		types := map[string]int{}
		if err := d.countDocumentTypes(ctx, key, b.list, types); err != nil {
			return nil, false, err
		}
		for _, v := range values {
			out[v.Value] = types[v.Value]
		}
		return out, false, nil
	case "folder":
		for _, v := range values {
			parent := v.Value + "/"
			if v.Value == "/" {
				parent = "/"
			}
			rows, err := d.Store.DirentList(ctx, direntTenant(ctx), key.Scope, direntScopeID(key), parent)
			if err != nil {
				return nil, false, err
			}
			seen := map[string]bool{}
			for doc, paths := range namesByDocument(rows) {
				if seen[doc] || !b.list.readable(doc) {
					continue
				}
				for _, p := range paths {
					if b.acl.nameReadable(ctx, p) {
						seen[doc] = true
						break
					}
				}
			}
			out[v.Value] = len(seen)
		}
		return out, false, nil
	case "concept":
		return d.conceptCorpusCounts(ctx, key, b, values, schemes)
	}
	return nil, false, fmt.Errorf("baseline: unknown facet %q", kind)
}

// conceptCorpusCounts is corpusCounts for the concept facet.
func (d *Document) conceptCorpusCounts(ctx context.Context, key sqlmem.ScopeKey, b *facetBaseline,
	values []facetValue, schemes []memrank.OntologyScheme) (map[string]int, bool, error) {
	out := map[string]int{}
	want := map[string]bool{}
	for _, v := range values {
		want[v.Value] = true
	}
	if key.Scope == "tenant" {
		tagOf := map[string]string{}
		var ids []string
		for _, s := range schemes {
			for _, c := range s.Concepts {
				if c.ChunkID != "" && want[c.Tag] {
					tagOf[c.ChunkID] = c.Tag
					ids = append(ids, c.ChunkID)
				}
			}
		}
		docs := map[string]map[string]bool{} // tag → documents
		err := d.eachConceptMember(ctx, key, ids, func(conceptID, docID, _ string) {
			tag := tagOf[conceptID]
			if tag == "" || !b.list.readable(docID) {
				return
			}
			if docs[tag] == nil {
				docs[tag] = map[string]bool{}
			}
			docs[tag][docID] = true
		})
		if err != nil {
			return nil, false, err
		}
		for tag, ds := range docs {
			out[tag] = len(ds)
		}
		return out, false, nil
	}
	var all []string
	for _, s := range schemes {
		for _, c := range s.Concepts {
			all = append(all, c.Tag)
		}
	}
	capped := false
	for _, v := range values {
		res, err := d.query(ctx, key, `SELECT DISTINCT c.document_id, t.tag FROM chunk_tags t JOIN chunks c ON c.id = t.chunk_id
			 WHERE t.tag = ? OR t.tag LIKE ? ESCAPE '!'`, v.Value, escapeLike(v.Value+"/", '!')+"%")
		if err != nil {
			return nil, false, err
		}
		capped = capped || res.Truncated
		seen := map[string]bool{}
		for _, r := range res.Rows {
			doc := asStr(r[0])
			if !seen[doc] && b.list.readable(doc) && deepestConceptTag(all, asStr(r[1])) == v.Value {
				seen[doc] = true
			}
		}
		out[v.Value] = len(seen)
	}
	return out, capped, nil
}
