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
	"errors"
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
// entry under a retired one, and a review rule naming a retired kind (its rejections
// still apply; an acceptance of a retired kind is refused by applyRuling). An instance not yet adopted whose class is
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
		// Review rules are list items, and an unknown section's entries are not in force
		// at all: neither holds entries a status could apply to.
		if kind != memrank.SectionReview && kind != sectionOutOfForce {
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
						"it may still reject the proposals it selects, but none it accepts goes in force", rule.Index, k))
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

// retiredConceptChunks maps the chunk id of each retired scheme and concept to its
// mirror tag, which names it in a refusal.
func retiredConceptChunks(read OntologyRead) map[string]string {
	out := map[string]string{}
	for _, s := range read.Schemes {
		if s.EntryStatus != "" {
			out[s.ChunkID] = memrank.MirrorTag(s.Name, nil)
		}
		for _, c := range s.Concepts {
			if c.EntryStatus != "" {
				out[c.ChunkID] = c.Tag
			}
		}
	}
	return out
}

// ErrEntryRetired is an operator-plane write refused because it would make something new
// use a retired entry (RFC GQ P2.1).
var ErrEntryRetired = errors.New("entry_retired")

// retiredClassNamed is the retired class typ names after the seed layering
// (case-insensitively, as a type is matched), if it is one.
func retiredClassNamed(read OntologyRead, typ string) (memrank.OntologyTerm, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" {
		return memrank.OntologyTerm{}, false
	}
	for _, c := range memrank.EffectiveClasses(read.Terms, true) {
		if c.EntryStatus != "" && strings.EqualFold(c.Name, typ) {
			return c, true
		}
	}
	return memrank.OntologyTerm{}, false
}

