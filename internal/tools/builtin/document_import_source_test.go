package builtin

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// importChunkIDs is every chunk of a document, root first.
func importChunkIDs(t *testing.T, d *Document, ctx context.Context, docID string) []string {
	t.Helper()
	key := sidecarScope(t, d, ctx)
	roots, byParent, err := d.loadChunkTree(ctx, key, docID)
	if err != nil {
		t.Fatalf("loadChunkTree: %v", err)
	}
	var ids []string
	var walk func([]chunkRow)
	walk = func(rows []chunkRow) {
		for _, r := range rows {
			ids = append(ids, r.ID)
			walk(byParent[r.ID])
		}
	}
	walk(roots)
	return ids
}

func bodyOriginOf(t *testing.T, d *Document, ctx context.Context, st store.Store, chunkID string) string {
	t.Helper()
	sk := sidecarScope(t, d, ctx)
	prov, err := st.MemoryProvenanceGet(ctx, direntTenant(ctx), store.MemoryScopeUser, sk.ScopeID, "doc.chunk:"+chunkID)
	if err != nil {
		t.Fatalf("MemoryProvenanceGet %s: %v", chunkID, err)
	}
	return prov.Origin
}

const importMDSample = "# Board mail\n\nThe board met on Monday.\n\n## Decisions\n\nBudget approved.\n"

// An import_md that names its source is an import: every chunk body carries
// the import origin (document text, never a fact), and the root says where
// it came from, the link stored without its credential.
func TestImportMD_SourceStampsImportOriginAndRoot(t *testing.T) {
	d, ctx, st := documentFixture(t)
	call, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": importMDSample,
		"source_url":   "https://outlook.office.com/mail/inbox/id/AAMk?ItemID=AAMk1&tempauth=SECRETVALUE123#access_token=SECRETFRAG",
		"source_kind":  "Email",
		"source_title": "Re: board\nminutes\t2026"})
	out, res := docExec(t, d, ctx, string(call))
	if res.IsError {
		t.Fatalf("import_md: %s", res.Text)
	}
	docID := asStr(out["document_id"])
	ids := importChunkIDs(t, d, ctx, docID)
	if len(ids) != 2 {
		t.Fatalf("chunks = %v", ids)
	}
	for _, id := range ids {
		o := bodyOriginOf(t, d, ctx, st, id)
		if o != store.OriginImport {
			t.Errorf("chunk %s body origin = %q, want %q", id, o, store.OriginImport)
		}
		if got := store.ClassifyMemoryRow("doc.chunk:"+id, o, "doc.chunk:"); got != store.MemoryRowDocument {
			t.Errorf("imported chunk %s classifies as %q, want document", id, got)
		}
	}

	got, res := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+docID+`"}`)
	if res.IsError {
		t.Fatalf("get_document: %s", res.Text)
	}
	if strings.Contains(res.Text, "SECRETVALUE123") || strings.Contains(res.Text, "SECRETFRAG") {
		t.Errorf("get_document shows the source URL's credential: %s", res.Text)
	}
	prov, _ := got["provenance"].(map[string]any)
	if prov["origin"] != store.OriginImport || prov["source_kind"] != "email" || prov["source_title"] != "Re: board minutes 2026" {
		t.Errorf("provenance = %v", prov)
	}
	if u, _ := prov["source_url"].(string); !strings.HasPrefix(u, "https://outlook.office.com/mail/inbox/id/AAMk?ItemID=AAMk1&tempauth=[redacted") || strings.Contains(u, "#") {
		t.Errorf("source_url = %q, want the item's link kept, the credential masked, no fragment", u)
	}
	if tags, _ := got["tags"].([]any); !slices.Contains(tags, any("source/email")) {
		t.Errorf("tags = %v, want source/email", got["tags"])
	}

	// export_md says the same, in its result and, clean, under the title.
	exp, res := docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`","include_metadata":false}`)
	if res.IsError {
		t.Fatalf("export_md: %s", res.Text)
	}
	if p, _ := exp["provenance"].(map[string]any); p["source_kind"] != "email" {
		t.Errorf("export_md provenance = %v", exp["provenance"])
	}
	md := asStr(exp["markdown"])
	if !strings.HasPrefix(md, "# Board mail\n\n> Source: Re: board minutes 2026 https://outlook.office.com/") || !strings.Contains(md, "(email)") {
		t.Errorf("clean export = %q", md)
	}
	// A metadata export carries it in the root's loom comment, and adds no line.
	exp, _ = docExec(t, d, ctx, `{"op":"export_md","scope":"user","document_id":"`+docID+`"}`)
	if md := asStr(exp["markdown"]); !strings.Contains(md, `"source_kind":"email"`) || strings.Contains(md, "> Source:") {
		t.Errorf("metadata export = %q", md)
	}
}

