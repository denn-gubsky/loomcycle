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

// A driver whose access-control set hides one covered document may neither read
// the folder's inputs (they name and describe every document below) nor write
// its annotation.
func TestFolderAnnotation_InputsNeedEveryCoveredDocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	seedFolderCorpus(t, d, op)
	drv := tools.WithRunID(tools.WithRunPolicy(base, tools.RunPolicy{
		Tools:            []string{"*"},
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		StatementMethods: []string{annotationGrant},
		ACL: aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
			Resources: []string{"document:tenant:/kb/products/pricing"}}),
	}), "run-confined")
	for _, q := range []string{
		`{"op":"annotation_inputs","scope":"tenant","level":"path","path":"/kb/products"}`,
		`{"op":"set_annotation","scope":"tenant","level":"path","path":"/kb/products","brief":"b","abstract":"a","inputs_sha256":"x"}`,
	} {
		_, r := docExec(t, d, drv, q)
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission || strings.Contains(r.Text, "Pricing") {
			t.Errorf("%s = %+v, want a permission refusal naming nothing", q, r)
		}
	}
}

// An operator's edit from the explorer names a folder's record by its chunk.
func TestFolderAnnotation_EditedByItsChunk(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	seedFolderCorpus(t, d, op)
	rec := folderWrite(t, d, op, "/kb/products", "Product guides")
	out, r := docExec(t, d, op, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","id":%q,"level":"path","brief":"Hand-written","abstract":"An operator's text.","manual":true}`, rec["chunk_id"]))
	if r.IsError {
		t.Fatalf("edit by chunk: %s", r.Text)
	}
	if a := out["annotation"].(map[string]any); a["folder"] != "/kb/products" || a["manual"] != true || a["brief"] != "Hand-written" {
		t.Fatalf("edited record = %v", a)
	}
}

// Before its record exists, an _annotation document's folder is read from where
// it is named, never from its root's editable path field.
func TestFolderAnnotation_ItsFolderIsWhereItIsNamed(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	seedFolderCorpus(t, d, op)
	out, r := docExec(t, d, op, `{"op":"create_document","scope":"tenant","title":"products","path":"/kb/products/_annotation","type":"Annotation"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	root := out["root_chunk_id"].(string)
	g, _ := docExec(t, d, op, `{"op":"get_chunk","scope":"tenant","id":"`+root+`"}`)
	if _, r := docExec(t, d, op, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"fields":{"path":"/kb/elsewhere"},"revision":%v}`, root, g["revision"])); r.IsError {
		t.Fatal(r.Text)
	}
	key, _, _ := d.resolveScope(op, "tenant")
	folder, err := d.folderOfChunk(op, key, root)
	if err != nil || folder != "/kb/products" {
		t.Fatalf("folder = %q, %v; want /kb/products", folder, err)
	}
}

// --- RFC GM P6: typical documents, sibling folders and member statuses ---

// folderSiblingCorpus is seedFolderCorpus with two sibling folders of
// /kb/products, each carrying its own record, and a done document.
func folderSiblingCorpus(t *testing.T, d *Document, ctx context.Context) (folderCorpus, string) {
	t.Helper()
	c := seedFolderCorpus(t, d, ctx)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Support plan","path":"/kb/services/support","type":"Guide","status":"done"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	support := out["document_id"].(string)
	annWrite(t, d, ctx, `"document_id":"`+support+`"`, "About the support plan")
	out, r = docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Terms","path":"/kb/legal/terms","type":"Policy"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	annWrite(t, d, ctx, `"document_id":"`+out["document_id"].(string)+`"`, "About the terms")
	folderWriteAt(t, d, ctx, "/kb/services", "Support plans and the services offered to customers", folderContextPromptVersion)
	folderWriteAt(t, d, ctx, "/kb/legal", "Legal terms and conditions that govern every sale", folderContextPromptVersion)
	return c, support
}

