package builtin

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// untitledEntry adds a class whose chunk has no title, named by its body's heading, as
// an operator can write one; it returns its chunk id.
func (f changeFixture) untitledEntry(heading, meaning string) string {
	f.t.Helper()
	read := f.read()
	id := f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": read.SectionChunks["classes"], "title": heading, "body": "#### " + heading + "\n" + meaning})["id"].(string)
	// create_chunk takes a title; an operator's later edit can clear it.
	if r := f.change(f.ctx, "update_chunk", id, map[string]any{"title": ""}); r.IsError {
		f.t.Fatalf("clear the title: %s", r.Text)
	}
	if e, ok := planEntries(f.read())[id]; !ok || e.Name != heading {
		f.t.Fatalf("the untitled entry reads as %+v (in force %v), want %q", e, ok, heading)
	}
	return id
}

// entryName is the name the reader gives chunk id, "" when it is not an entry in force.
func (f changeFixture) entryName(id string) string {
	f.t.Helper()
	return planEntries(f.read())[id].Name
}

// TestAcceptChange_MeaningEditKeepsAnUntitledEntrysName: an entry with no title is
// named by its body's first heading. A new meaning replaces the meaning, not that
// heading, so the entry keeps its name; a heading inside the proposed meaning is
// quoted text and renames nothing.
func TestAcceptChange_MeaningEditKeepsAnUntitledEntrysName(t *testing.T) {
	f := newAcceptFixture(t)
	for name, meaning := range map[string]string{
		"plain meaning":      "A schedule attached to an act.",
		"meaning heading it": "#### Hijacked\nA schedule attached to an act.",
	} {
		id := f.untitledEntry("Annex", "An appendix.")
		p := f.file(map[string]any{"change": "edit", "entry": id, "meaning": meaning})
		if _, err := f.accept(p, f.fingerprintNow()); err != nil {
			t.Fatalf("%s: accept: %v", name, err)
		}
		if got := f.entryName(id); got != "Annex" {
			t.Errorf("%s: the entry reads as %q after a meaning edit, want Annex; body %q", name, got, f.body(id))
		}
		f.change(f.ctx, "delete_chunk", id, nil)
	}
}

// TestAcceptChange_UnplannableBodyIsInvalid: an edit whose composed body is empty (its
// directives cleared from a body of nothing else) or over the plan's 32 KiB cannot be
// planned, so it is refused as invalid and nothing is written — also beside a rename
// that can be planned, whose fingerprint must not carry the body through unplanned.
func TestAcceptChange_UnplannableBodyIsInvalid(t *testing.T) {
	f := newAcceptFixture(t)
	filedUnder, provision := f.chunk("filed_under"), f.chunk("Provision")
	if r := f.change(f.ctx, "update_chunk", provision, map[string]any{"body": strings.Repeat("Long meaning. ", 2400)}); r.IsError {
		t.Fatal(r.Text)
	}
	for name, v := range map[string]map[string]any{
		"empty":    {"change": "edit", "entry": filedUnder, "name": "filed_in", "directives": map[string]any{}},
		"oversize": {"change": "edit", "entry": provision, "name": "Clause", "directives": map[string]any{"memory_scope": "tenant"}},
	} {
		target := v["entry"].(string)
		before := f.body(target)
		p := f.file(v)
		if _, err := f.accept(p, f.fingerprintNow()); !errors.Is(err, ErrChangeProposalInvalid) {
			t.Errorf("%s: accept = %v, want ErrChangeProposalInvalid", name, err)
		}
		if f.body(target) != before || !f.exists(p) {
			t.Errorf("%s: a refused accept wrote something", name)
		}
	}
}

// TestAcceptChange_EditThatChangesNothingStillAccepts: a proposed meaning equal to the
// entry's own leaves nothing to plan; the accept writes no change and removes the
// proposal rather than refusing it.
func TestAcceptChange_EditThatChangesNothingStillAccepts(t *testing.T) {
	f := newAcceptFixture(t)
	provision := f.chunk("Provision")
	if r := f.change(f.ctx, "update_chunk", provision, map[string]any{"body": "A clause of an act."}); r.IsError {
		t.Fatal(r.Text)
	}
	p := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "A clause of an act."})
	rep, err := f.accept(p, "")
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	if f.exists(p) || f.body(provision) != "A clause of an act." || len(rep.Applied) != 0 {
		t.Errorf("a no-op accept: proposal kept %v, body %q, applied %v", f.exists(p), f.body(provision), rep.Applied)
	}
}

