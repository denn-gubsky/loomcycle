package builtin

// document_ontology_change_proposal.go — a proposal to CHANGE or RETIRE an entry that is
// in force (RFC GQ P4.1, D1, D12). propose_entity with change "edit" or "retire".
//
// THE SHAPE extends the whole-scheme proposal's (#2729, RFC GQ 9b C5): one proposed chunk,
// its body the filer's reason with every directive line quoted (neutraliseDirectives),
// and what the change would do as STRUCTURED, INERT data in the chunk's fields under a
// third mark beside scheme_proposal and concept_proposal:
//
//	{"change_proposal": memrank.ProposedChange{op, target, target_name, section, kind,
//	                                           name?, parent?, parent_name?, meaning?,
//	                                           sets_directives?, directives?}}
//
// The chunk is filed under the target's SECTION, never under the target: the reader walks
// a proposal's children against the nearest in-force ancestor, so a proposal under its
// target would read as a proposed child of it. Its title is "<op>: <target name>", so it
// collides with no name an add proposal checks for duplicates.
//
// Inert three ways, as the scheme proposal is: the chunk is proposed, so every reader
// skips it; nothing it would change is in a body; and the target is not touched. Only an
// operator accepts, and accepting a change proposal comes with P4.2 (AcceptChangeProposal,
// which re-checks every field here against the ontology as it then stands). Until then the
// operator's proposals route refuses to accept one (ChangeProposalMarked), so the generic
// accept — which clears the proposal's own status — can never put "retire: X" in force as
// a class named that. Rejecting is the route's ordinary reject.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	changeProposalField = "change_proposal"
	// changeMaxDirectives / changeMaxDirectiveValue bound an edit's directive set.
	changeMaxDirectives     = 32
	changeMaxDirectiveValue = 1000
	// lockedDirective is RFC FO 3.1a's @locked (RFC GQ D5). A proposal may neither set
	// nor clear it, so it is refused here by name whether or not this build parses it.
	lockedDirective = "locked"
)

// changeDirectives are the directives an edit may set, per kind of entry: the names each
// section's parser acts on (ParseClassBody, ParseRelationBody, ParseSchemeBody,
// ParseConceptBody, ParseInstanceBody). @section and @version on a class are absent on
// purpose: on a root child they would turn a class into a section.
var changeDirectives = map[string]map[string]bool{
	planKindClass: directiveSet("memory_scope", "identity", "norm", "identifier_pattern", "title_pattern", "definitions",
		"boilerplate", "anchor_grammar", "citation_grammar", "exclude", "renamed_from", "annotate"),
	planKindRelation: directiveSet("inverse", "domain", "range", "symmetric", "asymmetric", "irreflexive", "transitive",
		"transitive_within", "functional", "qualifiers", "discriminator", "dated", "walk", "auto_assert", "renamed_from"),
	planKindScheme:   directiveSet("ordered", "render", "domain", "projects", "origin", "source", "version", "period"),
	planKindConcept:  directiveSet("notation", "alt", "exact_match", "focus", "retention", "acl"),
	planKindInstance: directiveSet("a", "identity", "alt", "nesting"),
}

