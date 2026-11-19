package builtin

// document_ontology_plan.go — what a change to the tenant ontology would do, before it
// is made (RFC GQ P1).
//
// A PLAN WRITES NOTHING. It reads the ontology uncached, applies the changes to the
// chunks in memory (ontologyOverlay) and parses the result with the same reader every
// other caller uses, so the issues it reports are the ones the changed document would
// report. Then it counts what rests on each entry it touches, across the tenant scope
// and every user and agent scope of the tenant (document_ontology_plan_counts.go), and
// classifies each change:
//
//   - destroys — a delete: rows keyed by a chunk id of the entry's subtree go with it,
//     and rows keyed by its name are left naming nothing (RFC GQ 9c counts both);
//   - stales   — a rename, move or retirement with dependents, or an edit of the
//     entry's directives (identity, characteristics, policy) with dependents;
//   - none     — nothing stored rests on what the change touches, or the edit changes
//     only the meaning (D2: such edits stay direct).
//
// The fingerprint is sha256 of ontologyFingerprint, taken BEFORE the read: an edit that
// lands between the two makes the plan's fingerprint stale, never the reverse.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

const (
	// ontologyPlanMaxChanges bounds one plan, so its counting work is bounded too.
	ontologyPlanMaxChanges = 50
	// ontologyPlanSample is how many ids a count lists (tenant scope only).
	ontologyPlanSample = 5
	// ontologyPlanMaxName / ontologyPlanMaxBody bound a rename's name and an edit's body.
	ontologyPlanMaxName = 200
	ontologyPlanMaxBody = 32 << 10
)

// What an entry is (connector.OntologyPlanEntry.Kind).
const (
	planKindClass    = "class"
	planKindRelation = "relation"
	planKindScheme   = "scheme"
	planKindConcept  = "concept"
	planKindInstance = "instance"
)

// planEntry is one in-force entry of the ontology as a plan addresses it.
type planEntry struct {
	connector.OntologyPlanEntry
	tag         string // concept: its mirror tag; scheme: its own tag segment
	registryKey string // instance: the key its subject was adopted under
	locked      bool   // its body carries @locked (document_ontology_locked.go)
}

// plannedChange is one change resolved against the ontology as read.
type plannedChange struct {
	in    connector.OntologyChange
	entry planEntry
	// subtree is the entry's chunk and every chunk under it, whatever its status: what
	// a delete removes.
	subtree []string
	// under is the in-force entries below the entry, in document order.
	under []planEntry
	// What the name-keyed counts match: class names as declared, matched under Unicode
	// case folding (the entry's and its subclasses'), relation kinds (the entry's and its sub-kinds'), concept tags (a
	// document tagged with one, or under one, counts), and the concepts and instances
	// of the subtree.
	classes, kinds, tags []string
	concepts             []string
	instances            []planEntry
	// An edit's effect, from comparing the entry before and after it.
	directivesChanged                bool
	newlyFunctional, newlyAsymmetric bool
	editRenames                      bool // the edit's body changes the entry's name
	notes                            []string
	counts                           map[string]*connector.OntologyPlanCount
	// proposal is the proposal this change stands for (op proposal), "" for a direct one.
	proposal string
}

// planRows is the ontology document's chunk tree, every status included.
type planRows struct {
	kids   map[string][]string
	parent map[string]string
}

// PlanOntology answers what changes would do to the tenant ontology's dependents. It
// writes nothing. ctx carries the tenant (the run identity) and the tenant-scope
// grants; the caller decides who may ask (the connector admits operators only).
func (d *Document) PlanOntology(ctx context.Context, changes []connector.OntologyChange) (connector.PlanOntologyResult, error) {
	if d.Store == nil || d.SqlMem == nil {
		out := connector.PlanOntologyResult{Changes: []connector.OntologyPlannedChange{}, Issues: []string{}, NewIssues: []string{}}
		if err := validatePlanChanges(changes); err != nil {
			return out, err
		}
		return out, connector.WithMessage(connector.ErrOntologyPlanUnavailable, "planning ontology changes needs SQL Memory; none is configured")
	}
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return connector.PlanOntologyResult{Changes: []connector.OntologyPlannedChange{}, Issues: []string{}, NewIssues: []string{}},
			connector.WithMessage(connector.ErrOntologyPlanUnavailable, err.Error())
	}
	return d.planOntologyAt(ctx, key, mscope, changes)
}

