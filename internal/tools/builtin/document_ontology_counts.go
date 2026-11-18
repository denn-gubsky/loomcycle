package builtin

// document_ontology_counts.go — how many documents each concept and document class
// has (RFC GM P1a): describe_ontology with_counts.
//
// MEMBERSHIP IS THE SEARCH FILTER'S. A document is a member of a concept when one of
// its chunks is a member by the rule search's `concept` filter applies in the tenant
// scope (conceptPredicate): a live `subject` statement — or one of a declared
// sub-kind — from the chunk to the concept or to a narrower one. Proposed, rejected,
// refuted and retired statements are not membership. So `members` on a concept is the
// number of distinct documents a `search concept:<tag>` can return, and a count never
// promises a document the search will not find.
//
// EACH DOCUMENT ONCE PER NODE. The closure is the filter's, by tag: a concept counts
// every document at it or under it, and a document reaching a broader concept through
// two narrower ones counts once there. The rollup is done in Go over the distinct
// (concept, document) pairs, since a sum of the narrower concepts' counts would count
// it twice.
//
// NOT CACHED. Counts change with every statement, the ontology does not, so they are
// read per call and never kept in the cached ontology read (tenantOntologyRead).

import (
	"context"
	"fmt"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
)

// ontologyCountConceptBatch bounds one IN list of concept ids. A tenant's schemes
// can declare hundreds of concepts; a batch keeps each statement's parameter count
// well under both backends' limits.
const ontologyCountConceptBatch = 200

// ontologyMemberCounts is the membership of one scope's concepts and document types.
type ontologyMemberCounts struct {
	// concepts: concept chunk id → distinct documents at or under the concept.
	concepts map[string]int
	// conceptTypes: concept chunk id → document type → those documents, by the
	// document's own type. An untyped document is in concepts only.
	conceptTypes map[string]map[string]int
	// schemes is index-aligned with the schemes counted: distinct documents about
	// any concept of the scheme.
	schemes []int
	// schemeTypes is index-aligned with schemes: document type → the documents of
	// that type about any concept of the scheme. An untyped document is in schemes
	// only.
	schemeTypes []map[string]int
	// docSchemes: member document → the indexes of the schemes it is in.
	docSchemes map[string]map[int]bool
	// types: document type → documents of exactly that type (no subtypes).
	types map[string]int
}

// countOntologyMembers counts, in the scope key names, the documents of each concept
// of schemes and of each document type. readable narrows the count to the documents
// a reader may read (readableDocList); nil counts every document of the scope, which
// is what a whole-scope count (an operator's, or a rebuild's plan) passes.
//
// Reads only, outside any transaction: on SQLite a scope has one connection, and a
// read beside an open transaction on it would wait on itself.
func (d *Document) countOntologyMembers(ctx context.Context, key sqlmem.ScopeKey,
	schemes []memrank.OntologyScheme, readable *docReadList) (ontologyMemberCounts, error) {
	out := ontologyMemberCounts{
		concepts:     map[string]int{},
		conceptTypes: map[string]map[string]int{},
		schemes:      make([]int, len(schemes)),
		schemeTypes:  make([]map[string]int, len(schemes)),
		types:        map[string]int{},
	}
	for i := range out.schemeTypes {
		out.schemeTypes[i] = map[string]int{}
	}
	if err := d.countDocumentTypes(ctx, key, readable, out.types); err != nil {
		return out, err
	}

	m, err := d.conceptMembership(ctx, key, schemes, readable)
	if err != nil {
		return out, err
	}
	for id, docs := range m.members {
		out.concepts[id] = len(docs)
		for docID := range docs {
			typ := m.docType[docID]
			if typ == "" {
				continue
			}
			if out.conceptTypes[id] == nil {
				out.conceptTypes[id] = map[string]int{}
			}
			out.conceptTypes[id][typ]++
		}
	}
	out.docSchemes = m.docSchemes
	for docID, in := range m.docSchemes {
		for i := range in {
			out.schemes[i]++
			if typ := m.docType[docID]; typ != "" {
				out.schemeTypes[i][typ]++
			}
		}
	}
	return out, nil
}

