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

// TestOntologyProposals_GenericAcceptCannotApplyAChangeProposal: accepting a change or
// retire proposal on the route is refused (409) until P4.2 — the generic accept would
// clear the proposal's own status and put "retire: Memo" in force as a class — even
// when its mark no longer decodes; the proposal stays proposed and its target as it
// was. Rejecting it works as for any proposal.
func TestOntologyProposals_GenericAcceptCannotApplyAChangeProposal(t *testing.T) {
	s, mi, exec := changeProposalFixture(t)
	retire := exec(map[string]any{"op": "propose_entity", "change": "retire", "entry": "Memo"})["chunk_id"].(string)
	edit := exec(map[string]any{"op": "propose_entity", "change": "edit", "entry": "Memo", "meaning": "A note."})["chunk_id"].(string)
	// The operator's editor can break the mark; the route must still not accept it.
	g := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": edit})
	exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": edit, "revision": g["revision"],
		"fields": map[string]any{"change_proposal": "garbage"}})

	for _, id := range []string{retire, edit} {
		body, _ := json.Marshal(map[string]string{"chunk_id": id, "action": "accept"})
		req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/proposals?tenant="+mi.Tenant, strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.handleOntologyProposal(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "comes with P4.2") {
			t.Errorf("accept %s = %d %s, want 409 naming P4.2", id, rec.Code, rec.Body.String())
		}
		if st, _ := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["status"].(string); st != meminject.OntologyStatusProposed {
			t.Errorf("%s status = %q after a refused accept", id, st)
		}
	}
	state := s.ontologyStateOrFail(t, mi.Tenant)
	for _, term := range state.Terms {
		if strings.Contains(term.Name, ":") || (term.Name == "Memo" && term.EntryStatus != "") {
			t.Errorf("a refused accept changed the classes: %+v", term)
		}
	}

	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": retire, "action": "reject"}, http.StatusOK)
	for _, p := range after.Proposals {
		if p.ChunkID == retire && p.Status != meminject.OntologyStatusRejected {
			t.Errorf("rejected proposal = %+v", p)
		}
	}
}
