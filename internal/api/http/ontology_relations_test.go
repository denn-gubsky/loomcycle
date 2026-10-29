package http

import (
	"context"
	"net/http"
	"strings"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
)

func relationsLayout(kinds ...ontologyTestChunk) []ontologyTestChunk {
	return []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{{title: "Provision", body: "- `anchor` anchor\n"}}},
		{title: "Relations", body: "@section relations\n", kids: kinds},
	}
}

// TestOntology_RelationBlockReachesThePromptOnlyForTheTenantsOwnKinds (RFC FO 10d.2).
//
// An empty Relations section brings the seed into force but lists nothing, so the prompt
// is byte-identical to the same ontology without one; a declared kind adds the block.
func TestOntology_RelationBlockReachesThePromptOnlyForTheTenantsOwnKinds(t *testing.T) {
	withoutSection := func() string {
		s, mi := ontologyFixture(t)
		replaceOntologyWithSections(t, s, mi, relationsLayout()[:1])
		s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
		return s.renderOntology(context.Background(), mi)
	}()

	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, relationsLayout())
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if got := s.renderOntology(context.Background(), mi); got != withoutSection {
		t.Errorf("an empty Relations section changed the prompt:\n got %q\nwant %q", got, withoutSection)
	}

	s2, mi2 := ontologyFixture(t)
	replaceOntologyWithSections(t, s2, mi2, relationsLayout(ontologyTestChunk{
		title: "stricter_than", body: "Sets a stricter version of the same requirement.\n@domain Provision\n@range Provision\n@inverse looser_than\n"}))
	s2.setOntologyStatus(t, mi2.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	got := s2.renderOntology(context.Background(), mi2)
	if !strings.HasPrefix(got, withoutSection) || !strings.Contains(got, "# Relation kinds") || !strings.Contains(got, "**stricter_than**") {
		t.Errorf("a declared kind should append the relation block after the unchanged entity block:\n%s", got)
	}
	if strings.Contains(got, "**cites**") {
		t.Errorf("a seed kind reached the prompt:\n%s", got)
	}
}

// TestOntologyAPI_ReportsRelationsAndTheirGate.
func TestOntologyAPI_ReportsRelationsAndTheirGate(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, relationsLayout(
		ontologyTestChunk{title: "references", body: "@inverse hijacked\n", kids: []ontologyTestChunk{{title: "cross_refers"}}},
		ontologyTestChunk{title: "stricter_than", body: "@domain Provision\n"},
	))
	draft := s.getOntology(t, mi.Tenant)
	if len(draft.Relations) != 3 || len(draft.EffectiveRelations) != 0 {
		t.Errorf("draft: relations %d (want 3), effective %d (want 0 — the gate)", len(draft.Relations), len(draft.EffectiveRelations))
	}
	reservedMarked := false
	for _, r := range draft.Relations {
		if r.Name == "references" && len(r.Issues) > 0 {
			reservedMarked = true
		}
	}
	if !reservedMarked {
		t.Error("the entry named after a reserved kind should carry an issue")
	}
	if !contains(draft.AdoptableRelations, "amends") || contains(draft.AdoptableRelations, "stricter_than") {
		t.Errorf("adoptable = %v", draft.AdoptableRelations)
	}
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	got := s.getOntology(t, mi.Tenant)
	if len(got.EffectiveRelations) < len(meminject.SeedRelations()) {
		t.Errorf("confirmed: effective relations %d, want the seed and more", len(got.EffectiveRelations))
	}
}

// TestOntologyAdopt_ARelationKindIsCopiedWithItsDirectives.
func TestOntologyAdopt_ARelationKindIsCopiedWithItsDirectives(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, relationsLayout())
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)

	got := s.postOntologyJSON(t, "/v1/_ontology/adopt", mi.Tenant, map[string]string{"section": "relations", "name": "amends"}, http.StatusOK)
	var amends *meminject.OntologyRelation
	for i := range got.Relations {
		if got.Relations[i].Name == "amends" {
			amends = &got.Relations[i]
		}
	}
	if amends == nil {
		t.Fatalf("amends not in the tenant layer after adopt: %v", got.Relations)
	}
	seed := meminject.SeedRelations()
	var want meminject.OntologyRelation
	for _, r := range seed {
		if r.Name == "amends" {
			want = r
		}
	}
	if amends.Inverse != want.Inverse || amends.Discriminator != "mod_type" || !amends.Dated ||
		len(amends.Qualifiers) != len(want.Qualifiers) || len(amends.Qualifiers[0].Values) != len(want.Qualifiers[0].Values) {
		t.Errorf("adopted copy does not parse back to the seed kind:\n got %#v\nwant %#v", *amends, want)
	}
	if !strings.Contains(s.renderOntology(context.Background(), mi), "**amends**") {
		t.Error("an adopted kind should reach the prompt")
	}
	s.postOntologyJSON(t, "/v1/_ontology/adopt", mi.Tenant, map[string]string{"section": "relations", "name": "amends"}, http.StatusConflict)
	s.postOntologyJSON(t, "/v1/_ontology/adopt", mi.Tenant, map[string]string{"section": "relations", "name": "invented"}, http.StatusBadRequest)
}

// TestOntologyAdopt_RelationNeedsADeclaredSection.
func TestOntologyAdopt_RelationNeedsADeclaredSection(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, relationsLayout()[:1])
	s.postOntologyJSON(t, "/v1/_ontology/adopt", mi.Tenant, map[string]string{"section": "relations", "name": "amends"}, http.StatusConflict)
}
