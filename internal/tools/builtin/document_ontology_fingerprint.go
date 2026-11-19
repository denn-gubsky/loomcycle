package builtin

// document_ontology_fingerprint.go — a plan's fingerprint before a destructive change
// to the tenant ontology (RFC GQ D2, plan phase P2.4).
//
// WHAT NEEDS ONE. A change to an in-force entry that the plan (RFC GQ P1,
// document_ontology_plan.go) classes `destroys` or `stales` — that is, one with
// dependents:
//
//   - update_chunk renaming it (its title, or its body's first heading when the title is
//     empty), retiring it (status `retired`), or editing its body so its declaration
//     changes beyond the meaning (@identity, a relation characteristic such as
//     @functional or @asymmetric, a policy directive — whatever the plan's
//     entrySignature compares);
//   - update_chunk changing or clearing an adopted instance's registry_key (in fields):
//     the plan finds the instance's subject by it, so it is planned as a retire;
//   - move_chunk taking it under another parent;
//   - a change to a chunk that is not an entry — a section header whose body changes how
//     the section is read, an unnamed wrapper retired, titled or moved, a header moved —
//     planned as what it does to the entries under it: each one leaving the read or read
//     as another kind is a retire, each one the reader places elsewhere a move
//     (structuralPlanGate).
//
// The call carries plan_fingerprint, the fingerprint a plan reports (sha256 of
// ontologyFingerprint, hex). Missing → plan_required; not the current one →
// plan_stale. Both carry the current fingerprint. Additions, edits of the meaning alone
// and any change to an entry nothing rests on stay direct (D2), and so does
// reorder_chunk: it never changes a parent, and nothing stored is keyed by a sibling's
// position.
//
// FOR EVERY CALLER, the operator plane and an admin included, like the delete refusal
// and the lock (document_ontology_delete.go, document_ontology_locked.go): the check
// sits inside the handlers. An agent never reaches it for update_chunk, which
// guardOntologyWrite refuses on the ontology before the handler runs. It runs after the
// lock checks, so a locked entry answers entry_locked, which no fingerprint lifts.
//
// THE PLAN DECIDES, NOT A SECOND RULE. Whether a change needs a fingerprint is the risk
// the plan reports for it, so the refusal and the plan the caller reads cannot
// disagree. A plan that could not count every user and agent scope in its budget
// (complete false) needs a fingerprint whatever its counts: an incomplete zero is not
// "nothing rests on it". A matching fingerprint is checked first and skips the count:
// the caller planned against this exact ontology.
//
// THE WINDOW. The check runs before the write, outside any transaction (on SQLite a
// scope has one connection, and the plan reads every scope of the tenant). A change to
// the ontology between the check and the write is not seen. The target chunk's own
// revision guard still holds.

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/sqlmem"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// The refusals' codes. The refusal's text is JSON — {code, error, fingerprint} — so a
// client reads the current fingerprint without parsing prose (as the ACL refusals do).
const (
	planRequiredCode = "plan_required"
	planStaleCode    = "plan_stale"
)

// ontologyPlanRefusalBody is a plan_required / plan_stale refusal's text.
type ontologyPlanRefusalBody struct {
	Code        string `json:"code"`
	Error       string `json:"error"`
	Fingerprint string `json:"fingerprint"`
}

// OntologyPlanRefusalCode reports whether a failed Document result (its text and
// structure) is a plan_required or plan_stale refusal and, if so, its code. The
// operator HTTP route answers one with 409 and the text's fields as its body.
func OntologyPlanRefusalCode(text string, info *tools.ErrorInfo) (string, bool) {
	if info == nil || info.Category != tools.CategoryBusiness {
		return "", false
	}
	var b ontologyPlanRefusalBody
	if json.Unmarshal([]byte(text), &b) != nil || b.Fingerprint == "" {
		return "", false
	}
	switch b.Code {
	case planRequiredCode, planStaleCode:
		return b.Code, true
	}
	return "", false
}

