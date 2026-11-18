package http

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// TestOntologyProposal_WholeSchemeOverTheRoute (RFC GM P3.1): a whole-scheme proposal
// lists as one proposal with its outline, accepts over the operator route with its
// concepts, rejects to a tombstone, and a stored proposal whose data no longer passes
// its checks is refused with 422 and changes nothing.
func TestOntologyProposal_WholeSchemeOverTheRoute(t *testing.T) {
	s, mi := ontologyFixture(t)
	s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{memory:ontology}}"}, mi)
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	file := func(name string) string {
		req, _ := json.Marshal(map[string]any{"op": "import_scheme", "scope": "tenant", "name": name,
			"markdown": "- `A` Alpha\n  - `A1` Alpha one\n- Beta\n"})
		res, err := doc.Execute(dctx, req)
		if err != nil || res.IsError {
			t.Fatalf("import_scheme: %v %s", err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out["chunk_id"].(string)
	}
	keep, drop, broken := file("Kept"), file("Dropped"), file("Broken")

	state := s.ontologyStateOrFail(t, mi.Tenant)
	var listed *meminject.OntologyProposal
	for i := range state.Proposals {
		if state.Proposals[i].ChunkID == keep {
			listed = &state.Proposals[i]
		}
	}
	if listed == nil || listed.Scheme == nil || listed.Scheme.Concepts != 3 || len(listed.Scheme.Outline) != 3 ||
		listed.Scheme.Header.Origin != "adopted" || len(state.Proposals) != 3 {
		t.Fatalf("proposals = %+v", state.Proposals)
	}

	after := s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
		map[string]string{"chunk_id": keep, "action": "accept"}, http.StatusOK)
	if after.SchemeAccepted == nil || after.SchemeAccepted.Concepts != 3 {
		t.Errorf("scheme_accepted = %+v", after.SchemeAccepted)
	}
	var kept *meminject.OntologyScheme
	for i := range after.Schemes {
		if after.Schemes[i].Name == "Kept" {
			kept = &after.Schemes[i]
		}
	}
	if kept == nil || len(kept.Concepts) != 3 || kept.Origin != "adopted" || kept.Concepts[1].Notation != "A1" {
		t.Fatalf("schemes = %+v", after.Schemes)
	}

	after = s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
		map[string]string{"chunk_id": drop, "action": "reject"}, http.StatusOK)
	for _, sc := range after.Schemes {
		if sc.Name == "Dropped" {
			t.Error("a rejected scheme is in force")
		}
	}

	// Tamper with the stored data, as an operator's editor could: accepting re-checks it.
	g, _ := json.Marshal(map[string]any{"op": "get_chunk", "scope": "tenant", "id": broken})
	res, _ := doc.Execute(dctx, g)
	var cur struct {
		Revision int `json:"revision"`
	}
	_ = json.Unmarshal([]byte(res.Text), &cur)
	u, _ := json.Marshal(map[string]any{"op": "update_chunk", "scope": "tenant", "id": broken, "revision": cur.Revision,
		"fields": map[string]any{"scheme_proposal": map[string]any{"header": map[string]any{"origin": "stolen"}}}})
	if res, err := doc.Execute(dctx, u); err != nil || res.IsError {
		t.Fatalf("tamper: %v %s", err, res.Text)
	}
	s.postOntologyJSON(t, "/v1/_ontology/proposals", mi.Tenant,
		map[string]string{"chunk_id": broken, "action": "accept"}, http.StatusUnprocessableEntity)
	for _, sc := range s.ontologyStateOrFail(t, mi.Tenant).Schemes {
		if sc.Name == "Broken" {
			t.Error("a refused accept put the scheme in force")
		}
	}
}
