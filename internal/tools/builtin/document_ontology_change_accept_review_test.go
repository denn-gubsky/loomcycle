package builtin

import (
	"errors"
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