// ontologyUpdatePlanRefusal is update_chunk's check: the changes the update makes to an
// in-force entry of the tenant ontology, gated on the plan. nil outside the ontology,
// for a chunk that is not an entry, and for an update that renames, retires and
// re-declares nothing. Fails closed.
func (d *Document) ontologyUpdatePlanRefusal(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, row chunkRow, in docInput, present map[string]json.RawMessage) *tools.Result {
	_, hasTitle := present["title"]
	_, hasBody := present["body"]
	_, hasFields := present["fields"]
	_, hasStatus := present["status"]
	retiring := hasStatus && memrank.IsRetiredEntityStatus(in.Status) && !memrank.IsRetiredEntityStatus(row.Status)
	if key.Scope != "tenant" || (!hasTitle && !hasBody && !hasFields && !retiring) {
		return nil
	}
	fail := func(err error) *tools.Result {
		r := errFrom("update_chunk: could not tell whether the update needs a plan of the tenant ontology, so nothing was changed: "+err.Error(), err)
		return &r
	}
	rctx := noWrites(ctx)
	read, e, ok, err := d.ontologyEntryOf(rctx, key, mscope, row)
	if err != nil {
		return fail(err)
	}
	if read.DocumentID == "" {
		return nil // not the tenant ontology
	}
	if read.partial {
		return lockReadPartial("update_chunk")
	}
	cur, err := d.readBody(rctx, mscope, key, row.ID)
	if err != nil {
		return fail(err)
	}
	// What the chunk will hold after the update: an edit is judged with the fields it
	// writes, not the stored ones.
	next := cur
	if hasBody {
		next.Body = in.Body
	}
	if hasFields {
		next.Fields = in.Fields
	}
	if !ok {
		// Not an entry (a section header, an unnamed wrapper, the root), but what the
		// reader makes of the entries under it can still change.
		ov := newOntologyOverlay()
		if hasTitle {
			ov.titles[row.ID] = in.Title
		}
		ov.bodies[row.ID] = next
		if retiring {
			ov.gone[row.ID] = true
		}
		return d.structuralPlanGate(ctx, key, mscope, "update_chunk", read, ov, row.ID, in.PlanFingerprint)
	}
	var changes []connector.OntologyChange
	if retiring {
		changes = append(changes, connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: e.ChunkID})
	}
	if registryKeyChanged(ctx, e, cur.Fields, in.Fields, hasFields) && !retiring {
		// The plan finds an adopted instance's subject by this key; a changed or cleared
		// key leaves the subject as a retirement leaves it.
		changes = append(changes, connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: e.ChunkID})
	}
	body := next.Body
	if hasTitle || hasBody {
		title := row.Title
		if hasTitle {
			title = in.Title
		}
		name := memrank.TrimOntologyName(title)
		if name == "" {
			name = memrank.FirstHeadingName(body)
		}
		switch {
		case name == e.Name:
		case name == "" && !retiring:
			// With no name the reader leaves the entry out, and what rests on it is left
			// as a retirement leaves it: planned as one.
			changes = append(changes, connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: e.ChunkID})
		case name != "":
			changes = append(changes, connector.OntologyChange{Op: connector.OntologyPlanRename, Entry: e.ChunkID, Name: name})
		}
	}
	if hasBody && in.Body != cur.Body {
		redeclares, err := d.editRedeclares(rctx, key, mscope, read, e, next)
		if err != nil {
			return fail(err)
		}
		if redeclares {
			changes = append(changes, connector.OntologyChange{Op: connector.OntologyPlanEdit, Entry: e.ChunkID, Body: in.Body})
		}
	}
	return d.ontologyPlanGate(ctx, key, mscope, "update_chunk", read.DocumentID, changes, in.PlanFingerprint)
}

// structuralPlanGate gates a change to chunk top (and so to everything under it) given as
// an overlay: the ontology is read with the overlay applied, and each in-force entry of
// top's subtree that the change takes out of the read, or reads as another kind or
// section, is planned as a retire; one whose place in the reader's tree changes is
// planned as a move (as a retire when its new place is not one a plan can name, such as
// a chunk the change itself makes an entry). before is the ontology as read before the
// change; a partial read on either side fails closed.
func (d *Document) structuralPlanGate(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op string,
	before OntologyRead, ov *ontologyOverlay, top, given string) *tools.Result {
	fail := func(err error) *tools.Result {
		r := errFrom(op+": could not tell whether the change needs a plan of the tenant ontology, so nothing was changed: "+err.Error(), err)
		return &r
	}
	rctx := noWrites(ctx)
	after, err := d.ontologyForKey(rctx, key, mscope, memrank.OntologyPath, ov)
	if err != nil {
		return fail(err)
	}
	if before.partial || after.partial {
		return lockReadPartial(op)
	}
	rows, err := d.planChunkRows(rctx, key, before.DocumentID)
	if err != nil {
		return fail(err)
	}
	changes := structuralChanges(before, after, rows, ov, top)
	return d.ontologyPlanGate(ctx, key, mscope, op, before.DocumentID, changes, given)
}

