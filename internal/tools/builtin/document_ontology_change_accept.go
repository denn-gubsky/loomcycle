package builtin

// document_ontology_change_accept.go — accepting a change or retire proposal IN PLACE
// (RFC GQ P4.2, D1). The change lands on the TARGET chunk, which keeps its chunk id, and
// the proposal chunk is then deleted (RFC GQ 9c, Q3): no `applied` status, which every
// reader that predates it would read as in force.
//
// THE MARK IS NEVER TRUSTED. A proposal chunk's fields can be written by anyone on the
// operator plane, so a mark may be forged or edited after filing. The change is rebuilt
// from the mark into the input propose_entity takes and run through the same checks
// filing ran (checkChange): target in force, not retired, an entry, not locked (nor any
// entry under it, for a retire, rename or move); the name free and normalised; the
// parent in the same section, live and not under the target; directives the section
// allows, parsed with no issue, never @locked, naming nothing retired; the size bounds.
// The rebuilt change must then EQUAL the stored mark: what the reviewer was shown is what
// is applied, or nothing is (ErrChangeProposalInvalid, 422 on the route). The proposal
// must also sit where filing puts one (under its target's section, never under the
// target) and carry the title filing gives it.
//
// WHAT IS WRITTEN goes through update_chunk and move_chunk, so every guard of a direct
// edit applies (the retire cascade, the locks, the demotion and delete refusals, the
// plan fingerprint). One update carries the title, the body and the status, at the
// target's revision as read BEFORE the checks: a concurrent edit of the target while
// they run is a revision conflict (ErrChangeTargetChanged), and nothing is applied. A
// move-only edit sends a revision-only update first for the same reason. The move's
// refusals are asked before anything is written; the move follows the update only when
// the ontology is exactly what the update left, and if it then fails the accept is
// ErrChangePartiallyApplied, which says what was written (applyChangeAccept).
//
// THE BODY of an edit is the entry's naming heading, its directive lines, its field
// bullets, then its meaning. The heading (the body's first, which names an entry with no
// title) is ALWAYS the entry's own, so a new meaning never renames or unnames it.
// Directives: the proposal's set when it sets one (each a bare `@name value` line), else
// the entry's own lines as they are. Field bullets (a class's "- `name`"): ALWAYS the
// entry's own — they are declarations, and a change proposal carries no field changes,
// so accepting one never adds or removes a field. A proposed meaning is meaning text:
// its directive lines are quoted again (neutraliseDirectives, never trusting that the
// mark's were), and so are its field-shaped bullets and headings, which
// neutraliseDirectives leaves alone. Without a proposed meaning the entry keeps its own.
//
// THE PLAN. The changes the accept makes are planned as the direct changes they are
// (retire; rename, move, edit). A class rename that anything rests on is refused with
// rename_job_pending until P5's rename job can carry it through stored rows (RFC GQ Q4);
// relation and concept renames apply with the plan. A change the plan classes destroys or
// stales needs the plan's fingerprint (P2.4, ontologyPlanGate), passed by the caller.
// The writes carry the caller's fingerprint and never one the accept computed, so a
// write the caller sent none for is planned again by update_chunk's own gate.
//
// A SECOND PROPOSAL. Filing serialises per replica only, so two open proposals for the
// same target and op can exist. Accepting one marks the other open proposals with the
// same target and op rejected, a line saying why appended to each body: they were filed
// against the entry as it was. A proposal of another op for the same target stays open
// and is checked afresh when accepted.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Why a change proposal was not accepted. Nothing was written for any of them.
var (
	// ErrChangeProposalInvalid: the stored proposal fails its checks now, or its mark is
	// not the change those checks rebuild (forged, edited, or filed against an entry that
	// changed since).
	ErrChangeProposalInvalid = errors.New("invalid_proposal")
	// ErrChangeEntryLocked: the target, or an entry under it, carries @locked.
	ErrChangeEntryLocked = errors.New("entry_locked")
	// ErrChangeRenameJobPending: a class rename with dependents (RFC GQ Q4).
	ErrChangeRenameJobPending = errors.New("rename_job_pending")
	// ErrChangeTargetChanged: the target was edited between the checks and the write.
	ErrChangeTargetChanged = errors.New("target_changed")
	// ErrChangePartiallyApplied: the update was written, then the move was not. Unlike
	// the errors above, something WAS written: it never wraps them, and the report's
	// Applied says what.
	ErrChangePartiallyApplied = errors.New("partially_applied")
)