// Without a source (and outside a File import) import_md writes prose as it
// always did: no origin, no provenance.
func TestImportMD_WithoutSourceIsNotStamped(t *testing.T) {
	d, ctx, st := documentFixture(t)
	call, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": importMDSample})
	out, res := docExec(t, d, ctx, string(call))
	if res.IsError {
		t.Fatalf("import_md: %s", res.Text)
	}
	for _, id := range importChunkIDs(t, d, ctx, asStr(out["document_id"])) {
		if o := bodyOriginOf(t, d, ctx, st, id); o != "" {
			t.Errorf("chunk %s body origin = %q, want none", id, o)
		}
	}
	got, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+asStr(out["document_id"])+`"}`)
	if _, ok := got["provenance"]; ok {
		t.Errorf("an unsourced import has provenance: %v", got["provenance"])
	}
}

// The text being imported cannot write the provenance fields: what it gives
// for one is dropped, whether or not the import sets that field itself.
func TestImportMD_MarkdownCannotForgeProvenanceFields(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	md := "# Note\n<!-- loom: {\"fields\":{\"captured_from\":\"mcp:ms365/get\",\"sha256\":\"forged\",\"origin\":\"consolidator\",\"keep\":\"me\"}} -->\n\nbody\n"
	call, _ := json.Marshal(map[string]any{"op": "import_md", "scope": "user", "markdown": md, "source_title": "A note"})
	out, res := docExec(t, d, ctx, string(call))
	if res.IsError {
		t.Fatalf("import_md: %s", res.Text)
	}
	got, _ := docExec(t, d, ctx, `{"op":"get_document","scope":"user","id":"`+asStr(out["document_id"])+`"}`)
	prov, _ := got["provenance"].(map[string]any)
	if prov["origin"] != store.OriginImport || prov["captured_from"] != nil || prov["sha256"] != nil || prov["source_kind"] != "other" {
		t.Errorf("provenance = %v", prov)
	}
	key := sidecarScope(t, d, ctx)
	body, err := d.readBody(ctx, store.MemoryScopeUser, key, asStr(out["root_chunk_id"]))
	if err != nil || !strings.Contains(string(body.Fields), `"keep":"me"`) {
		t.Errorf("the root's own fields were lost: %s %v", body.Fields, err)
	}
}

func TestImportMD_SourceRefusals(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	for name, src := range map[string]map[string]any{
		"ftp":        {"source_url": "ftp://files.example/a.pdf"},
		"script":     {"source_url": "javascript:alert(1)"},
		"no host":    {"source_url": "https:///a"},
		"userinfo":   {"source_url": "https://me:pw@files.example/a"},
		"space":      {"source_url": "https://files.example/a b"},
		"too long":   {"source_url": "https://files.example/" + strings.Repeat("a", 2100)},
		"bad kind":   {"source_kind": "telegram"},
		"into a doc": {"source_title": "x", "document_id": "some-doc"},
	} {
		in := map[string]any{"op": "import_md", "scope": "user", "markdown": importMDSample}
		for k, v := range src {
			in[k] = v
		}
		call, _ := json.Marshal(in)
		if _, res := docExec(t, d, ctx, string(call)); !res.IsError {
			t.Errorf("%s: import accepted", name)
		}
	}
	if out, _ := docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`); len(out["documents"].([]any)) != 0 {
		t.Errorf("a refused import left documents: %v", out["documents"])
	}
}

func TestImportSource_TitleIsCutToItsLimit(t *testing.T) {
	s, err := ImportSource{Title: strings.Repeat("я", 400)}.normalize()
	if err != nil || len([]rune(s.Title)) != importSourceMaxTitleRunes {
		t.Errorf("title runes = %d, %v", len([]rune(s.Title)), err)
	}
}
