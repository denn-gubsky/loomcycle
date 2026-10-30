package builtin

// document_relation_retrieval.go — search and related reaching across relations
// (RFC FN P4 §7.2).
//
// A search ranks chunks by their own text. A regulation's clause that "applies
// Article 12" rarely repeats what Article 12 says, so the clause ranks and the
// article it depends on does not — and a reader handed the clause alone answers
// half the question. `expand` adds, after ranking, the chunks the top hits are
// related to: one hop, within a budget, each marked with the relation and its
// quote. It is completeness, not search: the expanded rows are not ranked and do
// not displace a hit.

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// searchExpand is search's `expand` argument.
type searchExpand struct {
	Kinds       []string `json:"kinds,omitempty"`
	Hops        int      `json:"hops,omitempty"`
	BudgetChars int      `json:"budget_chars,omitempty"`
}

const (
	// expandFromTop is how many of the ranked hits the expansion starts from: the
	// answer is about the best matches, and expanding from all fifty would turn a
	// search into a walk.
	expandFromTop = 5
	// expandDefaultBudget bounds the expansion by the characters of the rows it adds
	// (titles and quotes; search rows carry no bodies).
	expandDefaultBudget = 2000
	expandMaxBudget     = 8000
)

// expandUndeclaredSkip are the kinds an expansion does not follow when no kinds
// are named and the vocabulary declares no walk set: `about` and `mentions` join
// a chunk to a subject's identity node, which is a name, not a passage.
var expandUndeclaredSkip = map[string]bool{"about": true, "mentions": true}

// validateExpand refuses an expand a search cannot honour.
func validateExpand(e *searchExpand) error {
	if e == nil {
		return nil
	}
	if e.Hops != 0 && e.Hops != 1 {
		return fmt.Errorf("expand.hops must be 1 — expansion is one hop; use op=graph_recall for a walk")
	}
	if e.BudgetChars < 0 || e.BudgetChars > expandMaxBudget {
		return fmt.Errorf("expand.budget_chars must be 0..%d", expandMaxBudget)
	}
	return nil
}