// ChangePlanRefusal is a plan_required / plan_stale refusal of an accept: the change
// needs the fingerprint of a current plan. Fingerprint is the one a plan reports now.
type ChangePlanRefusal struct {
	Code        string
	Message     string
	Fingerprint string
}

func (e *ChangePlanRefusal) Error() string { return e.Message }

// ChangeAcceptReport says what accepting a change or retire proposal did.
type ChangeAcceptReport struct {
	Proposal   string `json:"proposal"`
	Target     string `json:"target"`
	TargetName string `json:"target_name"`
	Kind       string `json:"kind"`
	Op         string `json:"op"`
	// Applied lists what was written to the target: retire, rename, move, edit.
	Applied []string `json:"applied"`
	// Risk is the plan's highest risk over the changes; Counts what it found resting
	// on them (by kind of dependent, summed); Complete whether every scope was counted.
	Risk     string         `json:"risk"`
	Counts   map[string]int `json:"counts,omitempty"`
	Complete bool           `json:"complete"`
	// Superseded are the other open proposals of the same target and op, rejected.
	Superseded []string `json:"superseded,omitempty"`
	Warnings   []string `json:"warnings,omitempty"`
}

// changeAccept is one accept, as checked: what to write and what was planned.
type changeAccept struct {
	key      sqlmem.ScopeKey
	mscope   store.MemoryScope
	self     chunkRow
	tg       changeTarget
	mark     memrank.ProposedChange
	revision int
	title    *string
	body     *string
	changes  []connector.OntologyChange
}

// AcceptChangeProposal accepts a change or retire proposal: it applies the change to the
// target in place and deletes the proposal (see the file comment). planFingerprint is
// the fingerprint of a plan of the change, needed when the plan classes it destroys or
// stales. handled is false for a chunk that carries no change_proposal mark, which the
// caller accepts as before.
func (d *Document) AcceptChangeProposal(ctx context.Context, scope, chunkID, planFingerprint string) (handled bool, rep ChangeAcceptReport, err error) {
	key, mscope, err := d.resolveScope(ctx, scope)
	if err != nil {
		return false, rep, err
	}
	// The same lock as the scheme accept: one accept at a time per tenant ontology,
	// checks and writes together. Within this process; the target's revision guards the
	// write across replicas.
	unlock, err := d.editions.lock(ctx, key, "ontology_accept", memrank.OntologyPath)
	if err != nil {
		return false, rep, err
	}
	defer unlock()
	self, ok, err := d.getChunkRow(ctx, key, chunkID)
	if err != nil || !ok {
		return false, rep, err
	}
	b, err := d.readBody(ctx, mscope, key, chunkID)
	if err != nil {
		return false, rep, err
	}
	var marks map[string]json.RawMessage
	if len(b.Fields) == 0 || json.Unmarshal(b.Fields, &marks) != nil {
		return false, rep, nil
	}
	raw, marked := marks[changeProposalField]
	if !marked {
		return false, rep, nil
	}
	a, err := d.checkChangeAccept(ctx, key, mscope, self, raw)
	if err != nil {
		return true, rep, err
	}
	rep = ChangeAcceptReport{Proposal: chunkID, Target: a.mark.Target, TargetName: a.mark.TargetName,
		Kind: a.mark.Kind, Op: a.mark.Op}
	// An edit that changes nothing (a meaning equal to the entry's own) has nothing to
	// plan; it still goes through the revision-only update below, then the proposal goes.
	rep.Risk, rep.Complete = connector.OntologyRiskNone, true
	fp := strings.TrimSpace(planFingerprint)
	if len(a.changes) > 0 {
		if err := d.planChangeAccept(ctx, a, fp, &rep); err != nil {
			return true, rep, err
		}
	}
	// The ontology as it stands before the change, when no release holds it yet (owner
	// ruling Q2): otherwise the apply release's diff would carry every direct edit made
	// since the last release as part of this change. A record, not a gate.
	var releaseWarnings []string
	if _, _, err := d.CatchUpOntologyRelease(ctx); err != nil {
		slog.WarnContext(ctx, "ontology: catch_up release before an accept failed", "proposal", chunkID, "err", err)
		releaseWarnings = append(releaseWarnings, "No release was taken of the ontology before this change: "+err.Error())
	}
	if err := d.applyChangeAccept(ctx, a, fp, &rep); err != nil {
		return true, rep, err
	}
	rep.Superseded, rep.Warnings = d.supersedeChangeProposals(ctx, a)
	rep.Warnings = append(rep.Warnings, releaseWarnings...)
	if _, err := d.execOp(ctx, map[string]any{"op": "delete_chunk", "scope": "tenant", "id": chunkID}); err != nil {
		return true, rep, fmt.Errorf("the change was applied to %s, but the proposal could not be removed (reject it): %w",
			strconv.Quote(a.mark.TargetName), err)
	}
	// After the proposal is gone, so the release's fingerprint is the ontology the accept
	// left. Taken with the report so far as its plan summary.
	if _, err := d.TakeOntologyRelease(ctx, OntologyReleaseApply, rep); err != nil {
		slog.WarnContext(ctx, "ontology: apply release failed", "proposal", chunkID, "err", err)
		rep.Warnings = append(rep.Warnings, "The change was applied, but no release of it was taken: "+err.Error())
	}
	return true, rep, nil
}

