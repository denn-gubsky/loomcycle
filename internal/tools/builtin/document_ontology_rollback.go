package builtin

// document_ontology_rollback.go — rolling the tenant ontology back to a release (RFC GQ
// P5.7, D8): a plan of the direct changes that make the ontology in force declare what
// the release declared, then their application. Never automatic.
//
// THE PLAN. {"op": "release", "seq": N} is a plan request's only change. It is expanded
// from the diff of the ontology in force against release N — the release diff's own
// comparison, entry by entry, matched by chunk id — into direct changes, each carrying
// release N:
//
//   - an entry in both: rename (to the release's title), edit (to the release's body,
//     when its meaning or directives differ), move (under the release's parent), retire
//     or unretire;
//   - an entry in force now and not in the release (added since): delete when nothing
//     rests on it, else retire (D3). It is planned as a delete, then planned again as a
//     retire when the delete's risk is not none or not every scope was counted;
//   - an entry only in the release: add. Nothing rests on an entry not in force.
//
// Two differences a plan cannot name as a direct change are listed as fixed changes of
// risk stales, the conservative class, since the plan counts nothing for them: a grammar
// chunk of a class (not an entry), and a move under an entry the rollback itself adds back.
// More than 50 direct changes are planned in batches of 50, as ontologyPlanGate plans
// them; every batch must report the same fingerprint, or the ontology moved under the
// plan and it is refused (unavailable: plan again).
//
// A CLASS RENAME goes through the rename job, as any rename of a class does: the write is
// update_chunk, whose own gate starts the job. A rename the job cannot carry yet
// (rename_job_pending) or that would collide stored keys refuses the whole rollback
// before anything is written. A class renamed through the job keeps `@renamed_from`, so
// the content version after such a rollback differs from the release's: the report says
// so (matches false).
//
// AN ENTRY ADDED BACK gets a new chunk id (owner ruling Q6: its old id is kept only if
// an id-keeping writer can be reused, and no chunk writer takes a caller's id), so the
// content version, which hashes chunk ids, then differs from the release's too. One whose
// chunk is still stored, out of force, is restored in place and keeps its id.
//
// THE APPLY (ApplyOntologyRollback) runs under the per-tenant accept lock and refuses
// unless the caller sends the plan's fingerprint: plan_required without one, plan_stale
// with an old one. Every write goes through the Document's own ops (update_chunk,
// create_chunk, move_chunk, delete_chunk), so every guard of a direct edit applies. Before
// each write the fingerprint is read again and must be the one the previous write left:
// a write by anyone else in between stops the rollback (partially_applied, naming what was
// written). The fingerprint is passed to the gated writes as plan_fingerprint. Known
// limit, as on the accept path: a foreign write landing between one of ours and the read
// after it is not seen. A rollback that applied ends with a `rollback` release.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ontologyRollbackMaxChanges bounds one rollback: ten plan batches. A release that far
// from the ontology in force is rolled back in steps (an operator edits toward it), so
// that one call's counting work stays bounded.
const ontologyRollbackMaxChanges = 10 * ontologyPlanMaxChanges

// ErrRollbackPartiallyApplied: some of a rollback's writes were made, then one was
// refused or the ontology moved. Unlike a refusal, something WAS written; the report's
// Applied says what.
var ErrRollbackPartiallyApplied = errors.New("partially_applied")

// RollbackStep is one write a rollback made, and RollbackReport what the rollback did:
// the connector's shapes, which every transport answers with.
type (
	RollbackStep   = connector.OntologyRollbackStep
	RollbackReport = connector.OntologyRollbackReport
)

// rollbackEntry is what the rollback does to one chunk, with the release's values.
type rollbackEntry struct {
	id, name, kind               string
	title, body, status, typ     string
	fields                       json.RawMessage
	parent                       string
	position, depth              int
	grammar                      bool
	add, inPlace                 bool // add: the chunk is not in force now; inPlace: it is still stored
	rename, edit, move, unretire bool
	retire                       bool
	removed                      bool // in force now, not in the release
}

// curChunk is one chunk of the ontology document as it stands.
type curChunk struct {
	parent, title, status string
	revision              int
}

