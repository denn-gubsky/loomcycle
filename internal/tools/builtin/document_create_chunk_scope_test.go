package builtin

import (
	"strings"
	"testing"
)

// A document created in one scope and a create_chunk sent in another, naming the
// document's REAL root as parent_id. Measured on qwen3.6 (1.98.0): the document
// went to agent scope and create_chunk defaulted to user. The refusal said "no
// such parent_id … pass the document's root_chunk_id", which is exactly what it
// had passed, so the model retried the id, then dropped it, then repeated an
// identical failing call before working out the scope on its own. The document
// must be resolved first, so the refusal names what is actually wrong.
func TestCreateChunk_AWrongScopeSaysTheDocumentIsNotInThisScope(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"agent","title":"trip plan"}`)
	if r.IsError {
		t.Fatalf("create_document in agent scope: %s", r.Text)
	}
	docID, root := asStr(out["document_id"]), asStr(out["root_chunk_id"])

	for name, extra := range map[string]string{
		"with the root as parent_id": `,"parent_id":"` + root + `"`,
		"with the root as after_id":  `,"after_id":"` + root + `"`,
		"with no parent":             ``,
	} {
		t.Run(name, func(t *testing.T) {
			_, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
				`","title":"Flights"`+extra+`}`)
			if !r.IsError {
				t.Fatal("create_chunk in the wrong scope succeeded")
			}
			if !strings.Contains(r.Text, "not found in this scope") {
				t.Errorf("the refusal must say the document is not in this scope, got: %s", r.Text)
			}
			if strings.Contains(r.Text, "no such parent_id") || strings.Contains(r.Text, "after_id") {
				t.Errorf("the refusal blames the parent or sibling the caller passed correctly: %s", r.Text)
			}
		})
	}

	// In the document's own scope the same call still works.
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"agent","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Flights"}`); r.IsError {
		t.Fatalf("create_chunk in the document's scope: %s", r.Text)
	}
}
