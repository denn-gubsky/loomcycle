package builtin

import (
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// importedFixture imports subjectOutline as an agent, under the given header, and
// accepts it as the operator. f.ctx is the operator plane.
func importedFixture(t *testing.T, propose string) *retiredFixture {
	t.Helper()
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	out, r := docExec(t, d, agentCtx, propose)
	if r.IsError {
		t.Fatalf("propose: %s", refusalText(r))
	}
	op := operatorCtx(opCtx)
	acceptOrFail(t, d, op, out["chunk_id"].(string))
	return &retiredFixture{t: t, d: d, ctx: op}
}

func importSubjectOutline(t *testing.T) *retiredFixture {
	t.Helper()
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	out, r := importSchemeCall(t, d, agentCtx, map[string]any{"markdown": subjectOutline})
	if r.IsError {
		t.Fatalf("import: %s", refusalText(r))
	}
	op := operatorCtx(opCtx)
	acceptOrFail(t, d, op, out["chunk_id"].(string))
	return &retiredFixture{t: t, d: d, ctx: op}
}

// TestImportScheme_AdoptedConceptsAreLocked (RFC GM P5 "Adopted schemes"): accepting an
// imported scheme locks every concept of it, so a published scheme's concepts cannot be
// renamed, moved, retired or deleted — by the operator either — until an operator takes
// the line out. The scheme entry itself carries no lock.
func TestImportScheme_AdoptedConceptsAreLocked(t *testing.T) {
	f := importSubjectOutline(t)
	s := schemeNamed(f.read(), "Subject headings")
	if s == nil || len(s.Concepts) != 5 {
		t.Fatalf("scheme = %#v", s)
	}
	if s.Locked {
		t.Error("the scheme entry carries @locked; only its concepts should")
	}
	for _, c := range s.Concepts {
		if !c.Locked {
			t.Errorf("%s is not locked", c.Tag)
		}
		if !memrank.BodyLocked(f.body(c.ChunkID)) {
			t.Errorf("%s: the body has no @locked line: %q", c.Tag, f.body(c.ChunkID))
		}
	}
	id := f.chunk("Probability")
	wantLocked(t, "rename", f.update(f.ctx, id, map[string]any{"title": "Chance"}), `concept "Probability" carries @locked`)
	wantLocked(t, "move", f.move(f.ctx, id, f.chunk("Physics")), `concept "Probability" carries @locked`)
	wantLocked(t, "retire", f.setStatus(f.ctx, id, memrank.OntologyStatusRetired), `concept "Probability"`)
	wantLocked(t, "delete", f.deleteChunk(f.ctx, id), `concept "Probability"`)
	// The unlocked scheme entry is held by the locked concepts under it.
	wantLocked(t, "retire the scheme", f.setStatus(f.ctx, s.ChunkID, memrank.OntologyStatusRetired), "above a locked entry")
	// The text stays the operator's to edit; taking the line out unlocks the concept.
	if text := f.update(f.ctx, id, map[string]any{"body": "@notation QA273\nThe study of chance."}); text != "" {
		t.Fatalf("the operator's edit removing the lock was refused: %s", text)
	}
	if text := f.update(f.ctx, id, map[string]any{"title": "Chance"}); text != "" {
		t.Errorf("rename after the lock was removed: %s", text)
	}
}

// TestImportScheme_LocalSchemeIsNotLocked: a scheme proposed as generated (or declared,
// or with no origin) accepts with no lock on any concept.
func TestImportScheme_LocalSchemeIsNotLocked(t *testing.T) {
	f := importedFixture(t, aircraftProposal) // @origin generated
	s := schemeNamed(f.read(), "Aircraft systems")
	if s == nil || len(s.Concepts) != 3 {
		t.Fatalf("scheme = %#v", s)
	}
	for _, c := range s.Concepts {
		if c.Locked || memrank.DeclaresLocked(f.body(c.ChunkID)) {
			t.Errorf("%s of a generated scheme is locked: %q", c.Tag, f.body(c.ChunkID))
		}
	}
	if text := f.update(f.ctx, f.chunk("Pumps"), map[string]any{"title": "Pump units"}); text != "" {
		t.Errorf("a rename in a generated scheme was refused: %s", text)
	}
}

// TestImportScheme_LeftoverAdoptedConceptIsLocked: a concept left proposed when its
// adopted scheme was accepted part-way is locked when it is accepted on its own.
func TestImportScheme_LeftoverAdoptedConceptIsLocked(t *testing.T) {
	d, opCtx, agentCtx := schemeProposalFixture(t, schemesOntologyMD)
	out, r := importSchemeCall(t, d, agentCtx, map[string]any{"markdown": subjectOutline})
	if r.IsError {
		t.Fatalf("import: %s", refusalText(r))
	}
	op := operatorCtx(opCtx)
	f := &retiredFixture{t: t, d: d, ctx: op}
	// Detach Mechanics from the gather by stripping its proposal mark while the scheme
	// is accepted, then put the mark back: what a store fault part-way leaves.
	id := f.chunk("Mechanics")
	fields := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["fields"]
	if text := f.update(op, id, map[string]any{"fields": map[string]any{}}); text != "" {
		t.Fatalf("strip mark: %s", text)
	}
	acceptOrFail(t, d, op, out["chunk_id"].(string))
	if text := f.update(op, id, map[string]any{"fields": fields}); text != "" {
		t.Fatalf("restore mark: %s", text)
	}
	if rep := acceptOrFail(t, d, op, id); rep.Concepts != 1 {
		t.Fatalf("leftover accept = %#v", rep)
	}
	if c := conceptOfTag(f.read(), "subject-headings/physics/mechanics"); !c.Locked {
		t.Errorf("leftover concept = %#v, want it locked", c)
	}
}

// TestImportScheme_LocalChildUnderAdoptedConcept: a lock holds the adopted concept's
// name and place, not what lies under it — a local concept proposed under it is
// accepted, carries no lock, and the adopted parent stays locked (RFC GM P5.2 growth).
func TestImportScheme_LocalChildUnderAdoptedConcept(t *testing.T) {
	f := importSubjectOutline(t)
	out, r := docExec(t, f.d, f.ctx, `{"op":"propose_entity","scope":"tenant","section":"schemes",`+
		`"name":"Quantum computing","parent":"subject-headings/mathematics/computer-science","body":"local growth"}`)
	if r.IsError {
		t.Fatalf("propose a local child: %s", refusalText(r))
	}
	child := out["chunk_id"].(string)
	if text := f.setStatus(f.ctx, child, ""); text != "" {
		t.Fatalf("accept the local child: %s", text)
	}
	read := f.read()
	c := conceptOfTag(read, "subject-headings/mathematics/computer-science/quantum-computing")
	if c.ChunkID != child || c.Locked {
		t.Errorf("local child = %#v, want in force and unlocked", c)
	}
	if p := conceptOfTag(read, "subject-headings/mathematics/computer-science"); !p.Locked {
		t.Errorf("adopted parent = %#v, want it still locked", p)
	}
	if text := f.update(f.ctx, child, map[string]any{"title": "Quantum information"}); text != "" {
		t.Errorf("a rename of the local child was refused: %s", text)
	}
}