// rollbackExpansion is a release's rollback as the plan expands it.
type rollbackExpansion struct {
	docID, rootID                  string
	releaseVersion, currentVersion string
	direct                         []connector.OntologyChange
	fixed                          []connector.OntologyPlannedChange
	entries                        map[string]*rollbackEntry
	cur                            map[string]curChunk
	// placeable are the chunks a plan's move can name as a parent: the entries in force,
	// the section headers and the root.
	placeable map[string]bool
	issues    []string
}

// planReleaseErr is the plan's error for a release it cannot read.
func planReleaseErr(seq int64, err error) error {
	if errors.Is(err, connector.ErrOntologyReleaseNotFound) {
		return connector.WithMessage(connector.ErrOntologyPlanNotFound, "no complete ontology release has seq "+strconv.FormatInt(seq, 10))
	}
	return planReadErr(err)
}

// validateReleaseChange checks the fields op release and op add take, and that no other
// op carries a seq. n is how many changes the request holds.
func validateReleaseChange(at string, ch connector.OntologyChange, n int) error {
	switch ch.Op {
	case connector.OntologyPlanRelease:
		if ch.Seq <= 0 || ch.Entry != "" || ch.Name != "" || ch.Parent != "" || ch.Body != "" {
			return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": release needs seq (a release's sequence number) and nothing else")
		}
		if n != 1 {
			return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": release is a plan's only change; plan other changes apart")
		}
	case connector.OntologyPlanAdd:
		if ch.Seq <= 0 || ch.Name != "" || ch.Parent != "" || ch.Body != "" {
			return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": add needs seq (a release) and entry (a chunk id of it), and nothing else")
		}
	default:
		if ch.Seq != 0 {
			return connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": seq is for ops release and add only")
		}
	}
	return nil
}

// kindSection is the ontology section an entry of kind sits in.
func kindSection(kind string) string {
	switch kind {
	case planKindRelation:
		return memrank.SectionRelations
	case planKindScheme, planKindConcept:
		return memrank.SectionSchemes
	case planKindInstance:
		return memrank.SectionInstances
	}
	return memrank.SectionClasses
}

// planAddBack plans adding back entry ch.Entry of release ch.Seq (changes[i]): an entry
// of the release that is not in force now. Risk none.
func (d *Document) planAddBack(ctx context.Context, key sqlmem.ScopeKey, i int, ch connector.OntologyChange, before OntologyRead) (*connector.OntologyPlannedChange, error) {
	at := "changes[" + strconv.Itoa(i) + "]"
	rel, err := d.loadRelease(ctx, key, ch.Seq)
	if err != nil {
		return nil, planReleaseErr(ch.Seq, err)
	}
	id := strings.TrimSpace(ch.Entry)
	item, ok := rel.items[id]
	if !ok {
		return nil, connector.WithMessage(connector.ErrOntologyPlanNotFound,
			at+": "+strconv.Quote(id)+" is not an entry of release "+strconv.FormatInt(ch.Seq, 10))
	}
	if _, inForce := planEntries(before)[id]; inForce {
		return nil, connector.WithMessage(connector.ErrOntologyPlanInvalid, at+": "+strconv.Quote(item.state.Name)+" is in force; there is nothing to add back")
	}
	_, stored, err := d.getChunkRow(ctx, key, id)
	if err != nil {
		return nil, planReadErr(err)
	}
	return addBackPlan(id, item, ch.Seq, stored), nil
}

// addBackPlan is the plan of adding back one entry of release seq.
func addBackPlan(id string, item diffItem, seq int64, stored bool) *connector.OntologyPlannedChange {
	out := &connector.OntologyPlannedChange{Op: connector.OntologyPlanAdd, Release: seq, Risk: connector.OntologyRiskNone,
		Entry:  connector.OntologyPlanEntry{ChunkID: id, Name: item.state.Name, Section: kindSection(item.kind), Kind: item.kind},
		Counts: map[string]connector.OntologyPlanCount{}}
	if stored {
		out.Notes = []string{"the chunk is still stored, out of force; it is restored in place and keeps its id"}
	} else {
		out.Notes = []string{"added back under a new chunk id: no writer keeps a removed chunk's id (RFC GQ ruling Q6)"}
	}
	if item.kind == planKindGrammar {
		out.Risk = connector.OntologyRiskStales
		out.Notes = append(out.Notes, grammarNote)
	}
	return out
}

const grammarNote = "a grammar chunk of a class: the plan counts nothing that rests on it, so it is planned as stales"