func folderInputsAt(t *testing.T, d *Document, ctx context.Context, folder string, version int) map[string]any {
	t.Helper()
	in, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"annotation_inputs","scope":"tenant","level":"path","path":%q,"prompt_version":%d}`, folder, version))
	if r.IsError {
		t.Fatalf("annotation_inputs %s: %s", folder, r.Text)
	}
	return in
}

func folderWriteAt(t *testing.T, d *Document, ctx context.Context, folder, brief string, version int) map[string]any {
	t.Helper()
	in := folderInputsAt(t, d, ctx, folder, version)
	out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","level":"path","path":%q,"brief":%q,"abstract":%q,"inputs_sha256":%q,"prompt_version":%d}`,
		folder, brief, "What "+folder+" holds.", in["inputs_sha256"], version))
	if r.IsError {
		t.Fatalf("set_annotation %s: %s", folder, r.Text)
	}
	return out["annotation"].(map[string]any)
}

func folderRecordStale(t *testing.T, d *Document, ctx context.Context, folder string) bool {
	t.Helper()
	got, r := docExec(t, d, ctx, `{"op":"get_annotation","scope":"tenant","level":"path","path":"`+folder+`"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	a := got["annotations"].([]any)
	if len(a) != 1 {
		t.Fatalf("get_annotation %s = %v, want one record", folder, got)
	}
	return a[0].(map[string]any)["stale"] == true
}

func exemplarTitles(in map[string]any) []string {
	var out []string
	ex, _ := in["exemplars"].([]any)
	for _, e := range ex {
		out = append(out, e.(map[string]any)["title"].(string))
	}
	return out
}

// TestFolderAnnotation_ContextVersionCarriesExemplarsSiblingsAndStatuses — from
// the folder context prompt version on, a folder's request names its typical
// documents (with their abstracts), its sibling folders with their briefs, and
// its documents' stored statuses.
func TestFolderAnnotation_ContextVersionCarriesExemplarsSiblingsAndStatuses(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := annOperator(base)
		folderSiblingCorpus(t, d, ctx)
		in := folderInputsAt(t, d, ctx, "/kb/products", folderContextPromptVersion)
		raw, _ := json.Marshal(in["siblings"])
		if string(raw) != `[{"brief":"Legal terms and conditions that govern every sale","name":"legal"},{"brief":"Support plans and the services offered to customers","name":"services"}]` {
			t.Errorf("siblings = %s", raw)
		}
		ex, _ := in["exemplars"].([]any)
		for _, e := range ex {
			if a, _ := e.(map[string]any)["abstract"].(string); a == "" {
				t.Errorf("exemplar %v has no abstract", e)
			}
		}
		// The folder's majority class first, then by name.
		if got := fmt.Sprint(exemplarTitles(in)); got != "[Gadget guide Widget guide Pricing]" {
			t.Errorf("exemplars = %v", got)
		}
		raw, _ = json.Marshal(in["member_statuses"])
		if string(raw) != `[{"count":3,"name":"none"}]` {
			t.Errorf("member_statuses = %s", raw)
		}
		// /kb: one typical document from each sub-folder before a second from any.
		in = folderInputsAt(t, d, ctx, "/kb", folderContextPromptVersion)
		if got := fmt.Sprint(exemplarTitles(in)); got != "[Gadget guide Support plan Terms]" {
			t.Errorf("/kb exemplars = %v", got)
		}
		raw, _ = json.Marshal(in["member_statuses"])
		if string(raw) != `[{"count":4,"name":"none"},{"count":1,"name":"done"}]` {
			t.Errorf("/kb member_statuses = %s", raw)
		}
		if _, ok := in["siblings"]; ok {
			t.Errorf("/kb has no sibling folder, got %v", in["siblings"])
		}
	})
}

// TestFolderAnnotation_EarlierVersionInputsAreUnchanged — below the folder context
// version the request and its hash are what they were: no new field, and a sibling
// folder's record changes nothing.
func TestFolderAnnotation_EarlierVersionInputsAreUnchanged(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	seedFolderCorpus(t, d, ctx)
	before := folderInputsAt(t, d, ctx, "/kb/products", 2)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Terms","path":"/kb/legal/terms","type":"Policy"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	annWrite(t, d, ctx, `"document_id":"`+out["document_id"].(string)+`"`, "About the terms")
	folderWriteAt(t, d, ctx, "/kb/legal", "Legal terms", 2)
	after := folderInputsAt(t, d, ctx, "/kb/products", 2)
	for _, k := range []string{"exemplars", "siblings", "siblings_more", "member_statuses"} {
		if _, ok := after[k]; ok {
			t.Errorf("version 2 inputs carry %s: %v", k, after[k])
		}
	}
	if before["inputs_sha256"] != after["inputs_sha256"] {
		t.Fatalf("a sibling's record changed a version 2 hash: %v -> %v", before["inputs_sha256"], after["inputs_sha256"])
	}
}

// TestFolderAnnotation_SiblingRewriteMarksNothingStale — a sibling is shown but not
// hashed: siblings rewritten in turn never make each other stale, so a version bump
// rewrites each folder once, and a sibling's first record does not make the folders
// beside it stale either (a first pass writes each folder once).
func TestFolderAnnotation_SiblingRewriteMarksNothingStale(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	folderSiblingCorpus(t, d, ctx)
	folderWriteAt(t, d, ctx, "/kb/products", "Product guides", folderContextPromptVersion)
	folderWriteAt(t, d, ctx, "/kb/services", "Support services, rewritten", folderContextPromptVersion)
	folderWriteAt(t, d, ctx, "/kb/legal", "Legal terms, rewritten", folderContextPromptVersion)
	for _, f := range []string{"/kb/products", "/kb/services", "/kb/legal"} {
		if folderRecordStale(t, d, ctx, f) {
			t.Fatalf("a sibling's rewrite made %s stale", f)
		}
	}
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Hiring","path":"/kb/people/hiring","type":"Guide"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	annWrite(t, d, ctx, `"document_id":"`+out["document_id"].(string)+`"`, "About hiring")
	folderWriteAt(t, d, ctx, "/kb/people", "People", folderContextPromptVersion)
	if folderRecordStale(t, d, ctx, "/kb/products") {
		t.Fatal("a new sibling folder's first record made /kb/products stale")
	}
	// It is named the next time /kb/products is written.
	in := folderInputsAt(t, d, ctx, "/kb/products", folderContextPromptVersion)
	if raw, _ := json.Marshal(in["siblings"]); !strings.Contains(string(raw), `"name":"people"`) {
		t.Fatalf("siblings = %s, want people named", raw)
	}
}

// TestFolderAnnotation_ExemplarRewriteMarksTheFolderStale — a typical document's
// abstract is in the request, so its rewrite is in the hash.
func TestFolderAnnotation_ExemplarRewriteMarksTheFolderStale(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	_, support := folderSiblingCorpus(t, d, ctx)
	folderWriteAt(t, d, ctx, "/kb", "The knowledge base", folderContextPromptVersion)
	if folderRecordStale(t, d, ctx, "/kb") {
		t.Fatal("precondition: /kb is current")
	}
	annWrite(t, d, ctx, `"document_id":"`+support+`"`, "About the support plan, again")
	if !folderRecordStale(t, d, ctx, "/kb") {
		t.Fatal("rewriting a typical document left /kb current")
	}
}

// TestFolderAnnotation_RestrictedSiblingIsLeftOut — a sibling folder holding a
// document some member of the tenant may not read is not named (RFC GM C8): the
// folder's own readers need not be able to read it.
func TestFolderAnnotation_RestrictedSiblingIsLeftOut(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := annOperator(base)
		folderSiblingCorpus(t, d, ctx)
		restrictForSome(t, d, "/kb/legal/terms")
		in := folderInputsAt(t, d, ctx, "/kb/products", folderContextPromptVersion)
		raw, _ := json.Marshal(in["siblings"])
		if string(raw) != `[{"brief":"Support plans and the services offered to customers","name":"services"}]` {
			t.Fatalf("siblings = %s, want legal left out", raw)
		}
	})
}

// confinedAnnDriver is an annotating run whose own rules deny it paths.
func confinedAnnDriver(t *testing.T, base context.Context, paths ...string) context.Context {
	t.Helper()
	var res []string
	for _, p := range paths {
		res = append(res, "document:tenant:"+p)
	}
	return tools.WithRunPolicy(base, tools.RunPolicy{
		Memory:           tools.MemoryPolicyValue{AllowedScopes: []string{"tenant"}},
		SqlMem:           tools.SqlMemPolicyValue{AllowedScopes: []string{"tenant"}},
		StatementMethods: []string{annotationGrant},
		ACL: aclSet(t,
			authz.Rule{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:tenant"}},
			authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: res}),
	})
}

// folderOpsDirect runs annotation_inputs or set_annotation at level path past the
// op's access gate, as a caller the gate let through would reach it.
func folderOpsDirect(t *testing.T, d *Document, ctx context.Context, op string, in docInput) tools.Result {
	t.Helper()
	key, mscope, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	in.Op, in.Level = op, annotationLevelPath
	var res tools.Result
	if op == "annotation_inputs" {
		res, err = d.folderAnnotationInputs(ctx, key, mscope, in)
	} else {
		res, err = d.setFolderAnnotation(ctx, key, mscope, in, "A brief long enough to stand as it is written", "An abstract.")
	}
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestFolderAnnotation_InputsAreGatedOnTheWholeScope — a caller with a read deny on
// any document of the scope is refused annotation_inputs at level path before any
// folder is read: the op asks about the whole scope, and a deny naming one instance
// refuses that question (authz DecideScope). set_annotation asks write, so its own
// covers check (TestFolderAnnotation_InputsNeedEverySiblingDocument) is what holds.
func TestFolderAnnotation_InputsAreGatedOnTheWholeScope(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	folderSiblingCorpus(t, d, op)
	drv := confinedAnnDriver(t, base, "/kb/legal/terms")
	for _, q := range []string{
		fmt.Sprintf(`{"op":"annotation_inputs","scope":"tenant","level":"path","path":"/kb/products","prompt_version":%d}`, folderContextPromptVersion),
	} {
		_, r := docExec(t, d, drv, q)
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission || strings.Contains(r.Text, "Legal terms") {
			t.Errorf("%s = %+v, want a permission refusal naming nothing", q, r)
		}
	}
}

// TestFolderAnnotation_InputsNeedEverySiblingDocument — past the scope gate, a
// caller who may not read a document a named sibling's brief was written from may
// neither read the folder's inputs (they show that brief) nor write its annotation,
// even where every member of the tenant may read it.
func TestFolderAnnotation_InputsNeedEverySiblingDocument(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	folderSiblingCorpus(t, d, op)
	drv := confinedAnnDriver(t, base, "/kb/legal/terms")
	for _, which := range []string{"annotation_inputs", "set_annotation"} {
		r := folderOpsDirect(t, d, drv, which, docInput{Path: "/kb/products", PromptVersion: folderContextPromptVersion, InputsSHA256: "x"})
		if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission || strings.Contains(r.Text, "Legal terms") {
			t.Errorf("%s = %+v, want a permission refusal naming nothing", which, r)
		}
	}
	// set_annotation asks write of the scope, which the read deny does not refuse:
	// through the tool, with the current hash, its covers check still refuses it.
	cur := folderInputsAt(t, d, op, "/kb/products", folderContextPromptVersion)
	_, r := docExec(t, d, drv, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","level":"path","path":"/kb/products","prompt_version":%d,"brief":"A brief long enough to stand as it is written","abstract":"a","inputs_sha256":%q}`,
		folderContextPromptVersion, cur["inputs_sha256"]))
	if !r.IsError || r.Error == nil || r.Error.Category != tools.CategoryPermission {
		t.Errorf("set_annotation through the tool = %+v, want a permission refusal", r)
	}
	// The earlier version names no sibling: the same caller reads it.
	if r := folderOpsDirect(t, d, drv, "annotation_inputs", docInput{Path: "/kb/products", PromptVersion: 2}); r.IsError {
		t.Fatalf("version 2 inputs refused: %s", r.Text)
	}
}

