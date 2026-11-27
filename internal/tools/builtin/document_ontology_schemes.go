package builtin

// document_ontology_schemes.go — the Schemes section on the runtime side (RFC FO P1c-1):
// the tag mirror that gives every concept its cross-scope identity, and the concept
// proposals a curator may file.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// SchemeMirrorReport says what the tag mirror changed (or, dry run, would change).
type SchemeMirrorReport struct {
	// Concepts is how many concept chunks were looked at.
	Concepts int `json:"concepts"`
	// Stale lists the concepts whose tags were (or would be) changed, by mirror tag.
	Stale   []string `json:"stale,omitempty"`
	Added   int      `json:"added"`
	Removed int      `json:"removed"`
}

// MirrorSchemeTags gives every concept chunk of the run's tenant ontology its mirror tag
// (RFC FO decision 10f.2) and removes mirror-shaped tags that no longer match — a
// concept renamed or moved keeps no tag from its old place. With dryRun nothing is
// written and the report says what would change.
//
// It touches CONCEPT CHUNKS ONLY. A tag an operator or agent put on a document is that
// document's classification, and the mirror has no say over it. A concept's rename or
// move carries the documents filed under it to the new tag through the re-tag job
// (document_ontology_retag_job.go), which in turn leaves these chunks to the mirror.
//
// "Mirror-shaped" means the first segment is a declared scheme's mirror segment: a
// concept chunk's other tags are left alone. Idempotent: a second run changes nothing.
func (d *Document) MirrorSchemeTags(ctx context.Context, dryRun bool) (SchemeMirrorReport, error) {
	var rep SchemeMirrorReport
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return rep, err
	}
	// Read directly, not through the cache: a mirror run is an operator action that
	// must see the document as it is now, and it is rare enough not to need the cache.
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return rep, err
	}
	if read.partial {
		// A concept whose body could not be read still has its title, so its tag is
		// right; but an unreadable SECTION body hides the whole section. Never strip tags
		// on a read that may have lost part of the tree.
		return rep, fmt.Errorf("the ontology could not be read completely; try again")
	}
	roots := map[string]bool{}
	for _, s := range read.Schemes {
		roots[memrank.MirrorTag(s.Name, nil)] = true
	}
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			rep.Concepts++
			have, err := d.listChunkTags(ctx, key, c.ChunkID)
			if err != nil {
				return rep, err
			}
			want := []string{c.Tag}
			added, removed := true, 0
			for _, t := range have {
				switch {
				case t == c.Tag:
					added = false
				case roots[strings.SplitN(t, "/", 2)[0]]:
					removed++ // a mirror tag from the concept's old name or place
				default:
					want = append(want, t)
				}
			}
			if !added && removed == 0 {
				continue
			}
			rep.Stale = append(rep.Stale, c.Tag)
			if added {
				rep.Added++
			}
			rep.Removed += removed
			if dryRun {
				continue
			}
			sort.Strings(want)
			if err := d.replaceChunkTags(ctx, key, c.ChunkID, want); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

// focusEdgeKind is the statement a concept's @focus becomes (RFC GM D15). Seeded in the
// relation vocabulary, so it is in force wherever a Relations section is.
const focusEdgeKind = "focus"

// focusMirrorOp names the focus mirror's own retire and revive on the change feed.
const focusMirrorOp = "ontology_focus"

// SchemeFocusReport says what the focus mirror changed (or, dry run, would change).
type SchemeFocusReport struct {
	// Concepts is how many concepts declare a @focus.
	Concepts int `json:"concepts"`
	// Stale lists each change as "<concept tag> -> <target>", with "(retire)" for a
	// statement whose @focus was removed or changed.
	Stale   []string `json:"stale,omitempty"`
	Written int      `json:"written"`
	Retired int      `json:"retired"`
	// Kept counts the @focus pairs already joined by a live focus statement another
	// method wrote (a person's link, say). That statement is left as it is and stands
	// for the @focus: never counted as written, and no ontology statement is written
	// beside it. Reported on every confirm, so it is not a change.
	Kept int `json:"kept"`
	// Errors name a @focus that could not be written this time; its concept's existing
	// statements are kept until it can.
	Errors []string `json:"errors,omitempty"`
}