func directiveSet(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// wantsChange reports a propose_entity call carrying any field of a change proposal.
func wantsChange(in docInput) bool {
	return strings.TrimSpace(in.Change) != "" || strings.TrimSpace(in.Entry) != "" ||
		in.Meaning != nil || len(in.Directives) > 0
}

// changeTarget is a change proposal's target resolved against the ontology as read.
type changeTarget struct {
	read    OntologyRead
	entries map[string]planEntry
	rows    planRows
	retired map[string]string
	entry   planEntry
}

// proposeChange files a change or retire proposal (propose_entity with change set).
func (d *Document) proposeChange(ctx context.Context, in docInput) (tools.Result, error) {
	op := strings.ToLower(strings.TrimSpace(in.Change))
	if r := checkChangeShape(op, in); r != nil {
		return *r, nil
	}
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return errBusiness("propose_entity: "+err.Error(), "This deployment has no SQL Memory; ask an operator to enable it."), nil
	}
	tg, r, err := d.resolveChangeTarget(ctx, key, mscope, in)
	if err != nil {
		return errFrom("propose_entity: "+err.Error(), err), nil
	}
	if r != nil {
		return *r, nil
	}
	e := tg.entry
	if why := tg.retired[e.ChunkID]; why != "" {
		if op == memrank.ChangeOpRetire {
			return errBusiness("propose_entity: already_retired: "+why+" is already retired",
				"Nothing to propose; an operator brings it back by clearing its status."), nil
		}
		return *retiredNameRefusal(why), nil
	}
	mark := memrank.ProposedChange{Op: op, Target: e.ChunkID, TargetName: e.Name, Section: e.Section, Kind: e.Kind}
	if op == memrank.ChangeOpEdit {
		if r := tg.fillEdit(&mark, in); r != nil {
			return *r, nil
		}
	}
	// A lock refuses retiring, renaming or moving the entry and any ancestor of a locked
	// one (RFC FO 3.1a), and an edit of a locked entry could clear it.
	ids := []string{e.ChunkID}
	if op == memrank.ChangeOpRetire || mark.Name != "" || mark.Parent != "" {
		ids = tg.rows.subtree(e.ChunkID)
	}
	locked, err := d.lockedAmong(ctx, key, mscope, ids, tg.entries)
	if err != nil {
		return errFrom("propose_entity: "+err.Error(), err), nil
	}
	if locked != "" {
		return errBusiness("propose_entity: entry_locked: "+locked+" carries @locked, so no proposal may "+op+
			" it (or an entry above it); only an operator's own edit removes the lock",
			"Ask an operator; a proposal cannot set or clear @locked."), nil
	}
	for _, p := range tg.read.Proposals {
		if p.Change != nil && p.Status == memrank.OntologyStatusProposed && p.Change.Target == e.ChunkID && p.Change.Op == op {
			return errBusiness("propose_entity: already_proposed: an open "+op+" proposal for "+strconv.Quote(e.Name)+
				" exists ("+p.ChunkID+")", "Do not propose it again; an operator decides."), nil
		}
	}
	return d.fileChangeProposal(ctx, key, mscope, tg, mark, in.Body)
}

// checkChangeShape refuses what is malformed before anything is read.
func checkChangeShape(op string, in docInput) *tools.Result {
	refuse := func(msg, fix string) *tools.Result { r := errValidation("propose_entity: "+msg, fix); return &r }
	switch op {
	case memrank.ChangeOpEdit, memrank.ChangeOpRetire:
	case "":
		return refuse("entry, meaning and directives belong to a change proposal: pass change=\"edit\" or \"retire\"",
			"Omit them to propose a new entry.")
	default:
		return refuse("change must be \"edit\" or \"retire\", not "+strconv.Quote(in.Change), "")
	}
	if strings.TrimSpace(in.Entry) == "" {
		return refuse("entry is required: the in-force entry to "+op+", by chunk id (or name)", "")
	}
	if wantsWholeScheme(in) {
		return refuse("concepts and header describe a NEW scheme, not a change", "")
	}
	if op == memrank.ChangeOpRetire && (strings.TrimSpace(in.Name) != "" || strings.TrimSpace(in.Parent) != "" ||
		in.Meaning != nil || len(in.Directives) > 0) {
		return refuse("a retire proposal names only its entry; name, parent, meaning and directives belong to change=\"edit\"", "")
	}
	if len(in.Body) > proposalBodyMax || (in.Meaning != nil && len(*in.Meaning) > proposalBodyMax) {
		return refuse(fmt.Sprintf("body and meaning are each at most %d bytes", proposalBodyMax), "")
	}
	return nil
}

