package builtin

// document_place.go — Document op=place (RFC GM §6.4, plan P7): for a set of
// documents, or the top hits of a query, per taxonomy and for the class tree the
// LOWEST node that covers a stated share of them (min_share, default 0.6, not strict),
// with its brief and the share. It is the walker's PLACE move by the taxonomy, where
// the folder PLACE finds the lowest common folder: "these hits are mostly about
// topics › risk › outsourcing, and mostly design proposals".
//
// MEMBERSHIP. A document is under a node by op=memberships' rule (membershipsOf),
// live placements only, and with the broader concepts a placement implies: a node
// covers a document when the concept filter (search concept:<tag>) would return it.
// The class tree is the document's class chain (its type and each broader class).
// Lowest is deepest: a concept's path length, a class's distance from the root of
// its chain; ties go to the larger count, then the name.
//
// ACCESS (RFC GM §7). Every count is over documents the caller may read: a named id
// the caller may not read is answered exactly as one that does not exist (listed in
// not_found, never told apart), and a query's hits come from search, which ranks
// only what the caller may read, and are checked again. Classes above a document's
// own type, the taxonomies and node ids are the ontology's text and need read on the
// ontology document (membershipsOf, ontologyReadable); briefs need the tenant scope
// and are shown as get_annotation shows them (nodeBriefs). A new op, so no existing
// answer changes.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	placeOp = "place"
	// placeMaxDocuments bounds a set: each document costs a memberships read, and a
	// search returns at most 50 hits.
	placeMaxDocuments = 50
	placeDefaultLimit = 10
	placeDefaultShare = 0.6
	placeShareEpsilon = 1e-9
	placeLevelClass   = "class"
	placeLevelScheme  = "scheme"
	placeLevelConcept = "concept"
)

// placeNode is the node a view places the set under.
type placeNode struct {
	Level   string   `json:"level"`
	Name    string   `json:"name"`
	Tag     string   `json:"tag,omitempty"`
	Path    []string `json:"path,omitempty"`
	ChunkID string   `json:"chunk_id,omitempty"`
	Brief   string   `json:"brief,omitempty"`
	// Count is how many documents of the set the node covers; Share is Count over
	// the set.
	Count int     `json:"count"`
	Share float64 `json:"share"`
}

// placeTaxonomy is one taxonomy that holds a document of the set.
type placeTaxonomy struct {
	Scheme string `json:"scheme"`
	// Members is how many documents of the set the taxonomy holds anywhere; Share is
	// Members over the set.
	Members int     `json:"members"`
	Share   float64 `json:"share"`
	// Node is the lowest node covering min_share of the set: a concept, else the
	// scheme itself; absent when not even the scheme does (the set is spread).
	Node *placeNode `json:"node,omitempty"`
}

type placeAnswer struct {
	// Documents is the set placed, in the order given (a query: rank order).
	Documents  []string        `json:"documents"`
	MinShare   float64         `json:"min_share"`
	Class      *placeNode      `json:"class,omitempty"`
	Taxonomies []placeTaxonomy `json:"taxonomies"`
	NotFound   []string        `json:"not_found,omitempty"`
	Issues     []string        `json:"issues,omitempty"`
}

