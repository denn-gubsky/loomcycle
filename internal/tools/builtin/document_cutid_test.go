package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func TestLooksCut(t *testing.T) {
	for id, want := range map[string]bool{
		// Measured in the 1.99.0 eval.
		"6fbd5245":                true,
		"36d88349ac1730a178...":   true,
		"abc26269b??":             true,
		"36d88349ac1730a178…":     true,
		"1bda3bb9d37086dd0fedd12": true,
		// Whole ids and things that are not an id at all.
		"1bda3bb9d37086dd0fedd120c3e550bc":  false,
		"":                                  false,
		"d1":                                false, // too short to be a copied prefix
		"abc":                               false,
		"doc-1":                             false,
		"zz-eval Trip plan":                 false,
		"id1727400000000000000":             false, // newDocID's fallback shape
		"1BDA3BB9D37086DD":                  false, // not our lowercase hex
		"1bda3bb9d37086dd0fedd120c3e550bc0": false, // longer than an id, not a cut
	} {
		if got := looksCut(id); got != want {
			t.Errorf("looksCut(%q) = %v, want %v", id, got, want)
		}
	}
}

// Measured: ornith sent "6fbd5245" for the id create_document had just
// returned, was told "not found in this scope", and created a second document.
// A cut id is now refused as cut, before any lookup, in any id field.
func TestDocument_ACutIDIsRefusedAsCut(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Trip plan"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID := asStr(out["document_id"])

	for name, call := range map[string]string{
		"document_id, bare prefix": `{"op":"create_chunk","scope":"user","document_id":"` + docID[:8] + `","title":"Flights"}`,
		"id, with an ellipsis":     `{"op":"get_document","scope":"user","id":"` + docID[:18] + `..."}`,
		"parent_id, with ??":       `{"op":"create_chunk","scope":"user","document_id":"` + docID + `","parent_id":"` + docID[:9] + `??","title":"Hotels"}`,
		"seed_ids element":         `{"op":"graph_recall","scope":"user","seed_ids":["` + docID[:12] + `"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, r := docExec(t, d, ctx, call)
			if !r.IsError || !strings.Contains(r.Text, "is cut short") {
				t.Fatalf("want the cut-id refusal, got %s", r.Text)
			}
			if r.Error == nil || r.Error.Category != tools.CategoryValidation {
				t.Errorf("want validation, got %+v", r.Error)
			}
		})
	}

	// The whole id still works, and a made-up short name still reads as not found.
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`); r.IsError {
		t.Errorf("the whole id was refused: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"doc-1"}`); !r.IsError || strings.Contains(r.Text, "cut short") {
		t.Errorf("a made-up name should be refused as not found, not as cut: %s", r.Text)
	}
}