// resolveChangeTarget reads the ontology and finds the entry the proposal names: by chunk
// id, else by name (or concept tag) within section when given. Only an entry is a
// target: the root, a section header and a proposal are refused.
func (d *Document) resolveChangeTarget(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, in docInput) (changeTarget, *tools.Result, error) {
	var tg changeTarget
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return tg, nil, err
	}
	if read.DocumentID == "" || read.RootChunkID == "" {
		r := errBusiness("propose_entity: this tenant has no ontology document yet", "")
		return tg, &r, nil
	}
	if tg.rows, err = d.planChunkRows(ctx, key, read.DocumentID); err != nil {
		return tg, nil, err
	}
	tg.read, tg.entries, tg.retired = read, planEntries(read), retiredEntryChunks(read)
	want := strings.TrimSpace(in.Entry)
	refuse := func(msg string) (changeTarget, *tools.Result, error) {
		r := errValidation("propose_entity: "+msg, "Pass an entry's chunk id from describe_ontology.")
		return tg, &r, nil
	}
	if want == read.RootChunkID {
		return refuse("the ontology's root is not an entry; it cannot be changed or retired by a proposal")
	}
	for kind, id := range read.SectionChunks {
		if want == id {
			return refuse("the " + kind + " section header is not an entry; it cannot be changed or retired by a proposal")
		}
	}
	if e, ok := tg.entries[want]; ok {
		tg.entry = e
		return tg, nil, nil
	}
	section := strings.ToLower(strings.TrimSpace(in.Section))
	var found []planEntry
	for _, e := range tg.entries {
		if (section == "" || e.Section == section) && (strings.EqualFold(e.Name, want) || (e.tag != "" && e.tag == want)) {
			found = append(found, e)
		}
	}
	switch len(found) {
	case 1:
		tg.entry = found[0]
		return tg, nil, nil
	case 0:
		return refuse(strconv.Quote(want) + " is not an entry in force in this tenant's ontology (a class, relation, scheme, " +
			"concept or instance; a section, the root and a proposal are not entries)")
	}
	ids := make([]string, 0, len(found))
	for _, e := range found {
		ids = append(ids, e.Kind+" "+e.ChunkID)
	}
	sort.Strings(ids)
	return refuse(strconv.Quote(want) + " names more than one entry (" + strings.Join(ids, ", ") + "); pass its chunk id")
}

// fillEdit sets an edit's changes on mark, refusing one that changes nothing or that
// would make something new use a retired or taken name or parent.
func (tg changeTarget) fillEdit(mark *memrank.ProposedChange, in docInput) *tools.Result {
	e := tg.entry
	refuse := func(msg string) *tools.Result { r := errValidation("propose_entity: "+msg, ""); return &r }
	parentID := tg.rows.parent[e.ChunkID]
	if want := strings.TrimSpace(in.Parent); want != "" {
		id, name, r := tg.resolveChangeParent(want)
		if r != nil {
			return r
		}
		if id == parentID {
			return refuse(strconv.Quote(e.Name) + " is already under that parent")
		}
		mark.Parent, mark.ParentName, parentID = id, name, id
	}
	if name := strings.TrimSpace(in.Name); name != "" {
		if strings.ContainsAny(name, "\r\n") || utf8.RuneCountInString(name) > ontologyPlanMaxName {
			return refuse("name is one line of at most " + strconv.Itoa(ontologyPlanMaxName) + " characters")
		}
		if name != e.Name {
			if r := tg.nameRefusal(name, parentID); r != nil {
				return r
			}
			mark.Name = name
		}
	}
	if in.Meaning != nil {
		m := strings.TrimSpace(*in.Meaning)
		if m == "" {
			return refuse("meaning is empty; omit it to keep the entry's meaning")
		}
		mark.Meaning = neutraliseDirectives(m)
	}
	if len(in.Directives) > 0 {
		dirs, r := checkChangeDirectives(e.Kind, e.Name, in.Directives)
		if r != nil {
			return r
		}
		mark.SetsDirectives, mark.Directives = true, dirs
	}
	if mark.Name == "" && mark.Parent == "" && in.Meaning == nil && !mark.SetsDirectives {
		return refuse("an edit proposal changes nothing: pass a new name, parent, meaning or directives")
	}
	return nil
}

// resolveChangeParent finds an edit's new parent: the target's section header, or an
// in-force entry of its section (by chunk id, else by name or concept tag) that can hold
// it, never the target or anything under it, and never a retired entry.
func (tg changeTarget) resolveChangeParent(want string) (id, name string, refusal *tools.Result) {
	e := tg.entry
	section := tg.read.SectionChunks[e.Section]
	if section == "" && e.Section == memrank.SectionClasses {
		section = tg.read.RootChunkID
	}
	refuse := func(msg string) (string, string, *tools.Result) {
		r := errValidation("propose_entity: parent "+strconv.Quote(want)+" "+msg, "")
		return "", "", &r
	}
	id = want
	if _, ok := tg.entries[want]; !ok && want != section {
		var hits []string
		for _, p := range tg.entries {
			if p.Section == e.Section && (strings.EqualFold(p.Name, want) || (p.tag != "" && p.tag == want)) {
				hits = append(hits, p.ChunkID)
			}
		}
		if len(hits) != 1 {
			return refuse("names no single entry of the " + e.Section + " section; pass its chunk id")
		}
		id = hits[0]
	}
	inSubtree := map[string]bool{}
	for _, c := range tg.rows.subtree(e.ChunkID) {
		inSubtree[c] = true
	}
	if err := planMoveParent("propose_entity", e, id, tg.read, tg.entries, inSubtree); err != nil {
		r := errValidation(err.Error(), "")
		return "", "", &r
	}
	if why := tg.retired[id]; why != "" {
		return "", "", retiredParentRefusal(why)
	}
	return id, tg.entries[id].Name, nil
}