// errEntryRetired is the refusal of a new write that would use a retired entry. A
// validation error, not a business one: the fact-relation path reads a business refusal
// as an operator's rejection and would report it as one.
// op is "" where the caller prefixes its own op name.
func errEntryRetired(op, what string) tools.Result {
	if op != "" {
		op += ": "
	}
	return errValidation(op+"entry_retired: "+what+" is retired, so nothing new may use it; "+
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

// retiredNewUseRefusal is retiredStatementRefusal for a statement that may revise
// another (propose_statement with id). A revision replaces the statement it revises, so
// what it shares with one IN FORCE is no new use (RFC FO 3.7 B2): only a kind or an end
// the revision changes is checked. A base not in force (proposed, rejected, expired or
// withheld by a judge) uses nothing yet, so its revision is checked in full.
func (d *Document) retiredNewUseRefusal(ctx context.Context, key sqlmem.ScopeKey, kind, from, to, revisionOf string) (*tools.Result, error) {
	if revisionOf != "" {
		base, err := d.statementByID(ctx, key, revisionOf)
		if err != nil {
			return nil, err
		}
		if statementInForce(base) {
			if base.Kind == kind {
				kind = ""
			}
			if base.FromID == from {
				from = ""
			}
			if base.ToID == to {
				to = ""
			}
		}
	}
	if kind == "" && from == "" && to == "" {
		return nil, nil
	}
	return d.retiredStatementRefusal(ctx, key, kind, from, to), nil
}

// statementInForce: asserted, not expired, and not withheld by a refuting verdict.
func statementInForce(s *statementRow) bool {
	return s != nil && s.Status == edgeStatusAsserted && s.ExpiredAt == nil &&
		(s.VerdictConfidence == nil || *s.VerdictConfidence >= withholdBelowConfidence)
}

// retiredStatementRefusal refuses a NEW statement of a retired relation kind, or one
// from or to a retired entry: a concept (a new placement under it), a class or relation
// entry's chunk, or a retired instance — its entry chunk or the subject it was adopted
// as. "" for kind, from or to is not checked. Only a confirmed ontology governs; a
// fault fails open, like every other vocabulary check.
func (d *Document) retiredStatementRefusal(ctx context.Context, key sqlmem.ScopeKey, kind, from, to string) *tools.Result {
	// A reserved kind (about, derived_from, …) is the runtime's own and never a tenant
	// entry; skipping it keeps the per-fact runtime edges free of an ontology read.
	if d.Store == nil || d.SqlMem == nil || memrank.IsReservedRelation(kind) {
		return nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return nil
	}
	if kind != "" {
		for _, r := range memrank.EffectiveRelations(read.Relations, hasSection(read.Sections, memrank.SectionRelations), true) {
			if r.Name == kind && r.EntryStatus != "" {
				res := errEntryRetired("", "the relation kind "+strconv.Quote(kind))
				return &res
			}
		}
	}
	if from == "" && to == "" {
		return nil
	}
	retired := retiredEntryChunks(read)
	for _, id := range []string{from, to} {
		if what := retired[id]; id != "" && what != "" {
			res := errEntryRetired("", what)
			return &res
		}
	}
	// An adopted instance's subject lives in the tenant scope under the key adoption
	// recorded on the instance; one never adopted has no subject to protect.
	if key.Scope != "tenant" {
		return nil
	}
	for _, in := range read.Instances {
		if in.EntryStatus == "" || in.RegistryKey == "" {
			continue
		}
		subject, err := d.chunkIDByNaturalKey(ctx, key, in.RegistryKey)
		if err != nil || subject == "" {
			continue
		}
		if subject == from || subject == to {
			res := errEntryRetired("", "the instance "+strconv.Quote(in.Name))
			return &res
		}
	}
	return nil
}

// retiredEntryChunks names, by chunk id, every retired entry a statement could point
// at: concepts and schemes (by mirror tag), classes, relation kinds and instances.
func retiredEntryChunks(read OntologyRead) map[string]string {
	out := map[string]string{}
	for id, tag := range retiredConceptChunks(read) {
		out[id] = "the concept " + tag
	}
	for _, t := range read.Terms {
		if id := read.classChunkID(t.Name); t.EntryStatus != "" && id != "" {
			out[id] = "the class " + strconv.Quote(t.Name)
		}
	}
	for _, r := range read.Relations {
		if r.EntryStatus != "" && r.ChunkID != "" {
			out[r.ChunkID] = "the relation kind " + strconv.Quote(r.Name)
		}
	}
	for _, in := range read.Instances {
		if in.EntryStatus != "" && in.ChunkID != "" {
			out[in.ChunkID] = "the instance " + strconv.Quote(in.Name)
		}
	}
	return out
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
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
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

// retiredProposalRefusal refuses a proposal, filed at parentID with title in the
// ontology document docID, that would make something new use a retired entry: one
// filed under a retired entry, or one named as a retired entry of the same section (in
// the Schemes section, under the same parent, as a concept's tag is). selfID is the
// proposal's own chunk when it exists. The root and the section headers are not entries
// (a status on them is ignored), so they never count as a retired ancestor. The same
// rules propose_entity applies, for a proposal that did not come through it: an
// agent's raw create_chunk/upsert_chunk, or one accepted after its parent was retired.
func (d *Document) retiredProposalRefusal(ctx context.Context, key sqlmem.ScopeKey, docID, selfID, parentID, title string) (*tools.Result, error) {
	res, err := d.query(ctx, key, `SELECT id, coalesce(parent_id, ''), coalesce(status, ''), coalesce(title, '') FROM chunks WHERE document_id = ?`, docID)
	if err != nil {
		return nil, err
	}
	type node struct{ parent, status, name string }
	nodes := map[string]node{}
	root := ""
	for _, r := range res.Rows {
		if len(r) < 4 {
			continue
		}
		n := node{parent: asStr(r[1]), status: asStr(r[2]), name: memrank.TrimOntologyName(asStr(r[3]))}
		nodes[asStr(r[0])] = n
		if n.parent == "" {
			root = asStr(r[0])
		}
	}
	// sectionOf walks up to the child of the root; "" for the root or an unknown chunk.
	sectionOf := func(id string) string {
		for seen := 0; id != "" && seen <= len(nodes); seen++ {
			n, ok := nodes[id]
			if !ok || n.parent == "" {
				return ""
			}
			if n.parent == root {
				return id
			}
			id = n.parent
		}
		return ""
	}
	section := sectionOf(parentID)
	if parentID == root || section == "" {
		return nil, nil // a section header of its own, or outside any section
	}
	for id, seen := parentID, 0; id != section && seen <= len(nodes); seen++ {
		n := nodes[id]
		if memrank.IsRetiredEntityStatus(n.status) {
			return retiredParentRefusal(strconv.Quote(n.name)), nil
		}
		id = n.parent
	}
	name := memrank.TrimOntologyName(title)
	if name == "" {
		return nil, nil
	}
	schemes := false
	if read, ok := d.tenantOntologyRead(ctx); ok {
		schemes = read.SectionChunks[memrank.SectionSchemes] == section
	}
	for id, n := range nodes {
		if id == selfID || !memrank.IsRetiredEntityStatus(n.status) || sectionOf(id) != section {
			continue
		}
		if schemes && n.parent != parentID {
			continue
		}
		if strings.EqualFold(n.name, name) || (schemes && memrank.MirrorSegment(n.name) == memrank.MirrorSegment(name)) {
			return retiredNameRefusal(strconv.Quote(n.name)), nil
		}
	}
	return nil, nil
}

// RetiredProposalProblem says why accepting the ontology proposal chunkID would make
// something new use a retired entry (filed under one, or named as one), "" when it
// would not. For the operator's accept route, which writes the proposal's status alone.
func (d *Document) RetiredProposalProblem(ctx context.Context, scope, chunkID string) (string, error) {
	key, _, err := d.resolveScope(ctx, scope)
	if err != nil {
		return "", err
	}
	row, found, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil || !found {
		return "", err
	}
	r, err := d.retiredProposalRefusal(ctx, key, row.DocumentID, row.ID, row.ParentID, row.Title)
	if err != nil || r == nil {
		return "", err
	}
	return r.Text, nil
}

// retiredTagRefusal refuses a tag write that would place something new under a retired
// concept. Outside the tenant scope a concept's members are found by its mirror tag
// (resolveConceptFilters), so a NEW tag equal to a retired scheme's or concept's, or
// under it, is a placement there; a tag the target already carries is kept (an edit,
// RFC FO 3.7 B2). In the tenant scope membership is by statement, which
// retiredStatementRefusal guards, so a tag there places nothing. The dispatch calls
// this for the direct tag writes; import_md and the other faithful copies write tags
// internally and keep what they copy.
func (d *Document) retiredTagRefusal(ctx context.Context, key sqlmem.ScopeKey, in docInput) *tools.Result {
	if key.Scope == "tenant" || len(in.Tags) == 0 {
		return nil
	}
	switch in.Op {
	case "create_document", "create_chunk", "upsert_chunk", "update_chunk", "add_tags":
	default:
		return nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return nil
	}
	var retired []string
	for _, tag := range retiredConceptChunks(read) {
		retired = append(retired, tag)
	}
	if len(retired) == 0 {
		return nil
	}
	have := map[string]bool{}
	var kept []string
	switch {
	case in.Op == "update_chunk" || (in.Op == "add_tags" && in.ID != ""):
		kept, _ = d.listChunkTags(ctx, key, in.ID)
	case in.Op == "add_tags" && in.DocumentID != "":
		kept, _ = d.listDocumentTags(ctx, key, in.DocumentID)
	case in.Op == "upsert_chunk" && strings.TrimSpace(in.NaturalKey) != "":
		if id, err := d.chunkIDByNaturalKey(ctx, key, in.NaturalKey); err == nil && id != "" {
			kept, _ = d.listChunkTags(ctx, key, id)
		}
	}
	for _, t := range kept {
		have[t] = true
	}
	for _, t := range sanitizeTags(in.Tags) {
		if have[t] {
			continue
		}
		for _, r := range retired {
			if t == r || strings.HasPrefix(t, r+"/") {
				res := errEntryRetired(in.Op, "the concept "+r+" (tag "+t+")")
				return &res
			}
		}
	}
	return nil
}
