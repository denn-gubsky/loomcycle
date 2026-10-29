package http

import (
	"net/http"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

func instancesLayout() []ontologyTestChunk {
	return []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{{title: "OrganizationalUnit"}}},
		{title: "Instances", body: "@section instances\n", kids: []ontologyTestChunk{
			{title: "Acme Bank", body: "@a organization\n@nesting sub_organization_of\n", kids: []ontologyTestChunk{
				{title: "Compliance Department", body: "@a OrganizationalUnit\n"},
			}},
		}},
	}
}

func instanceState(r *builtin.InstanceReport, name string) string {
	if r == nil {
		return ""
	}
	for _, s := range r.Instances {
		if s.Name == name {
			return s.State
		}
	}
	return ""
}

// TestOntologyAPI_ConfirmAdoptsInstancesAndOnlyConfirm (RFC FO P1c-2, §6).
//
// The panel reports each instance as pending until a confirm adopts it; a draft flip
// adopts nothing; the confirm reports what it did, and afterwards the read shows the
// instances as unchanged under their keys.
func TestOntologyAPI_ConfirmAdoptsInstancesAndOnlyConfirm(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, instancesLayout())

	before := s.getOntology(t, mi.Tenant)
	if instanceState(before.Instances, "Acme Bank") != "pending" {
		t.Fatalf("before confirm: %#v", before.Instances)
	}
	draft := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyDraftStatus, http.StatusOK)
	if draft.InstanceAdoption != nil || instanceState(draft.Instances, "Acme Bank") != "pending" {
		t.Errorf("a draft flip adopted instances: %#v", draft.InstanceAdoption)
	}
	got := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if got.InstanceAdoption == nil || got.InstanceAdoption.Adopted != 2 || got.InstanceAdoption.Statements == 0 {
		t.Fatalf("confirm should adopt both instances with their nesting: %#v", got.InstanceAdoption)
	}
	if st := instanceState(got.Instances, "Acme Bank"); st != "unchanged" {
		t.Errorf("after confirm the read should show the instance adopted (unchanged), got %q", st)
	}
}
