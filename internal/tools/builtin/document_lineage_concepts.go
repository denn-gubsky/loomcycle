package builtin

// document_lineage_concepts.go — lineage with_memberships (RFC GM §6.2, plan P7):
// beside the sections, document and folders above a chunk, the chunk's document's
// class chain and, per taxonomy, the chain of each concept it is placed under up to
// the scheme's top — each node with its brief. The walker steps from a hit into each
// view that holds it, and from a concept to its other members (search concept:),
// without a describe_ontology and a memberships call first.
//
// ONE MEMBERSHIP RULE. The placements are op=memberships' (membershipsOf), live ones
// only: a proposal makes the document a member of nothing, and a walker that stepped
// to a proposed concept would search a concept filter that does not return the
// document. A chain starts at each concept placed directly; a concept that is only
// above another listed placement is in that chain, not a chain of its own.
//
// ACCESS. The document was asked read before the op ran (lineage's docACLChunk). The
// class chain above the document's own type and every taxonomy are the ontology's
// text: membershipsOf gives neither to a caller who may not read the ontology
// document, and says why. A node's brief is tenant data written from the node's
// members, so it needs the tenant scope (as get_annotation of a node does), and it is
// shown only when nodeRecordShown shows it — to every reader of the ontology alike
// (RFC GM C8: written from the documents every member may read).

import (
	"context"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// walkNode is one node of the taxonomy or the class tree as a walk shows it.
type walkNode struct {
	Name    string `json:"name"`
	Tag     string `json:"tag,omitempty"`
	ChunkID string `json:"chunk_id,omitempty"`
	Brief   string `json:"brief,omitempty"`
}

// lineageConceptChain is one placement's chain in one taxonomy: the concept placed
// first, then each broader concept up to the scheme's top level.
type lineageConceptChain struct {
	Scheme walkNode   `json:"scheme"`
	Chain  []walkNode `json:"chain"`
}

// lineageMemberships is lineage's `memberships`.
type lineageMemberships struct {
	// Classes is the class chain, the document's own type first.
	Classes  []walkNode            `json:"classes"`
	Concepts []lineageConceptChain `json:"concepts"`
	Issues   []string              `json:"issues,omitempty"`
}

// lineageMembershipsOf builds lineage's memberships for the document docID of the
// scope key names.
func (d *Document) lineageMembershipsOf(ctx context.Context, key sqlmem.ScopeKey, docID string) (lineageMemberships, error) {
	out := lineageMemberships{Classes: []walkNode{}, Concepts: []lineageConceptChain{}}
	mem, _, err := d.membershipsOf(ctx, key, docID, false)
	if err != nil {
		return out, err
	}
	out.Issues = mem.Issues
	// Node ids and briefs only where the ontology is shown: without it the chain is
	// the document's own type, a name the caller already has, and nothing is looked up.
	read, ok := d.tenantOntologyRead(ctx)
	shown := false
	if ok {
		if shown, err = d.ontologyReadable(ctx, read); err != nil {
			return out, err
		}
	}
	if !shown {
		for _, name := range mem.Classes {
			out.Classes = append(out.Classes, walkNode{Name: name})
		}
		return out, nil
	}
	want := map[string]string{} // node chunk id → annotation level
	for _, name := range mem.Classes {
		n := walkNode{Name: name, ChunkID: read.classChunkID(name)}
		if n.ChunkID != "" {
			want[n.ChunkID] = annotationLevelClass
		}
		out.Classes = append(out.Classes, n)
	}
	for _, tx := range mem.Taxonomies {
		var scheme *memrank.OntologyScheme
		for i := range read.Schemes {
			if read.Schemes[i].Name == tx.Scheme {
				scheme = &read.Schemes[i]
				break
			}
		}
		if scheme == nil {
			continue
		}
		byTag := map[string]memrank.OntologyConcept{}
		for _, c := range scheme.Concepts {
			if _, dup := byTag[c.Tag]; !dup {
				byTag[c.Tag] = c
			}
		}
		// The leaves: each concept placed directly that no other direct placement
		// lies under.
		var placed []string
		for _, c := range tx.Concepts {
			if c.Implied || !hasLivePlacement(c.Placements) {
				continue
			}
			placed = append(placed, c.Tag)
		}
		sort.Strings(placed)
		sn := walkNode{Name: scheme.Name, ChunkID: scheme.ChunkID}
		if sn.ChunkID != "" {
			want[sn.ChunkID] = annotationLevelScheme
		}
		for _, tag := range placed {
			if hasNarrowerTag(placed, tag) {
				continue // its chain is part of the narrower placement's
			}
			var chain []walkNode
			for p := tag; ; {
				if c, ok := byTag[p]; ok {
					chain = append(chain, walkNode{Name: c.Name, Tag: c.Tag, ChunkID: c.ChunkID})
					if c.ChunkID != "" {
						want[c.ChunkID] = annotationLevelConcept
					}
				}
				j := strings.LastIndex(p, "/")
				if j < 0 {
					break
				}
				p = p[:j]
			}
			if len(chain) > 0 {
				out.Concepts = append(out.Concepts, lineageConceptChain{Scheme: sn, Chain: chain})
			}
		}
	}
	briefs, issue, err := d.nodeBriefs(ctx, want)
	if err != nil {
		return out, err
	}
	if issue != "" {
		out.Issues = append(out.Issues, issue)
	}
	for i := range out.Classes {
		out.Classes[i].Brief = briefs[out.Classes[i].ChunkID]
	}
	for i := range out.Concepts {
		c := &out.Concepts[i]
		c.Scheme.Brief = briefs[c.Scheme.ChunkID]
		for j := range c.Chain {
			c.Chain[j].Brief = briefs[c.Chain[j].ChunkID]
		}
	}
	return out, nil
}

// hasNarrowerTag reports whether a tag of tags lies under tag.
func hasNarrowerTag(tags []string, tag string) bool {
	for _, t := range tags {
		if strings.HasPrefix(t, tag+"/") {
			return true
		}
	}
	return false
}

// nodeBriefs reads the brief of each node record shown (nodeRecordShown) among want
// (node chunk id → level) in the tenant scope. A caller without the tenant scope gets
// no brief, and the issue says why; a scope with no annotation table has none.
func (d *Document) nodeBriefs(ctx context.Context, want map[string]string) (map[string]string, string, error) {
	out := map[string]string{}
	delete(want, "")
	if len(want) == 0 {
		return out, "", nil
	}
	if _, _, err := d.resolveScope(ctx, "tenant"); err != nil {
		return out, "the briefs of taxonomy nodes need the tenant scope, which this caller is not granted; none are given", nil
	}
	tkey, tscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return nil, "", err
	}
	byLevel := map[string][]string{}
	for id, lvl := range want {
		byLevel[lvl] = append(byLevel[lvl], id)
	}
	s := d.newAnnotationScope(tkey, tscope)
	for _, lvl := range nodeAnnotationLevels {
		ids := byLevel[lvl]
		if len(ids) == 0 {
			continue
		}
		sort.Strings(ids)
		recs, err := d.annotationRecordsFor(ctx, tkey, ids, lvl)
		if err != nil {
			if schemaMissing(err) {
				return out, "", nil
			}
			return nil, "", err
		}
		for _, id := range ids {
			rec, ok := recs[id][lvl]
			if !ok {
				continue
			}
			shown, err := s.nodeRecordShown(ctx, rec)
			if err != nil {
				return nil, "", err
			}
			if shown {
				out[id] = rec.Brief
			}
		}
	}
	return out, "", nil
}