// readCurrentChunks reads the ontology document's chunks as they stand, materialised
// before anything else is read (a SQLite scope has one connection).
func (d *Document) readCurrentChunks(ctx context.Context, key sqlmem.ScopeKey, docID string) (map[string]curChunk, error) {
	res, err := d.query(ctx, key, `SELECT id, coalesce(parent_id, ''), title, coalesce(status, ''), revision FROM chunks WHERE document_id = ?`, docID)
	if err != nil {
		return nil, err
	}
	if res.Truncated {
		return nil, fmt.Errorf("the ontology document has more chunks than one read returns")
	}
	out := make(map[string]curChunk, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) < 5 {
			continue
		}
		out[asStr(r[0])] = curChunk{parent: asStr(r[1]), title: asStr(r[2]), status: asStr(r[3]), revision: asInt(r[4])}
	}
	return out, nil
}

// expandRollback lists the changes that make the ontology in force declare what release
// seq declared (see the file comment).
func (d *Document) expandRollback(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, seq int64) (*rollbackExpansion, error) {
	rel, err := d.loadRelease(ctx, key, seq)
	if err != nil {
		return nil, planReleaseErr(seq, err)
	}
	if rel.partial {
		return nil, connector.WithMessage(connector.ErrOntologyPlanUnavailable, "release "+strconv.FormatInt(seq, 10)+" could not be read whole")
	}
	cur, treeParent, err := d.ontologyTreeForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return nil, planReadErr(err)
	}
	if cur.DocumentID == "" {
		return nil, connector.WithMessage(connector.ErrOntologyPlanNotFound, "this tenant has no ontology document")
	}
	if cur.partial {
		return nil, connector.WithMessage(connector.ErrOntologyPlanUnavailable, "the ontology in force could not be read whole")
	}
	exp := &rollbackExpansion{docID: cur.DocumentID, rootID: cur.RootChunkID, releaseVersion: rel.contentVersion,
		currentVersion: cur.ContentVersion, entries: map[string]*rollbackEntry{}, placeable: map[string]bool{cur.RootChunkID: true}}
	for id := range planEntries(cur) {
		exp.placeable[id] = true
	}
	for _, id := range cur.SectionChunks {
		exp.placeable[id] = true
	}
	if exp.cur, err = d.readCurrentChunks(ctx, key, cur.DocumentID); err != nil {
		return nil, planReadErr(err)
	}
	if rel.orphans > 0 {
		exp.issues = append(exp.issues, fmt.Sprintf("release %d: %d entries sit under a proposal the release did not copy; the rollback leaves them as they are", seq, rel.orphans))
	}
	if len(rel.erased) > 0 {
		exp.issues = append(exp.issues, fmt.Sprintf("release %d: %d entries were erased by a subject erasure; the rollback leaves them as they are", seq, len(rel.erased)))
	}
	diffs := diffEntries(diffItems(cur, treeParent), rel.items, rel.erased)
	removed := map[string]bool{}
	for _, df := range diffs {
		if df.Change == connector.OntologyDiffRemoved {
			removed[df.ChunkID] = true
		}
	}
	for _, df := range diffs {
		if df.Change == connector.OntologyDiffErased {
			continue
		}
		if err := d.expandRollbackEntry(ctx, key, mscope, exp, rel, seq, df, removed); err != nil {
			return nil, err
		}
	}
	return exp, nil
}

// expandRollbackEntry adds one difference's changes to exp.
func (d *Document) expandRollbackEntry(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, exp *rollbackExpansion,
	rel *loadedRelease, seq int64, df connector.OntologyEntryDiff, removed map[string]bool) error {
	id := df.ChunkID
	e := &rollbackEntry{id: id, kind: df.Kind, grammar: df.Kind == planKindGrammar}
	if df.Change == connector.OntologyDiffRemoved {
		e.name, e.removed = df.Before.Name, true
		// An entry under another one that goes goes with it: the ancestor's delete takes
		// its subtree, and its retirement retires it.
		for up := exp.cur[id].parent; up != ""; up = exp.cur[up].parent {
			if removed[up] {
				return nil
			}
		}
		exp.entries[id] = e
		if e.grammar {
			exp.fixed = append(exp.fixed, grammarPlan(connector.OntologyPlanDelete, id, e.name, seq))
			return nil
		}
		exp.direct = append(exp.direct, connector.OntologyChange{Op: connector.OntologyPlanDelete, Entry: id})
		return nil
	}
	rr := rel.rows[id]
	b, err := rel.body(id)
	if err != nil {
		return planReadErr(err)
	}
	e.name, e.title, e.body, e.fields, e.status, e.typ = df.After.Name, rr.title, b.Body, b.Fields, rr.status, rr.typ
	e.parent, e.position = rr.parentID, rr.position
	for up := rr.parentID; up != ""; up = rel.rows[up].parentID {
		e.depth++
		if e.depth > len(rel.rows) {
			break // a malformed tree; bounded all the same
		}
	}
	exp.entries[id] = e
	if df.Change == connector.OntologyDiffAdded {
		c, stored := exp.cur[id]
		e.add, e.inPlace = true, stored
		exp.fixed = append(exp.fixed, *addBackPlan(id, rel.items[id], seq, stored))
		if stored && c.parent != e.parent {
			e.move = true
		}
		return nil
	}
	return d.expandChangedEntry(ctx, key, mscope, exp, e, seq, df)
}

