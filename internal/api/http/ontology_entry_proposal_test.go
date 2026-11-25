package http

import (
	"net/http"
	"reflect"
	"testing"
)

// TestAcceptEntry_TamperedMarkIsRefused422 (RFC GQ P6.3): the route accepts a class or
// relation add carrying an entry_proposal mark by its checked data — the relation is
// in force with its domain — and answers 422 invalid_proposal, writing nothing, for one
// whose mark was edited after filing into something filing would not store.
func TestAcceptEntry_TamperedMarkIsRefused422(t *testing.T) {
	s, mi, exec := changeProposalFixture(t)
	good := exec(map[string]any{"op": "propose_entity", "section": "relations", "name": "approves",
		"meaning": "One party approves another's memo.", "directives": map[string]any{"domain": "Memo"}})["chunk_id"].(string)
	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": good, "action": "accept"}, http.StatusOK)
	var inForce bool
	for _, rel := range after.Relations {
		inForce = inForce || (rel.Name == "approves" && rel.ChunkID == good && reflect.DeepEqual(rel.Domain, []string{"Memo"}))
	}
	if !inForce {
		t.Errorf("approves is not in force with its domain: %+v", after.Relations)
	}

	bad := exec(map[string]any{"op": "propose_entity", "section": "relations", "name": "reviews",
		"meaning": "One party reviews another's memo.", "directives": map[string]any{"domain": "Memo"}})["chunk_id"].(string)
	g := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": bad})
	fields := g["fields"].(map[string]any)
	fields["entry_proposal"].(map[string]any)["directives"] = []map[string]any{{"name": "acl", "value": "admins"}}
	exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": bad, "revision": g["revision"], "fields": fields})

	code, body := postProposal(t, s, mi.Tenant, map[string]string{"chunk_id": bad, "action": "accept"})
	if code != http.StatusUnprocessableEntity || body["code"] != "invalid_proposal" {
		t.Fatalf("tampered accept = %d %v, want 422 invalid_proposal", code, body)
	}
	state := s.ontologyStateOrFail(t, mi.Tenant)
	for _, rel := range state.Relations {
		if rel.Name == "reviews" {
			t.Errorf("the refused proposal went into force: %+v", rel)
		}
	}
	var stillProposed bool
	for _, p := range state.Proposals {
		stillProposed = stillProposed || (p.ChunkID == bad && p.Status == "proposed")
	}
	if !stillProposed {
		t.Error("the refused proposal is no longer an open proposal")
	}
}
