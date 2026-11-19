package builtin

import (
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
