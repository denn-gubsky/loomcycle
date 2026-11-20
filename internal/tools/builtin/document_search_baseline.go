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
//   - concept: the concept a chunk in force of the document is about directly
//     (facetConcepts at now) — in the tenant scope a live subject statement to
//     that concept, elsewhere a tag whose deepest concept is the value. Never the
//     closure: a facet value is the concept the statement names, so its baseline
//     is too.
//
// Assumption: the corpus side counts what is in force now. A search with as_of
// counts its facets as of that moment and its baseline as of now; on a corpus
// whose placements rarely retire the two differ by little, and the walker never
// sends as_of with facets.
//
// ACCESS (RFC GM §7). Every corpus number is over documents the caller may read
// (readableDocList), as every facet count is: a count over hidden documents would
// tell the caller they exist and what they are about. The reader's list goes into
// each count query and the counts are grouped (countReadableGrouped), so hidden
// documents never take rows under the operator's cap either. Only the values the facet
// already lists (at most facetValuesCap each) are counted, so the baseline names
// nothing the facet did not.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

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

// conceptCorpusCounts is corpusCounts for the concept facet, by facetConcepts' rule
// at now: in the tenant scope a live subject statement (or a narrower kind) to the
// concept itself from a chunk in force; elsewhere a tag on a chunk in force whose
// deepest concept is the value. Each count is one grouped query per reader
// partition (countReadableGrouped), so a hidden document never takes a row.
func (d *Document) conceptCorpusCounts(ctx context.Context, key sqlmem.ScopeKey, b *facetBaseline,
	values []facetValue, schemes []memrank.OntologyScheme) (map[string]int, bool, error) {
	out := map[string]int{}
	want := map[string]bool{}
	for _, v := range values {
		want[v.Value] = true
	}
	chunkLive, cargs := asOfClause(time.Now().UnixNano())
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
		if len(ids) == 0 {
			return out, false, nil
		}
		kinds, _ := d.expandKindFilter(ctx, []string{"subject"})
		kph, kargs := inPlaceholders(kinds)
		iph, iargs := inPlaceholders(ids)
		live, largs := statementVisibility{}.clause()
		stmt := `SELECT e.to_id, COUNT(DISTINCT c.document_id) FROM chunk_edges e JOIN chunks c ON c.id = e.from_id
		          LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
		          WHERE e.kind IN (` + kph + `) AND e.to_id IN (` + iph + `) AND ` + live + ` AND ` + chunkLive + `%s
		          GROUP BY e.to_id`
		byID, capped, err := d.countReadableGrouped(ctx, key, b.list, stmt, append(append(append(kargs, iargs...), largs...), cargs...))
		if err != nil {
			return nil, false, err
		}
		for id, n := range byID {
			out[tagOf[id]] += n
		}
		return out, capped, nil
	}
	var all []string
	for _, s := range schemes {
		for _, c := range s.Concepts {
			all = append(all, c.Tag)
		}
	}
	capped := false
	for _, v := range values {
		// The tags whose deepest concept is v: v and the tags under it, less those
		// under a narrower concept (deepestConceptTag, written as SQL).
		cond := `(t.tag = ? OR t.tag LIKE ? ESCAPE '!')`
		args := []any{v.Value, escapeLike(v.Value+"/", '!') + "%"}
		for _, c := range all {
			if strings.HasPrefix(c, v.Value+"/") {
				cond += ` AND NOT (t.tag = ? OR t.tag LIKE ? ESCAPE '!')`
				args = append(args, c, escapeLike(c+"/", '!')+"%")
			}
		}
		stmt := `SELECT '', COUNT(DISTINCT c.document_id) FROM chunk_tags t JOIN chunks c ON c.id = t.chunk_id
		          LEFT JOIN chunk_memory_meta m ON m.chunk_id = c.id
		          WHERE ` + cond + ` AND ` + chunkLive + `%s`
		n, cut, err := d.countReadableGrouped(ctx, key, b.list, stmt, append(args, cargs...))
		if err != nil {
			return nil, false, err
		}
		capped = capped || cut
		out[v.Value] = n[""]
	}
	return out, capped, nil
}

// countReadableGrouped runs stmt — a grouped count of (key, distinct documents)
// whose `%s` takes a document filter at the end of its WHERE — over the documents
// list leaves readable, as countDocumentTypes counts types: the whole scope less the
// hidden documents when the bare scope is readable, else the readable documents
// alone, in batches. The batches partition documents, so their distinct counts add
// up. The reader's list is in the query, so a hidden document never takes one of
// the rows the operator's cap allows; capped says a grouped answer was cut anyway.
func (d *Document) countReadableGrouped(ctx context.Context, key sqlmem.ScopeKey, list *docReadList, stmt string, args []any) (map[string]int, bool, error) {
	out := map[string]int{}
	capped := false
	run := func(filter string, fargs []any, sign int) error {
		res, err := d.query(ctx, key, fmt.Sprintf(stmt, filter), append(append([]any{}, args...), fargs...)...)
		if err != nil {
			return err
		}
		capped = capped || res.Truncated
		for _, r := range res.Rows {
			if len(r) < 2 {
				return fmt.Errorf("baseline: a row of %d columns", len(r))
			}
			out[asStr(r[0])] += sign * asInt(r[1])
		}
		return nil
	}
	if list == nil || list.unnamedReadable {
		if err := run("", nil, 1); err != nil {
			return nil, false, err
		}
	}
	if list == nil || len(list.differs) == 0 {
		return out, capped, nil
	}
	docs := make([]string, 0, len(list.differs))
	for id := range list.differs {
		docs = append(docs, id)
	}
	sort.Strings(docs)
	sign := 1 // the listed documents are the readable ones
	if list.unnamedReadable {
		sign = -1 // the listed documents are hidden
	}
	for start := 0; start < len(docs); start += facetDocBatch {
		ph, dargs := inPlaceholders(docs[start:min(start+facetDocBatch, len(docs))])
		if err := run(` AND c.document_id IN (`+ph+`)`, dargs, sign); err != nil {
			return nil, false, err
		}
	}
	return out, capped, nil
}
