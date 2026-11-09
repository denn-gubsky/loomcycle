package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A folder's annotation: an _annotation document of type Annotation, with the
// path record on its root.

type folderCorpus struct {
	widgets, gadgets, secret string // document ids
}

func seedFolderCorpus(t *testing.T, d *Document, ctx context.Context) folderCorpus {
	t.Helper()
	mk := func(title, path, typ string) string {
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":%q,"path":%q,"type":%q}`, title, path, typ))
		if r.IsError {
			t.Fatalf("create_document %s: %s", path, r.Text)
		}
		doc := out["document_id"].(string)
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Body","body":"The %s is described here in some detail for the reader."}`, doc, title)); r.IsError {
			t.Fatal(r.Text)
		}
		annWrite(t, d, ctx, `"document_id":"`+doc+`"`, "About the "+title)
		return doc
	}
	return folderCorpus{
		widgets: mk("Widget guide", "/kb/products/widgets", "Guide"),
		gadgets: mk("Gadget guide", "/kb/products/gadgets", "Guide"),
		secret:  mk("Pricing", "/kb/products/pricing", "Proposal"),
	}
}

func folderWrite(t *testing.T, d *Document, ctx context.Context, folder, brief string) map[string]any {
	t.Helper()
	in, r := docExec(t, d, ctx, `{"op":"annotation_inputs","scope":"tenant","level":"path","path":"`+folder+`"}`)
	if r.IsError {
		t.Fatalf("annotation_inputs %s: %s", folder, r.Text)
	}
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","level":"path","path":%q,"brief":%q,"abstract":%q,"inputs_sha256":%q}`,
		folder, brief, "What "+folder+" holds.", in["inputs_sha256"]))
	if r.IsError {
		t.Fatalf("set_annotation %s: %s", folder, r.Text)
	}
	return out["annotation"].(map[string]any)
}

func TestFolderAnnotation_LivesOnAnAnnotationDocument(t *testing.T) {
	statementsBothTiers(t, testFolderAnnotationLivesOnAnAnnotationDocument)
}

func testFolderAnnotationLivesOnAnAnnotationDocument(t *testing.T, d *Document, base context.Context) {
	ctx := annOperator(base)
	c := seedFolderCorpus(t, d, ctx)
	in, r := docExec(t, d, ctx, `{"op":"annotation_inputs","scope":"tenant","level":"path","path":"/kb/products"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if n := len(in["covers"].([]any)); n != 3 {
		t.Fatalf("covers = %v, want the three documents", in["covers"])
	}
	raw, _ := json.Marshal(in["classes"])
	if string(raw) != `[{"count":2,"name":"Guide"},{"count":1,"name":"Proposal"}]` {
		t.Fatalf("classes = %s", raw)
	}
	kids := in["children"].([]any)
	if len(kids) != 3 || kids[0].(map[string]any)["brief"] != "About the Gadget guide" {
		t.Fatalf("children = %v", kids)
	}
	rec := folderWrite(t, d, ctx, "/kb/products", "Product guides and pricing")
	if rec["level"] != "path" || rec["folder"] != "/kb/products" {
		t.Fatalf("record = %v", rec)
	}
	// The record's home is a real document, named and typed as the folder's
	// annotation, with an empty body.
	doc, r := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","path":"/kb/products/_annotation"}`)
	if r.IsError {
		t.Fatalf("the _annotation document: %s", r.Text)
	}
	if doc["type"] != "Annotation" || doc["title"] != "products" {
		t.Fatalf("_annotation document = %v", doc)
	}
	root, _ := docExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+doc["root_chunk_id"].(string)+`"}`)
	if root["body"] != nil && root["body"] != "" {
		t.Fatalf("the annotation document has a body: %v", root["body"])
	}
	if f, _ := root["fields"].(map[string]any); f["brief"] != "Product guides and pricing" || f["path"] != "/kb/products" {
		t.Fatalf("root fields = %v", root["fields"])
	}
	got, _ := docExec(t, d, ctx, `{"op":"get_annotation","scope":"tenant","level":"path","path":"/kb/products"}`)
	if a := got["annotations"].([]any); len(a) != 1 || a[0].(map[string]any)["stale"] != false {
		t.Fatalf("get_annotation = %v", got)
	}
	// documents_summary labels it with the folder's brief.
	sum, _ := docExec(t, d, ctx, `{"op":"documents_summary","scope":"tenant","document_ids":["`+doc["document_id"].(string)+`"]}`)
	if row := sum["documents"].([]any)[0].(map[string]any); row["brief"] != "Product guides and pricing" {
		t.Fatalf("documents_summary row = %v", row)
	}
	// A document's annotation rewritten: the folder goes stale; a second write
	// updates the same document rather than making another.
	annWrite(t, d, ctx, `"document_id":"`+c.widgets+`"`, "About the widgets, again")
	got, _ = docExec(t, d, ctx, `{"op":"get_annotation","scope":"tenant","level":"path","path":"/kb/products"}`)
	if a := got["annotations"].([]any); a[0].(map[string]any)["stale"] != true {
		t.Fatalf("folder not stale after a child changed: %v", got)
	}
	folderWrite(t, d, ctx, "/kb/products", "Product guides and pricing, again")
	ls, _ := docExec(t, d, ctx, `{"op":"query_documents","scope":"tenant","type":"Annotation"}`)
	if n := len(ls["documents"].([]any)); n != 1 {
		t.Fatalf("annotation documents = %d, want 1", n)
	}
	// The parent folder sees the sub-folder's brief.
	in, _ = docExec(t, d, ctx, `{"op":"annotation_inputs","scope":"tenant","level":"path","path":"/kb"}`)
	sub := in["children"].([]any)[0].(map[string]any)
	if sub["kind"] != "folder" || sub["brief"] != "Product guides and pricing, again" {
		t.Fatalf("/kb children = %v", in["children"])
	}
}