// nameRefusal refuses a rename to a name an entry of the same kind already has (beside
// it, for a concept, whose tag is its parent's plus its own), pointing at un-retiring
// when that entry is retired (RFC FO 3.1a).
func (tg changeTarget) nameRefusal(name, parentID string) *tools.Result {
	e := tg.entry
	if e.Kind == planKindRelation {
		if iss := memrank.RelationNameIssue(name); iss != "" || memrank.IsReservedRelation(name) {
			r := errValidation("propose_entity: "+strconv.Quote(name)+" is not a name a relation kind can take "+iss, "")
			return &r
		}
	}
	same := func(a, b string) bool { return strings.EqualFold(a, b) }
	if e.Kind == planKindConcept || e.Kind == planKindScheme {
		same = func(a, b string) bool { return memrank.MirrorSegment(a) == memrank.MirrorSegment(b) }
	}
	for _, o := range tg.entries {
		if o.ChunkID == e.ChunkID || o.Kind != e.Kind || !same(o.Name, name) {
			continue
		}
		if e.Kind == planKindConcept && tg.rows.parent[o.ChunkID] != parentID {
			continue
		}
		if why := tg.retired[o.ChunkID]; why != "" {
			return retiredNameRefusal(why)
		}
		r := errBusiness("propose_entity: the "+e.Kind+" "+strconv.Quote(o.Name)+" is already in force under that name",
			"Pick another name.")
		return &r
	}
	return nil
}

