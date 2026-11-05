package builtin

import (
	"context"
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// Second review of RFC FY P2: the probes, kept.

// moveChunkToDocument re-homes a chunk the way fact homing or a subject merge does:
// the natural key now lives in another document.
func moveChunkToDocument(t *testing.T, e reservedEnv, chunkID, docID string) {
	t.Helper()
	key := mustTenantKey(t, e.d, e.opCtx)
	out, r := docExec(t, e.d, e.opCtx, `{"op":"get_document","scope":"tenant","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatalf("get_document %s: %s", docID, r.Text)
	}
	root, _ := out["root_chunk_id"].(string)
	if err := e.d.exec(context.Background(), key, `UPDATE chunks SET document_id = ?, parent_id = ? WHERE id = ?`, docID, root, chunkID); err != nil {
		t.Fatalf("re-home: %v", err)
	}
}

// TestOntologyGuard_AReKeyedUpsertOutsideTheOntologyIsNotRefused is the reviewer's
// probe for the "addresses must agree" rule: the consolidator names its home document
// beside a natural key that homing has moved elsewhere, and that must keep working —
// the rule is for calls that reach the ontology.
func TestOntologyGuard_AReKeyedUpsertOutsideTheOntologyIsNotRefused(t *testing.T) {
	setup := func(t *testing.T) (reservedEnv, string, string) {
		e := newReservedEnv(t)
		out, r := docExec(t, e.d, e.opCtx, `{"op":"create_document","scope":"tenant","title":"B","path":"/notes/b"}`)
		if r.IsError {
			t.Fatalf("B: %s", r.Text)
		}
		b, _ := out["document_id"].(string)
		out, r = docExec(t, e.d, e.agentCtx, `{"op":"upsert_chunk","scope":"tenant","document_id":"`+e.decoy+`","natural_key":"note:k1","title":"K","body":"one"}`)
		if r.IsError {
			t.Fatalf("first upsert: %s", r.Text)
		}
		chunk, _ := out["id"].(string)
		return e, b, chunk
	}
	again := func(doc string) string {
		return `{"op":"upsert_chunk","scope":"tenant","document_id":"` + doc + `","natural_key":"note:k1","title":"K","body":"two","status":"proposed"}`
	}

	t.Run("neither side is the ontology", func(t *testing.T) {
		e, b, chunk := setup(t)
		moveChunkToDocument(t, e, chunk, b)
		if r := e.doc(t, e.agentCtx, again(e.decoy)); r.IsError {
			t.Errorf("a re-keyed upsert outside the ontology was refused: %s", r.Text)
		}
	})
	t.Run("the key moved into the ontology", func(t *testing.T) {
		e, _, chunk := setup(t)
		moveChunkToDocument(t, e, chunk, e.ontology)
		if r := e.doc(t, e.agentCtx, again(e.decoy)); !r.IsError {
			t.Errorf("an upsert naming a decoy reached an ontology chunk by its key: %s", r.Text)
		}
	})
	t.Run("the call names the ontology", func(t *testing.T) {
		e, b, chunk := setup(t)
		moveChunkToDocument(t, e, chunk, b)
		if r := e.doc(t, e.agentCtx, again(e.ontology)); !r.IsError {
			t.Errorf("an upsert naming the ontology with a key held elsewhere was allowed: %s", r.Text)
		}
	})
}

// TestOntologyGuard_AProposalCannotRewriteAnAcceptedEntity is the reviewer's four-step
// probe: an upsert into the ontology marked `proposed` whose key names an ACCEPTED
// entity rewrote it in place and dropped it back to proposed. Only a new chunk is a
// proposal.
func TestOntologyGuard_AProposalCannotRewriteAnAcceptedEntity(t *testing.T) {
	e := newReservedEnv(t)
	read, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
	// 1. The operator writes an accepted entity with a natural key.
	if r := e.doc(t, e.opCtx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","natural_key":"ent:accepted","title":"accepted","body":"orig"}`,
		read.DocumentID, read.RootChunkID)); r.IsError {
		t.Fatalf("operator upsert: %s", r.Text)
	}
	// 2. An agent "proposes" over it by the same key.
	r := e.doc(t, e.agentCtx, fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","natural_key":"ent:accepted","title":"hijacked","body":"rewritten","status":"proposed"}`,
		read.DocumentID))
	if !r.IsError || !strings.Contains(r.Text, "ontology") {
		t.Errorf("an agent's proposal rewrote an accepted entity: %s", r.Text)
	}
	// 3. The entity is unchanged and still in force.
	after, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
	names := map[string]bool{}
	for _, term := range after.Terms {
		names[term.Name] = true
	}
	if !names["accepted"] || names["hijacked"] {
		t.Errorf("the accepted entity changed: terms %v", names)
	}
	// 4. A new key is still a proposal an agent may file, and re-filing its own
	// proposal is still allowed.
	newProp := fmt.Sprintf(`{"op":"upsert_chunk","scope":"tenant","document_id":"%s","parent_id":"%s","natural_key":"ent:new","title":"suggested","body":"x","status":"proposed"}`,
		read.DocumentID, read.RootChunkID)
	if r := e.doc(t, e.agentCtx, newProp); r.IsError {
		t.Errorf("a new proposal was refused: %s", r.Text)
	}
	if r := e.doc(t, e.agentCtx, strings.Replace(newProp, `"body":"x"`, `"body":"y"`, 1)); r.IsError {
		t.Errorf("re-filing an agent's own proposal was refused: %s", r.Text)
	}
}