// checkChangeAccept rebuilds the change from the stored mark, runs filing's checks on it
// and refuses a mark that is not the change they rebuild.
func (d *Document) checkChangeAccept(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, self chunkRow, raw json.RawMessage) (changeAccept, error) {
	a := changeAccept{key: key, mscope: mscope, self: self}
	invalid := func(why string) (changeAccept, error) {
		return a, fmt.Errorf("%w: %s", ErrChangeProposalInvalid, why)
	}
	if strings.ToLower(strings.TrimSpace(self.Status)) != memrank.OntologyStatusProposed {
		return invalid("it is not an open proposal")
	}
	var stored memrank.ProposedChange
	if json.Unmarshal(raw, &stored) != nil {
		return invalid("its change_proposal mark does not decode")
	}
	// The target's revision is read BEFORE the checks and the write is pinned to it, so
	// an edit of the target while the checks run is a revision conflict at the write.
	target, ok, err := d.getChunkRow(ctx, key, strings.TrimSpace(stored.Target))
	if err != nil {
		return a, err
	}
	if !ok {
		return invalid("its target is gone")
	}
	if d.duringAcceptChecks != nil {
		d.duringAcceptChecks()
	}
	op := strings.ToLower(strings.TrimSpace(stored.Op))
	in, err := changeInputOf(stored)
	if err != nil {
		return invalid(err.Error())
	}
	tg, mark, refusal, err := d.checkChange(ctx, key, mscope, op, in)
	if err != nil {
		return a, err
	}
	if refusal != nil {
		why := strings.TrimPrefix(refusal.res.Text, "propose_entity: ")
		switch {
		case refusal.locked:
			return a, fmt.Errorf("%w: %s", ErrChangeEntryLocked, why)
		case refusal.unavailable:
			return a, errors.New(why)
		}
		return invalid(why)
	}
	if tg.read.DocumentID != self.DocumentID {
		return invalid("it is not in this tenant's ontology")
	}
	if !reflect.DeepEqual(mark, stored) {
		return invalid("its mark is not the change the ontology now allows (filed against an entry that changed since, " +
			"or edited after filing); reject it and propose again")
	}
	section := tg.read.SectionChunks[mark.Section]
	if section == "" {
		section = tg.read.RootChunkID
	}
	if self.ParentID != section {
		return invalid("a change proposal is filed under its target's section header; this one is not")
	}
	if memrank.TrimOntologyName(self.Title) != mark.Op+": "+mark.TargetName {
		return invalid("its title is not the one filing gives it (" + strconv.Quote(mark.Op+": "+mark.TargetName) + ")")
	}
	a.tg, a.mark, a.revision = tg, mark, target.Revision
	return a, d.composeChangeAccept(ctx, &a, target)
}