// checkChangeDirectives decodes an edit's directive set — {name: value | true | [values]}
// — and checks it with the parser of the entry's section: only that section's directives,
// one line each, and no value the parser reports as unusable. Returned in name order.
func checkChangeDirectives(kind, entryName string, raw json.RawMessage) ([]memrank.ProposedDirective, *tools.Result) {
	refuse := func(msg string) ([]memrank.ProposedDirective, *tools.Result) {
		r := errValidation("propose_entity: directives: "+msg,
			"Pass an object of directive name to value: a string, true for a flag, or a list for a repeated directive.")
		return nil, &r
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return refuse("not an object")
	}
	if len(m) > changeMaxDirectives {
		return refuse(fmt.Sprintf("at most %d directives", changeMaxDirectives))
	}
	allowed := changeDirectives[kind]
	var out []memrank.ProposedDirective
	for rawName, v := range m {
		name := strings.TrimPrefix(strings.TrimSpace(rawName), "@")
		if strings.EqualFold(name, lockedDirective) {
			return refuse("@locked cannot be set or cleared by a proposal (RFC FO 3.1a); only an operator's own edit does that")
		}
		if !allowed[name] {
			return refuse("@" + name + " is not a directive a " + kind + " takes (" + strings.Join(directiveNames(allowed), ", ") + ")")
		}
		values, err := directiveValues(v)
		if err != nil {
			return refuse("@" + name + ": " + err.Error())
		}
		for _, val := range values {
			out = append(out, memrank.ProposedDirective{Name: name, Value: val})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	if issues := changeDirectiveIssues(kind, entryName, directiveLines(out)); len(issues) > 0 {
		return refuse(strings.Join(issues, "; "))
	}
	return out, nil
}

// directiveValues decodes one directive's value: a string, true (a flag; false sets
// nothing), or a list of strings for a directive that repeats. Each is one line.
func directiveValues(v json.RawMessage) ([]string, error) {
	var flag bool
	if json.Unmarshal(v, &flag) == nil {
		if flag {
			return []string{""}, nil
		}
		return nil, nil
	}
	var one string
	var many []string
	switch {
	case json.Unmarshal(v, &one) == nil:
		many = []string{one}
	case json.Unmarshal(v, &many) == nil:
	default:
		return nil, errors.New("a value is a string, true, or a list of strings")
	}
	for i, s := range many {
		s = strings.TrimSpace(s)
		if strings.ContainsAny(s, "\r\n") || utf8.RuneCountInString(s) > changeMaxDirectiveValue {
			return nil, fmt.Errorf("each value is one line of at most %d characters", changeMaxDirectiveValue)
		}
		many[i] = s
	}
	return many, nil
}

// directiveLines renders directives as the bare lines an entry's body declares them in.
func directiveLines(ds []memrank.ProposedDirective) string {
	lines := make([]string, 0, len(ds))
	for _, dir := range ds {
		lines = append(lines, strings.TrimSpace("@"+dir.Name+" "+dir.Value))
	}
	return strings.Join(lines, "\n")
}

// changeDirectiveIssues is what the section's own parser reports of body.
func changeDirectiveIssues(kind, name, body string) []string {
	switch kind {
	case planKindClass:
		decl := memrank.ParseClassBody(body)
		if decl.MemoryScopeIssue != "" {
			return append(decl.Issues, decl.MemoryScopeIssue)
		}
		return decl.Issues
	case planKindRelation:
		return memrank.ParseRelationBody(name, body).Issues
	case planKindScheme:
		return memrank.ParseSchemeBody(name, body).Issues
	case planKindConcept:
		return memrank.ParseConceptBody(name, name, []string{name}, body).Issues
	case planKindInstance:
		return memrank.ParseInstanceBody(name, body).Issues
	}
	return []string{"no directives are known for a " + kind}
}

func directiveNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lockedAmong is the name of the first chunk of ids whose body declares @locked, "" for
// none. Read from the bodies, so it holds before this build parses the directive.
func (d *Document) lockedAmong(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, ids []string, entries map[string]planEntry) (string, error) {
	for _, id := range ids {
		b, err := d.readBody(ctx, mscope, key, id)
		if err != nil {
			return "", err
		}
		for _, dir := range memrank.ParseDirectives(b.Body) {
			if dir.Name == lockedDirective {
				if e, ok := entries[id]; ok {
					return strconv.Quote(e.Name), nil
				}
				return id, nil
			}
		}
	}
	return "", nil
}

// fileChangeProposal writes the proposal chunk under the target's section.
func (d *Document) fileChangeProposal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope,
	tg changeTarget, mark memrank.ProposedChange, reason string) (tools.Result, error) {
	parent := tg.read.SectionChunks[mark.Section]
	if parent == "" {
		parent = tg.read.RootChunkID // a class of a document without sections
	}
	fields, _ := json.Marshal(map[string]any{changeProposalField: mark})
	title := mark.Op + ": " + mark.TargetName
	id, err := d.createProposalChunk(ctx, key, mscope, tg.read.DocumentID, parent, title, reason, fields)
	if err != nil {
		return errFrom("propose_entity: "+err.Error(), err), nil
	}
	return jsonResult(map[string]any{
		"proposed":  title,
		"chunk_id":  id,
		"parent_id": parent,
		"change":    mark,
		"note": "Filed as a suggestion, not in force: nothing changes for " + strconv.Quote(mark.TargetName) +
			" or any run until an operator accepts it.",
	})
}

// withChangeMarks sets Change on each proposal carrying a change_proposal mark, read from
// its chunk's fields.
func withChangeMarks(ps []memrank.OntologyProposal, chunkOf func(string) chunkBody) []memrank.OntologyProposal {
	for i := range ps {
		var ch memrank.ProposedChange
		if proposalMark(chunkOf(ps[i].ChunkID).Fields, changeProposalField, &ch) {
			ps[i].Change = &ch
		}
	}
	return ps
}

// ChangeProposalMarked reports whether chunkID's fields carry a change_proposal mark at
// all — readable or not, so a mark edited into garbage still counts. For the operator's
// proposals route, which must not accept one by clearing its status (see the file
// comment).
func (d *Document) ChangeProposalMarked(ctx context.Context, scope, chunkID string) (bool, error) {
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return false, err
	}
	b, err := d.readBody(ctx, mscope, key, chunkID)
	if err != nil {
		return false, err
	}
	var m map[string]json.RawMessage
	if len(b.Fields) == 0 || json.Unmarshal(b.Fields, &m) != nil {
		return false, nil
	}
	_, ok := m[changeProposalField]
	return ok, nil
}
