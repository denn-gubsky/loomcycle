package builtin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDocumentExportImport_RoundTripsValidity(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	doc := newValidityDoc(t, d, ctx, "/qg/norms/ed1")
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","document_id":%q,"title":"Dated","body":"text","valid_at":%d,"invalid_at":%d}`, doc, ns2024, ns2026))
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","document_id":%q,"title":"Open","body":"text","valid_at":0}`, doc))
	vExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","document_id":%q,"title":"Undated","body":"text"}`, doc))
	exp := vExec(t, d, ctx, fmt.Sprintf(`{"op":"export_md","document_id":%q}`, doc))
	md := resultField(toolResult(exp), "markdown")
	if !strings.Contains(md, fmt.Sprintf(`"valid_at":%d`, ns2024)) || !strings.Contains(md, fmt.Sprintf(`"invalid_at":%d`, ns2026)) {
		t.Fatalf("export_md did not write the interval into the loom comment:\n%s", md)
	}
	imp, _ := json.Marshal(map[string]any{"op": "import_md", "markdown": md})
	newDoc := resultField(toolResult(vExec(t, d, ctx, string(imp))), "document_id")
	q := vExec(t, d, ctx, fmt.Sprintf(`{"op":"query_chunks","document_id":%q}`, newDoc))
	var rows struct {
		Chunks []map[string]any `json:"chunks"`
	}
	dec := json.NewDecoder(strings.NewReader(q))
	dec.UseNumber()
	_ = dec.Decode(&rows)
	byTitle := map[string]map[string]any{}
	for _, r := range rows.Chunks {
		byTitle[r["title"].(string)] = r
	}
	num := func(v any) string {
		if v == nil {
			return "<nil>"
		}
		return v.(json.Number).String()
	}
	if num(byTitle["Dated"]["valid_at"]) != fmt.Sprint(ns2024) || num(byTitle["Dated"]["invalid_at"]) != fmt.Sprint(ns2026) {
		t.Errorf("Dated after round trip = %v", byTitle["Dated"])
	}
	if num(byTitle["Open"]["valid_at"]) != "0" || byTitle["Open"]["invalid_at"] != nil {
		t.Errorf("Open after round trip = %v", byTitle["Open"])
	}
	if byTitle["Undated"]["valid_at"] != nil || byTitle["Undated"]["invalid_at"] != nil {
		t.Errorf("Undated after round trip = %v", byTitle["Undated"])
	}
	// A second export is identical apart from the ids.
	exp2 := resultField(toolResult(vExec(t, d, ctx, fmt.Sprintf(`{"op":"export_md","document_id":%q}`, newDoc))), "markdown")
	if normaliseIDs(exp2) != normaliseIDs(md) {
		t.Errorf("export → import → export changed the document:\n%s\n---\n%s", md, exp2)
	}
}

// An inconsistent interval in a metadata comment is refused before anything is
// written, rather than leaving half a document behind.
func TestDocumentImport_RefusesAnEmptyIntervalBeforeWriting(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	md := fmt.Sprintf("# Doc\n\n## Bad\n<!-- loom: {\"valid_at\":%d,\"invalid_at\":%d} -->\n\ntext\n", ns2026, ns2024)
	imp, _ := json.Marshal(map[string]any{"op": "import_md", "markdown": md})
	res, _ := d.Execute(ctx, imp)
	if !res.IsError {
		t.Fatalf("import_md accepted an empty interval: %s", res.Text)
	}
	q := vExec(t, d, ctx, `{"op":"query_documents"}`)
	if strings.Contains(q, `"Doc"`) {
		t.Errorf("a refused import left a document behind: %s", q)
	}
}
