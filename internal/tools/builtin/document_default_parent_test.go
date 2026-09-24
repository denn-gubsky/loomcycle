package builtin

import (
	"fmt"
	"strings"
	"testing"
)

// A chunk created with no parent is a section of the document: it hangs off the
// root, and export_md renders it one level under the title. It used to be stored
// with no parent at all, beside the root, and exported as a second "# " heading.
func TestCreateChunk_OmittedParentGoesUnderTheRoot(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Launch plan"}`)
	docID, root := doc["document_id"].(string), doc["root_chunk_id"].(string)

	ch, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Timeline","body":"GA in April."}`, docID))
	if res.IsError {
		t.Fatalf("create_chunk: %s", res.Text)
	}
	if ch["parent_id"] != root {
		t.Errorf("parent_id = %v, want the root chunk %s", ch["parent_id"], root)
	}
	md, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"export_md","scope":"user","document_id":%q,"include_metadata":false}`, docID))
	if res.IsError {
		t.Fatalf("export_md: %s", res.Text)
	}
	text := md["markdown"].(string)
	if !strings.Contains(text, "## Timeline") || strings.Contains(text, "\n# Timeline") || strings.HasPrefix(text, "# Timeline") {
		t.Errorf("export_md does not show Timeline as a section of the document:\n%s", text)
	}
}

// Looking up the root is also an existence check: a mistyped document_id is an
// error, not a chunk that no document shows.
func TestCreateChunk_UnknownDocumentIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, res := docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"0000000000000000000000000000dead","title":"Lost"}`)
	if !res.IsError || !strings.Contains(res.Text, "not found") {
		t.Errorf("create_chunk on an unknown document: %q, want a not-found refusal", res.Text)
	}
}

// move_chunk with an empty new_parent_id moves the chunk directly under the
// title — and leaves the root where it is when the root is what is moved.
func TestMoveChunk_EmptyParentMovesUnderTheRoot(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Plan"}`)
	docID, root := doc["document_id"].(string), doc["root_chunk_id"].(string)
	sec, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Section"}`, docID))
	sub, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"parent_id":%q,"title":"Sub"}`, docID, sec["id"]))

	moved, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"move_chunk","scope":"user","id":%q}`, sub["id"]))
	if res.IsError || moved["new_parent_id"] != root {
		t.Errorf("move to empty parent: %s, want new_parent_id = root %s", res.Text, root)
	}
	if _, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"move_chunk","scope":"user","id":%q}`, root)); res.IsError {
		t.Errorf("moving the root to an empty parent: %s, want it left as the root", res.Text)
	}
	got, _ := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_chunk","scope":"user","id":%q}`, root))
	if p, _ := got["parent_id"].(string); p != "" {
		t.Errorf("root chunk gained parent %q", p)
	}
}
