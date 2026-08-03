package builtin

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMarkdownRoundTrip_FencedHeadingsAreNotStructure is the invariant export_md
// and import_md exist to uphold: exporting a document and importing the result
// reproduces it.
//
// It failed for any document whose body contained a Markdown sample — which is
// most technical documentation, including this project's own RFCs and guides.
// export_md was already right (it emits the body verbatim); import_md matched
// headings line-by-line with no awareness of fenced code blocks, so the sample's
// "## Example" lines became real chunks and the surrounding body was truncated at
// the fence. The document came back with MORE chunks than it had.
func TestMarkdownRoundTrip_FencedHeadingsAreNotStructure(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)

	body := "Here is a config sample:\n\n```markdown\n## Not A Heading\n\nsome text\n" +
		"### Deeper Not A Heading\n```\n\nTrailing prose."
	bj, _ := json.Marshal(body)
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Section One","body":`+string(bj)+`}`); r.IsError {
		t.Fatalf("create one: %s", r.Text)
	}
	// A real sibling, so fabricated structure is distinguishable from real structure.
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Section Two"}`); r.IsError {
		t.Fatalf("create two: %s", r.Text)
	}

	md, r := docExec(t, d, ctx, `{"op":"export_md","scope":"user","id":"`+docID+`"}`)
	if r.IsError {
		t.Fatalf("export: %s", r.Text)
	}
	exported := asStr(md["markdown"])

	ij, _ := json.Marshal(exported)
	out, r2 := docExec(t, d, ctx, `{"op":"import_md","scope":"user","markdown":`+string(ij)+`}`)
	if r2.IsError {
		t.Fatalf("import: %s", r2.Text)
	}
	// root + Section One + Section Two. Anything more is fabricated from the fence.
	if n := asInt(out["chunks_created"]); n != 3 {
		t.Errorf("re-import created %d chunks, want 3 — the fenced sample's headings were "+
			"parsed as document structure", n)
	}

	// And the shape is stable: a second export matches the first once ids are
	// stripped (they are freshly minted per import and are not structure).
	md2, _ := docExec(t, d, ctx,
		`{"op":"export_md","scope":"user","id":"`+asStr(out["document_id"])+`"}`)
	if a, b := stripLoomMeta(exported), stripLoomMeta(asStr(md2["markdown"])); a != b {
		t.Errorf("round-trip is not stable:\n--- first ---\n%s\n--- second ---\n%s", a, b)
	}

	// The body must survive whole — the fence content included.
	if !strings.Contains(asStr(md2["markdown"]), "### Deeper Not A Heading") {
		t.Errorf("the fenced sample was lost from the body: %s", asStr(md2["markdown"]))
	}
	if !strings.Contains(asStr(md2["markdown"]), "Trailing prose.") {
		t.Errorf("body after the fence was truncated: %s", asStr(md2["markdown"]))
	}
}

// TestMarkdownRoundTrip_TildeAndNestedFences covers the delimiter rules the fix
// relies on: ~~~ fences, and a shorter run INSIDE a longer one staying literal.
func TestMarkdownRoundTrip_TildeAndNestedFences(t *testing.T) {
	d, ctx, docID, root := entityFixture(t)

	// A ````-opened block containing a ``` run and a heading: the inner run is too
	// short to close the outer fence, so everything stays body.
	body := "~~~\n## Tilde Fenced\n~~~\n\n````\n```\n## Still Not A Heading\n```\n````"
	bj, _ := json.Marshal(body)
	if _, r := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"Fences","body":`+string(bj)+`}`); r.IsError {
		t.Fatalf("create: %s", r.Text)
	}
	md, _ := docExec(t, d, ctx, `{"op":"export_md","scope":"user","id":"`+docID+`"}`)
	ij, _ := json.Marshal(asStr(md["markdown"]))
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"user","markdown":`+string(ij)+`}`)
	if r.IsError {
		t.Fatalf("import: %s", r.Text)
	}
	if n := asInt(out["chunks_created"]); n != 2 {
		t.Errorf("re-import created %d chunks, want 2 (root + Fences) — a fenced heading "+
			"became structure", n)
	}
}

// stripLoomMeta drops the per-chunk metadata comments, whose ids are freshly
// minted on every import and are therefore not part of the structure being
// compared.
func stripLoomMeta(s string) string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "<!-- loom:") {
			continue
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
