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

// TestOntologyProposal_AcceptUnderOrNamedAsARetiredEntryIsRefused (RFC GQ P2.1): a
// class or relation proposal filed under an entry retired since, or named as a retired
// entry, is refused at accept with 409 entry_retired and stays a proposal; one under a
// live entry is accepted.
func TestOntologyProposal_AcceptUnderOrNamedAsARetiredEntryIsRefused(t *testing.T) {
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
	docID := exec(map[string]any{"op": "get_document", "scope": "tenant", "path": meminject.OntologyPath})["document_id"].(string)
	byTitle := func(title string) string {
		q := exec(map[string]any{"op": "query_chunks", "scope": "tenant",
			"sql": "SELECT id FROM chunks WHERE document_id = '" + docID + "' AND title = '" + title + "'"})
		return q["rows"].([]any)[0].([]any)[0].(string)
	}
	propose := func(parent, title string) string {
		return exec(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": docID,
			"parent_id": parent, "title": title, "status": meminject.OntologyStatusProposed})["id"].(string)
	}
	memo, supervises, classes := byTitle("Memo"), byTitle("supervises"), byTitle("Classes")
	underClass, underKind, live := propose(memo, "Board Memo"), propose(supervises, "oversees"), propose(classes, "Note")
	retire := func(id string) {
		g := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})
		exec(map[string]any{"op": "update_chunk", "scope": "tenant", "id": id, "revision": g["revision"], "status": "retired"})
	}
	retire(memo)
	retire(supervises)
	sameName := propose(classes, "Memo")

	for _, id := range []string{underClass, underKind, sameName} {
		body, _ := json.Marshal(map[string]string{"chunk_id": id, "action": "accept"})
		req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/proposals?tenant="+mi.Tenant, strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.handleOntologyProposal(rec, req)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "entry_retired") {
			t.Errorf("accept %s = %d %s, want 409 entry_retired", id, rec.Code, rec.Body.String())
		}
		if st, _ := exec(map[string]any{"op": "get_chunk", "scope": "tenant", "id": id})["status"].(string); st != meminject.OntologyStatusProposed {
			t.Errorf("%s status = %q after a refused accept", id, st)
		}
	}
	s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant, map[string]string{"chunk_id": live, "action": "accept"}, http.StatusOK)
}