func (d *Document) place(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (tools.Result, error) {
	op := placeOp
	if (len(in.DocumentIDs) == 0) == (in.Query == "") {
		return errValidation(op+": pass document_ids or query, not both", "document_ids places a set you hold; query places the top hits of a search."), nil
	}
	share := placeDefaultShare
	if in.MinShare != nil {
		share = *in.MinShare
		if !(share > 0 && share <= 1) {
			return errValidation(op+": min_share must be above 0 and at most 1", "Omit it for 0.6."), nil
		}
	}
	var ids []string
	if in.Query != "" {
		limit := placeDefaultLimit
		if in.Limit > 0 {
			limit = min(in.Limit, placeMaxDocuments)
		}
		hits, refused, err := d.placeHits(ctx, key, mscope, in, limit)
		if refused != nil || err != nil {
			if err != nil {
				return errFrom(op+": "+err.Error(), err), nil
			}
			return *refused, nil
		}
		ids = hits
	} else {
		seen := map[string]bool{}
		for _, id := range in.DocumentIDs {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) > placeMaxDocuments {
			return errValidation(fmt.Sprintf("%s: at most %d documents", op, placeMaxDocuments), "Place a smaller set, or the top hits of a query."), nil
		}
	}

	out := placeAnswer{Documents: []string{}, MinShare: share, Taxonomies: []placeTaxonomy{}}
	set, notFound, err := d.placeReadable(ctx, key, ids)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	out.NotFound = notFound
	out.Documents = set
	if len(set) == 0 {
		return jsonResult(out)
	}

	read, ok := d.tenantOntologyRead(ctx)
	shown := false
	if ok {
		if shown, err = d.ontologyReadable(ctx, read); err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
	}
	classCount := map[string]int{}
	classDepth := map[string]int{}
	conceptCount := map[string]map[string]int{} // scheme → tag → documents
	schemeCount := map[string]int{}
	issues := map[string]bool{}
	for _, doc := range set {
		mem, _, err := d.membershipsOf(ctx, key, doc, false)
		if err != nil {
			return errFrom(op+": "+err.Error(), err), nil
		}
		for _, is := range mem.Issues {
			if !issues[is] {
				issues[is] = true
				out.Issues = append(out.Issues, is)
			}
		}
		for i, name := range mem.Classes {
			classCount[name]++
			classDepth[name] = max(classDepth[name], len(mem.Classes)-1-i)
		}
		for _, tx := range mem.Taxonomies {
			if len(tx.Concepts) == 0 {
				continue
			}
			schemeCount[tx.Scheme]++
			if conceptCount[tx.Scheme] == nil {
				conceptCount[tx.Scheme] = map[string]int{}
			}
			for _, c := range tx.Concepts {
				conceptCount[tx.Scheme][c.Tag]++
			}
		}
	}
	n := len(set)
	covers := func(count int) bool { return float64(count)/float64(n) >= share-placeShareEpsilon }
	want := map[string]string{} // node chunk id → annotation level, for the briefs

	var best *placeNode
	for name, count := range classCount {
		if !covers(count) {
			continue
		}
		if best == nil || classDepth[name] > classDepth[best.Name] ||
			(classDepth[name] == classDepth[best.Name] && (count > best.Count || (count == best.Count && name < best.Name))) {
			best = &placeNode{Level: placeLevelClass, Name: name, Count: count}
		}
	}
	if best != nil {
		best.Share = float64(best.Count) / float64(n)
		if shown {
			best.ChunkID = read.classChunkID(best.Name)
			want[best.ChunkID] = annotationLevelClass
		}
		out.Class = best
	}

	if shown && read.Confirmed {
		for _, s := range read.Schemes {
			members := schemeCount[s.Name]
			if members == 0 {
				continue
			}
			tx := placeTaxonomy{Scheme: s.Name, Members: members, Share: float64(members) / float64(n)}
			tx.Node = lowestConcept(s, conceptCount[s.Name], covers, n)
			if tx.Node == nil && covers(members) {
				tx.Node = &placeNode{Level: placeLevelScheme, Name: s.Name, ChunkID: s.ChunkID, Count: members, Share: tx.Share}
			}
			if tx.Node != nil {
				level := annotationLevelConcept
				if tx.Node.Level == placeLevelScheme {
					level = annotationLevelScheme
				}
				want[tx.Node.ChunkID] = level
			}
			out.Taxonomies = append(out.Taxonomies, tx)
		}
	}
	if shown {
		briefs, issue, err := d.nodeBriefs(ctx, want)
		if err != nil {
			return errFrom(op+": briefs: "+err.Error(), err), nil
		}
		if issue != "" {
			out.Issues = append(out.Issues, issue)
		}
		if out.Class != nil {
			out.Class.Brief = briefs[out.Class.ChunkID]
		}
		for _, tx := range out.Taxonomies {
			if tx.Node != nil {
				tx.Node.Brief = briefs[tx.Node.ChunkID]
			}
		}
	}
	return jsonResult(out)
}

// lowestConcept is the deepest concept of s whose count covers the set; ties go to
// the larger count, then the tag. nil when none does.
func lowestConcept(s memrank.OntologyScheme, counts map[string]int, covers func(int) bool, n int) *placeNode {
	var best *memrank.OntologyConcept
	bestCount := 0
	for i := range s.Concepts {
		c := &s.Concepts[i]
		count := counts[c.Tag]
		if count == 0 || !covers(count) {
			continue
		}
		if best == nil || len(c.Path) > len(best.Path) ||
			(len(c.Path) == len(best.Path) && (count > bestCount || (count == bestCount && c.Tag < best.Tag))) {
			best, bestCount = c, count
		}
	}
	if best == nil {
		return nil
	}
	return &placeNode{Level: placeLevelConcept, Name: best.Name, Tag: best.Tag, Path: best.Path,
		ChunkID: best.ChunkID, Count: bestCount, Share: float64(bestCount) / float64(n)}
}

// placeHits runs the search a query names and returns its hits' documents, each
// once, in rank order, at most limit of them.
func (d *Document) placeHits(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput, limit int) ([]string, *tools.Result, error) {
	res, err := d.searchChunks(ctx, key, mscope, docInput{Op: "search", Scope: in.Scope, Query: in.Query, Limit: limit})
	if err != nil {
		return nil, nil, err
	}
	if res.IsError {
		return nil, &res, nil
	}
	var page struct {
		Chunks []struct {
			DocumentID string `json:"document_id"`
		} `json:"chunks"`
	}
	if err := json.Unmarshal([]byte(res.Text), &page); err != nil {
		return nil, nil, fmt.Errorf("search: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, h := range page.Chunks {
		if h.DocumentID != "" && !seen[h.DocumentID] {
			seen[h.DocumentID] = true
			out = append(out, h.DocumentID)
		}
	}
	return out, nil, nil
}

// placeReadable splits ids into the documents that exist and the caller may read,
// in order, and the rest — not told apart, so not_found says nothing about a
// document the caller may not read.
func (d *Document) placeReadable(ctx context.Context, key sqlmem.ScopeKey, ids []string) ([]string, []string, error) {
	exists := map[string]bool{}
	if _, err := d.eachFacetBatch(ctx, key, ids, `SELECT id FROM documents WHERE id IN (%s)`, nil, nil, func(r []any) {
		exists[asStr(r[0])] = true
	}); err != nil {
		return nil, nil, err
	}
	acl := d.newDocACL(ctx, key)
	var set, notFound []string
	for _, id := range ids {
		ok := exists[id]
		if ok {
			var err error
			if ok, err = acl.readableOrAll(ctx, id); err != nil {
				return nil, nil, err
			}
		}
		if ok {
			set = append(set, id)
		} else {
			notFound = append(notFound, id)
		}
	}
	if set == nil {
		set = []string{}
	}
	sort.SliceStable(notFound, func(i, j int) bool { return notFound[i] < notFound[j] })
	return set, notFound, nil
}