// TestFolderAnnotation_InputsWithholdARecordDrawnOnUnreadableDocuments — the stored
// record annotation_inputs returns beside the current inputs is the caller's only if
// it may read every document that record was withheld on, not only today's.
func TestFolderAnnotation_InputsWithholdARecordDrawnOnUnreadableDocuments(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	folderSiblingCorpus(t, d, op)
	folderWriteAt(t, d, op, "/kb/products", "Product guides drawn beside the legal folder", folderContextPromptVersion)
	// legal loses its record: today's inputs name no sibling drawn on terms, but the
	// stored record was written from legal's brief.
	if _, r := docExec(t, d, op, `{"op":"delete_annotation","scope":"tenant","level":"path","path":"/kb/legal"}`); r.IsError {
		t.Fatal(r.Text)
	}
	drv := confinedAnnDriver(t, base, "/kb/legal/terms")
	r := folderOpsDirect(t, d, drv, "annotation_inputs", docInput{Path: "/kb/products", PromptVersion: folderContextPromptVersion})
	if r.IsError {
		t.Fatalf("annotation_inputs: %s", r.Text)
	}
	var in map[string]any
	_ = json.Unmarshal([]byte(r.Text), &in)
	if in["record"] != nil || strings.Contains(r.Text, "Product guides drawn") || in["stale"] != true {
		t.Fatalf("inputs = %s, want no record and stale", r.Text)
	}
}