// expandChangedEntry lists the direct changes of an entry in both the ontology and the
// release.
func (d *Document) expandChangedEntry(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, exp *rollbackExpansion,
	e *rollbackEntry, seq int64, df connector.OntologyEntryDiff) error {
	c := exp.cur[e.id]
	changed := map[string]bool{}
	for _, what := range df.Changed {
		changed[what] = true
	}
	e.rename = c.title != e.title
	if changed[connector.OntologyDiffMeaning] || changed[connector.OntologyDiffDirectives] || (changed[connector.OntologyDiffRenamed] && !e.rename) {
		// A name that is not the title is the body's first heading: restored with the body.
		now, err := d.readBody(ctx, mscope, key, e.id)
		if err != nil {
			return planReadErr(err)
		}
		e.edit = now.Body != e.body
	}
	if changed[connector.OntologyDiffStatus] {
		e.retire = memrank.IsRetiredEntityStatus(e.status)
		e.unretire = !e.retire && memrank.IsRetiredEntityStatus(c.status)
	}
	e.move = c.parent != e.parent
	if e.grammar {
		if e.rename || e.edit || e.move || e.retire || e.unretire {
			exp.fixed = append(exp.fixed, grammarPlan(connector.OntologyPlanEdit, e.id, e.name, seq))
		}
		return nil
	}
	if name := memrank.TrimOntologyName(e.title); e.rename && name != "" {
		exp.direct = append(exp.direct, connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: e.id, Name: name})
	}
	if e.edit {
		if strings.TrimSpace(e.body) == "" || len(e.body) > ontologyPlanMaxBody {
			return connector.WithMessage(connector.ErrOntologyPlanInvalid, "the release's body of "+strconv.Quote(e.name)+
				" is empty or over "+strconv.Itoa(ontologyPlanMaxBody)+" bytes, which cannot be planned")
		}
		exp.direct = append(exp.direct, connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: e.id, Body: e.body})
	}
	if e.unretire {
		exp.direct = append(exp.direct, connector.OntologyChange{Op: connector.OntologyPlanUnretire, Entry: e.id})
	}
	if e.move {
		if exp.placeable[e.parent] {
			exp.direct = append(exp.direct, connector.OntologyChange{Op: connector.OntologyPlanMove, Entry: e.id, Parent: e.parent})
		} else {
			fixed := connector.OntologyPlannedChange{Op: connector.OntologyPlanMove, Release: seq, Risk: connector.OntologyRiskStales,
				Entry:  connector.OntologyPlanEntry{ChunkID: e.id, Name: e.name, Section: kindSection(e.kind), Kind: e.kind},
				Counts: map[string]connector.OntologyPlanCount{},
				Notes:  []string{"its parent in the release is not an entry in force now (the rollback adds it back); the plan cannot count a move under it, so it is planned as stales"}}
			exp.fixed = append(exp.fixed, fixed)
		}
	}
	if e.retire {
		exp.direct = append(exp.direct, connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: e.id})
	}
	return nil
}

// grammarPlan is a grammar chunk's difference as a fixed planned change (see grammarNote).
func grammarPlan(op, id, name string, seq int64) connector.OntologyPlannedChange {
	return connector.OntologyPlannedChange{Op: op, Release: seq, Risk: connector.OntologyRiskStales,
		Entry:  connector.OntologyPlanEntry{ChunkID: id, Name: name, Section: memrank.SectionClasses, Kind: planKindGrammar},
		Counts: map[string]connector.OntologyPlanCount{}, Notes: []string{grammarNote}}
}

