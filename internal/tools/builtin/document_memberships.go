package builtin

// document_memberships.go — Document op=memberships (RFC GM §6.4a, plan P1.3): every
// place one document holds in the tenant's taxonomies, its class chain and its
// folders; get_document's with_memberships returns the same object.
//
// ONE MEMBERSHIP RULE. A placement is what the concept filter reads in the scope
// (conceptPredicate): in the tenant scope, a statement of kind subject (or a declared
// sub-kind) from any chunk of the document to a concept — the statements
// conceptMembership counts; in a user or agent scope, where no statement reaches the
// ontology, a chunk carrying the concept's mirror tag or a tag under it. A live
// placement makes the document a member of every broader concept too, by the closure
// the counts roll up, so those are listed with implied: true.
//
// PROPOSALS are listed as get_edges lists them: by default, and left out with
// include_proposed false. A proposal makes the document a member of nothing, so it
// implies no broader concept.
//
// ACCESS. Execute has asked read on the document, as get_document asks it. Classes
// above the document's own type and the taxonomies are the ontology's text: a caller
// who may not read the ontology document gets neither, and an issue saying why.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// membershipsAnswer is memberships' answer, and get_document's `memberships`.
type membershipsAnswer struct {
	DocumentID string `json:"document_id"`
	Scope      string `json:"scope"`
	// Classes is the class chain: the document's own type, then each broader class of
	// the ontology in force up to the root. [] for an untyped document.
	Classes []string `json:"classes"`
	// Taxonomies are the schemes holding a concept for the document, in the
	// ontology's order.
	Taxonomies []taxonomyMembership `json:"taxonomies"`
	// Folders are the Path names the document is filed under, each with its folder.
	Folders []membershipFolder `json:"folders"`
	Issues  []string           `json:"issues,omitempty"`
}

type taxonomyMembership struct {
	Scheme   string          `json:"scheme"`
	Concepts []memberConcept `json:"concepts"`
}

// memberConcept is one concept of a taxonomy the document is under.
type memberConcept struct {
	Tag  string `json:"tag"`
	Name string `json:"name"`
	// Path is the concept's names from the scheme's top level down to itself.
	Path    []string `json:"path"`
	ChunkID string   `json:"chunk_id"`
	// Implied: the document is a member through a live placement under this concept,
	// and no live placement names the concept itself (it may still carry a proposal).
	Implied    bool               `json:"implied,omitempty"`
	Placements []conceptPlacement `json:"placements,omitempty"`
}

// conceptPlacement is one placement of the document under a concept: a statement in
// the tenant scope, a carried tag elsewhere.
type conceptPlacement struct {
	StatementID string `json:"statement_id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	// ChunkID is the chunk of the document the placement starts from.
	ChunkID    string   `json:"chunk_id"`
	Method     string   `json:"method,omitempty"`
	Status     string   `json:"status,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	// Via is "tag" for a placement by a carried mirror tag; Tag is that tag.
	Via string `json:"via,omitempty"`
	Tag string `json:"tag,omitempty"`
}

type membershipFolder struct {
	Folder string `json:"folder"`
	Path   string `json:"path"`
}

// memberships answers op=memberships.
func (d *Document) memberships(ctx context.Context, key sqlmem.ScopeKey, in docInput) (tools.Result, error) {
	const op = "memberships"
	docID, err := d.docIDFromInput(ctx, key, in)
	if err != nil {
		return docFail(op+": ", err), nil
	}
	out, found, err := d.membershipsOf(ctx, key, docID, in.IncludeProposed == nil || *in.IncludeProposed)
	if err != nil {
		return errFrom(op+": "+err.Error(), err), nil
	}
	if !found {
		return errNotFound(op+": no such document: "+docID, "Find the document with op=query_documents in the same scope."), nil
	}
	return jsonResult(out)
}

// membershipsOf builds the answer for one document of the scope key names. found is
// false when there is no such document.
func (d *Document) membershipsOf(ctx context.Context, key sqlmem.ScopeKey, docID string, includeProposed bool) (membershipsAnswer, bool, error) {
	out := membershipsAnswer{DocumentID: docID, Scope: key.Scope, Classes: []string{},
		Taxonomies: []taxonomyMembership{}, Folders: []membershipFolder{}}
	res, err := d.query(ctx, key, `SELECT type FROM documents WHERE id = ? LIMIT 1`, docID)
	if err != nil {
		return out, false, err
	}
	if len(res.Rows) == 0 {
		return out, false, nil
	}
	typ := asStr(res.Rows[0][0])
	if typ != "" {
		out.Classes = []string{typ}
	}
	if out.Folders, err = d.membershipFolders(ctx, key, docID); err != nil {
		return out, true, err
	}

	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		out.Issues = append(out.Issues, "the tenant ontology could not be read, so no class above the document's type and no taxonomy is shown; read again")
		return out, true, nil
	}
	if shown, err := d.ontologyReadable(ctx, read); err != nil {
		return out, true, err
	} else if !shown {
		out.Issues = append(out.Issues, "the caller may not read the tenant ontology, so no class above the document's type and no taxonomy is shown")
		return out, true, nil
	}
	out.Classes = ontologyClassChain(read, typ)
	if !read.Confirmed {
		if read.DocumentID != "" {
			out.Issues = append(out.Issues, "the tenant ontology is not confirmed, so it has no taxonomies")
		}
		return out, true, nil
	}
	var direct map[string][]conceptPlacement // concept chunk id → its placements
	if key.Scope == "tenant" {
		direct, err = d.statementPlacements(ctx, key, docID, read.Schemes, includeProposed, &out.Issues)
	} else {
		direct, err = d.tagPlacements(ctx, key, docID, read.Schemes)
	}
	if err != nil {
		return out, true, err
	}
	out.Taxonomies = groupMemberships(read.Schemes, direct)
	return out, true, nil
}