// planOntologyAt is PlanOntology on a tenant scope key already resolved. The delete
// paths call it with the key of the call they guard (document_ontology_delete.go), so
// "has dependents" is the plan's own answer, not a second count.
func (d *Document) planOntologyAt(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, changes []connector.OntologyChange) (connector.PlanOntologyResult, error) {
	ctx = noWrites(ctx)
	out := connector.PlanOntologyResult{Changes: []connector.OntologyPlannedChange{}, Issues: []string{}, NewIssues: []string{}}
	if err := validatePlanChanges(changes); err != nil {
		return out, err
	}
	docID, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		return out, planReadErr(err)
	}
	if docID == "" {
		return out, connector.WithMessage(connector.ErrOntologyPlanNotFound, "this tenant has no ontology document")
	}
	if out.Fingerprint, err = d.ontologyPlanFingerprint(ctx, key, docID); err != nil {
		return out, planReadErr(err)
	}

	before, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return out, planReadErr(err)
	}
	if before.DocumentID == "" {
		return out, connector.WithMessage(connector.ErrOntologyPlanNotFound, "this tenant has no ontology document")
	}
	rows, err := d.planChunkRows(ctx, key, before.DocumentID)
	if err != nil {
		return out, planReadErr(err)
	}
	entries := planEntries(before)
	items, err := d.expandPlanProposals(ctx, key, mscope, changes, before)
	if err != nil {
		return out, err
	}
	ov := newOntologyOverlay()
	planned := make([]*plannedChange, 0, len(items))
	for _, it := range items {
		if it.fixed != nil {
			continue
		}
		p, err := d.resolvePlanChange(ctx, key, mscope, it.index, it.change, before, rows, entries, ov)
		if err != nil {
			return out, err
		}
		p.proposal = it.proposal
		planned = append(planned, p)
	}

	after, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, ov)
	if err != nil {
		return out, planReadErr(err)
	}
	had := map[string]bool{}
	for _, s := range ontologyIssues(before) {
		had[s] = true
	}
	out.Issues = append(out.Issues, ontologyIssues(after)...)
	for _, s := range out.Issues {
		if !had[s] {
			out.NewIssues = append(out.NewIssues, s)
		}
	}
	for _, p := range planned {
		if p.in.Op != connector.OntologyPlanEdit {
			continue
		}
		// An edit is judged against the ontology with that edit alone: in `after`, a
		// rename, move or delete elsewhere in the request (of the entry, or an ancestor)
		// changes its declaration too and would read as a directive change.
		alone := after
		if len(planned) > 1 {
			only := newOntologyOverlay()
			only.bodies[p.entry.ChunkID] = ov.bodies[p.entry.ChunkID]
			if alone, err = d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, only); err != nil {
				return out, planReadErr(err)
			}
		}
		compareEditedEntry(before, alone, p)
		if a, ok := planEntries(alone)[p.entry.ChunkID]; ok && a.Name != p.entry.Name {
			p.editRenames = true // a body heading names an untitled entry
		}
	}
	for _, p := range planned {
		p.noteLock(entries, ov)
	}

	counted, skipped, complete, err := d.countPlanDependents(ctx, key, before, after, planned)
	if err != nil {
		return out, planReadErr(err)
	}
	out.ScopesCounted, out.ScopesSkipped, out.Complete = counted, skipped, complete
	next := 0
	for _, it := range items {
		if it.fixed != nil {
			out.Changes = append(out.Changes, *it.fixed)
			continue
		}
		out.Changes = append(out.Changes, planned[next].result())
		next++
	}
	return out, nil
}