// TestFolderAnnotation_SiblingRestrictedSinceWithholdsTheRecord — a record written
// from a sibling's brief is withheld from a reader who may not read that sibling's
// documents, until it is written again without the sibling.
func TestFolderAnnotation_SiblingRestrictedSinceWithholdsTheRecord(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	folderSiblingCorpus(t, d, op)
	folderWriteAt(t, d, op, "/kb/products", "Product guides", folderContextPromptVersion)
	member := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb/legal/terms"}}))
	got, r := docExec(t, d, member, `{"op":"get_annotation","scope":"tenant","level":"path","path":"/kb/products"}`)
	if r.IsError || len(got["annotations"].([]any)) != 0 {
		t.Fatalf("member get_annotation = %v %s, want nothing: the record drew on /kb/legal", got, r.Text)
	}
	got, _ = docExec(t, d, tenantDocCtx(base, nil), `{"op":"get_annotation","scope":"tenant","level":"path","path":"/kb/products"}`)
	if len(got["annotations"].([]any)) != 1 {
		t.Fatalf("a reader with no rule: get_annotation = %v", got)
	}
	// Restricted for some members: the hash moves, and the rewrite leaves legal out
	// and is shown to that member again.
	restrictForSome(t, d, "/kb/legal/terms")
	if !folderRecordStale(t, d, op, "/kb/products") {
		t.Fatal("restricting a sibling's document left /kb/products current")
	}
	folderWriteAt(t, d, op, "/kb/products", "Product guides", folderContextPromptVersion)
	got, _ = docExec(t, d, member, `{"op":"get_annotation","scope":"tenant","level":"path","path":"/kb/products"}`)
	if len(got["annotations"].([]any)) != 1 {
		t.Fatalf("member get_annotation after the rewrite = %v", got)
	}
}

