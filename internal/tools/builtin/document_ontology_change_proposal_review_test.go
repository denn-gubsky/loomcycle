package builtin

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// changeMark reads the change_proposal mark of the proposal chunk id.
func (f changeFixture) changeMark(id any) memrank.ProposedChange {
	f.t.Helper()
	g := f.must(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
	raw, _ := json.Marshal(g["fields"])
	var mark memrank.ProposedChange
	if !proposalMark(raw, changeProposalField, &mark) {
		f.t.Fatalf("no change_proposal mark in %s", raw)
	}
	return mark
}

// TestProposeChange_NullDirectivesAreAbsent: a literal directives null is no directive
// set — not "clear them" — so it neither makes an edit change something nor routes an
// add proposal into the change path.
func TestProposeChange_NullDirectivesAreAbsent(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	f.refused(map[string]any{"change": "edit", "entry": "Provision", "directives": nil}, "changes nothing")
	out := f.mustPropose(map[string]any{"change": "edit", "entry": "Provision", "meaning": "Reworded.", "directives": nil})
	if m := f.changeMark(out["chunk_id"]); m.SetsDirectives || len(m.Directives) != 0 {
		t.Errorf("a null directive set was filed as %+v", m)
	}
	f.refused(map[string]any{"change": "retire", "entry": "Memo", "directives": nil, "meaning": "x"}, "names only its entry")
	if _, r := f.propose(map[string]any{"name": "Gadget", "directives": nil}); r.IsError {
		t.Errorf("an add proposal with directives null was refused: %s", refusalText(r))
	}
}

// TestProposeChange_MovedConceptNameMustBeFreeUnderTheNewParent: a concept moved under
// its old name collides under the new parent as a rename would.
func TestProposeChange_MovedConceptNameMustBeFreeUnderTheNewParent(t *testing.T) {
	md := strings.Replace(retiredOntologyMD, "#### Reporting\n", "#### Reporting\n\n##### Outsourcing\n", 1)
	f := newChangeFixture(t, md)
	f.refused(map[string]any{"change": "edit", "entry": "topics/reporting/outsourcing", "parent": "Risk Management"}, "already in force")
	f.mustPropose(map[string]any{"change": "edit", "entry": "topics/reporting/outsourcing", "parent": "Risk Management", "name": "Vendors"})
}

// TestProposeChange_NewNameIsNormalised: a new name is trimmed as an entry name is (a
// leading '#' included), stored that way, and refused when nothing is left.
func TestProposeChange_NewNameIsNormalised(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	out := f.mustPropose(map[string]any{"change": "edit", "entry": "Provision", "name": "## Clause "})
	if m := f.changeMark(out["chunk_id"]); m.Name != "Clause" {
		t.Errorf("name stored as %q, want Clause", m.Name)
	}
	f.refused(map[string]any{"change": "edit", "entry": "Memo", "name": " ## "}, "name is empty")
}

// TestProposeChange_DirectiveSetIsBounded: the values of a directive set and its encoded
// size are capped, so a list cannot grow the mark without bound.
func TestProposeChange_DirectiveSetIsBounded(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	many := make([]string, 65)
	for i := range many {
		many[i] = "label"
	}
	f.refused(map[string]any{"change": "edit", "entry": "topics/reporting", "directives": map[string]any{"alt": many}}, "at most 64 values")
	long := make([]string, 10)
	for i := range long {
		long[i] = strings.Repeat("x", 900)
	}
	f.refused(map[string]any{"change": "edit", "entry": "topics/reporting", "directives": map[string]any{"alt": long}}, "bytes")
}

// TestProposeChange_InstanceKeepsItsRelationDirectives: an instance's complete directive
// set may carry its relation directives (@<kind> target), which ParseInstanceBody reads
// as statements; a known directive of another section is still refused.
func TestProposeChange_InstanceKeepsItsRelationDirectives(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	out := f.mustPropose(map[string]any{"change": "edit", "entry": "Acme", "section": "instances",
		"directives": map[string]any{"a": "organization", "supervises": "Globex"}})
	want := []memrank.ProposedDirective{{Name: "a", Value: "organization"}, {Name: "supervises", Value: "Globex"}}
	if m := f.changeMark(out["chunk_id"]); len(m.Directives) != 2 || m.Directives[0] != want[0] || m.Directives[1] != want[1] {
		t.Errorf("directives = %+v, want %+v", m.Directives, want)
	}
	f.refused(map[string]any{"change": "edit", "entry": "Globex", "section": "instances",
		"directives": map[string]any{"a": "organization", "section": "classes"}}, "not a directive")
	f.refused(map[string]any{"change": "edit", "entry": "Globex", "section": "instances",
		"directives": map[string]any{"a": "organization", "x\n@section": "y"}}, "not a directive")
}

// TestProposeChange_RenameToAStandardRelationKindIsRefused, as the add path refuses it.
func TestProposeChange_RenameToAStandardRelationKindIsRefused(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	declared := map[string]bool{}
	for _, r := range f.read().Relations {
		declared[r.Name] = true
	}
	seed := ""
	for _, r := range memrank.SeedRelations() {
		if !declared[r.Name] && !memrank.IsReservedRelation(r.Name) {
			seed = r.Name
			break
		}
	}
	if seed == "" {
		t.Fatal("no standard relation kind outside the fixture")
	}
	f.refused(map[string]any{"change": "edit", "entry": "supervises", "name": seed}, "is a standard relation kind and already valid")
}

// TestProposeChange_RetiredReferenceIsRefused: a directive naming a retired entry —
// a relation's domain, a scheme's @projects, an instance's @a — is a new use of it.
func TestProposeChange_RetiredReferenceIsRefused(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	f.retire("Memo")
	f.refused(map[string]any{"change": "edit", "entry": "supervises", "directives": map[string]any{"domain": "Memo"}}, "entry_retired")
	f.refused(map[string]any{"change": "edit", "entry": "topics", "directives": map[string]any{"projects": "Provision, Memo"}}, "entry_retired")
	f.refused(map[string]any{"change": "edit", "entry": "Acme", "section": "instances", "directives": map[string]any{"a": "memo"}}, "entry_retired")
	f.retire("filed_under")
	f.refused(map[string]any{"change": "edit", "entry": "supervises", "directives": map[string]any{"inverse": "filed_under"}}, "entry_retired")
	f.mustPropose(map[string]any{"change": "edit", "entry": "supervises", "directives": map[string]any{"domain": "Provision"}})
}

// TestProposeChange_ALockedProposalUnderTheEntryDoesNotLockIt: only entries count for a
// lock; a proposal (or rejection) under the entry whose text says @locked does not.
func TestProposeChange_ALockedProposalUnderTheEntryDoesNotLockIt(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	read := f.read()
	f.must(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": f.chunk("Provision"), "title": "Sub", "status": memrank.OntologyStatusProposed, "body": "@locked"})
	f.mustPropose(map[string]any{"change": "retire", "entry": "Provision"})
}

// TestProposeChange_ConcurrentDuplicatesFileOnce: the duplicate check and the filing
// run under one lock, so concurrent identical proposals file exactly one.
func TestProposeChange_ConcurrentDuplicatesFileOnce(t *testing.T) {
	f := newChangeFixture(t, retiredOntologyMD)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := json.Marshal(map[string]any{"op": "propose_entity", "change": "retire", "entry": "Provision"})
			res, err := f.d.Execute(f.agent, req)
			if err == nil && !res.IsError {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	n := 0
	for _, p := range f.read().Proposals {
		if p.Change != nil && p.Change.Op == memrank.ChangeOpRetire {
			n++
		}
	}
	if ok != 1 || n != 1 {
		t.Errorf("%d filings succeeded and %d proposals exist, want 1 and 1", ok, n)
	}
}