// describeUnplaced sets each described scheme's unplaced: the documents of the
// classes it projects (projectedClasses) that no live placement puts under any of
// its concepts. Both sides come from counts, so a document is placed exactly when
// it is a member by the with_counts rule, and readable exactly when it is counted.
// The ontology document itself is never placed and is left out; the operational
// types are left out by projectedClasses.
func (d *Document) describeUnplaced(ctx context.Context, key sqlmem.ScopeKey, readable *docReadList,
	read OntologyRead, counts ontologyMemberCounts, out *ontologyDescription) error {
	ontologyType := ""
	if read.DocumentID != "" && readable.readable(read.DocumentID) {
		res, err := d.query(ctx, key, `SELECT type FROM documents WHERE id = ? LIMIT 1`, read.DocumentID)
		if err != nil {
			return err
		}
		if len(res.Rows) > 0 {
			ontologyType = asStr(res.Rows[0][0])
		}
	}
	for i := range out.Schemes {
		n := 0
		classes := projectedClasses(out.Schemes[i].Projects, out.Classes)
		for name := range classes {
			n += counts.types[name] - counts.schemeTypes[i][name]
		}
		if ontologyType != "" && classes[ontologyType] && !counts.docSchemes[read.DocumentID][i] {
			n--
		}
		out.Schemes[i].Unplaced = &n
	}
	return nil
}

// projectedClasses is the names of the classes a scheme arranges: those @projects
// names (matched case-insensitively, as CheckSchemeReferences matches them) and their
// subclasses, or — without @projects — every document class. The operational
// document types (an item or a run, never knowledge, which classify skips) are
// never arranged.
func projectedClasses(projects []string, classes []describedClass) map[string]bool {
	byLower := map[string]string{}
	kids := map[string][]string{}
	var roots []string
	for _, c := range classes {
		byLower[strings.ToLower(c.Name)] = c.Name
		if c.Parent != "" {
			kids[strings.ToLower(c.Parent)] = append(kids[strings.ToLower(c.Parent)], c.Name)
		}
		if len(projects) == 0 && c.Kind == "document" {
			roots = append(roots, c.Name)
		}
	}
	for _, p := range projects {
		if name, ok := byLower[strings.ToLower(p)]; ok {
			roots = append(roots, name)
		}
	}
	out := map[string]bool{}
	for len(roots) > 0 {
		name := roots[0]
		roots = roots[1:]
		if out[name] {
			continue
		}
		out[name] = true
		roots = append(roots, kids[strings.ToLower(name)]...)
	}
	for name := range out {
		if memrank.IsOperationalDocumentType(name) {
			delete(out, name)
		}
	}
	return out
}

// conceptMembership is who the members of a scope's concepts are.
type conceptMembership struct {
	// members: concept chunk id → the documents at or under the concept, each once.
	// A concept with no member has no entry.
	members map[string]map[string]bool
	// docType: member document → its own type ("" untyped).
	docType map[string]string
	// docSchemes: member document → the indexes of the schemes it is in.
	docSchemes map[string]map[int]bool
}

