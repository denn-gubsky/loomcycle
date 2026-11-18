package builtin

// document_ontology_retired.go — the `retired` entry status (RFC GQ P2.1, RFC FO 3.1a
// and 3.7 part B).
//
// A retired entry is IN FORCE FOR READS and OUT OF FORCE FOR NEW WRITES. It is not a
// third inert word: an inert entry vanishes from every read, which would turn a retired
// class's documents into undeclared types and its statements into undeclared kinds. So
// the reader keeps it, marked (EntryStatus), and each write path that could make
// something NEW use it asks here.
//
// STORAGE: the status is on each chunk. Retiring an entry through the operator's
// update_chunk sets it on the entry and, in one UPDATE, on every live descendant (RFC FO
// 3.1a: "retiring an entry retires its subtree in the same change"). The reader takes
// each chunk's own status and inherits nothing, so what an entry is can be read off its
// own chunk; a live entry left under a retired one (an operator cleared only the
// child's status, or added a child later) is reported as an issue, not silently
// re-read as retired. Un-retiring is the operator clearing the status, one chunk at a
// time.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// retiredIssues are the document-level advisories retired entries raise: a section
// header carrying the status (it cannot be retired; the status is ignored), a live
// entry under a retired one, and a review rule naming a retired kind (the rule still
// applies to the proposals it selects). An instance not yet adopted whose class is
// retired gets an issue of its own, since adoption skips it. With nothing retired it
// returns nil and touches nothing.
func retiredIssues(kids map[string][]ontologyChunk, rootID string, sectionOf map[string]string,
	name func(ontologyChunk) string, terms []memrank.OntologyTerm, relations []memrank.OntologyRelation,
	instances []memrank.OntologyInstance, review *memrank.ReviewPolicy) []string {
	var issues []string
	var walk func(chunks []ontologyChunk, under string)
	walk = func(chunks []ontologyChunk, under string) {
		for _, c := range chunks {
			if memrank.IsInertEntityStatus(c.status) {
				walk(kids[c.id], under)
				continue
			}
			n, retired := name(c), memrank.IsRetiredEntityStatus(c.status)
			if !retired && under != "" && n != "" {
				issues = append(issues, strconv.Quote(n)+" is live under the retired "+strconv.Quote(under)+
					": retire it too, or clear the status of "+strconv.Quote(under))
			}
			next := under
			if retired && under == "" && n != "" {
				next = n
			}
			walk(kids[c.id], next)
		}
	}
	for _, c := range kids[rootID] {
		kind, isSection := sectionOf[c.id]
		if !isSection {
			walk([]ontologyChunk{c}, "")
			continue
		}
		if memrank.IsRetiredEntityStatus(c.status) {
			issues = append(issues, "the section "+strconv.Quote(memrank.TrimOntologyName(c.title))+
				" cannot be retired; its status is ignored")
		}
		if kind != memrank.SectionReview {
			walk(kids[c.id], "")
		}
	}

	retiredKinds := map[string]bool{}
	for _, r := range relations {
		if r.EntryStatus != "" {
			retiredKinds[r.Name] = true
		}
	}
	if review != nil {
		for _, rule := range review.Rules {
			for _, k := range rule.Kinds {
				if retiredKinds[k] {
					issues = append(issues, fmt.Sprintf("Review rule %d names the retired relation kind %s; "+
						"it still rules on the proposals it selects", rule.Index, k))
				}
			}
		}
	}
	retiredClasses := map[string]bool{}
	for _, t := range terms {
		if t.EntryStatus != "" {
			retiredClasses[strings.ToLower(t.Name)] = true
		}
	}
	for i := range instances {
		in := &instances[i]
		if in.EntryStatus == "" && in.RegistryKey == "" && retiredClasses[strings.ToLower(in.Class)] {
			in.Issues = append(in.Issues, "@a "+in.Class+" is a retired class; the instance is not adopted while it is")
		}
	}
	return issues
}