// TestAcceptChange_WithoutACallerFingerprintTheWriteIsPlannedAgain: an accept sent with
// no fingerprint, of a rename nothing rested on when it was checked, is planned again by
// the write itself: a statement of the kind written meanwhile (which leaves the
// ontology, and so its fingerprint, as it was) makes it plan_required, and the kind
// keeps its name. The accept never hands the write a fingerprint of its own.
func TestAcceptChange_WithoutACallerFingerprintTheWriteIsPlannedAgain(t *testing.T) {
	f := newAcceptFixture(t)
	co := f.chunk("co_supervises")
	p := f.file(map[string]any{"change": "edit", "entry": co, "name": "co_oversees"})
	f.d.beforeAcceptWrite = func() {
		f.d.beforeAcceptWrite = nil
		a := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Carol"})["root_chunk_id"]
		b := f.must(map[string]any{"op": "create_document", "scope": "tenant", "title": "Dan"})["root_chunk_id"]
		f.must(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "co_supervises"})
	}
	_, err := f.accept(p, "")
	var pr *ChangePlanRefusal
	if !errors.As(err, &pr) || pr.Code != planRequiredCode {
		t.Errorf("accept = %v, want plan_required from the write's own plan", err)
	}
	if f.entryName(co) != "co_supervises" || !f.exists(p) {
		t.Error("the kind was renamed although a statement of it was written after the checks")
	}
}

// wantPartial asserts a partially applied accept: its own error, none of the
// nothing-written refusals, and a report naming what was written.
func wantPartial(t *testing.T, what string, rep ChangeAcceptReport, err error, applied []string) {
	t.Helper()
	var pr *ChangePlanRefusal
	if !errors.Is(err, ErrChangePartiallyApplied) || errors.Is(err, ErrChangeProposalInvalid) ||
		errors.Is(err, ErrChangeEntryLocked) || errors.Is(err, ErrChangeTargetChanged) || errors.As(err, &pr) {
		t.Errorf("%s: accept = %v, want ErrChangePartiallyApplied alone", what, err)
	}
	if !reflect.DeepEqual(rep.Applied, applied) {
		t.Errorf("%s: applied = %v, want %v", what, rep.Applied, applied)
	}
}

// TestAcceptChange_MoveRefusedAfterTheUpdateIsPartiallyApplied: when the move fails
// after the update was written, the accept says so — partially_applied, with what was
// applied — and never answers with a refusal that promises nothing was written.
func TestAcceptChange_MoveRefusedAfterTheUpdateIsPartiallyApplied(t *testing.T) {
	f := newAcceptFixture(t)
	provision, document, event := f.chunk("Provision"), f.chunk("Document"), f.chunk("event")
	p := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "A clause.", "parent": document})
	key := mustTenantKey(t, f.d, f.ctx)
	f.d.beforeMoveWrite = func() {
		f.d.beforeMoveWrite = nil
		if err := f.d.exec(f.ctx, key, `UPDATE chunks SET parent_id = ? WHERE id = ?`, event, provision); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := f.accept(p, f.fingerprintNow())
	wantPartial(t, "move refused", rep, err, []string{"edit"})
	if !f.exists(p) {
		t.Error("the proposal was removed after a partial apply")
	}
}

// TestAcceptChange_OntologyWriteBetweenUpdateAndMoveStopsTheMove: the move is made only
// when the ontology is exactly what the accept's update left; a write to another entry
// in between stops it (partially_applied), rather than the move carrying a fingerprint
// that hides that write.
func TestAcceptChange_OntologyWriteBetweenUpdateAndMoveStopsTheMove(t *testing.T) {
	f := newAcceptFixture(t)
	provision, document := f.chunk("Provision"), f.chunk("Document")
	p := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "A clause.", "parent": document})
	f.d.betweenAcceptWrites = func() {
		f.d.betweenAcceptWrites = nil
		if r := f.change(f.ctx, "update_chunk", f.chunk("Memo"), map[string]any{"body": "A short note."}); r.IsError {
			t.Fatal(r.Text)
		}
	}
	rep, err := f.accept(p, f.fingerprintNow())
	wantPartial(t, "write between", rep, err, []string{"edit"})
	if row, _, _ := f.d.getChunkRow(f.ctx, mustTenantKey(t, f.d, f.ctx), provision); row.ParentID == document {
		t.Error("the move was made after another write to the ontology")
	}
}

