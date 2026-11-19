package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// changeProposalFixture is a confirmed ontology with a class and a relation kind, and an
// executor on the operator's document context.
func changeProposalFixture(t *testing.T) (*Server, memInject, func(map[string]any) map[string]any) {
	t.Helper()
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes", kids: []ontologyTestChunk{{title: "Memo"}}},
		{title: "Relations", body: "@section relations", kids: []ontologyTestChunk{{title: "supervises"}}},
	})
	s.confirmOntology(t, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	exec := func(payload map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(payload)
		res, err := doc.Execute(dctx, b)
		if err != nil || res.IsError {
			t.Fatalf("%v: %v %s", payload["op"], err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	return s, mi, exec
}

// TestOntologyProposals_ChangeProposalIsListedWithItsChange: the operator's ontology
// state lists a change or retire proposal with what accepting it would do — the target,
// the operation and the proposed values — and the target stays in force.
func TestOntologyProposals_ChangeProposalIsListedWithItsChange(t *testing.T) {
	s, mi, exec := changeProposalFixture(t)
	edit := exec(map[string]any{"op": "propose_entity", "change": "edit", "entry": "supervises", "section": "relations",
		"name": "oversees", "directives": map[string]any{"functional": true}, "body": "Renamed in the 2026 charter."})
	retire := exec(map[string]any{"op": "propose_entity", "change": "retire", "entry": "Memo"})

	state := s.ontologyStateOrFail(t, mi.Tenant)
	found := map[string]*meminject.ProposedChange{}
	for _, p := range state.Proposals {
		if p.Change != nil {
			found[p.ChunkID] = p.Change
		}
	}
	e, r := found[edit["chunk_id"].(string)], found[retire["chunk_id"].(string)]
	if e == nil || e.Op != meminject.ChangeOpEdit || e.TargetName != "supervises" || e.Name != "oversees" ||
		e.Kind != "relation" || len(e.Directives) != 1 || e.Directives[0].Name != "functional" {
		t.Errorf("edit proposal listed as %+v", e)
	}
	if r == nil || r.Op != meminject.ChangeOpRetire || r.TargetName != "Memo" || r.Target == "" {
		t.Errorf("retire proposal listed as %+v", r)
	}
	var memo, supervises bool
	for _, term := range state.Terms {
		memo = memo || (term.Name == "Memo" && term.EntryStatus == "")
	}
	for _, rel := range state.Relations {
		supervises = supervises || (rel.Name == "supervises" && rel.EntryStatus == "")
	}
	if !memo || !supervises {
		t.Errorf("a target left force: memo %v supervises %v", memo, supervises)
	}
}

// postProposal posts to the proposals route and returns the status and the body.
func postProposal(t *testing.T, s *Server, tenant string, payload map[string]string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/proposals?tenant="+tenant, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.handleOntologyProposal(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestOntologyProposals_AcceptAppliesAChangeProposalInPlace (RFC GQ P4.2): accepting a
// retire and an edit applies each to the entry it names, keeping its chunk id; the
// proposal is gone, the answer says what was applied, and the change is recorded.
func TestOntologyProposals_AcceptAppliesAChangeProposalInPlace(t *testing.T) {
	s, mi, exec := changeProposalFixture(t)
	sink := &recordingSink{}
	s.reviewAudit = sink
	read := s.ontologyStateOrFail(t, mi.Tenant)
	var supervisesID string
	for _, rel := range read.Relations {
		if rel.Name == "supervises" {
			supervisesID = rel.ChunkID
		}
	}
	retire := exec(map[string]any{"op": "propose_entity", "change": "retire", "entry": "Memo"})["chunk_id"].(string)
	edit := exec(map[string]any{"op": "propose_entity", "change": "edit", "entry": "supervises", "section": "relations",
		"name": "oversees"})["chunk_id"].(string)

	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": retire, "action": "accept"}, http.StatusOK)
	if after.ChangeAccepted == nil || after.ChangeAccepted.Op != meminject.ChangeOpRetire || after.ChangeAccepted.TargetName != "Memo" {
		t.Errorf("change_accepted = %+v", after.ChangeAccepted)
	}
	after = s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": edit, "action": "accept"}, http.StatusOK)
	var renamed bool
	for _, rel := range after.Relations {
		renamed = renamed || (rel.Name == "oversees" && rel.ChunkID == supervisesID)
	}
	if !renamed {
		t.Errorf("supervises was not renamed in place: %+v", after.Relations)
	}
	for _, term := range after.Terms {
		if term.Name == "Memo" && term.EntryStatus != meminject.OntologyStatusRetired {
			t.Errorf("Memo = %+v, want retired", term)
		}
		if strings.Contains(term.Name, ":") {
			t.Errorf("a proposal was put in force as a class: %+v", term)
		}
	}
	for _, p := range after.Proposals {
		if p.ChunkID == retire || p.ChunkID == edit {
			t.Errorf("an accepted proposal is still listed: %+v", p)
		}
	}
	if len(sink.events) != 2 || sink.events[1].Action != "ontology_change_applied" || sink.events[1].OntologyProposalID != edit ||
		sink.events[1].OntologyEntryID != supervisesID || sink.events[1].OntologyOp != meminject.ChangeOpEdit ||
		sink.events[1].TargetName != "supervises" {
		t.Errorf("audit = %+v", sink.events)
	}
}

// TestOntologyProposals_TamperedChangeProposalIs422: a change proposal whose mark no
// longer decodes is refused as invalid (422), never accepted the generic way; it stays
// proposed and can still be rejected.
func TestOntologyProposals_TamperedChangeProposalIs422(t *testing.T) {
	s, mi, exec := changeProposalFixture(t)
	edit := exec(map[string]any{"op": "propose_entity", "change": "edit", "entry": "Memo", "meaning": "A note."})["chunk_id"].(string)
	g := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": edit})
	exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": edit, "revision": g["revision"],
		"fields": map[string]any{"change_proposal": "garbage"}})
	if code, out := postProposal(t, s, mi.Tenant, map[string]string{"chunk_id": edit, "action": "accept"}); code != http.StatusUnprocessableEntity || out["code"] != "invalid_proposal" {
		t.Errorf("accept = %d %v, want 422 invalid_proposal", code, out)
	}
	if st, _ := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": edit})["status"].(string); st != meminject.OntologyStatusProposed {
		t.Errorf("status = %q after a refused accept", st)
	}
	for _, term := range s.ontologyStateOrFail(t, mi.Tenant).Terms {
		if strings.Contains(term.Name, ":") {
			t.Errorf("a refused accept put a class in force: %+v", term)
		}
	}
	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": edit, "action": "reject"}, http.StatusOK)
	for _, p := range after.Proposals {
		if p.ChunkID == edit && p.Status != meminject.OntologyStatusRejected {
			t.Errorf("rejected proposal = %+v", p)
		}
	}
}

// TestOntologyProposals_ChangeAcceptRefusalsCarryTheirCodes: a rename of a kind in use
// needs the plan's fingerprint (409 plan_required, carrying it) and applies with it; a
// class rename with dependents is 409 rename_job_pending whatever the fingerprint.
func TestOntologyProposals_ChangeAcceptRefusalsCarryTheirCodes(t *testing.T) {
	s, mi, exec := changeProposalFixture(t)
	a := exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "Alice"})["root_chunk_id"]
	b := exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "Bob"})["root_chunk_id"]
	exec(map[string]any{"op": "link_chunks", "scope": "tenant", "from_id": a, "to_id": b, "kind": "supervises"})
	exec(map[string]any{"op": "create_document", "scope": "tenant", "title": "A memo", "type": "Memo"})
	edit := exec(map[string]any{"op": "propose_entity", "change": "edit", "entry": "supervises", "section": "relations",
		"name": "oversees"})["chunk_id"].(string)
	rename := exec(map[string]any{"op": "propose_entity", "change": "edit", "entry": "Memo", "name": "Memorandum"})["chunk_id"].(string)

	code, out := postProposal(t, s, mi.Tenant, map[string]string{"chunk_id": edit, "action": "accept"})
	fp, _ := out["fingerprint"].(string)
	if code != http.StatusConflict || out["code"] != "plan_required" || fp == "" {
		t.Fatalf("accept without a plan = %d %v, want 409 plan_required with the fingerprint", code, out)
	}
	if code, out := postProposal(t, s, mi.Tenant, map[string]string{"chunk_id": rename, "action": "accept", "plan_fingerprint": fp}); code != http.StatusConflict || out["code"] != "rename_job_pending" {
		t.Errorf("class rename = %d %v, want 409 rename_job_pending", code, out)
	}
	if code, out := postProposal(t, s, mi.Tenant, map[string]string{"chunk_id": edit, "action": "accept", "plan_fingerprint": fp}); code != http.StatusOK {
		t.Errorf("accept with the fingerprint = %d %v", code, out)
	}
}

// TestOntologyProposals_AddProposalStillAcceptsByItsStatus: an add proposal carries no
// change mark and is accepted as before — its own status cleared, in force where it sits.
func TestOntologyProposals_AddProposalStillAcceptsByItsStatus(t *testing.T) {
	s, mi, exec := changeProposalFixture(t)
	add := exec(map[string]any{"op": "propose_entity", "name": "Circular", "parent": "Memo"})["chunk_id"].(string)
	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": add, "action": "accept"}, http.StatusOK)
	if after.ChangeAccepted != nil {
		t.Errorf("an add proposal was accepted as a change: %+v", after.ChangeAccepted)
	}
	var found bool
	for _, term := range after.Terms {
		found = found || term.Name == "Circular"
	}
	if !found {
		t.Error("the accepted add proposal is not in force")
	}
}