// planRelease is the plan of rolling back to release seq.
func (d *Document) planRelease(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, seq int64) (connector.PlanOntologyResult, error) {
	out, _, err := d.planReleaseFull(ctx, key, mscope, seq)
	return out, err
}

// planReleaseFull plans rolling back to release seq and returns the expansion the
// apply writes from.
func (d *Document) planReleaseFull(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, seq int64) (connector.PlanOntologyResult, *rollbackExpansion, error) {
	ctx = noWrites(ctx)
	out := connector.PlanOntologyResult{Changes: []connector.OntologyPlannedChange{}, Issues: []string{}, NewIssues: []string{}, Complete: true}
	docID, err := d.resolveOntologyID(ctx, key)
	if err != nil {
		return out, nil, planReadErr(err)
	}
	if docID == "" {
		return out, nil, connector.WithMessage(connector.ErrOntologyPlanNotFound, "this tenant has no ontology document")
	}
	if out.Fingerprint, err = d.ontologyPlanFingerprint(ctx, key, docID); err != nil {
		return out, nil, planReadErr(err)
	}
	exp, err := d.expandRollback(ctx, key, mscope, seq)
	if err != nil {
		return out, nil, err
	}
	if n := len(exp.direct) + len(exp.fixed); n > ontologyRollbackMaxChanges {
		return out, nil, connector.WithMessage(connector.ErrOntologyPlanInvalid, "rolling back to release "+strconv.FormatInt(seq, 10)+
			" makes "+strconv.Itoa(n)+" changes; one rollback makes at most "+strconv.Itoa(ontologyRollbackMaxChanges))
	}
	out.ContentVersion = exp.currentVersion
	out.Issues = append(out.Issues, exp.issues...)
	planned, err := d.planReleaseBatches(ctx, key, mscope, seq, exp.direct, &out)
	if err != nil {
		return out, nil, err
	}
	// D3: an entry added since is deleted only when nothing rests on it.
	var again []connector.OntologyChange
	var at []int
	for i, ch := range planned {
		if ch.Op == connector.OntologyPlanDelete && (ch.Risk != connector.OntologyRiskNone || !out.Complete) {
			again = append(again, connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: ch.Entry.ChunkID})
			at = append(at, i)
		}
	}
	if len(again) > 0 {
		retired, err := d.planReleaseBatches(ctx, key, mscope, seq, again, &out)
		if err != nil {
			return out, nil, err
		}
		for k, i := range at {
			ch := retired[k]
			ch.Notes = append(ch.Notes[:len(ch.Notes):len(ch.Notes)], "added since the release, and something rests on it (or not every scope was counted): retired, not deleted")
			planned[i] = ch
		}
	}
	out.Changes = append(planned, exp.fixed...)
	return out, exp, nil
}

// planReleaseBatches plans changes in batches of ontologyPlanMaxChanges, folding each
// batch's counts and issues into out. Every batch must report out's fingerprint.
func (d *Document) planReleaseBatches(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, seq int64,
	changes []connector.OntologyChange, out *connector.PlanOntologyResult) ([]connector.OntologyPlannedChange, error) {
	planned := make([]connector.OntologyPlannedChange, 0, len(changes))
	seen := map[string]bool{}
	for _, s := range out.Issues {
		seen[s] = true
	}
	for start := 0; start < len(changes); start += ontologyPlanMaxChanges {
		batch := changes[start:min(start+ontologyPlanMaxChanges, len(changes))]
		// The release's values go through the checks a caller's would (a title too long
		// to be a name, an edit body over the cap).
		if err := validatePlanChanges(batch); err != nil {
			return nil, err
		}
		res, err := d.planChangesAt(ctx, key, mscope, batch, seq)
		if err != nil {
			return nil, err
		}
		if res.Fingerprint != out.Fingerprint {
			return nil, connector.WithMessage(connector.ErrOntologyPlanUnavailable, "the ontology changed while the rollback was planned; plan it again")
		}
		planned = append(planned, res.Changes...)
		out.Complete = out.Complete && res.Complete
		out.ScopesCounted, out.ScopesSkipped = max(out.ScopesCounted, res.ScopesCounted), max(out.ScopesSkipped, res.ScopesSkipped)
		for _, s := range res.Issues {
			if !seen[s] {
				seen[s] = true
				out.Issues = append(out.Issues, s)
			}
		}
		for _, s := range res.NewIssues {
			if !seen["new:"+s] {
				seen["new:"+s] = true
				out.NewIssues = append(out.NewIssues, s)
			}
		}
	}
	return planned, nil
}