// forNewWrites is the read with the retired concepts and instances left out: what a
// path that offers or places something NEW (classify) may choose from. Classes are
// filtered where they are consumed, after the seed layering — dropping a retired class
// here would let a seed class of the same name back in (RFC FO 3.7 B4). With nothing
// retired it returns read as it is; it never mutates the shared read.
func forNewWrites(read OntologyRead) OntologyRead {
	changed := false
	schemes := make([]memrank.OntologyScheme, 0, len(read.Schemes))
	for _, s := range read.Schemes {
		if s.EntryStatus != "" {
			changed = true
			continue
		}
		live := make([]memrank.OntologyConcept, 0, len(s.Concepts))
		for _, c := range s.Concepts {
			if c.EntryStatus == "" {
				live = append(live, c)
			}
		}
		if len(live) != len(s.Concepts) {
			changed = true
			s.Concepts = live
		}
		schemes = append(schemes, s)
	}
	instances := make([]memrank.OntologyInstance, 0, len(read.Instances))
	for _, in := range read.Instances {
		if in.EntryStatus == "" {
			instances = append(instances, in)
		}
	}
	if !changed && len(instances) == len(read.Instances) {
		return read
	}
	read.Schemes, read.Instances = schemes, instances
	return read
}

// retiredConceptChunks is the chunk ids of the retired schemes and concepts.
func retiredConceptChunks(read OntologyRead) map[string]bool {
	out := map[string]bool{}
	for _, s := range read.Schemes {
		if s.EntryStatus != "" {
			out[s.ChunkID] = true
		}
		for _, c := range s.Concepts {
			if c.EntryStatus != "" {
				out[c.ChunkID] = true
			}
		}
	}
	return out
}

// errEntryRetired is the refusal of a new write that would use a retired entry. A
// validation error, not a business one: the fact-relation path reads a business refusal
// as an operator's rejection and would report it as one.
func errEntryRetired(op, what string) tools.Result {
	return errValidation(op+": entry_retired: "+what+" is retired, so nothing new may use it; "+
		"what already uses it stays readable",
		"Use a live entry. An operator brings a retired entry back by clearing its status.")
}

// retiredNameRefusal refuses a proposal whose name is a retired entry's: the entry is
// brought back, not proposed again (RFC FO 3.1a).
func retiredNameRefusal(what string) *tools.Result {
	r := errBusiness("propose_entity: "+what+" exists and is retired; an operator brings it back by clearing its status",
		"Do not propose it again; ask an operator to un-retire it in the ontology document.")
	return &r
}

// retiredParentRefusal refuses a proposal filed under a retired entry.
func retiredParentRefusal(what string) *tools.Result {
	r := errBusiness("propose_entity: entry_retired: "+what+" is retired, so nothing new is filed under it",
		"Pass a live entry as parent.")
	return &r
}

// retiredProjectsRefusal refuses a proposed scheme header whose @projects names a
// retired class: a new @projects list may not name one (RFC FO 3.1a). Matched after the
// seed layering, case-insensitively, as the reader matches @projects.
func retiredProjectsRefusal(read OntologyRead, projects []string) *tools.Result {
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		if c.EntryStatus == "" {
			continue
		}
		for _, p := range projects {
			if strings.EqualFold(strings.TrimSpace(p), c.Name) {
				return retiredParentRefusal("the class " + c.Name + " (named in @projects)")
			}
		}
	}
	return nil
}

// copiedEdgeMethod reports the methods that copy relations someone else made (an
// import, a canvas, a peer's sync). A copy stays faithful to its source, as the
// vocabulary gate keeps it (annotated, never refused); every other new statement is a
// new use of its kind.
func copiedEdgeMethod(method string) bool {
	return method == edgeMethodImport || method == edgeMethodCanvas || method == edgeMethodSync
}

