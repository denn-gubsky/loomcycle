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

// TestOntologyProposal_AcceptOfABodyCarryingLockedIsRefused (RFC GQ P2.3): a proposal
// whose body carries @locked — filed raw, before the guard refused that — is refused at
// accept with 409 entry_locked and stays a proposal; the same proposal without the line
// is accepted.
func TestOntologyProposal_AcceptOfABodyCarryingLockedIsRefused(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes", kids: []ontologyTestChunk{{title: "Memo"}}},
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
	read := s.getOntology(t, mi.Tenant)
	propose := func(title, body string) string {
		return exec(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
			"parent_id": read.ClassChunks["Memo"], "title": title, "body": body, "status": meminject.OntologyStatusProposed})["id"].(string)
	}
	locked, plain := propose("Board Memo", "@locked\nA memo of the board."), propose("Staff Memo", "A memo to staff.")

	body, _ := json.Marshal(map[string]string{"chunk_id": locked, "action": "accept"})
	req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/proposals?tenant="+mi.Tenant, strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.handleOntologyProposal(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "entry_locked") {
		t.Errorf("accept of a proposal carrying @locked = %d %s, want 409 entry_locked", rec.Code, rec.Body.String())
	}
	if st, _ := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": locked})["status"].(string); st != meminject.OntologyStatusProposed {
		t.Errorf("status = %q after a refused accept", st)
	}
	s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": plain, "action": "accept"}, http.StatusOK)
}

// TestOntologyState_ReportsALockedEntry: GET /v1/_ontology carries `locked` on a locked
// class, relation and instance, so the Ontology page can mark it and withhold retire
// and delete; an entry without the line carries none.
func TestOntologyState_ReportsALockedEntry(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes", kids: []ontologyTestChunk{{title: "Memo", body: "@locked"}, {title: "Ticket"}}},
		{title: "Relations", body: "@section relations", kids: []ontologyTestChunk{{title: "supervises", body: "@locked"}}},
		{title: "Instances", body: "@section instances", kids: []ontologyTestChunk{{title: "Acme", body: "@a organization\n@locked"}}},
	})
	s.confirmOntology(t, mi)
	got := s.getOntology(t, mi.Tenant)
	for _, term := range got.Terms {
		if want := term.Name == "Memo"; term.Source == "tenant" && term.Locked != want {
			t.Errorf("class %s locked = %v, want %v", term.Name, term.Locked, want)
		}
	}
	found := false
	for _, r := range got.Relations {
		if r.Name == "supervises" {
			found = r.Locked
		}
	}
	if !found {
		t.Errorf("relation supervises does not read as locked: %+v", got.Relations)
	}
	if got.Instances == nil || len(got.Instances.Instances) != 1 || !got.Instances.Instances[0].Locked {
		t.Errorf("instance report = %+v, want Acme locked", got.Instances)
	}
}