// ontologyPlanFingerprint is the fingerprint a plan reports: sha256 of
// ontologyFingerprint, in hex. A destructive change compares a caller's against it
// (document_ontology_fingerprint.go).
func (d *Document) ontologyPlanFingerprint(ctx context.Context, key sqlmem.ScopeKey, docID string) (string, error) {
	raw, err := d.ontologyFingerprint(ctx, key, docID)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), nil
}

// planReadErr is the plan's error for a failed read. A tenant scope whose document
// tables are older than this runtime's schema is reported, not migrated: the plan
// writes nothing, and any document write to the scope brings them up to date.
func planReadErr(err error) error {
	if schemaMissing(err) || errors.Is(err, sqlmem.ErrPlaneAbsent) {
		return connector.WithMessage(connector.ErrOntologyPlanUnavailable,
			"the tenant scope's document tables are not at this runtime's schema; a plan writes nothing, so it does not migrate them (any document write to the tenant scope does)")
	}
	return fmt.Errorf("ontology plan: %w", err)
}

// validatePlanChanges checks the request's shape; whether each entry exists is
// resolvePlanChange's question.
func validatePlanChanges(changes []connector.OntologyChange) error {
	if len(changes) == 0 {
		return connector.WithMessage(connector.ErrOntologyPlanInvalid, "changes is required: at least one {op, entry}")
	}
	if len(changes) > ontologyPlanMaxChanges {
		return connector.WithMessage(connector.ErrOntologyPlanInvalid,
			fmt.Sprintf("a plan takes at most %d changes; got %d", ontologyPlanMaxChanges, len(changes)))
	}
	for i, ch := range changes {
		at := "changes[" + strconv.Itoa(i) + "]"
		if ch.Op == connector.OntologyPlanProposal {
			if strings.TrimSpace(ch.ID) == "" || ch.Entry != "" || ch.Name != "" || ch.Parent != "" || ch.Body != "" {
				return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": proposal needs id (a proposal's chunk id) and nothing else")
			}
			continue
		}
		if ch.ID != "" {
			return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": id is for op proposal only")
		}
		if strings.TrimSpace(ch.Entry) == "" {
			return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": entry is required (an entry's chunk id)")
		}
		switch ch.Op {
		case connector.OntologyPlanRetire, connector.OntologyPlanDelete:
		case connector.OntologyPlanRename:
			name := strings.TrimSpace(ch.Name)
			if name == "" || strings.ContainsAny(name, "\r\n") || utf8.RuneCountInString(name) > ontologyPlanMaxName {
				return connector.WithMessage(connector.ErrOntologyPlanInvalid,
					at+": rename needs name, one line of at most "+strconv.Itoa(ontologyPlanMaxName)+" characters")
			}
		case connector.OntologyPlanMove:
			if strings.TrimSpace(ch.Parent) == "" {
				return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": move needs parent (a chunk id)")
			}
		case connector.OntologyPlanEdit:
			if strings.TrimSpace(ch.Body) == "" || len(ch.Body) > ontologyPlanMaxBody {
				return connector.WithMessage(connector.ErrOntologyPlanInvalid,
					at+": edit needs body, at most "+strconv.Itoa(ontologyPlanMaxBody)+" bytes")
			}
		default:
			return connector.WithMessage(connector.ErrOntologyPlanInvalid,
				at+": unknown op "+strconv.Quote(ch.Op)+" (retire, rename, move, edit, delete or proposal)")
		}
	}
	return nil
}

// planChunkRows reads the ontology document's chunk tree, every status included: a
// delete takes proposals under an entry with it.
func (d *Document) planChunkRows(ctx context.Context, key sqlmem.ScopeKey, docID string) (planRows, error) {
	out := planRows{kids: map[string][]string{}, parent: map[string]string{}}
	res, err := d.query(ctx, key, `SELECT id, coalesce(parent_id, '') FROM chunks WHERE document_id = ? ORDER BY position`, docID)
	if err != nil {
		return out, err
	}
	if res.Truncated {
		return out, fmt.Errorf("the ontology document has more chunks than one read returns")
	}
	for _, r := range res.Rows {
		id, parent := asStr(r[0]), asStr(r[1])
		out.parent[id] = parent
		out.kids[parent] = append(out.kids[parent], id)
	}
	return out, nil
}