// ApplyOntologyRollback rolls the tenant ontology of ctx's tenant back to release seq:
// it plans the rollback again, refuses unless planFingerprint is that plan's
// fingerprint, then makes the planned writes (see the file comment). Errors: the plan's
// own (connector ErrOntologyPlan* sentinels), a *ChangePlanRefusal for a missing or stale
// fingerprint, ErrChangeRenameJobPending, ErrChangeEntryLocked or ErrChangeProposalInvalid
// for a write refused before anything was written, and ErrRollbackPartiallyApplied once
// something was.
func (d *Document) ApplyOntologyRollback(ctx context.Context, seq int64, planFingerprint string) (RollbackReport, error) {
	rep := RollbackReport{Release: seq, Applied: []RollbackStep{}, Risk: connector.OntologyRiskNone}
	if seq <= 0 {
		return rep, connector.WithMessage(connector.ErrOntologyPlanInvalid, "seq must be a release's sequence number, a positive integer")
	}
	if d.Store == nil || d.SqlMem == nil {
		return rep, connector.WithMessage(connector.ErrOntologyPlanUnavailable, "rolling the ontology back needs SQL Memory; none is configured")
	}
	key, mscope, err := d.ontologyTenantKey(ctx)
	if err != nil {
		return rep, connector.WithMessage(connector.ErrOntologyPlanUnavailable, err.Error())
	}
	// The accept's lock: one change to the tenant ontology at a time in this process.
	unlock, err := d.editions.lock(ctx, key, "ontology_accept", memrank.OntologyPath)
	if err != nil {
		return rep, err
	}
	defer unlock()
	plan, exp, err := d.planReleaseFull(ctx, key, mscope, seq)
	if err != nil {
		return rep, err
	}
	rep.ReleaseContentVersion = exp.releaseVersion
	if err := rollbackRefusal(plan, strings.TrimSpace(planFingerprint), &rep); err != nil {
		return rep, err
	}
	if len(plan.Changes) == 0 {
		rep.ContentVersion, rep.Matches = exp.currentVersion, exp.currentVersion == exp.releaseVersion
		rep.Notes = append(rep.Notes, "the ontology in force already declares what the release declared; nothing was written")
		return rep, nil
	}
	if _, _, err := d.CatchUpOntologyRelease(ctx); err != nil {
		slog.WarnContext(ctx, "ontology: catch_up release before a rollback failed", "release", seq, "err", err)
		rep.Warnings = append(rep.Warnings, "No release was taken of the ontology before this rollback: "+err.Error())
	}
	run := &rollbackRun{d: d, ctx: ctx, key: key, docID: exp.docID, expect: plan.Fingerprint, exp: exp, rep: &rep, newIDs: map[string]string{}}
	if err := run.apply(plan); err != nil {
		if len(rep.Applied) == 0 {
			return rep, err
		}
		return rep, fmt.Errorf("%w: applied %d writes of the rollback to release %d, then: %v; the rest was not made (plan the rollback again)",
			ErrRollbackPartiallyApplied, len(rep.Applied), seq, err)
	}
	if rel, err := d.TakeOntologyRelease(ctx, OntologyReleaseRollback, rep); err != nil {
		slog.WarnContext(ctx, "ontology: rollback release failed", "release", seq, "err", err)
		rep.Warnings = append(rep.Warnings, "The rollback was applied, but no release of it was taken: "+err.Error())
	} else {
		rep.RollbackRelease = rel.Seq
	}
	if read, ok := d.tenantOntologyRead(ctx); ok {
		rep.ContentVersion = read.ContentVersion
	}
	rep.Matches = rep.ContentVersion != "" && rep.ContentVersion == rep.ReleaseContentVersion
	if !rep.Matches {
		rep.Notes = append(rep.Notes, "the content version in force differs from the release's: an entry added back has a new chunk id, "+
			"a class renamed through the rename job keeps @renamed_from, or the ontology could not be read whole")
	}
	return rep, nil
}