// structuralChanges lists the plan changes standing for what an overlay does to the
// in-force entries of top's subtree (see structuralPlanGate).
func structuralChanges(before, after OntologyRead, rows planRows, ov *ontologyOverlay, top string) []connector.OntologyChange {
	was, now := planEntries(before), planEntries(after)
	moved := planRows{parent: make(map[string]string, len(rows.parent))}
	for id, p := range rows.parent {
		moved.parent[id] = p
	}
	for id, p := range ov.parents {
		moved.parent[id] = p
	}
	var out []connector.OntologyChange
	for _, id := range rows.subtree(top) {
		e, ok := was[id]
		if !ok {
			continue
		}
		retire := connector.OntologyChange{Op: connector.OntologyPlanRetire, Entry: id}
		a, still := now[id]
		if !still || a.Kind != e.Kind || a.Section != e.Section {
			out = append(out, retire)
			continue
		}
		// The reader places an entry under its nearest in-force ancestor (a proposal or
		// an unnamed chunk in between is skipped), so a change that leaves that ancestor
		// as it was changes nothing stored.
		from := readerParent(before, rows, was, rows.parent[id])
		to := readerParent(after, moved, now, moved.parent[id])
		if from == to {
			continue
		}
		inSubtree := map[string]bool{}
		for _, s := range rows.subtree(id) {
			inSubtree[s] = true
		}
		if planMoveParent("", e, to, before, was, inSubtree) != nil {
			out = append(out, retire)
			continue
		}
		out = append(out, connector.OntologyChange{Op: connector.OntologyPlanMove, Entry: id, Parent: to})
	}
	return out
}

// instanceKeyWrite marks the instance adoption's own write of an instance's
// registry_key (setInstanceRegistryKey): the key it records is the one the plan counts
// by, so that write is not a change of it.
type instanceKeyWrite struct{}

func withInstanceKeyWrite(ctx context.Context) context.Context {
	return context.WithValue(ctx, instanceKeyWrite{}, true)
}

// registryKeyChanged reports whether an update changes or clears the registry_key an
// in-force instance carries. A first key (none set) and adoption's own write are not.
func registryKeyChanged(ctx context.Context, e planEntry, stored, next json.RawMessage, hasFields bool) bool {
	if !hasFields || e.Kind != planKindInstance {
		return false
	}
	if marked, _ := ctx.Value(instanceKeyWrite{}).(bool); marked {
		return false
	}
	old, now := registryKeyOf(stored), registryKeyOf(next)
	return old != "" && old != now
}

func registryKeyOf(fields json.RawMessage) string {
	var f struct {
		RegistryKey string `json:"registry_key"`
	}
	if len(fields) == 0 || json.Unmarshal(fields, &f) != nil {
		return ""
	}
	return f.RegistryKey
}

// readerParent is the chunk the reader takes as the parent of an entry placed under id:
// id itself when it is an in-force entry, a section header or the root, else the
// nearest such ancestor. Bounded by the rows read, so a malformed tree ends.
func readerParent(read OntologyRead, rows planRows, entries map[string]planEntry, id string) string {
	sections := map[string]bool{read.RootChunkID: true}
	for _, s := range read.SectionChunks {
		sections[s] = true
	}
	for i, at := 0, id; at != "" && i <= len(rows.parent); i++ {
		if _, ok := entries[at]; ok || sections[at] {
			return at
		}
		at = rows.parent[at]
	}
	return id
}

// ontologyEntryOf reads the tenant ontology when row is a chunk of it and returns the
// in-force entry row is; ok is false outside the ontology and for a chunk that is not an
// entry (the root, a section header, a proposal).
func (d *Document) ontologyEntryOf(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, row chunkRow) (OntologyRead, planEntry, bool, error) {
	ontID, err := d.resolveOntologyID(ctx, key)
	if err != nil || ontID == "" || ontID != row.DocumentID {
		return OntologyRead{}, planEntry{}, false, err
	}
	read, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, nil)
	if err != nil {
		return OntologyRead{}, planEntry{}, false, err
	}
	e, ok := planEntries(read)[row.ID]
	return read, e, ok, nil
}

// editRedeclares reports whether the new body changes the entry's declaration beyond its
// meaning: the same comparison the plan makes for an edit (entrySignature), so a
// meaning-only edit is never counted. The plan then says whether anything rests on what
// changed.
func (d *Document) editRedeclares(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, before OntologyRead, e planEntry, next chunkBody) (bool, error) {
	ov := newOntologyOverlay()
	ov.bodies[e.ChunkID] = next
	after, err := d.ontologyForKey(ctx, key, mscope, memrank.OntologyPath, ov)
	if err != nil {
		return false, err
	}
	return entrySignature(before, e) != entrySignature(after, e), nil
}