// conceptMembership reads who the members of schemes' concepts are in the scope key
// names, narrowed to readable (nil = every document). It is the one membership rule
// of both the counts (with_counts) and the node statistics (node_stats), so a
// statistic is never over a set the count does not describe. Reads only, outside
// any transaction, as countOntologyMembers.
func (d *Document) conceptMembership(ctx context.Context, key sqlmem.ScopeKey,
	schemes []memrank.OntologyScheme, readable *docReadList) (conceptMembership, error) {
	out := conceptMembership{members: map[string]map[string]bool{}, docType: map[string]string{},
		docSchemes: map[string]map[int]bool{}}
	tagOf := map[string]string{}   // concept id → tag
	schemeOf := map[string]int{}   // concept id → scheme index
	byTag := map[string][]string{} // tag → concept ids (a tag is unique in a sound ontology)
	var ids []string
	for i, s := range schemes {
		for _, c := range s.Concepts {
			if c.ChunkID == "" || c.Tag == "" {
				continue
			}
			if _, dup := tagOf[c.ChunkID]; !dup {
				ids = append(ids, c.ChunkID)
			}
			tagOf[c.ChunkID], schemeOf[c.ChunkID] = c.Tag, i
			byTag[c.Tag] = append(byTag[c.Tag], c.ChunkID)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}

	direct := map[string]map[string]bool{} // document → the concepts it reaches itself
	err := d.eachConceptMember(ctx, key, ids, func(conceptID, docID, typ string) {
		if !readable.readable(docID) {
			return
		}
		if direct[docID] == nil {
			direct[docID] = map[string]bool{}
		}
		direct[docID][conceptID] = true
		out.docType[docID] = typ
	})
	if err != nil {
		return out, err
	}
	for docID, concepts := range direct {
		inScheme := map[int]bool{}
		for cid := range concepts {
			inScheme[schemeOf[cid]] = true
			// Every concept whose tag is this one's or a "/"-prefix of it: exactly the
			// concepts whose closure (conceptClosure) holds this one.
			for p := tagOf[cid]; ; {
				for _, id := range byTag[p] {
					if out.members[id] == nil {
						out.members[id] = map[string]bool{}
					}
					out.members[id][docID] = true
				}
				i := strings.LastIndex(p, "/")
				if i < 0 {
					break
				}
				p = p[:i]
			}
		}
		out.docSchemes[docID] = inScheme
	}
	return out, nil
}

// eachConceptMember hands fn each distinct (concept, document, document type) where a
// chunk of the document is about the concept directly, by conceptPredicate's
// statement rule. Batched on the concept ids and keyset-paged on (concept, document),
// so the operator's row cap never cuts the set silently.
func (d *Document) eachConceptMember(ctx context.Context, key sqlmem.ScopeKey, conceptIDs []string, fn func(conceptID, docID, typ string)) error {
	kinds, _ := d.expandKindFilter(ctx, []string{"subject"})
	kph, kargs := inPlaceholders(kinds)
	live, largs := statementVisibility{}.clause()
	for start := 0; start < len(conceptIDs); start += ontologyCountConceptBatch {
		iph, iargs := inPlaceholders(conceptIDs[start:min(start+ontologyCountConceptBatch, len(conceptIDs))])
		stmt := `SELECT e.to_id, c.document_id, doc.type FROM chunk_edges e
		           JOIN chunks c ON c.id = e.from_id
		           LEFT JOIN documents doc ON doc.id = c.document_id
		          WHERE e.kind IN (` + kph + `) AND e.to_id IN (` + iph + `) AND ` + live + `
		            AND (e.to_id > ? OR (e.to_id = ? AND c.document_id > ?))
		          GROUP BY e.to_id, c.document_id, doc.type
		          ORDER BY e.to_id, c.document_id LIMIT ?`
		afterConcept, afterDoc := "", ""
		for {
			args := append(append(append(append([]any{}, kargs...), iargs...), largs...),
				afterConcept, afterConcept, afterDoc, searchFilterPage)
			res, err := d.query(ctx, key, stmt, args...)
			if err != nil {
				return err
			}
			for _, r := range res.Rows {
				if len(r) < 3 {
					return fmt.Errorf("concept members: a row of %d columns", len(r))
				}
				fn(asStr(r[0]), asStr(r[1]), asStr(r[2]))
			}
			if len(res.Rows) == 0 || (!res.Truncated && len(res.Rows) < searchFilterPage) {
				break
			}
			last := res.Rows[len(res.Rows)-1]
			afterConcept, afterDoc = asStr(last[0]), asStr(last[1])
		}
	}
	return nil
}

// countDocumentTypes fills types with the readable documents of each type. Without
// a list it is one grouped count. With one, only the listed documents' types are
// read: the bare scope's verdict covers every other document, so a readable bare
// scope takes the grouped count minus the hidden documents, and an unreadable one
// counts the listed (readable) documents alone.
func (d *Document) countDocumentTypes(ctx context.Context, key sqlmem.ScopeKey, readable *docReadList, types map[string]int) error {
	if readable == nil || readable.unnamedReadable {
		res, err := d.query(ctx, key, `SELECT type, count(*) FROM documents GROUP BY type`)
		if err != nil {
			return err
		}
		if res.Truncated {
			return fmt.Errorf("document types: the count was cut short")
		}
		for _, r := range res.Rows {
			if t := asStr(r[0]); t != "" {
				types[t] += asInt(r[1])
			}
		}
	}
	if readable == nil || len(readable.differs) == 0 {
		return nil
	}
	docs := make([]string, 0, len(readable.differs))
	for id := range readable.differs {
		docs = append(docs, id)
	}
	sign := 1 // the listed documents are the readable ones
	if readable.unnamedReadable {
		sign = -1 // the listed documents are hidden
	}
	capped, err := d.eachFacetBatch(ctx, key, docs, `SELECT id, type FROM documents WHERE id IN (%s)`, nil, nil, func(r []any) {
		if t := asStr(r[1]); t != "" {
			types[t] += sign
		}
	})
	if err == nil && capped {
		err = fmt.Errorf("document types: a batch was cut short")
	}
	return err
}