// ontologyReadable reports whether the caller may read the tenant ontology document
// (true when there is none: the seed is compiled in), as describe_ontology asks it.
func (d *Document) ontologyReadable(ctx context.Context, read OntologyRead) (bool, error) {
	if read.DocumentID == "" {
		return true, nil
	}
	tkey, _, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return false, err
	}
	acl := d.newDocACL(ctx, tkey)
	if acl == nil {
		return true, nil
	}
	return acl.readable(ctx, read.DocumentID)
}

// ontologyClassChain is typ, then each broader class of the classes in force, matched
// case-insensitively as the gates match them. A type no class declares is its own
// chain; a cycle (which the reader reports elsewhere) ends it. A class's former name
// (@renamed_from) is given as the class, which is what the type names now.
func ontologyClassChain(read OntologyRead, typ string) []string {
	if typ == "" {
		return []string{}
	}
	typ = memrank.TenantClassAliases(read.Terms, read.Confirmed).Resolve(typ)
	byName := map[string]memrank.OntologyTerm{}
	for _, c := range memrank.EffectiveClasses(read.Terms, read.Confirmed) {
		byName[strings.ToLower(c.Name)] = c
	}
	chain := []string{typ}
	seen := map[string]bool{strings.ToLower(typ): true}
	for c, ok := byName[strings.ToLower(typ)]; ok && c.Parent != ""; {
		p := strings.ToLower(c.Parent)
		if seen[p] {
			break
		}
		seen[p] = true
		next, known := byName[p]
		if !known {
			chain = append(chain, c.Parent)
			break
		}
		chain = append(chain, next.Name)
		c = next
	}
	return chain
}

// membershipFolders is each Path name of the document with its folder, sorted. Read
// was asked on the document at every one of its names, so every name may be shown.
func (d *Document) membershipFolders(ctx context.Context, key sqlmem.ScopeKey, docID string) ([]membershipFolder, error) {
	names, err := d.namesOfDocuments(ctx, key, []string{docID})
	if err != nil {
		return nil, err
	}
	paths := append([]string(nil), names[docID]...)
	sort.Strings(paths)
	out := make([]membershipFolder, 0, len(paths))
	for _, p := range paths {
		parent, _, isRoot := splitPath(p)
		if isRoot {
			continue
		}
		folder := strings.TrimSuffix(parent, "/")
		if folder == "" {
			folder = "/"
		}
		out = append(out, membershipFolder{Folder: folder, Path: p})
	}
	return out, nil
}

// statementPlacements reads the subject statements (the kind and its declared
// sub-kinds) from the document's chunks to the schemes' concepts, with the visibility
// get_edges gives them: rejected, refuted and retired ones never, proposed ones when
// asked. Batched on the concept ids as eachConceptMember is.
func (d *Document) statementPlacements(ctx context.Context, key sqlmem.ScopeKey, docID string,
	schemes []memrank.OntologyScheme, includeProposed bool, issues *[]string) (map[string][]conceptPlacement, error) {
	var ids []string
	for _, s := range schemes {
		for _, c := range s.Concepts {
			if c.ChunkID != "" {
				ids = append(ids, c.ChunkID)
			}
		}
	}
	out := map[string][]conceptPlacement{}
	if len(ids) == 0 {
		return out, nil
	}
	kinds, _ := d.expandKindFilter(ctx, []string{"subject"})
	kph, kargs := inPlaceholders(kinds)
	vis, vargs := statementVisibility{includeProposed: includeProposed}.clause()
	for start := 0; start < len(ids); start += ontologyCountConceptBatch {
		iph, iargs := inPlaceholders(ids[start:min(start+ontologyCountConceptBatch, len(ids))])
		args := append(append(append([]any{docID}, kargs...), iargs...), vargs...)
		res, err := d.query(ctx, key, `SELECT e.id, e.from_id, e.to_id, e.kind, e.method, e.status, e.confidence
			  FROM chunk_edges e JOIN chunks c ON c.id = e.from_id
			 WHERE c.document_id = ? AND e.kind IN (`+kph+`) AND e.to_id IN (`+iph+`) AND `+vis+`
			 ORDER BY e.to_id, e.created_at, e.id`, args...)
		if err != nil {
			return nil, err
		}
		if res.Truncated {
			*issues = append(*issues, "the document has more placements than one read returns; some are not listed")
		}
		for _, r := range res.Rows {
			if len(r) < 7 {
				return nil, fmt.Errorf("placements: a row of %d columns", len(r))
			}
			to := asStr(r[2])
			out[to] = append(out[to], conceptPlacement{StatementID: asStr(r[0]), ChunkID: asStr(r[1]),
				Kind: asStr(r[3]), Method: asStr(r[4]), Status: asStr(r[5]), Confidence: asFloat64Ptr(r[6])})
		}
	}
	return out, nil
}