// rollbackRefusal refuses a rollback whose fingerprint is not the plan's, or that renames
// a class the rename job cannot carry; it sets the report's risk.
func rollbackRefusal(plan connector.PlanOntologyResult, given string, rep *RollbackReport) error {
	if given != plan.Fingerprint {
		code, why := planRequiredCode, "it needs the fingerprint of a plan of it (plan_fingerprint)"
		if given != "" {
			code, why = planStaleCode, "the plan_fingerprint sent is not the ontology's current one: it changed since that plan"
		}
		return &ChangePlanRefusal{Code: code, Fingerprint: plan.Fingerprint,
			Message: "rollback: " + code + ": " + why + ". Nothing was changed."}
	}
	rank := map[string]int{connector.OntologyRiskNone: 0, connector.OntologyRiskStales: 1, connector.OntologyRiskDestroys: 2}
	for _, ch := range plan.Changes {
		if rank[ch.Risk] > rank[rep.Risk] {
			rep.Risk = ch.Risk
		}
		if why, pending := renameJobPending(ch, plan.Complete); pending {
			return fmt.Errorf("%w: %s. Nothing was changed", ErrChangeRenameJobPending, why)
		}
		if ch.Job != nil && ch.Job.CollisionsTotal > 0 {
			return fmt.Errorf("%w: renaming the class %s back would collide %d stored keys (the plan lists them under job.collisions). Nothing was changed",
				ErrChangeRenameJobPending, strconv.Quote(ch.Entry.Name), ch.Job.CollisionsTotal)
		}
	}
	return nil
}

// rollbackRun is one rollback's writes in progress.
type rollbackRun struct {
	d      *Document
	ctx    context.Context
	key    sqlmem.ScopeKey
	docID  string
	expect string // the fingerprint the last write left
	exp    *rollbackExpansion
	rep    *RollbackReport
	newIDs map[string]string // a release chunk id added back -> the chunk it was written as
}

// apply makes the plan's writes in an order where each finds what it needs: renames,
// edits and unretirements first (a name freed before an entry added back takes it), then
// the entries added back, parent first, then moves (under them, if need be), then
// retirements, then what was added since.
func (r *rollbackRun) apply(plan connector.PlanOntologyResult) error {
	final := map[string]string{} // an entry added since -> delete or retire, as planned
	for _, ch := range plan.Changes {
		if e := r.exp.entries[ch.Entry.ChunkID]; e != nil && e.removed {
			final[e.id] = ch.Op
		}
	}
	ids := make([]string, 0, len(r.exp.entries))
	for id := range r.exp.entries {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := r.exp.entries[ids[i]], r.exp.entries[ids[j]]
		if a.depth != b.depth {
			return a.depth < b.depth
		}
		if a.position != b.position {
			return a.position < b.position
		}
		return a.id < b.id
	})
	steps := []func(*rollbackEntry) error{r.update, r.addBack, r.moveBack, r.retire, func(e *rollbackEntry) error { return r.remove(e, final[e.id]) }}
	for _, step := range steps {
		for _, id := range ids {
			if err := step(r.exp.entries[id]); err != nil {
				return err
			}
		}
	}
	return nil
}

// write sends one Document op, after checking that the ontology is what the last write
// left; gated writes carry that fingerprint. A refusal is the accept's error for it.
func (r *rollbackRun) write(req map[string]any, gated bool) (map[string]any, error) {
	now, err := r.d.ontologyPlanFingerprint(noWrites(r.ctx), r.key, r.docID)
	if err != nil {
		return nil, err
	}
	if now != r.expect {
		return nil, errors.New("the ontology was changed by another write during the rollback")
	}
	if gated {
		req["plan_fingerprint"] = now
	}
	req["scope"] = "tenant"
	res, err := r.d.execOpResult(r.ctx, req)
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, changeWriteError(res)
	}
	if r.expect, err = r.d.ontologyPlanFingerprint(noWrites(r.ctx), r.key, r.docID); err != nil {
		return nil, err
	}
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(res.Text))
	dec.UseNumber()
	_ = dec.Decode(&out) // a write's answer is JSON; what it says beyond success is optional
	return out, nil
}

// done records a write.
func (r *rollbackRun) done(op string, e *rollbackEntry, newID string) {
	r.rep.Applied = append(r.rep.Applied, RollbackStep{Op: op, Entry: e.id, Name: e.name, NewID: newID})
}