// ontologyPlanGate refuses changes the plan classes destroys or stales unless given is
// the fingerprint a plan reports now. op names the refused call.
func (d *Document) ontologyPlanGate(ctx context.Context, key sqlmem.ScopeKey, mscope store.MemoryScope, op, docID string, changes []connector.OntologyChange, given string) *tools.Result {
	if len(changes) == 0 {
		return nil
	}
	given = strings.TrimSpace(given)
	now, err := d.ontologyPlanFingerprint(noWrites(ctx), key, docID)
	if err != nil {
		r := errFrom(op+": could not read the tenant ontology's fingerprint, so nothing was changed: "+err.Error(), err)
		return &r
	}
	if given != "" && given == now {
		return nil
	}
	// A structural change can touch more entries than one plan takes: plan them in
	// batches, as the delete guard does.
	var risky []connector.OntologyPlannedChange
	complete, planned := true, now
	for start := 0; start < len(changes); start += ontologyPlanMaxChanges {
		plan, err := d.planOntologyAt(ctx, key, mscope, changes[start:min(start+ontologyPlanMaxChanges, len(changes))])
		if err != nil {
			return planGateError(op, err)
		}
		for _, ch := range plan.Changes {
			if ch.Risk != connector.OntologyRiskNone {
				risky = append(risky, ch)
			}
		}
		complete = complete && plan.Complete
		planned = plan.Fingerprint
	}
	if len(risky) == 0 && complete {
		return nil
	}
	// The plan's own fingerprint: the one the caller would get by planning now.
	return planFingerprintRefusal(op, given, planned, risky, complete)
}

// planGateError is the refusal when the change cannot be planned: the plan refuses it
// (a move under a parent the entry cannot have) or the read failed.
func planGateError(op string, err error) *tools.Result {
	var r tools.Result
	switch {
	case errors.Is(err, connector.ErrOntologyPlanInvalid), errors.Is(err, connector.ErrOntologyPlanNotFound):
		r = errValidation(op+": the change to the tenant ontology cannot be planned, so nothing was changed: "+err.Error(),
			"Plan it (POST /v1/_ontology/plan, or the ontology tool's plan op) to see why.")
	case errors.Is(err, connector.ErrOntologyPlanUnavailable):
		r = errTransient(op+": the change to the tenant ontology could not be planned, so nothing was changed: "+err.Error(), "Try again.")
	default:
		r = errFrom(op+": the change to the tenant ontology could not be planned, so nothing was changed: "+err.Error(), err)
	}
	return &r
}

// planFingerprintRefusal is the plan_required / plan_stale refusal, naming what rests on
// each risky change.
func planFingerprintRefusal(op, given, now string, risky []connector.OntologyPlannedChange, complete bool) *tools.Result {
	code, why := planRequiredCode, "it needs the fingerprint of a plan of the change (plan_fingerprint)"
	if given != "" {
		code, why = planStaleCode, "the plan_fingerprint sent is not the ontology's current one: it changed since that plan"
	}
	var named []string
	for i, ch := range risky {
		if i == ontologyDeleteNamed {
			named = append(named, "and "+strconv.Itoa(len(risky)-ontologyDeleteNamed)+" more")
			break
		}
		named = append(named, ch.Op+" of "+ch.Entry.Kind+" "+strconv.Quote(ch.Entry.Name)+" ("+affectedCounts(ch)+")")
	}
	what := "something rests on what this changes: " + strings.Join(named, "; ")
	if len(risky) == 0 && !complete {
		what = "the plan could not check every user and agent scope of the tenant in time, so what rests on the entry is not known"
	}
	text, _ := json.Marshal(ontologyPlanRefusalBody{
		Code:        code,
		Error:       op + ": " + code + ": " + what + "; " + why + ". Nothing was changed.",
		Fingerprint: now,
	})
	r := errBusiness(string(text), "Plan the change (POST /v1/_ontology/plan, or the ontology tool's plan op), read what it "+
		"stales or destroys, then send the same call with plan_fingerprint set to the plan's fingerprint.")
	return &r
}

// affectedCounts is "statements_of_kind 1 stale, typed_documents 3 stale" for the
// counts a change does not keep, by name.
func affectedCounts(ch connector.OntologyPlannedChange) string {
	var parts []string
	for name, c := range ch.Counts {
		if c.N > 0 && c.Effect != connector.OntologyEffectKept {
			parts = append(parts, name+" "+strconv.Itoa(c.N)+" "+c.Effect)
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
