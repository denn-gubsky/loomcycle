package builtin

import (
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// A model that has just been handed a document_id passes document_id. The ops
// that act on a whole document read it as well as `id`, instead of refusing
// with "missing required field: id".
func TestDocument_WholeDocumentOpsAcceptDocumentID(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	d.Cfg = &config.Config{DocumentSources: map[string]config.DocumentSource{"peerA": {Config: config.DocumentSourceConfig{BaseURL: "https://peer"}}}}
	out, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Local"}`)
	docID := out["document_id"].(string)

	got, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_document","scope":"user","document_id":%q}`, docID))
	if res.IsError || got["document_id"] != docID {
		t.Errorf("get_document by document_id: %s", res.Text)
	}
	if _, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_remote","scope":"user","document_id":%q,"source":"peerA","remote_ref":"/docs/remote"}`, docID)); res.IsError {
		t.Errorf("set_remote by document_id: %s", res.Text)
	}
	del, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_document","scope":"user","document_id":%q}`, docID))
	if res.IsError || del["deleted"] != true {
		t.Errorf("delete_document by document_id: %s", res.Text)
	}
}

// Two ids that disagree name two documents; acting on either would be a guess.
func TestDocument_ConflictingIDAndDocumentIDIsRefused(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	a, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"A"}`)
	b, _ := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"B"}`)
	_, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"delete_document","scope":"user","id":%q,"document_id":%q}`, a["document_id"], b["document_id"]))
	if !res.IsError || !strings.Contains(res.Text, "name different documents") {
		t.Fatalf("conflicting ids: %q, want a refusal", res.Text)
	}
	// Neither was deleted.
	for _, id := range []any{a["document_id"], b["document_id"]} {
		if _, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"get_document","scope":"user","id":%q}`, id)); res.IsError {
			t.Errorf("document %v gone after a refused delete: %s", id, res.Text)
		}
	}
}