// changeInputOf is the propose_entity input a stored mark stands for.
func changeInputOf(m memrank.ProposedChange) (docInput, error) {
	in := docInput{Scope: "tenant", Change: m.Op, Entry: m.Target, Section: m.Section, Name: m.Name, Parent: m.Parent}
	if m.Meaning != "" {
		meaning := m.Meaning
		in.Meaning = &meaning
	}
	if !m.SetsDirectives {
		if len(m.Directives) > 0 {
			return in, errors.New("its mark lists directives without setting them")
		}
		return in, nil
	}
	// The directive set as propose_entity takes it: a flag is true, a repeated
	// directive a list, in the order the mark lists them.
	byName := map[string][]string{}
	var order []string
	for _, dir := range m.Directives {
		if _, seen := byName[dir.Name]; !seen {
			order = append(order, dir.Name)
		}
		byName[dir.Name] = append(byName[dir.Name], dir.Value)
	}
	obj := make(map[string]any, len(order))
	for _, name := range order {
		vs := byName[name]
		if len(vs) == 1 && vs[0] == "" {
			obj[name] = true
			continue
		}
		obj[name] = vs
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return in, err
	}
	in.Directives = raw
	return in, nil
}

// composeChangeAccept sets what the update writes, and the changes it is planned as:
// every change the write makes, found as update_chunk's own gate finds them
// (ontologyUpdatePlanRefusal), so nothing the write does is left out of the plan the
// caller's fingerprint is checked against. target is the target's row as read before
// the checks. An edit whose body cannot be planned (empty, or over the plan's size) is
// refused. No changes at all is a no-op accept.
func (d *Document) composeChangeAccept(ctx context.Context, a *changeAccept, target chunkRow) error {
	m := a.mark
	if m.Op == memrank.ChangeOpRetire {
		a.changes = []connector.OntologyChange{{Op: connector.OntologyPlanRetire, Entry: m.Target}}
		return nil
	}
	if m.Name != "" {
		name := m.Name
		a.title = &name
	}
	cur, err := d.readBody(ctx, a.mscope, a.key, m.Target)
	if err != nil {
		return err
	}
	body := cur.Body
	if m.Meaning != "" || m.SetsDirectives {
		if next := composeChangeBody(m.Kind, cur.Body, m); next != cur.Body {
			if strings.TrimSpace(next) == "" || len(next) > ontologyPlanMaxBody {
				return fmt.Errorf("%w: the entry's new body would be empty or over %d bytes, which cannot be planned",
					ErrChangeProposalInvalid, ontologyPlanMaxBody)
			}
			body = next
			a.body = &body
			a.changes = append(a.changes, connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: m.Target, Body: body})
		}
	}
	// The name the entry reads under after the write: its title, else its body's first
	// heading. A changed name is a rename (Q4 applies to a class); none at all takes the
	// entry out of the read, which is planned as a retire.
	title := target.Title
	if a.title != nil {
		title = *a.title
	}
	name := memrank.TrimOntologyName(title)
	if name == "" {
		name = memrank.FirstHeadingName(body)
	}
	switch {
	case name == a.tg.entry.Name:
	case name == "":
		a.changes = append(a.changes, connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: m.Target})
	default:
		a.changes = append([]connector.OntologyChange{{Op: connector.OntologyPlanRename, Entry: m.Target, Name: name}}, a.changes...)
	}
	if m.Parent != "" {
		a.changes = append(a.changes, connector.OntologyChange{Op: connector.OntologyPlanMove, Entry: m.Target, Parent: m.Parent})
	}
	return nil
}