// MirrorSchemeFocus writes each concept's @focus as a `focus` statement, method
// ontology, from the concept's chunk to its target, and retires (expires — never
// deletes) the ontology's focus statements no @focus declares any more. With dryRun
// nothing is written and the report says what would change. Idempotent: a second run
// changes nothing.
//
// THE TARGET. A class's target is its chunk in the ontology document, found as
// instance_of finds it. An instance's target is the registry SUBJECT it was adopted as,
// not its entry in the Instances section: instance_of and the instances' own relations
// are written between the adopted subjects, and the subject is where the consolidator
// files facts — so "documents about X" (the concept) and "facts about X" (the registry)
// meet at one node. That is why this runs after AdoptInstances.
//
// What cannot be resolved NOW — a seed class with no chunk, an instance not adopted yet,
// a store fault — is an error, and that concept's existing statements are kept: a
// transient failure must not retire what the operator declared. A @focus that names
// nothing at all is the reader's advisory, and retires what the concept had.
func (d *Document) MirrorSchemeFocus(ctx context.Context, dryRun bool) (SchemeFocusReport, error) {
	var rep SchemeFocusReport
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return rep, err
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return rep, err
	}
	if read.partial {
		// A lost section body would read as every @focus removed.
		return rep, fmt.Errorf("the ontology could not be read completely; try again")
	}
	classes := memrank.EffectiveClasses(read.Terms, true)
	declared := map[string]string{}
	for _, c := range classes {
		declared[strings.ToLower(c.Name)] = c.Name
	}

	type pair struct{ from, to string }
	want := map[pair]string{}    // → "<tag> -> <target>"
	held := map[string]bool{}    // concept chunks whose statements are kept as they are
	tagOf := map[string]string{} // concept chunk → tag, to name what is retired
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			tagOf[c.ChunkID] = c.Tag
			if c.Focus == "" {
				continue
			}
			rep.Concepts++
			target, ok, _ := memrank.ResolveFocus(c.Focus, classes, read.Instances)
			if !ok {
				continue
			}
			if c.EntryStatus != "" || s.EntryStatus != "" || target.Retired {
				// Retired at either end (RFC FO 3.1a): no new focus statement, and the
				// one already written is kept, not retired with it.
				held[c.ChunkID] = true
				continue
			}
			to, why, err := d.focusTargetChunk(ctx, key, mscope, read, target, declared)
			if err != nil {
				return rep, err
			}
			if to == "" {
				held[c.ChunkID] = true
				rep.Errors = append(rep.Errors, c.Tag+": @focus "+c.Focus+": "+why)
				continue
			}
			want[pair{c.ChunkID, to}] = c.Tag + " -> " + target.Name
		}
	}

	res, err := d.query(ctx, key, `SELECT id, from_id, to_id, coalesce(expired_at, 0), coalesce(status, '') FROM chunk_edges
		WHERE kind = ? AND method = ?`, focusEdgeKind, edgeMethodOntology)
	if err != nil {
		return rep, err
	}
	type edge struct {
		id, status string
		live       bool
	}
	have := map[pair]edge{}
	for _, row := range res.Rows {
		if len(row) < 5 {
			continue
		}
		have[pair{asStr(row[1]), asStr(row[2])}] = edge{id: asStr(row[0]), live: asInt64Val(row[3]) == 0, status: asStr(row[4])}
	}
	// A statement another method wrote on the SAME KEY as the ontology's (from, to,
	// kind, no valid_at, no discriminator): link_chunks would only hand that row back,
	// so writing would report a change that never happens, on every confirm. One in
	// force is kept; one that is not (proposed, rejected, withheld, retired) blocks the
	// ontology's and is reported, never counted as kept or written. A dated or
	// discriminated one has another key and does not stand in for the ontology's.
	res, err = d.query(ctx, key, `SELECT e.from_id, e.to_id, CASE WHEN `+edgeLiveClause+` THEN 1 ELSE 0 END,
		coalesce(e.status, ''), coalesce(e.expired_at, 0), e.method FROM chunk_edges e
		WHERE e.kind = ? AND e.method <> ? AND e.valid_at IS NULL AND e.discriminator = ''`,
		focusEdgeKind, edgeMethodOntology)
	if err != nil {
		return rep, err
	}
	foreign := map[pair]string{} // → "" when in force, else why it blocks the ontology's
	for _, row := range res.Rows {
		if len(row) < 6 {
			continue
		}
		why := ""
		switch {
		case asInt64Val(row[2]) == 1:
		case asInt64Val(row[4]) != 0:
			why = "a retired " + asStr(row[5]) + " focus statement holds this pair; remove it to let the ontology's be written"
		default:
			why = "a " + asStr(row[3]) + " " + asStr(row[5]) + " focus statement that is not in force holds this pair; " +
				"rule on it (or remove it) to let the ontology's be written"
		}
		foreign[pair{asStr(row[0]), asStr(row[1])}] = why
	}

	pairs := make([]pair, 0, len(want))
	for p := range want {
		pairs = append(pairs, p)
	}
	sort.Slice(pairs, func(i, j int) bool { return want[pairs[i]] < want[pairs[j]] })
	for _, p := range pairs {
		e, exists := have[p]
		if exists && e.live {
			continue
		}
		if why, ok := foreign[p]; ok {
			if why == "" {
				rep.Kept++
			} else {
				rep.Errors = append(rep.Errors, want[p]+": "+why)
			}
			continue
		}
		if exists && e.status == edgeStatusRejected {
			// Retired after an operator rejected it: re-declaring the @focus does not
			// overrule that.
			rep.Errors = append(rep.Errors, want[p]+": the ontology's focus statement was rejected; "+
				"it stays rejected until an operator re-asserts it")
			continue
		}
		rep.Stale = append(rep.Stale, want[p])
		if dryRun {
			rep.Written++
			continue
		}
		if exists {
			// Re-declared after it was retired: the same statement comes back rather than
			// a second one beside it (its key would collide anyway).
			if err := d.exec(ctx, key, `UPDATE chunk_edges SET expired_at = NULL
				WHERE id = ? AND method = ? AND expired_at IS NOT NULL`, e.id, edgeMethodOntology); err != nil {
				rep.Errors = append(rep.Errors, want[p]+": "+err.Error())
				continue
			}
			d.publishEdgeChange(ctx, key, mscope, focusMirrorOp, e.id, p.from, p.to, focusEdgeKind, e.status)
			rep.Written++
			continue
		}
		r, err := d.linkChunks(ctx, key, mscope, docInput{Scope: "tenant", FromID: p.from, ToID: p.to,
			Kind: focusEdgeKind, edgeMethod: edgeMethodOntology})
		switch {
		case err != nil:
			rep.Errors = append(rep.Errors, want[p]+": "+err.Error())
		case r.IsError:
			rep.Errors = append(rep.Errors, want[p]+": "+r.Text)
		default:
			rep.Written++
		}
	}

	var retire []pair
	for p, e := range have {
		if _, keep := want[p]; keep || !e.live || held[p.from] {
			continue
		}
		retire = append(retire, p)
	}
	sort.Slice(retire, func(i, j int) bool {
		return retire[i].from+retire[i].to < retire[j].from+retire[j].to
	})
	now := time.Now().UnixNano()
	for _, p := range retire {
		from := tagOf[p.from]
		if from == "" {
			from = p.from // the concept itself is gone from the ontology
		}
		rep.Stale = append(rep.Stale, from+" -> "+p.to+" (retire)")
		if !dryRun {
			if err := d.exec(ctx, key, `UPDATE chunk_edges SET expired_at = ?
				WHERE id = ? AND method = ? AND expired_at IS NULL`, now, have[p].id, edgeMethodOntology); err != nil {
				rep.Errors = append(rep.Errors, p.from+" -> "+p.to+": "+err.Error())
				continue
			}
			d.publishEdgeChange(ctx, key, mscope, focusMirrorOp, have[p].id, p.from, p.to, focusEdgeKind, "expired")
		}
		rep.Retired++
	}
	return rep, nil
}