// revision is a chunk's revision now.
func (r *rollbackRun) revision(id string) (int, error) {
	row, ok, err := r.d.getChunkRow(r.ctx, r.key, id)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%s is gone", id)
	}
	return row.Revision, nil
}

// update writes an entry's title, body and un-retirement (and an in-place add's status).
func (r *rollbackRun) update(e *rollbackEntry) error {
	restore := e.add && e.inPlace
	if !restore && !e.rename && !e.edit && !e.unretire {
		return nil
	}
	rev, err := r.revision(e.id)
	if err != nil {
		return err
	}
	req := map[string]any{"op": "update_chunk", "id": e.id, "revision": rev}
	var did []string
	if e.rename || restore {
		req["title"] = e.title
		did = append(did, "rename")
	}
	if e.edit || restore {
		req["body"] = e.body
		did = append(did, "edit")
	}
	switch {
	case restore:
		req["status"] = e.status
	case e.unretire:
		req["status"] = ""
		did = append(did, "unretire")
	}
	out, err := r.write(req, true)
	if err != nil {
		return err
	}
	if raw, ok := out["rename_job"]; ok {
		var st connector.OntologyJobStatus
		if b, err := json.Marshal(raw); err == nil && json.Unmarshal(b, &st) == nil {
			r.rep.RenameJobs = append(r.rep.RenameJobs, st)
		}
	}
	r.done(strings.Join(did, "+"), e, "")
	return nil
}

// parentFor is the chunk an entry of the release goes under now: its release parent, or
// the chunk that parent was added back as, else its nearest such ancestor, else the root.
func (r *rollbackRun) parentFor(e *rollbackEntry) string {
	for up, n := e.parent, 0; up != "" && n <= len(r.exp.cur)+len(r.exp.entries); n++ {
		if id, ok := r.newIDs[up]; ok {
			return id
		}
		if _, ok := r.exp.cur[up]; ok {
			return up
		}
		pe := r.exp.entries[up]
		if pe == nil {
			break
		}
		up = pe.parent
	}
	return r.exp.rootID
}

// addBack creates an entry of the release that is not stored now.
func (r *rollbackRun) addBack(e *rollbackEntry) error {
	if !e.add || e.inPlace {
		return nil
	}
	req := map[string]any{"op": "create_chunk", "document_id": r.docID, "parent_id": r.parentFor(e), "title": e.title,
		"body": e.body, "position": e.position}
	if e.typ != "" {
		req["type"] = e.typ
	}
	if e.status != "" {
		req["status"] = e.status
	}
	if len(e.fields) > 0 && string(e.fields) != "null" {
		req["fields"] = e.fields
	}
	out, err := r.write(req, false)
	if err != nil {
		return err
	}
	newID, _ := out["id"].(string)
	if newID == "" {
		return fmt.Errorf("the chunk added back for %s reported no id", strconv.Quote(e.name))
	}
	r.newIDs[e.id] = newID
	r.done("add", e, newID)
	return nil
}

// moveBack puts an entry under its release parent.
func (r *rollbackRun) moveBack(e *rollbackEntry) error {
	if !e.move {
		return nil
	}
	if _, err := r.write(map[string]any{"op": "move_chunk", "id": e.id, "new_parent_id": r.parentFor(e)}, true); err != nil {
		return err
	}
	r.done("move", e, "")
	return nil
}

// retire retires an entry the release holds retired.
func (r *rollbackRun) retire(e *rollbackEntry) error {
	if !e.retire {
		return nil
	}
	return r.setRetired(e)
}

func (r *rollbackRun) setRetired(e *rollbackEntry) error {
	rev, err := r.revision(e.id)
	if err != nil {
		return err
	}
	if _, err := r.write(map[string]any{"op": "update_chunk", "id": e.id, "revision": rev, "status": memrank.OntologyStatusRetired}, true); err != nil {
		return err
	}
	r.done("retire", e, "")
	return nil
}

// remove deletes or retires an entry added since the release, as planned (D3).
func (r *rollbackRun) remove(e *rollbackEntry, op string) error {
	if !e.removed {
		return nil
	}
	if op == connector.OntologyPlanRetire {
		return r.setRetired(e)
	}
	if _, err := r.write(map[string]any{"op": "delete_chunk", "id": e.id}, false); err != nil {
		return err
	}
	r.done("delete", e, "")
	return nil
}