// composeChangeBody is an edited entry's new body (see the file comment): the heading
// that names an untitled entry, directive lines, field bullets, then the meaning.
func composeChangeBody(kind, current string, m memrank.ProposedChange) string {
	heading, directives, fields, meaning := splitEntryBody(kind, current)
	if m.SetsDirectives {
		directives = directiveLines(m.Directives)
	}
	if m.Meaning != "" {
		meaning = quoteMeaningText(neutraliseDirectives(strings.TrimSpace(m.Meaning)))
	}
	var parts []string
	for _, p := range []string{heading, directives, fields, meaning} {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "\n\n")
}

// splitEntryBody splits an entry's body into the line that names it when it has no
// title (its first heading, as memrank.FirstHeadingName finds it), the lines the reader
// takes as directives, the lines it takes as a class's field bullets, and the rest (its
// meaning), each in the body's order. The heading is kept apart so a new meaning never
// renames or unnames an untitled entry. A line is read as memrank.ParseDirectives and
// ParseOntologyFields do: a bullet whose first backticked token starts with @ is a
// directive wherever it is; a bare @name line is one outside a fence or an indented
// block; a bullet with another backticked token is a field of a class.
func splitEntryBody(kind, body string) (heading, directives, fields, meaning string) {
	var dl, fl, ml []string
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case heading == "" && memrank.FirstHeadingName(line) != "":
			heading = line
		case strings.HasPrefix(t, "```"):
			inFence = !inFence
			ml = append(ml, line)
		case strings.HasPrefix(t, "- ") && len(memrank.ParseDirectives(t)) > 0:
			dl = append(dl, line)
		case strings.HasPrefix(t, "- ") && kind == planKindClass && len(memrank.ParseOntologyFields(t)) > 0:
			fl = append(fl, line)
		case !inFence && !strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "\t") && len(memrank.ParseDirectives(t)) > 0:
			dl = append(dl, line)
		default:
			ml = append(ml, line)
		}
	}
	return heading, strings.Join(dl, "\n"), strings.Join(fl, "\n"), strings.Join(ml, "\n")
}

// quoteMeaningText quotes each line of proposed meaning text that the reader would take
// as a declaration rather than text, as neutraliseDirectives quotes directive lines: a
// class field ("- `name` …") and a heading, which names an entry that has no title. A
// proposal's meaning never declares a field or names an entry.
func quoteMeaningText(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if (strings.HasPrefix(t, "- ") && len(memrank.ParseOntologyFields(t)) > 0) || memrank.FirstHeadingName(t) != "" {
			lines[i] = "> " + t
		}
	}
	return strings.Join(lines, "\n")
}

// planChangeAccept plans the accept's changes and refuses a class rename anything rests
// on (Q4) and a destructive change without the current fingerprint (P2.4). The writes
// carry the caller's fingerprint and nothing else: one minted here would let their own
// gates skip planning (ontologyPlanGate's matching-fingerprint shortcut), so a write the
// caller sent none for is planned again by the write itself.
func (d *Document) planChangeAccept(ctx context.Context, a changeAccept, given string, rep *ChangeAcceptReport) error {
	plan, err := d.planOntologyAt(ctx, a.key, a.mscope, a.changes)
	if err != nil {
		if errors.Is(err, connector.ErrOntologyPlanInvalid) || errors.Is(err, connector.ErrOntologyPlanNotFound) {
			return fmt.Errorf("%w: the change cannot be planned: %v", ErrChangeProposalInvalid, err)
		}
		return err
	}
	rep.Risk, rep.Complete = connector.OntologyRiskNone, plan.Complete
	for _, ch := range plan.Changes {
		if ch.Risk != connector.OntologyRiskNone {
			rep.Risk = ch.Risk
		}
		for name, c := range ch.Counts {
			if c.N > 0 && c.Effect != connector.OntologyEffectKept {
				if rep.Counts == nil {
					rep.Counts = map[string]int{}
				}
				rep.Counts[name] += c.N
			}
		}
		if why, pending := renameJobPending(ch, plan.Complete); pending {
			return fmt.Errorf("%w: %s. Reject the proposal, or retire the class and declare the new one", ErrChangeRenameJobPending, why)
		}
	}
	if r := d.ontologyPlanGate(ctx, a.key, a.mscope, "accept", a.tg.read.DocumentID, a.changes, given); r != nil {
		return changeWriteError(*r)
	}
	return nil
}

