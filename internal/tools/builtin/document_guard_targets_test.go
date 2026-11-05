package builtin

import (
	"fmt"
	"strings"
	"testing"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// The ontology guard judges every field a handler addresses a document by, not the
// first one it finds (review of RFC FY P2).

// TestOntologyGuard_EveryAddressingFieldIsChecked: a decoy document_id beside an
// ontology chunk id, or a decoy chunk moved under an ontology parent, must not slip
// past the guard while the handler acts on the ontology.
func TestOntologyGuard_EveryAddressingFieldIsChecked(t *testing.T) {
	cases := []string{
		`{"op":"update_chunk",%[1]s,"id":"%[2]s","title":"renamed","revision":1}`,
		`{"op":"delete_chunk",%[1]s,"id":"%[2]s"}`,
		`{"op":"move_chunk",%[1]s,"id":"%[2]s","new_parent_id":"%[3]s"}`,
		`{"op":"reorder_chunk",%[1]s,"id":"%[2]s","direction":"up"}`,
		`{"op":"add_tags",%[1]s,"id":"%[2]s","tags":["x"]}`,
		`{"op":"remove_tags",%[1]s,"id":"%[2]s","tags":["x"]}`,
		`{"op":"judge_fact",%[1]s,"id":"%[2]s","verdict":"unsupported","reason":"x"}`,
		// A decoy's chunk carried INTO the ontology: the moved chunk is the decoy's, the
		// new parent is the ontology's.
		`{"op":"move_chunk",%[1]s,"id":"%[4]s","new_parent_id":"%[2]s"}`,
	}
	for _, tmpl := range cases {
		e := newReservedEnv(t)
		read, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
		event := e.d.chunkIDForEntity(e.opCtx, mustTenantKey(t, e.d, e.opCtx), read.DocumentID, "event")
		if event == "" {
			t.Fatal("no event chunk in the fixture")
		}
		out, r := docExec(t, e.d, e.opCtx, `{"op":"get_document","scope":"tenant","id":"`+e.decoy+`"}`)
		if r.IsError {
			t.Fatalf("decoy: %s", r.Text)
		}
		decoyRoot, _ := out["root_chunk_id"].(string)
		out, r = docExec(t, e.d, e.opCtx, `{"op":"create_chunk","scope":"tenant","document_id":"`+e.decoy+`","title":"loose","body":"x"}`)
		if r.IsError {
			t.Fatalf("decoy chunk: %s", r.Text)
		}
		loose, _ := out["id"].(string)
		req := fmt.Sprintf(tmpl, `"scope":"tenant","document_id":"`+e.decoy+`"`, event, decoyRoot, loose)
		r = e.doc(t, e.agentCtx, req)
		if !r.IsError || !(strings.Contains(r.Text, "ontology") || strings.Contains(r.Text, "more than one document")) {
			t.Errorf("not refused by the ontology guard: %s\n  -> %s", req, r.Text)
		}
		after, _ := e.d.OntologyTermsFromTree(e.opCtx, "tenant", memrank.OntologyPath)
		names := map[string]bool{}
		for _, term := range after.Terms {
			names[term.Name] = true
		}
		if !names["event"] || names["renamed"] || names["loose"] {
			t.Errorf("%s changed the ontology: terms %v", req, names)
		}
	}
}

// TestOntologyGuard_AddressesThatDisagreeAreRefused: a call whose own-target fields
// name two documents, one of them the ontology, is ambiguous about what it acts on, so
// it is refused, as docIDFromInput refuses an id and a document_id that differ.
// Outside the ontology the handlers decide (a re-keyed upsert is legitimate).
func TestOntologyGuard_AddressesThatDisagreeAreRefused(t *testing.T) {
	e := newReservedEnv(t)
	out, r := docExec(t, e.d, e.opCtx, `{"op":"get_document","scope":"tenant","id":"`+e.decoy+`"}`)
	if r.IsError {
		t.Fatalf("decoy: %s", r.Text)
	}
	decoyRoot, _ := out["root_chunk_id"].(string)
	r = e.doc(t, e.agentCtx, `{"op":"add_tags","scope":"tenant","document_id":"`+e.ontology+`","id":"`+decoyRoot+`","tags":["x"]}`)
	if !r.IsError || !strings.Contains(r.Text, "more than one document") {
		t.Errorf("a call naming the ontology and another document was not refused: %s", r.Text)
	}
	// Agreeing addresses still work.
	if r := e.doc(t, e.agentCtx, `{"op":"add_tags","scope":"tenant","document_id":"`+e.decoy+`","tags":["x"]}`); r.IsError {
		t.Errorf("an ordinary tag write was refused: %s", r.Text)
	}
}