// focusTargetChunk is the chunk a resolved @focus points at, or "" with the reason it
// has none yet. err is a store fault only.
func (d *Document) focusTargetChunk(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead,
	t memrank.FocusTarget, declared map[string]string) (string, string, error) {
	if t.Kind == memrank.FocusClass {
		if id := read.classChunkID(t.Name); id != "" {
			return id, "", nil
		}
		return "", "the class has no entry in the ontology document (a standard class); declare it to focus on it", nil
	}
	class, ok := declared[strings.ToLower(t.Class)]
	if !ok {
		return "", "the instance has no declared class, so it is not adopted", nil
	}
	for _, inst := range read.Instances {
		if inst.Name != t.Name {
			continue
		}
		nk, _ := d.instanceKey(ctx, key, mscope, inst, class, func(nk string) (string, error) {
			return d.chunkIDByNaturalKey(ctx, key, nk)
		})
		root, err := d.chunkIDByNaturalKey(ctx, key, nk)
		if err != nil {
			return "", "", err
		}
		if root == "" {
			return "", "the instance is not adopted yet; confirm again once it is", nil
		}
		return root, "", nil
	}
	return "", "the instance is not declared", nil
}

// classChunkID is the in-force chunk of the ontology document that declares the class,
// "" when none does (a standard class has no entry).
//
// Taken from the class walk itself, never matched by title: a concept is often titled
// after the class it focuses on (the topic "provider" about the class provider), and a
// relation entry, a grammar or an instance can share a class's name too — a title
// lookup would pick one of them at random.
func (r OntologyRead) classChunkID(name string) string {
	return r.classChunks[strings.ToLower(strings.TrimSpace(name))]
}