// applyChangeAccept writes the change to the target: one update at the revision read
// before the checks, then the move. given is the caller's fingerprint, the only one any
// write carries ("" when the caller sent none). rep.Applied names what was written, and
// only once it was.
//
// The move's own refusals are asked BEFORE anything is written (ontologyMoveRefusal),
// so a refusable move refuses the whole accept. Once the update is written, the move is
// made only when the ontology is exactly what the update left (onlyOwnWrite): then the
// caller's fingerprint, checked before the update, still covers it, and the move carries
// the fingerprint the update produced. Anything else after the update — a write in
// between, the move's conditional write failing — is ErrChangePartiallyApplied.
func (d *Document) applyChangeAccept(ctx context.Context, a changeAccept, given string, rep *ChangeAcceptReport) error {
	upd := map[string]any{"op": "update_chunk", "scope": "tenant", "id": a.mark.Target, "revision": a.revision,
		"plan_fingerprint": given}
	var parts []string
	if a.mark.Op == memrank.ChangeOpRetire {
		upd["status"] = memrank.OntologyStatusRetired
		parts = append(parts, "retire")
	}
	if a.title != nil {
		upd["title"] = *a.title
		parts = append(parts, "rename")
	}
	if a.body != nil {
		upd["body"] = *a.body
		parts = append(parts, "edit")
	}
	docID := a.tg.read.DocumentID
	var before string
	if a.mark.Parent != "" {
		row, ok, err := d.getChunkRow(ctx, a.key, a.mark.Target)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: %s is gone", ErrChangeTargetChanged, strconv.Quote(a.mark.TargetName))
		}
		if r := d.ontologyMoveRefusal(ctx, a.key, row, a.mark.Parent, given); r != nil {
			return changeWriteError(*r)
		}
	}
	// A move-only edit still sends the update, carrying nothing but the revision: it is
	// what refuses the accept when the target changed after the checks.
	if d.beforeAcceptWrite != nil {
		d.beforeAcceptWrite()
	}
	if a.mark.Parent != "" {
		var err error
		if before, err = d.ontologyFingerprint(noWrites(ctx), a.key, docID); err != nil {
			return err
		}
	}
	if r, err := d.execOpResult(ctx, upd); err != nil {
		return err
	} else if r.IsError {
		if cur, ok, gerr := d.getChunkRow(ctx, a.key, a.mark.Target); gerr == nil && (!ok || cur.Revision != a.revision) {
			return fmt.Errorf("%w: %s was changed after the proposal was checked; nothing was applied, accept it again",
				ErrChangeTargetChanged, strconv.Quote(a.mark.TargetName))
		}
		return changeWriteError(r)
	}
	rep.Applied = append(rep.Applied, parts...)
	if a.mark.Parent == "" {
		return nil
	}
	partial := func(why string) error {
		did := "nothing but its revision"
		if len(rep.Applied) > 0 {
			did = strings.Join(rep.Applied, ", ")
		}
		return fmt.Errorf("%w: applied %s to %s, then %s; the move was not made and the proposal stays (reject it, "+
			"and move the entry in the editor)", ErrChangePartiallyApplied, did, strconv.Quote(a.mark.TargetName), why)
	}
	if d.betweenAcceptWrites != nil {
		d.betweenAcceptWrites()
	}
	after, err := d.ontologyFingerprint(noWrites(ctx), a.key, docID)
	if err != nil {
		return partial("the ontology could not be read: " + err.Error())
	}
	cur, ok, err := d.getChunkRow(ctx, a.key, a.mark.Target)
	if err != nil || !ok || !onlyOwnWrite(before, after, int64(cur.Revision-a.revision)) {
		return partial("the ontology changed between the update and the move")
	}
	moveFP := ""
	if given != "" {
		sum := sha256.Sum256([]byte(after))
		moveFP = hex.EncodeToString(sum[:])
	}
	r, err := d.execOpResult(ctx, map[string]any{"op": "move_chunk", "scope": "tenant", "id": a.mark.Target,
		"new_parent_id": a.mark.Parent, "plan_fingerprint": moveFP})
	if err != nil {
		return partial("the move failed: " + err.Error())
	}
	if r.IsError {
		return partial("the move was refused: " + r.Text)
	}
	rep.Applied = append(rep.Applied, "move")
	return nil
}