// retiredStatementRefusal refuses a NEW statement of a retired relation kind, or one
// from or to a retired concept (a new placement under it). Only a confirmed ontology
// governs; a fault fails open, like every other vocabulary check.
func (d *Document) retiredStatementRefusal(ctx context.Context, kind, from, to string) *tools.Result {
	// A reserved kind (about, derived_from, …) is the runtime's own and never a tenant
	// entry; skipping it keeps the per-fact runtime edges free of an ontology read.
	if d.Store == nil || d.SqlMem == nil || memrank.IsReservedRelation(kind) {
		return nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return nil
	}
	for _, r := range memrank.EffectiveRelations(read.Relations, hasSection(read.Sections, memrank.SectionRelations), true) {
		if r.Name == kind && r.EntryStatus != "" {
			res := errEntryRetired("link", "the relation kind "+strconv.Quote(kind))
			return &res
		}
	}
	retired := retiredConceptChunks(read)
	for _, id := range []string{from, to} {
		if retired[id] {
			res := errEntryRetired("link", "the concept "+id)
			return &res
		}
	}
	return nil
}

// retiredClassRefusal refuses a write that gives a chunk a retired class as its type
// when the chunk did not already have it, or sets a concept(...) field to a retired
// concept it did not already hold (RFC FO 3.7 B2: a new row, or a change of type to
// it, is refused; an edit of a row that already uses it is not). class is the resolved
// class of the write's type; selfID is the chunk an update_chunk edits.
func (d *Document) retiredClassRefusal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	in *docInput, class memrank.OntologyTerm, typ, selfID string, fields map[string]any) *tools.Result {
	read, ok := d.tenantOntologyRead(ctx)
	if !ok {
		return nil
	}
	retiredConcept := conceptFieldRetired(read, class)
	if class.EntryStatus == "" && retiredConcept == nil {
		return nil
	}
	existing := selfID
	if existing == "" && in.Op == "upsert_chunk" && strings.TrimSpace(in.NaturalKey) != "" {
		existing, _ = d.chunkIDByNaturalKey(ctx, key, in.NaturalKey)
	}
	var oldType string
	var oldFields map[string]any
	if existing != "" {
		if row, found, err := d.getChunkRow(ctx, key, existing); err == nil && found {
			oldType = row.Type
		}
		if cb, err := d.readBody(ctx, mscope, key, existing); err == nil {
			oldFields = decodeFields(cb.Fields)
		}
	}
	if class.EntryStatus != "" && !strings.EqualFold(strings.TrimSpace(oldType), strings.TrimSpace(typ)) {
		r := errEntryRetired(in.Op, "the class "+strconv.Quote(class.Name))
		return &r
	}
	for field, isRetired := range retiredConcept {
		v, _ := fields[field].(string)
		if v == "" || !isRetired(v) {
			continue
		}
		if old, _ := oldFields[field].(string); old == v {
			continue
		}
		r := errEntryRetired(in.Op, "the concept "+strconv.Quote(v)+" (field "+field+")")
		return &r
	}
	return nil
}

// conceptFieldRetired maps each concept(...) field of class to a test of whether a value
// names a retired concept of the field's scheme (by name or mirror tag). nil when no
// field's scheme has a retired concept, so the common case reads nothing more.
func conceptFieldRetired(read OntologyRead, class memrank.OntologyTerm) map[string]func(string) bool {
	var out map[string]func(string) bool
	specs := append(append([]memrank.FieldSpec{}, class.InheritedSpecs...), class.FieldSpecs...)
	for _, f := range specs {
		if f.Type != memrank.FieldConcept || f.Range == "" {
			continue
		}
		var retired []memrank.OntologyConcept
		for _, s := range read.Schemes {
			if !strings.EqualFold(s.Name, f.Range) && memrank.MirrorSegment(s.Name) != memrank.MirrorSegment(f.Range) {
				continue
			}
			for _, c := range s.Concepts {
				if c.EntryStatus != "" {
					retired = append(retired, c)
				}
			}
		}
		if len(retired) == 0 {
			continue
		}
		if out == nil {
			out = map[string]func(string) bool{}
		}
		out[f.Name] = func(v string) bool {
			v = strings.TrimSpace(v)
			for _, c := range retired {
				if strings.EqualFold(c.Name, v) || c.Tag == v {
					return true
				}
			}
			return false
		}
	}
	return out
}