// tagPlacements finds, in a user or agent scope, the concepts the document's chunks
// carry by mirror tag: a tag names the deepest concept whose tag is it or a "/"-prefix
// of it, exactly the concepts a concept filter on that tag's closure matches.
func (d *Document) tagPlacements(ctx context.Context, key sqlmem.ScopeKey, docID string,
	schemes []memrank.OntologyScheme) (map[string][]conceptPlacement, error) {
	byTag := map[string]string{} // concept tag → chunk id
	for _, s := range schemes {
		for _, c := range s.Concepts {
			if c.Tag != "" && c.ChunkID != "" {
				if _, dup := byTag[c.Tag]; !dup {
					byTag[c.Tag] = c.ChunkID
				}
			}
		}
	}
	res, err := d.query(ctx, key, `SELECT t.chunk_id, t.tag FROM chunk_tags t JOIN chunks c ON c.id = t.chunk_id
		 WHERE c.document_id = ? ORDER BY t.tag, t.chunk_id`, docID)
	if err != nil {
		return nil, err
	}
	out := map[string][]conceptPlacement{}
	for _, r := range res.Rows {
		tag := asStr(r[1])
		for p := tag; p != ""; {
			if id, ok := byTag[p]; ok {
				out[id] = append(out[id], conceptPlacement{ChunkID: asStr(r[0]), Via: "tag", Tag: tag})
				break
			}
			i := strings.LastIndex(p, "/")
			if i < 0 {
				break
			}
			p = p[:i]
		}
	}
	return out, nil
}

// groupMemberships arranges the placements by scheme, adding each broader concept of
// a live placement as implied. Concepts sort by tag, which puts a broader concept
// before the narrower ones under it.
func groupMemberships(schemes []memrank.OntologyScheme, direct map[string][]conceptPlacement) []taxonomyMembership {
	out := []taxonomyMembership{}
	for _, s := range schemes {
		byTag := map[string]memrank.OntologyConcept{}
		for _, c := range s.Concepts {
			if _, dup := byTag[c.Tag]; !dup {
				byTag[c.Tag] = c
			}
		}
		listed := map[string]*memberConcept{}
		for _, c := range s.Concepts {
			ps := direct[c.ChunkID]
			if len(ps) == 0 || listed[c.Tag] != nil {
				continue
			}
			listed[c.Tag] = &memberConcept{Tag: c.Tag, Name: c.Name, Path: c.Path, ChunkID: c.ChunkID, Placements: ps}
		}
		for _, c := range s.Concepts {
			m := listed[c.Tag]
			if m == nil || m.Implied || !hasLivePlacement(m.Placements) {
				continue
			}
			for p := c.Tag; strings.Contains(p, "/"); {
				p = p[:strings.LastIndex(p, "/")]
				b, ok := byTag[p]
				if !ok {
					continue
				}
				if l := listed[p]; l != nil {
					// Listed for a proposal alone: still a member through this one.
					l.Implied = l.Implied || !hasLivePlacement(l.Placements)
					continue
				}
				listed[p] = &memberConcept{Tag: b.Tag, Name: b.Name, Path: b.Path, ChunkID: b.ChunkID, Implied: true}
			}
		}
		if len(listed) == 0 {
			continue
		}
		tm := taxonomyMembership{Scheme: s.Name}
		for _, m := range listed {
			tm.Concepts = append(tm.Concepts, *m)
		}
		sort.Slice(tm.Concepts, func(i, j int) bool { return tm.Concepts[i].Tag < tm.Concepts[j].Tag })
		out = append(out, tm)
	}
	return out
}

// hasLivePlacement: a placement that makes the document a member — anything but a
// proposal (a tag always does).
func hasLivePlacement(ps []conceptPlacement) bool {
	for _, p := range ps {
		if p.Status != edgeStatusProposed {
			return true
		}
	}
	return false
}