// subtree is id and every chunk under it, breadth first. Bounded by the rows read, so
// it terminates even on a malformed tree.
func (r planRows) subtree(id string) []string {
	out := []string{id}
	seen := map[string]bool{id: true}
	for i := 0; i < len(out); i++ {
		for _, k := range r.kids[out[i]] {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

// planEntries indexes the read's in-force entries by chunk id.
func planEntries(r OntologyRead) map[string]planEntry {
	out := map[string]planEntry{}
	put := func(id, name, section, kind string, locked bool) *planEntry {
		if id == "" {
			return nil
		}
		if _, dup := out[id]; dup {
			return nil
		}
		e := planEntry{OntologyPlanEntry: connector.OntologyPlanEntry{ChunkID: id, Name: name, Section: section, Kind: kind}, locked: locked}
		out[id] = e
		return &e
	}
	for _, t := range r.Terms {
		// By the class's own chunk: a second in-force class of the same name is an entry
		// too (its lock counts). classChunks is the fallback for a term built elsewhere.
		id := t.ChunkID
		if id == "" {
			id = r.classChunks[strings.ToLower(t.Name)]
		}
		put(id, t.Name, memrank.SectionClasses, planKindClass, t.Locked)
	}
	for _, rel := range r.Relations {
		put(rel.ChunkID, rel.Name, memrank.SectionRelations, planKindRelation, rel.Locked)
	}
	for _, s := range r.Schemes {
		if e := put(s.ChunkID, s.Name, memrank.SectionSchemes, planKindScheme, s.Locked); e != nil {
			e.tag = memrank.MirrorTag(s.Name, nil)
			out[s.ChunkID] = *e
		}
		for _, c := range s.Concepts {
			if e := put(c.ChunkID, c.Name, memrank.SectionSchemes, planKindConcept, c.Locked); e != nil {
				e.tag = c.Tag
				out[c.ChunkID] = *e
			}
		}
	}
	for _, in := range r.Instances {
		if e := put(in.ChunkID, in.Name, memrank.SectionInstances, planKindInstance, in.Locked); e != nil {
			e.registryKey = in.RegistryKey
			out[in.ChunkID] = *e
		}
	}
	return out
}

// resolvePlanChange resolves change i against the read and records its effect on the
// overlay. A change naming no in-force entry is refused rather than planned as a
// no-op: a plan that says "none" for an id it never found would read as safe.
func (d *Document) resolvePlanChange(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, i int,
	ch connector.OntologyChange, before OntologyRead, rows planRows, entries map[string]planEntry, ov *ontologyOverlay) (*plannedChange, error) {
	at := "changes[" + strconv.Itoa(i) + "]"
	e, ok := entries[strings.TrimSpace(ch.Entry)]
	if !ok {
		return nil, connector.WithMessage(connector.ErrOntologyPlanNotFound,
			at+": "+strconv.Quote(ch.Entry)+" is not an in-force entry of the tenant ontology (a class, relation, scheme, concept or instance chunk; a section, the root and a proposal are not entries)")
	}
	p := &plannedChange{in: ch, entry: e, subtree: rows.subtree(e.ChunkID), counts: map[string]*connector.OntologyPlanCount{}}
	inSubtree := map[string]bool{}
	for _, id := range p.subtree {
		inSubtree[id] = true
		if u, ok := entries[id]; ok && id != e.ChunkID {
			p.under = append(p.under, u)
		}
	}
	p.collectNames()

	switch ch.Op {
	case connector.OntologyPlanRetire, connector.OntologyPlanDelete:
		// A retired entry is read as gone here, as a deleted one is: what the changed
		// ontology reports (a scheme projecting a class no longer there, a relation
		// whose domain names nothing) is the same for both until P2 gives retirement a
		// status of its own.
		ov.gone[e.ChunkID] = true
		if ch.Op == connector.OntologyPlanRetire {
			p.notes = append(p.notes, "the ontology after the change is read as if the entry and its subtree were gone; its dependents are kept")
		}
	case connector.OntologyPlanRename:
		name := strings.TrimSpace(ch.Name)
		ov.titles[e.ChunkID] = name
		for _, other := range entries {
			if other.ChunkID != e.ChunkID && other.Kind == e.Kind && strings.EqualFold(other.Name, name) {
				p.notes = append(p.notes, "another "+e.Kind+" is already named "+strconv.Quote(other.Name))
				break
			}
		}
		if e.Kind == planKindClass {
			p.notes = append(p.notes, "a class rename with dependents is refused until the rename job exists (RFC GQ P5): stored types, identity and subject keys would keep the old name")
		}
	case connector.OntologyPlanMove:
		parent := strings.TrimSpace(ch.Parent)
		if err := planMoveParent(at, e, parent, before, entries, inSubtree); err != nil {
			return nil, err
		}
		if rows.parent[e.ChunkID] == parent {
			p.notes = append(p.notes, "the entry is already under that parent")
		}
		ov.parents[e.ChunkID] = parent
	case connector.OntologyPlanEdit:
		cur, err := d.readBody(ctx, mscope, key, e.ChunkID)
		if err != nil {
			return nil, fmt.Errorf("ontology plan: %w", err)
		}
		ov.bodies[e.ChunkID] = chunkBody{Body: ch.Body, Fields: cur.Fields}
	}
	return p, nil
}

// collectNames fills what the name-keyed counts match, from the entry and the entries
// of its subtree. A rename changes only the entry's own name: rows typed with a
// subclass, or statements of a sub-kind, keep theirs (RFC GQ 4.2), so only the entry's
// class or kind is matched. A concept's tag carries its ancestors' segments, so its
// tag prefix (below) still covers the subtree.
func (p *plannedChange) collectNames() {
	for i, e := range append([]planEntry{p.entry}, p.under...) {
		renamedOnly := p.in.Op == connector.OntologyPlanRename && i > 0
		switch e.Kind {
		case planKindClass:
			if !renamedOnly {
				p.classes = appendUnique(p.classes, e.Name)
			}
		case planKindRelation:
			if !renamedOnly {
				p.kinds = appendUnique(p.kinds, e.Name)
			}
		case planKindConcept:
			p.concepts = append(p.concepts, e.ChunkID)
		case planKindInstance:
			p.instances = append(p.instances, e)
		}
	}
	// A tag names its narrower concepts' tags as prefixes, so the entry's own tag
	// covers its subtree; a scheme's covers every concept of it.
	if p.entry.Kind == planKindConcept || p.entry.Kind == planKindScheme {
		p.tags = []string{p.entry.tag}
	}
}

func appendUnique(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// planMoveParent checks a move's new parent: the entry's own section (the root, for a
// class of a document without sections), or an in-force entry of the same section
// that can hold it — never the entry itself or anything under it.
func planMoveParent(at string, e planEntry, parent string, r OntologyRead, entries map[string]planEntry, inSubtree map[string]bool) error {
	refuse := func(why string) error {
		return connector.WithMessage(connector.ErrOntologyPlanNotFound, at+": parent "+strconv.Quote(parent)+" "+why)
	}
	if inSubtree[parent] {
		return refuse("is the entry or under it")
	}
	section := r.SectionChunks[e.Section]
	if section == "" && e.Section == memrank.SectionClasses {
		section = r.RootChunkID
	}
	if parent == section {
		if e.Kind == planKindConcept {
			return refuse("is the Schemes section; a concept goes under a scheme or another concept")
		}
		return nil
	}
	pe, ok := entries[parent]
	if !ok || pe.Section != e.Section {
		return refuse("is not the entry's section or an in-force entry of it")
	}
	switch e.Kind {
	case planKindScheme:
		return refuse("is an entry; a scheme sits directly under the Schemes section")
	case planKindConcept:
		if pe.Kind != planKindScheme && pe.Kind != planKindConcept {
			return refuse("is not a scheme or a concept")
		}
	}
	return nil
}

// ontologyIssues lists a read's advisories, each prefixed with where it was found.
func ontologyIssues(r OntologyRead) []string {
	out := []string{}
	add := func(where string, issues ...string) {
		for _, s := range issues {
			if s != "" {
				out = append(out, where+": "+s)
			}
		}
	}
	add("ontology", r.Issues...)
	if r.DepthCapped {
		add("ontology", "classes nest deeper than "+strconv.Itoa(memrank.OntologyMaxDepth)+" levels; the deeper ones were flattened")
	}
	for _, t := range r.Terms {
		add("class "+strconv.Quote(t.Name), append(append([]string{}, t.Issues...), t.MemoryScopeIssue)...)
	}
	for _, rel := range r.Relations {
		add("relation "+strconv.Quote(rel.Name), append(append([]string{}, rel.Issues...), rel.NameIssue)...)
	}
	for _, s := range r.Schemes {
		add("scheme "+strconv.Quote(s.Name), s.Issues...)
		for _, c := range s.Concepts {
			add("concept "+strconv.Quote(c.Tag), c.Issues...)
		}
	}
	for _, in := range r.Instances {
		add("instance "+strconv.Quote(in.Name), in.Issues...)
	}
	if r.Review != nil {
		add("review", r.Review.Issues...)
	}
	return out
}

// compareEditedEntry sets what an edit changes beyond the meaning: the entry's parsed
// declaration with its meaning and advisories left out, before and after. A
// characteristic the edit adds is recorded so the statements it would break are counted.
func compareEditedEntry(before, after OntologyRead, p *plannedChange) {
	p.directivesChanged = entrySignature(before, p.entry) != entrySignature(after, p.entry)
	if p.entry.Kind != planKindRelation {
		if !p.directivesChanged {
			p.notes = append(p.notes, "the edit changes the meaning only; it needs no plan to apply")
		}
		return
	}
	was, now := relationByChunk(before, p.entry.ChunkID), relationByChunk(after, p.entry.ChunkID)
	p.newlyFunctional = now.Functional && !was.Functional
	p.newlyAsymmetric = now.Asymmetric && !was.Asymmetric
	if !p.directivesChanged {
		p.notes = append(p.notes, "the edit changes the meaning only; it needs no plan to apply")
	}
}

func relationByChunk(r OntologyRead, id string) memrank.OntologyRelation {
	for _, rel := range r.Relations {
		if rel.ChunkID == id {
			return rel
		}
	}
	return memrank.OntologyRelation{}
}

// entrySignature is an entry's parsed declaration without its meaning, advisories or
// (for a scheme) concepts, as JSON. "" when the read does not hold the entry.
func entrySignature(r OntologyRead, e planEntry) string {
	var v any
	switch e.Kind {
	case planKindClass:
		for _, t := range r.Terms {
			if r.classChunks[strings.ToLower(t.Name)] == e.ChunkID {
				t.Meaning, t.Issues, t.MemoryScopeIssue = "", nil, ""
				v = t
				break
			}
		}
	case planKindRelation:
		if rel := relationByChunk(r, e.ChunkID); rel.ChunkID != "" {
			rel.Meaning, rel.Issues, rel.NameIssue = "", nil, ""
			v = rel
		}
	case planKindScheme, planKindConcept:
		for _, s := range r.Schemes {
			if s.ChunkID == e.ChunkID {
				s.Meaning, s.Issues, s.Concepts = "", nil, nil
				v = s
			}
			for _, c := range s.Concepts {
				if c.ChunkID == e.ChunkID {
					c.Meaning, c.Issues = "", nil
					v = c
				}
			}
		}
	case planKindInstance:
		for _, in := range r.Instances {
			if in.ChunkID == e.ChunkID {
				in.Issues = nil
				v = in
			}
		}
	}
	if v == nil {
		return ""
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// result renders the change's plan: its counts, each with what the change does to
// it, and its risk class — the worst effect any non-zero count has.
func (p *plannedChange) result() connector.OntologyPlannedChange {
	out := connector.OntologyPlannedChange{Op: p.in.Op, Entry: p.entry.OntologyPlanEntry, Risk: connector.OntologyRiskNone,
		Counts: map[string]connector.OntologyPlanCount{}, Notes: p.notes, Proposal: p.proposal}
	for _, u := range p.under {
		out.Subtree = append(out.Subtree, u.OntologyPlanEntry)
	}
	names := make([]string, 0, len(p.counts))
	for name := range p.counts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := *p.counts[name]
		c.Effect = planEffect(name, p)
		out.Counts[name] = c
		if c.N == 0 {
			continue
		}
		switch c.Effect {
		case connector.OntologyEffectDestroyed, connector.OntologyEffectOrphaned:
			out.Risk = connector.OntologyRiskDestroys
		case connector.OntologyEffectStale:
			if out.Risk == connector.OntologyRiskNone {
				out.Risk = connector.OntologyRiskStales
			}
		}
	}
	return out
}

// planEffects is what each op does to each kind of dependent; a pair not listed is
// kept. editDirectives is an edit that changes more than the meaning (an edit of the
// meaning alone keeps everything, D2).
const editDirectives = "edit-directives"

var planEffects = map[string]map[string]string{
	// Rows keyed by a chunk id of the subtree: a delete removes them.
	"statements":       {connector.OntologyPlanDelete: connector.OntologyEffectDestroyed, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale},
	"node_annotations": {connector.OntologyPlanDelete: connector.OntologyEffectDestroyed, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale},
	"members":          {connector.OntologyPlanDelete: connector.OntologyEffectDestroyed, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale},
	// Rows a delete leaves behind (node statistics and series are keyed by a concept's
	// chunk id but not deleted with it; a class's by its name).
	"node_stats":     {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale},
	"measure_points": {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale},
	// Rows keyed by a class name.
	"typed_documents":      {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale, editDirectives: connector.OntologyEffectStale},
	"typed_chunks":         {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale, editDirectives: connector.OntologyEffectStale},
	"identity_keys":        {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale, editDirectives: connector.OntologyEffectStale},
	"subject_keys":         {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale},
	"document_annotations": {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, editDirectives: connector.OntologyEffectStale},
	"instances":            {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale},
	"projects":             {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale},
	// Rows keyed by a relation kind.
	"statements_of_kind":    {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale, editDirectives: connector.OntologyEffectStale},
	"characteristic_breaks": {editDirectives: connector.OntologyEffectStale},
	"review_rules":          {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale},
	// Rows keyed by a concept's tag path.
	"concept_tags":   {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale, connector.OntologyPlanRename: connector.OntologyEffectStale, connector.OntologyPlanMove: connector.OntologyEffectStale},
	"policy_changes": {connector.OntologyPlanMove: connector.OntologyEffectStale, editDirectives: connector.OntologyEffectStale},
	// An instance's adopted subject and what is said about it.
	"subject":     {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale},
	"facts_about": {connector.OntologyPlanDelete: connector.OntologyEffectOrphaned, connector.OntologyPlanRetire: connector.OntologyEffectStale},
}

// planEffect is what the change does to the named dependent.
func planEffect(name string, p *plannedChange) string {
	op := p.in.Op
	if op == connector.OntologyPlanEdit {
		if !p.directivesChanged {
			return connector.OntologyEffectKept
		}
		op = editDirectives
	}
	// A concept's statistics and series are keyed by its chunk id, so a rename keeps
	// them; a class's are keyed by its name, so a rename leaves them under the old one.
	if op == connector.OntologyPlanRename && p.entry.Kind == planKindClass && (name == "node_stats" || name == "measure_points") {
		return connector.OntologyEffectStale
	}
	if e, ok := planEffects[name][op]; ok {
		return e
	}
	return connector.OntologyEffectKept
}