// ontologyRetireSubtree is called by update_chunk BEFORE the revision moves when a call
// sets status `retired` on a chunk. Outside the tenant ontology the word means nothing
// and nothing is returned. Inside it, the root, a section header and a proposal cannot
// be retired (refused), and the result is the live descendants the same change retires.
// Only the operator reaches here for the ontology: guardOntologyWrite refuses an agent's
// update_chunk on it before the handler runs.
func (d *Document) ontologyRetireSubtree(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, row chunkRow) (subtree []string, isOntology bool, refused *tools.Result, err error) {
	if key.Scope != "tenant" {
		return nil, false, nil, nil
	}
	docID, err := d.resolveOntologyID(ctx, key)
	if err != nil || docID == "" || docID != row.DocumentID {
		return nil, false, nil, err
	}
	if row.ParentID == "" {
		r := errValidation("update_chunk: the ontology's root cannot be retired",
			"Retire an entry of a section instead; the root's status is the document's draft or confirmed.")
		return nil, true, &r, nil
	}
	if memrank.IsInertEntityStatus(row.Status) {
		r := errValidation("update_chunk: a proposal is not an entry and cannot be retired",
			"Accept or reject it on the Ontology page.")
		return nil, true, &r, nil
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath)
	if err != nil {
		return nil, true, nil, err
	}
	for kind, id := range read.SectionChunks {
		if id == row.ID {
			r := errValidation("update_chunk: the "+kind+" section header cannot be retired",
				"Retire the entries in it instead.")
			return nil, true, &r, nil
		}
	}
	res, err := d.query(ctx, key, `SELECT id, coalesce(parent_id, ''), coalesce(status, '') FROM chunks WHERE document_id = ?`, docID)
	if err != nil {
		return nil, true, nil, err
	}
	kids := map[string][]ontologyChunk{}
	for _, r := range res.Rows {
		if len(r) >= 3 {
			kids[asStr(r[1])] = append(kids[asStr(r[1])], ontologyChunk{id: asStr(r[0]), status: asStr(r[2])})
		}
	}
	var out []string
	seen := map[string]bool{row.ID: true}
	frontier := []string{row.ID}
	for len(frontier) > 0 {
		var next []string
		for _, id := range frontier {
			for _, k := range kids[id] {
				if seen[k.id] {
					continue
				}
				seen[k.id] = true
				next = append(next, k.id)
				// A proposal under the entry stays a proposal; one already retired keeps
				// its own revision.
				if !memrank.IsInertEntityStatus(k.status) && !memrank.IsRetiredEntityStatus(k.status) {
					out = append(out, k.id)
				}
			}
		}
		frontier = next
	}
	return out, true, nil, nil
}

// retireChunks sets the retired status on the entry and its subtree in ONE statement,
// so the subtree goes with its entry or not at all (RFC FO 3.1a). The entry's revision
// was already moved by update_chunk's guarded bump; each descendant's moves here, so an
// editor holding the old one must re-read.
func (d *Document) retireChunks(ctx context.Context, key sqlmem.ScopeKey, entryID string, subtree []string) error {
	ph, args := inPlaceholders(append([]string{entryID}, subtree...))
	return d.exec(ctx, key, `UPDATE chunks SET status = ?, updated_at = ?,
		revision = CASE WHEN id = ? THEN revision ELSE revision + 1 END WHERE id IN (`+ph+`)`,
		append([]any{memrank.OntologyStatusRetired, time.Now().UnixNano(), entryID}, args...)...)
}