// TestAcceptChange_RefusedUpdateReportsNothingApplied: an update refused at the write
// leaves the report's applied list empty, so nothing is recorded as applied.
func TestAcceptChange_RefusedUpdateReportsNothingApplied(t *testing.T) {
	f := newAcceptFixture(t)
	provision := f.chunk("Provision")
	p := f.file(map[string]any{"change": "edit", "entry": provision, "name": "Clause", "meaning": "A clause."})
	f.d.beforeAcceptWrite = func() {
		f.d.beforeAcceptWrite = nil
		if r := f.change(f.ctx, "update_chunk", provision, map[string]any{"body": "Edited meanwhile."}); r.IsError {
			t.Fatal(r.Text)
		}
	}
	rep, err := f.accept(p, f.fingerprintNow())
	if !errors.Is(err, ErrChangeTargetChanged) || len(rep.Applied) != 0 {
		t.Errorf("accept = %v, applied %v; want target_changed with nothing applied", err, rep.Applied)
	}
}

// TestAcceptChange_EditDuringTheChecksIsRefused: the target's revision is read before
// the checks, so an edit of the target while they run is a revision conflict at the
// write, not a change the accept builds on and overwrites.
func TestAcceptChange_EditDuringTheChecksIsRefused(t *testing.T) {
	f := newAcceptFixture(t)
	provision := f.chunk("Provision")
	p := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "From the proposal."})
	f.d.duringAcceptChecks = func() {
		f.d.duringAcceptChecks = nil
		if r := f.change(f.ctx, "update_chunk", provision, map[string]any{"body": "Edited during the checks."}); r.IsError {
			t.Fatal(r.Text)
		}
	}
	if _, err := f.accept(p, f.fingerprintNow()); !errors.Is(err, ErrChangeTargetChanged) {
		t.Errorf("accept = %v, want ErrChangeTargetChanged", err)
	}
	if got := f.body(provision); got != "Edited during the checks." || !f.exists(p) {
		t.Errorf("target body %q, proposal kept %v", got, f.exists(p))
	}
}

// TestAcceptChange_MoveOfAnEntryInUseCarriesTheUpdatesFingerprint: moving an entry
// something rests on needs the caller's fingerprint, refused before anything is written
// without it; with it the update and the move both apply, the move carrying the
// fingerprint the update left.
func TestAcceptChange_MoveOfAnEntryInUseCarriesTheUpdatesFingerprint(t *testing.T) {
	f := newAcceptFixture(t)
	memo, provision := f.chunk("Memo"), f.chunk("Provision")
	p := f.file(map[string]any{"change": "edit", "entry": memo, "meaning": "A short note.", "parent": provision})
	var pr *ChangePlanRefusal
	if _, err := f.accept(p, ""); !errors.As(err, &pr) || pr.Code != planRequiredCode {
		t.Errorf("accept without a fingerprint = %v, want plan_required", err)
	}
	if strings.Contains(f.body(memo), "A short note.") {
		t.Error("a refused accept wrote the update")
	}
	rep, err := f.accept(p, f.fingerprintNow())
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	row, _, _ := f.d.getChunkRow(f.ctx, mustTenantKey(t, f.d, f.ctx), memo)
	if row.ParentID != provision || !reflect.DeepEqual(rep.Applied, []string{"edit", "move"}) {
		t.Errorf("parent %s applied %v", row.ParentID, rep.Applied)
	}
}

// TestAcceptChange_EditWithAMoveAppliesBoth: an edit that also moves the entry writes
// the update and then the move, with the caller's fingerprint and without one.
func TestAcceptChange_EditWithAMoveAppliesBoth(t *testing.T) {
	for name, withFP := range map[string]bool{"with a fingerprint": true, "without one": false} {
		f := newAcceptFixture(t)
		provision, document := f.chunk("Provision"), f.chunk("Document")
		p := f.file(map[string]any{"change": "edit", "entry": provision, "meaning": "A clause.", "parent": document})
		fp := ""
		if withFP {
			fp = f.fingerprintNow()
		}
		rep, err := f.accept(p, fp)
		if err != nil {
			t.Fatalf("%s: accept: %v", name, err)
		}
		row, _, _ := f.d.getChunkRow(f.ctx, mustTenantKey(t, f.d, f.ctx), provision)
		if row.ParentID != document || !reflect.DeepEqual(rep.Applied, []string{"edit", "move"}) || f.exists(p) {
			t.Errorf("%s: parent %s applied %v proposal kept %v", name, row.ParentID, rep.Applied, f.exists(p))
		}
	}
}
