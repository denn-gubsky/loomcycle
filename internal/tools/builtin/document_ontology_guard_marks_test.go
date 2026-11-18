package builtin

import (
	"encoding/json"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// TestOntologyGuard_AnAgentCannotWriteAProposalMark: an agent's raw create_chunk or
// upsert_chunk may not carry a proposal mark in its fields; only propose_entity and
// import_scheme write them.
func TestOntologyGuard_AnAgentCannotWriteAProposalMark(t *testing.T) {
	d, opCtx, agentCtx := ontologyAgentFixture(t)
	read, err := d.OntologyTermsFromTree(opCtx, "tenant", memrank.OntologyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{changeProposalField, schemeProposalField, conceptProposalField} {
		fields := map[string]any{key: map[string]any{"op": "retire", "target": "x"}}
		for _, op := range []string{"create_chunk", "upsert_chunk"} {
			req, _ := json.Marshal(map[string]any{"op": op, "scope": "tenant", "document_id": read.DocumentID,
				"parent_id": read.RootChunkID, "title": "forged " + key, "status": memrank.OntologyStatusProposed,
				"natural_key": "forged:" + key + ":" + op, "fields": fields})
			if _, r := docExec(t, d, agentCtx, string(req)); !r.IsError || !strings.Contains(r.Text, key) {
				t.Errorf("%s with %s = %q, want a refusal naming it", op, key, r.Text)
			}
		}
	}
	// A plain proposal is still allowed.
	req, _ := json.Marshal(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": read.DocumentID,
		"parent_id": read.RootChunkID, "title": "plain", "status": memrank.OntologyStatusProposed, "fields": map[string]any{"note": 1}})
	if _, r := docExec(t, d, agentCtx, string(req)); r.IsError {
		t.Errorf("a plain proposal was refused: %s", r.Text)
	}
}