// onlyOwnWrite reports whether the ontology fingerprint after an update differs from the
// one before only as that update makes it differ: the same document status, chunk count
// and tree, and a revision sum moved by exactly the target's own revision step (revDelta).
// Every other chunk write bumps a revision, adds or removes a chunk, or re-parents one,
// so any of them in between shows. The latest updated_at is not compared: the update
// moves it. The format is ontologyFingerprint's: doc|status|count|updated|revisions|tree.
func onlyOwnWrite(before, after string, revDelta int64) bool {
	b, a := strings.Split(before, "|"), strings.Split(after, "|")
	if len(b) != 6 || len(a) != 6 || revDelta < 1 {
		return false
	}
	bs, err1 := strconv.ParseInt(b[4], 10, 64)
	as, err2 := strconv.ParseInt(a[4], 10, 64)
	return err1 == nil && err2 == nil && a[0] == b[0] && a[1] == b[1] && a[2] == b[2] && a[5] == b[5] && as-bs == revDelta
}

// changeWriteError is the accept's error for a refused write: a plan refusal keeps its
// code and fingerprint, a lock is entry_locked, anything else the proposal is invalid.
func changeWriteError(r tools.Result) error {
	if code, ok := OntologyPlanRefusalCode(r.Text, r.Error); ok {
		var b ontologyPlanRefusalBody
		_ = json.Unmarshal([]byte(r.Text), &b)
		return &ChangePlanRefusal{Code: code, Message: b.Error, Fingerprint: b.Fingerprint}
	}
	if strings.Contains(r.Text, entryLockedCode+":") {
		return fmt.Errorf("%w: %s", ErrChangeEntryLocked, r.Text)
	}
	if r.Error != nil && (r.Error.Category == tools.CategoryBusiness || r.Error.Category == tools.CategoryValidation) {
		return fmt.Errorf("%w: %s", ErrChangeProposalInvalid, r.Text)
	}
	return errors.New(r.Text)
}

// supersedeChangeProposals rejects the other open proposals of the same target and op,
// saying why in each body. A failure is a warning: the change is applied, and a
// proposal left open is checked afresh if accepted.
func (d *Document) supersedeChangeProposals(ctx context.Context, a changeAccept) (rejected, warnings []string) {
	for _, p := range a.tg.read.Proposals {
		if p.Change == nil || p.ChunkID == a.self.ID || p.Status != memrank.OntologyStatusProposed ||
			p.Change.Target != a.mark.Target || p.Change.Op != a.mark.Op {
			continue
		}
		row, ok, err := d.getChunkRow(ctx, a.key, p.ChunkID)
		if err == nil && ok {
			var b chunkBody
			if b, err = d.readBody(ctx, a.mscope, a.key, p.ChunkID); err == nil {
				why := "> Rejected: proposal " + a.self.ID + ", which " + a.mark.Op + "s the same entry, was accepted; " +
					"this one was filed against the entry as it was."
				_, err = d.execOp(ctx, map[string]any{"op": "update_chunk", "scope": "tenant", "id": p.ChunkID,
					"revision": row.Revision, "status": memrank.OntologyStatusRejected,
					"body": strings.TrimSpace(strings.TrimSpace(b.Body) + "\n\n" + why)})
			}
		}
		if err != nil || !ok {
			warnings = append(warnings, "the open proposal "+p.ChunkID+" for the same change could not be rejected; reject it")
			continue
		}
		rejected = append(rejected, p.ChunkID)
	}
	return rejected, warnings
}

// execOpResult runs one Document op on ctx (the operator's) and returns its result,
// refusals included, so the caller can tell them apart.
func (d *Document) execOpResult(ctx context.Context, req map[string]any) (tools.Result, error) {
	raw, _ := json.Marshal(req)
	return d.Execute(ctx, raw)
}
