package builtin

import (
	"fmt"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A model that copies a 32-character document or chunk id and cuts it short.
// Measured in the 1.99.0 tool-usage eval: ornith sent "6fbd5245" and gpt-oss
// "36d88349ac1730a178..." for ids create_document had just returned, and a
// third call carried "abc26269b??". Each was refused "not found in this
// scope", which is true but does not say the id was cut, so both models went
// on to create another document.
//
// Every document and chunk id comes from newDocID: 32 lowercase hex characters
// (imports and canvases mint new ones rather than keeping foreign ids). A value
// that is a hex prefix of that and nothing else, or a hex prefix followed by
// an ellipsis or question marks, can only be a cut id. The check is syntax
// only: it never looks anything up, so it says nothing about whether an id
// exists.

// minCutIDPrefix is the shortest bare hex run treated as a cut id. The measured
// cuts were 8 and 18 characters; a shorter run ("d1", "abc") is as likely to be
// a made-up name, which the lookup refuses as not found anyway.
const minCutIDPrefix = 8

// cutIDTail is what a model writes where it stopped copying.
const cutIDTail = ".…?"

// looksCut reports whether id is a document or chunk id cut short.
func looksCut(id string) bool {
	n := 0
	for n < len(id) && isLowerHex(id[n]) {
		n++
	}
	if n == 32 && n == len(id) {
		return false // a whole id
	}
	rest := id[n:]
	if rest == "" {
		return n >= minCutIDPrefix && n < 32
	}
	return n >= 4 && strings.Trim(rest, cutIDTail) == ""
}

func isLowerHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// refuseCutIDs refuses a call that carries a cut id in any field that holds a
// document or chunk id. ok is false when there is none.
func refuseCutIDs(in docInput) (tools.Result, bool) {
	fields := []struct {
		name string
		vals []string
	}{
		{"id", []string{in.ID}},
		{"document_id", []string{in.DocumentID}},
		{"document_ids", in.DocumentIDs},
		{"parent_id", []string{in.ParentID}},
		{"new_parent_id", []string{in.NewParentID}},
		{"after_id", []string{in.AfterID}},
		{"from_id", []string{in.FromID}},
		{"to_id", []string{in.ToID}},
		{"supersedes_id", []string{in.SupersedesID}},
		{"about", []string{in.About}},
		{"seed_ids", in.SeedIDs},
	}
	for _, f := range fields {
		for _, v := range f.vals {
			if looksCut(v) {
				return errValidation(
					fmt.Sprintf("Document %s: %s %q is cut short — a document or chunk id is 32 hex characters. Nothing was done.", in.Op, f.name, v),
					"Copy the whole id from the result that returned it (create_document, create_chunk, query_chunks) — every character, no \"...\"."), true
			}
		}
	}
	return tools.Result{}, false
}