func TestFolderAnnotation_PathLsWithSummary(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	seedFolderCorpus(t, d, ctx)
	folderWrite(t, d, ctx, "/kb/products", "Product guides and pricing")
	p := &Path{Store: d.Store, Annotations: d}
	run := func(q string) map[string]any {
		res, err := p.Execute(ctx, json.RawMessage(q))
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %s", q, err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	plain := run(`{"op":"ls","scope":"tenant","path":"/kb"}`)
	if strings.Contains(fmt.Sprint(plain), "summary") {
		t.Fatalf("ls without with_summary carries summaries: %v", plain)
	}
	out := run(`{"op":"ls","scope":"tenant","path":"/kb","with_summary":true}`)
	e := out["entries"].([]any)[0].(map[string]any)
	if sm, _ := e["summary"].(map[string]any); sm["brief"] != "Product guides and pricing" || sm["abstract"] == nil {
		t.Fatalf("/kb/products entry = %v", e)
	}
	out = run(`{"op":"ls","scope":"tenant","path":"/kb/products","with_summary":true}`)
	for _, raw := range out["entries"].([]any) {
		e := raw.(map[string]any)
		sm, _ := e["summary"].(map[string]any)
		switch e["name"] {
		case "widgets":
			if sm["brief"] != "About the Widget guide" {
				t.Errorf("widgets = %v", e)
			}
		case "_annotation":
			if sm["brief"] != "Product guides and pricing" {
				t.Errorf("_annotation = %v", e)
			}
		}
	}
}

// A folder's annotation speaks for every document it covers: a member who may
// not read one of them is shown neither the record nor its document, and still
// sees the annotations of the documents it can read.
func TestFolderAnnotation_WithheldUnlessEveryCoveredDocumentIsReadable(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	c := seedFolderCorpus(t, d, op)
	folderWrite(t, d, op, "/kb/products", "Product guides and pricing")
	member := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb/products/pricing"}}))
	got, r := docExec(t, d, member, `{"op":"get_annotation","scope":"tenant","level":"path","path":"/kb/products"}`)
	if r.IsError || len(got["annotations"].([]any)) != 0 {
		t.Fatalf("member get_annotation = %v %s, want nothing", got, r.Text)
	}
	if _, r := docExec(t, d, member, `{"op":"get_document","scope":"tenant","path":"/kb/products/_annotation"}`); !r.IsError {
		t.Fatal("member read the _annotation document")
	}
	list, _ := docExec(t, d, member, `{"op":"query_documents","scope":"tenant","type":"Annotation"}`)
	if n := len(list["documents"].([]any)); n != 0 {
		t.Fatalf("member lists %d annotation documents", n)
	}
	p := &Path{Store: d.Store, Annotations: d}
	res, _ := p.Execute(member, json.RawMessage(`{"op":"ls","scope":"tenant","path":"/kb","with_summary":true}`))
	if strings.Contains(res.Text, "Product guides") {
		t.Fatalf("member ls shows the folder's brief: %s", res.Text)
	}
	got, r = docExec(t, d, member, `{"op":"get_document","scope":"tenant","id":"`+c.widgets+`","with_summary":true}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	if sm, _ := got["summary"].(map[string]any); sm["brief"] != "About the Widget guide" {
		t.Fatalf("member's readable document summary = %v", got["summary"])
	}
}

func TestFolderAnnotation_TheNameIsReserved(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	seedFolderCorpus(t, d, op)
	member := tools.WithRunID(tenantDocCtx(base, nil), "run-member")
	if _, r := docExec(t, d, member, `{"op":"create_document","scope":"tenant","title":"Fake","path":"/kb/products/_annotation"}`); !r.IsError || r.Error.Category != tools.CategoryPermission {
		t.Fatalf("create at _annotation = %+v, want a permission refusal", r)
	}
	p := &Path{Store: d.Store}
	if res, _ := p.Execute(member, json.RawMessage(`{"op":"mv","scope":"tenant","path":"/kb/products/widgets","to":"/kb/products/_annotation"}`)); !res.IsError {
		t.Fatalf("mv to _annotation = %s, want refused", res.Text)
	}
	// An annotation document is not evidence.
	folderWrite(t, d, op, "/kb/products", "Product guides")
	doc, _ := docExec(t, d, op, `{"op":"get_document","scope":"tenant","path":"/kb/products/_annotation"}`)
	other, _ := docExec(t, d, op, `{"op":"get_document","scope":"tenant","path":"/kb/products/widgets"}`)
	_, r := docExec(t, d, op, fmt.Sprintf(`{"op":"propose_statement","scope":"tenant","from_id":%q,"to_id":%q,"kind":"cites","evidence":[{"chunk_id":%q,"quote":"products"}]}`,
		other["root_chunk_id"], doc["root_chunk_id"], doc["root_chunk_id"]))
	if !r.IsError || !strings.Contains(r.Text, "evidence_source_operational") {
		t.Fatalf("evidence from an annotation document = %s", r.Text)
	}
	// Deleting the folder's annotation removes its document.
	if out, r := docExec(t, d, op, `{"op":"delete_annotation","scope":"tenant","level":"path","path":"/kb/products"}`); r.IsError || out["deleted"] != true {
		t.Fatalf("delete_annotation = %v %s", out, r.Text)
	}
	if _, r := docExec(t, d, op, `{"op":"get_document","scope":"tenant","path":"/kb/products/_annotation"}`); !r.IsError {
		t.Fatal("the _annotation document outlived its record")
	}
}

// A folder that is itself a document carries its path record on that
// document's root, and the document's content is untouched.
func TestFolderAnnotation_AFolderThatIsADocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Products","path":"/kb/products","type":"Index"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	idx := out["document_id"].(string)
	seedFolderCorpus(t, d, ctx)
	before, _ := docExec(t, d, ctx, `{"op":"export_md","scope":"tenant","id":"`+idx+`"}`)
	rec := folderWrite(t, d, ctx, "/kb/products", "Product guides")
	if rec["document_id"] != idx {
		t.Fatalf("record lives on %v, want the folder's own document %s", rec["document_id"], idx)
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","path":"/kb/products/_annotation"}`); !r.IsError {
		t.Fatal("an _annotation document was made for a folder that is a document")
	}
	after, _ := docExec(t, d, ctx, `{"op":"export_md","scope":"tenant","id":"`+idx+`"}`)
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("the folder's document changed:\n%v\n%v", before, after)
	}
	in, _ := docExec(t, d, ctx, `{"op":"annotation_inputs","scope":"tenant","level":"path","path":"/kb/products"}`)
	for _, id := range in["covers"].([]any) {
		if id == idx {
			t.Fatal("a folder's own document is in what it covers")
		}
	}
}