// folderScopeInputs computes a folder's inputs through a fresh annotation scope
// and returns the scope, whose counters say what the computation loaded.
func folderScopeInputs(t *testing.T, d *Document, ctx context.Context, folder string) (folderInputs, *annotationScope) {
	t.Helper()
	key, mscope, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	s := d.newAnnotationScope(key, mscope)
	in, err := s.folderInputsFor(ctx, folder, folderContextPromptVersion, nil)
	if err != nil {
		t.Fatal(err)
	}
	return in, s
}

// TestFolderAnnotation_ExemplarTextIsLoadedOnlyForTheChosen — a folder's request
// is recomputed on every stale check: it loads the annotation text of its typical
// documents only, never of every document it covers.
func TestFolderAnnotation_ExemplarTextIsLoadedOnlyForTheChosen(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	folderSiblingCorpus(t, d, ctx)
	in, s := folderScopeInputs(t, d, ctx, "/kb")
	if len(in.Covers) != 5 || len(in.Exemplars) != 3 {
		t.Fatalf("precondition: covers %d exemplars %d, want 5 and 3", len(in.Covers), len(in.Exemplars))
	}
	if s.exemplarTextLoads != 3 {
		t.Fatalf("loaded the annotation text of %d documents, want only the 3 typical ones", s.exemplarTextLoads)
	}
}