// ClassChunkID is classChunkID for the operator plane: the Ontology page retires a
// class by its chunk (RFC GQ P2.2).
func (r OntologyRead) ClassChunkID(name string) string {
	return r.classChunkID(name)
}

// proposeConcept files an inert concept into a scheme. The parent is a scheme name, a
// concept name, or a concept's mirror tag — the tag disambiguates a concept name that
// two schemes share.
func (d *Document) proposeConcept(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, read OntologyRead, name string, in docInput) (tools.Result, error) {
	if read.SectionChunks[memrank.SectionSchemes] == "" {
		return errBusiness("propose_entity: this tenant's ontology declares no Schemes section, so there is no "+
			"concept scheme to propose into", "Describe the topic in prose instead; an operator decides whether to declare schemes."), nil
	}
	want := strings.TrimSpace(in.Parent)
	if want == "" {
		return errValidation("propose_entity: a concept needs a parent — the scheme or the broader concept it belongs under",
			"Pass parent: a scheme name (e.g. topics), a concept name, or a concept tag (topics/risk-management)."), nil
	}
	// parentName is the resolved parent's name: what a proposal's Parent reads (the
	// nearest in-force ancestor's name), whether the call named it by name or by tag.
	var parentChunk, parentTag, parentName string
	var have []string
	parentRetired := false
	for _, s := range read.Schemes {
		have = append(have, s.Name)
		if parentChunk == "" && (s.Name == want || memrank.MirrorTag(s.Name, nil) == want) {
			parentChunk, parentTag, parentName, parentRetired = s.ChunkID, memrank.MirrorTag(s.Name, nil), s.Name, s.EntryStatus != ""
		}
		for _, c := range s.Concepts {
			if parentChunk == "" && (c.Tag == want || strings.EqualFold(c.Name, want)) {
				parentChunk, parentTag, parentName, parentRetired = c.ChunkID, c.Tag, c.Name, c.EntryStatus != ""
			}
		}
	}
	if parentRetired {
		return *retiredParentRefusal("the scheme or concept " + want), nil
	}
	if parentChunk == "" {
		return errValidation("propose_entity: no scheme or concept named "+want+" is declared (schemes: "+
			strings.Join(have, ", ")+")", "Pass a declared scheme, concept name or concept tag as parent."), nil
	}
	tag := parentTag + "/" + memrank.MirrorSegment(name)
	for _, s := range read.Schemes {
		for _, c := range s.Concepts {
			if c.Tag == tag && c.EntryStatus != "" {
				return *retiredNameRefusal("the concept " + tag), nil
			}
			if c.Tag == tag {
				return errBusiness("propose_entity: concept "+tag+" is already declared", "Use it as it is."), nil
			}
		}
	}
	for _, p := range read.Proposals {
		if p.Section == memrank.SectionSchemes && strings.EqualFold(p.Name, name) && strings.EqualFold(p.Parent, parentName) {
			return errBusiness("propose_entity: concept "+name+" is already "+p.Status+" — an operator has seen it",
				"Do not propose it again; an operator decides."), nil
		}
	}
	return d.createOntologyProposal(ctx, key, mscope, read.DocumentID, parentChunk, name, neutraliseDirectives(in.Body))
}

// EffectiveSchemesFor is the run's tenant's concept schemes in force: those of a
// CONFIRMED ontology, through the same cached read the gates use; nil otherwise.
func (d *Document) EffectiveSchemesFor(ctx context.Context) []memrank.OntologyScheme {
	if d.Store == nil || d.SqlMem == nil {
		return nil
	}
	read, ok := d.tenantOntologyRead(ctx)
	if !ok || !read.Confirmed {
		return nil
	}
	return read.Schemes
}