// expandHits adds the relation neighbours of the top hits to a search answer.
// hits are the ranked rows (each with chunk_id); the returned rows carry
// chunk_id / title / type / document_id like a hit, plus expanded_via. filter is
// the search's resolved structural filter (nil when none was named).
func (d *Document) expandHits(ctx context.Context, key sqlmem.ScopeKey, in docInput, hits []map[string]any, filter *searchFilterSet) ([]map[string]any, bool, error) {
	if in.Expand == nil || len(hits) == 0 {
		return nil, false, nil
	}
	budget := in.Expand.BudgetChars
	if budget == 0 {
		budget = expandDefaultBudget
	}
	// The expansion starts only from hits the caller may read. The listing filter
	// drops an unreadable hit from the answer AFTER this runs, so expanding from
	// one would put its id (from_chunk_id) and its relations into the answer anyway.
	acl := d.newDocACL(ctx, key)
	limits, err := acl.limitsReading(ctx)
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	frontier := make([]string, 0, expandFromTop)
	for _, h := range hits {
		id, _ := h["chunk_id"].(string)
		seen[id] = true
		if len(frontier) >= expandFromTop || id == "" {
			continue
		}
		if limits {
			docID, _ := h["document_id"].(string)
			if docID == "" {
				continue
			}
			ok, err := acl.readable(ctx, docID)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				continue
			}
		}
		frontier = append(frontier, id)
	}
	if len(frontier) == 0 {
		return nil, false, nil
	}
	walkIn := docInput{Kinds: in.Expand.Kinds, AsOf: in.AsOf, Walk: "declared"}
	walk, err := d.graphWalkSpec(ctx, walkIn)
	if err != nil {
		return nil, false, err
	}
	rows, _, err := d.walkNeighbours(ctx, key, frontier, 1, walkIn, walk, graphHopRowCap)
	if err != nil {
		return nil, false, err
	}
	if walk.mode == "all" {
		kept := rows[:0]
		for _, r := range rows {
			if !expandUndeclaredSkip[r.ViaKind] {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	meta, err := d.chunkMetaByIDs(ctx, key, ids)
	if err != nil {
		return nil, false, err
	}
	// ⚠️ DROP WHAT THE CALLER IS NOT SHOWN BEFORE THE BUDGET. The listing filter
	// removes an unreadable row from the answer afterwards, but by then it has been
	// counted in `expanded`, may have set `expand_truncated`, and has spent budget a
	// visible row could have used — so either count moving would say a readable hit
	// is related to something the caller cannot read. The search's own filter is
	// applied here for the same reason: a ranked hit could never have come from an
	// excluded path, and neither may a row added beside it.
	rows, err = visibleRelationRows(ctx, rows, meta, filter, acl, limits)
	if err != nil {
		return nil, false, err
	}
	d.attachViaQuotes(ctx, key, rows)
	out := []map[string]any{}
	used, truncated := 0, false
	for _, r := range rows {
		if seen[r.ID] {
			continue
		}
		cost := len(r.Title) + len(r.ViaQuote)
		if used+cost > budget {
			truncated = true
			continue
		}
		used += cost
		seen[r.ID] = true
		via := map[string]any{"kind": r.ViaKind, "from_chunk_id": r.ViaID}
		if r.ViaQuote != "" {
			via["quote"] = r.ViaQuote
		}
		m := map[string]any{"chunk_id": r.ID, "expanded_via": via}
		if md, ok := meta[r.ID]; ok {
			if md.title != "" {
				m["title"] = md.title
			}
			if md.typ != "" {
				m["type"] = md.typ
			}
			if md.documentID != "" {
				m["document_id"] = md.documentID
			}
			putValidity(m, md.validAt, md.invalidAt)
		}
		out = append(out, m)
	}
	return out, truncated, nil
}

// relatedByRelation is related's `related_to`: the chunks a chunk is related to by
// relations (statements), one hop, both directions — the graph's answer beside the
// embedding's. filter is the call's resolved structural filter (nil when none was
// named), applied as it is to the semantic neighbours.
func (d *Document) relatedByRelation(ctx context.Context, key sqlmem.ScopeKey, in docInput, limit int, filter *searchFilterSet) ([]map[string]any, error) {
	walkIn := docInput{Kinds: in.Kinds, Kind: in.Kind, AsOf: in.AsOf, Walk: "declared"}
	walk, err := d.graphWalkSpec(ctx, walkIn)
	if err != nil {
		return nil, err
	}
	// A filtered walk reads up to the hop cap and cuts to limit after filtering: a
	// LIMIT taken before the filter fills with the rows it then drops.
	rowCap := limit
	if filter != nil {
		rowCap = graphHopRowCap
	}
	rows, _, err := d.walkNeighbours(ctx, key, []string{in.RelatedTo}, 1, walkIn, walk, rowCap)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	meta, err := d.chunkMetaByIDs(ctx, key, ids)
	if err != nil {
		return nil, err
	}
	// related reads the whole scope (its access-control question is the scope's),
	// so only the structural filter narrows the rows here.
	if rows, err = visibleRelationRows(ctx, rows, meta, filter, nil, false); err != nil {
		return nil, err
	}
	d.attachViaQuotes(ctx, key, rows)
	out := make([]map[string]any, 0, len(rows)) // rows is bounded by the walk's row cap; limit is cut in the loop
	seen := map[string]bool{in.RelatedTo: true}
	for _, r := range rows {
		if len(out) >= limit {
			break
		}
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		m := map[string]any{"chunk_id": r.ID, "via_kind": r.ViaKind}
		if r.ViaQuote != "" {
			m["via_quote"] = r.ViaQuote
		}
		if md, ok := meta[r.ID]; ok {
			if md.title != "" {
				m["title"] = md.title
			}
			if md.typ != "" {
				m["type"] = md.typ
			}
			if md.documentID != "" {
				m["document_id"] = md.documentID
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// visibleRelationRows keeps the rows a relation-following answer may show: inside
// the structural filter (when one was named) and, when the caller's reading is
// limited, in a document it may read. A row whose document is unknown is dropped
// under a limit — there is nothing to ask the access-control set about.
func visibleRelationRows(ctx context.Context, rows []graphChunk, meta map[string]chunkMeta, filter *searchFilterSet, acl *docACL, limits bool) ([]graphChunk, error) {
	if filter == nil && !limits {
		return rows, nil
	}
	inFilter := searchFilterMembership(filter)
	readable := map[string]bool{}
	kept := rows[:0]
	for _, r := range rows {
		if !inFilter(r.ID) {
			continue
		}
		if limits {
			docID := meta[r.ID].documentID
			if docID == "" {
				continue
			}
			ok, seen := readable[docID]
			if !seen {
				var err error
				if ok, err = acl.readable(ctx, docID); err != nil {
					return nil, err
				}
				readable[docID] = ok
			}
			if !ok {
				continue
			}
		}
		kept = append(kept, r)
	}
	return kept, nil
}

// searchFilterMembership reports whether a chunk is inside a resolved filter: in
// its allowlist when it sends one, and never in its denylist. A filter that
// matched nothing sends neither list, so it is checked by its count.
func searchFilterMembership(f *searchFilterSet) func(string) bool {
	if f == nil {
		return func(string) bool { return true }
	}
	if f.echo.MatchedChunks == 0 {
		return func(string) bool { return false }
	}
	var allow map[string]bool
	if f.ids != nil {
		allow = make(map[string]bool, len(f.ids))
		for _, id := range f.ids {
			allow[id] = true
		}
	}
	deny := make(map[string]bool, len(f.excludeIDs))
	for _, id := range f.excludeIDs {
		deny[id] = true
	}
	return func(id string) bool {
		return (allow == nil || allow[id]) && !deny[id]
	}
}
