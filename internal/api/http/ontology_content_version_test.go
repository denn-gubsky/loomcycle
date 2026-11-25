package http

import (
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestOntologyAPI_ContentVersionSurvivesConfirmAndAdoption (RFC GQ P3.1).
//
// GET /v1/_ontology reports the content version, and a confirm does not move it: the
// confirm bumps the root's revision (ontology_version) and adopts the instances, which
// writes each one's registry key onto its chunk, yet nothing anyone declared changed.
// A version that moved here would mark every row written before the first confirm as
// written under an older vocabulary. The plan reports the same value.
func TestOntologyAPI_ContentVersionSurvivesConfirmAndAdoption(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, instancesLayout())

	before := s.getOntology(t, mi.Tenant)
	if len(before.ContentVersion) != 16 {
		t.Fatalf("content_version = %q, want 16 hex characters", before.ContentVersion)
	}
	confirmed := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if confirmed.InstanceAdoption == nil || confirmed.InstanceAdoption.Adopted != 2 {
		t.Fatalf("the confirm should adopt both instances: %#v", confirmed.InstanceAdoption)
	}
	after := s.getOntology(t, mi.Tenant)
	if after.OntologyVersion == before.OntologyVersion {
		t.Fatal("the confirm did not bump ontology_version; the document did not change")
	}
	if confirmed.ContentVersion != before.ContentVersion || after.ContentVersion != before.ContentVersion {
		t.Errorf("content_version moved on a confirm: %s before, %s in the confirm's answer, %s after",
			before.ContentVersion, confirmed.ContentVersion, after.ContentVersion)
	}

	plan, err := s.PlanOntology(tierOperator(mi.Tenant, "op"), connector.PlanOntologyRequest{Changes: []connector.OntologyChange{
		{Op: connector.OntologyPlanRetire, Entry: after.ClassChunks["OrganizationalUnit"]}}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if plan.ContentVersion != after.ContentVersion {
		t.Errorf("the plan's content_version = %q, want the ontology's %q", plan.ContentVersion, after.ContentVersion)
	}
}
