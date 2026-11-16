package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// colorSchemeFixture creates n documents under /docs, each root stamped with
// color_enabled and a palette the size the doc-colorizer bundle writes (about
// 4 KB), and returns their ids.
func colorSchemeFixture(t *testing.T, d *Document, ctx context.Context, n int) []string {
	t.Helper()
	pal := map[string]string{}
	for _, typ := range []string{"rfc", "document", "research", "publication", "plan", "note", "guide", "spec"} {
		for _, st := range []string{"draft", "review", "confirmed", "done", "retired", "asserted", "proposed", "rejected", "stale", "archived", "open", "closed", "blocked", "deferred"} {
			pal["doc."+typ+"."+st] = fmt.Sprintf("#%06x", len(pal)*7919%0xffffff)
		}
	}
	scheme, _ := json.Marshal(pal)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out, res := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"agent","title":"Doc %d","path":"/docs/doc-%d"}`, i, i))
		if res.IsError {
			t.Fatalf("create %d: %s", i, res.Text)
		}
		root := out["root_chunk_id"].(string)
		if _, res := docExec(t, d, ctx, `{"op":"update_chunk","scope":"agent","id":"`+root+`","revision":1,"type":"rfc","status":"done","fields":{"color_enabled":true,"color_scheme":`+string(scheme)+`}}`); res.IsError {
			t.Fatalf("stamp %d: %s", i, res.Text)
		}
		ids = append(ids, out["document_id"].(string))
	}
	return ids
}

// summaryText runs documents_summary over ids (plus extra input) and returns
// the raw result and its entries.
func summaryText(t *testing.T, d *Document, ctx context.Context, ids []string, extra string) (string, []map[string]any) {
	t.Helper()
	b, _ := json.Marshal(ids)
	res, err := d.Execute(ctx, json.RawMessage(`{"op":"documents_summary","scope":"agent","document_ids":`+string(b)+extra+`}`))
	if err != nil || res.IsError {
		t.Fatalf("documents_summary: %v %s", err, res.Text)
	}
	var out struct {
		Documents []map[string]any `json:"documents"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Documents) != len(ids) {
		t.Fatalf("documents_summary = %d docs, want %d", len(out.Documents), len(ids))
	}
	return res.Text, out.Documents
}

// A model reads documents through the tool in a run or over MCP, where the
// ctx carries no WithDocumentColorSchemes. The palette (about 4 KB of hex per
// document) must not reach it, while color_enabled, type and status stay.
func TestDocument_ModelFacingReadsOmitColorScheme(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := colorSchemeFixture(t, d, ctx, 12)

	plain, docs := summaryText(t, d, ctx, ids, "")
	for _, e := range docs {
		if _, has := e["color_scheme"]; has {
			t.Fatalf("documents_summary entry carries color_scheme: %v", e["document_id"])
		}
		if e["color_enabled"] != true || e["type"] != "rfc" || e["status"] != "done" {
			t.Errorf("entry lost its display metadata: %v", e)
		}
	}
	full, _ := summaryText(t, d, WithDocumentColorSchemes(ctx), ids, "")
	t.Logf("documents_summary of %d documents: %d bytes with color_scheme, %d without", len(ids), len(full), len(plain))
	if len(plain)*5 > len(full) {
		t.Errorf("model-facing summary %d bytes is not a fraction of the %d-byte app answer", len(plain), len(full))
	}

	out, res := docExec(t, d, ctx, `{"op":"get_document","scope":"agent","id":"`+ids[0]+`"}`)
	if res.IsError {
		t.Fatalf("get_document: %s", res.Text)
	}
	if _, has := out["color_scheme"]; has || out["color_enabled"] != true {
		t.Errorf("get_document = %s, want color_enabled and no color_scheme", res.Text)
	}

	out, res = docExec(t, d, ctx, `{"op":"graph_snapshot","scope":"agent"}`)
	if res.IsError {
		t.Fatalf("graph_snapshot: %s", res.Text)
	}
	sec, _ := out["documents"].(map[string]any)
	if sec == nil {
		t.Fatalf("graph_snapshot has no documents section: %.300s", res.Text)
	}
	if _, has := sec["color_scheme"]; has || strings.Contains(res.Text, "doc.rfc.done") {
		t.Errorf("graph_snapshot carries color_scheme")
	}
}

// The Web UI explorer, the universe and the published @loomcycle/explorer read
// the palette from the off-run route without asking; that route stamps
// WithDocumentColorSchemes. include_color_scheme overrides either default.
func TestDocument_AppSurfaceReadsKeepColorScheme(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ids := colorSchemeFixture(t, d, ctx, 2)
	app := WithDocumentColorSchemes(ctx)

	hasScheme := func(docs []map[string]any) bool {
		cs, _ := docs[0]["color_scheme"].(map[string]any)
		return cs["doc.rfc.done"] != nil
	}
	if _, docs := summaryText(t, d, app, ids, ""); !hasScheme(docs) {
		t.Errorf("app documents_summary lost color_scheme: %v", docs[0])
	}
	if _, docs := summaryText(t, d, ctx, ids, `,"include_color_scheme":true`); !hasScheme(docs) {
		t.Errorf("include_color_scheme:true did not add color_scheme: %v", docs[0])
	}
	if _, docs := summaryText(t, d, app, ids, `,"include_color_scheme":false`); hasScheme(docs) {
		t.Errorf("include_color_scheme:false kept color_scheme on the app surface")
	}

	out, res := docExec(t, d, app, `{"op":"get_document","scope":"agent","id":"`+ids[0]+`"}`)
	if cs, _ := out["color_scheme"].(map[string]any); res.IsError || cs["doc.rfc.done"] == nil {
		t.Errorf("app get_document lost color_scheme: %s", res.Text)
	}

	out, res = docExec(t, d, app, `{"op":"graph_snapshot","scope":"agent"}`)
	sec, _ := out["documents"].(map[string]any)
	if col, _ := sec["color_scheme"].([]any); res.IsError || len(col) != 2 || col[0] == nil {
		t.Errorf("app graph_snapshot color_scheme column = %v", sec["color_scheme"])
	}
}