// TestFolderAnnotation_SiblingWorkIsBounded — a folder reads only the records of
// the folders beside it, not every folder record of the scope, and checks no
// sibling past the ones it names.
func TestFolderAnnotation_SiblingWorkIsBounded(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	const n = maxFolderSiblings + 3
	for i := 1; i <= n; i++ {
		folder := fmt.Sprintf("/kb/s%02d", i)
		out, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_document","scope":"tenant","title":"Doc %d","path":%q,"type":"Guide"}`, i, folder+"/doc"))
		if r.IsError {
			t.Fatal(r.Text)
		}
		annWrite(t, d, ctx, `"document_id":"`+out["document_id"].(string)+`"`, fmt.Sprintf("About document %d", i))
		folderWriteAt(t, d, ctx, folder, fmt.Sprintf("Folder number %d of the sample set", i), folderContextPromptVersion)
	}
	// Records outside /kb/'s children: the folder itself and the root.
	folderWriteAt(t, d, ctx, "/kb", "The whole sample knowledge base", folderContextPromptVersion)
	folderWriteAt(t, d, ctx, "/", "The root of the sample knowledge base", folderContextPromptVersion)
	in, s := folderScopeInputs(t, d, ctx, "/kb/s01")
	if len(in.Siblings) != maxFolderSiblings {
		t.Fatalf("siblings = %d, want %d", len(in.Siblings), maxFolderSiblings)
	}
	if s.siblingRows != n-1 {
		t.Errorf("read %d folder records, want only the %d siblings", s.siblingRows, n-1)
	}
	if s.siblingChecks != maxFolderSiblings {
		t.Errorf("checked %d siblings, want the %d it names", s.siblingChecks, maxFolderSiblings)
	}
}

// TestFolderAnnotation_SiblingRewrittenBetweenReadAndWriteIsRefused — the brief a
// driver writes was written from the siblings annotation_inputs showed: a sibling
// rewritten in between refuses the write, so the stored record is withheld on the
// documents of the siblings its text came from.
func TestFolderAnnotation_SiblingRewrittenBetweenReadAndWriteIsRefused(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	folderSiblingCorpus(t, d, ctx)
	in := folderInputsAt(t, d, ctx, "/kb/products", folderContextPromptVersion)
	if _, r := docExec(t, d, ctx, `{"op":"delete_annotation","scope":"tenant","level":"path","path":"/kb/legal"}`); r.IsError {
		t.Fatal(r.Text)
	}
	_, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_annotation","scope":"tenant","level":"path","path":"/kb/products","prompt_version":%d,"brief":"Product guides set apart from the legal terms","abstract":"a","inputs_sha256":%q}`,
		folderContextPromptVersion, in["inputs_sha256"]))
	if !r.IsError || !strings.Contains(r.Text, "annotation_inputs_changed") {
		t.Fatalf("write after a shown sibling went = %+v, want annotation_inputs_changed", r)
	}
	// The sibling's change alone does not make the stored record stale.
	folderWriteAt(t, d, ctx, "/kb/products", "Product guides set apart from support", folderContextPromptVersion)
	folderWriteAt(t, d, ctx, "/kb/services", "Support plans, written once more for the test", folderContextPromptVersion)
	if folderRecordStale(t, d, ctx, "/kb/products") {
		t.Fatal("a sibling's rewrite made /kb/products stale")
	}
}
