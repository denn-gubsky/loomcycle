package builtin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// RFC FT P0 — copy_documents: a set of documents copied into another scope on
// the server, with their history, tags, assets and the relations between them.

type copySeed struct {
	planDoc, planChunk, guideDoc, guideChunk string
}

// seedCopyDocs writes two linked user-scope documents: a plan whose part was
// edited once (so it has history), tagged, and a guide the plan relates to.
func seedCopyDocs(t *testing.T, d *Document, ctx context.Context) copySeed {
	t.Helper()
	var s copySeed
	mk := func(path, title, body string) (string, string) {
		out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"`+title+`","path":"`+path+`"}`)
		if r.IsError {
			t.Fatalf("create_document %s: %s", path, r.Text)
		}
		doc := asStr(out["document_id"])
		out, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+doc+`","title":"`+title+` part","body":"`+body+`"}`)
		if r.IsError {
			t.Fatalf("create_chunk %s: %s", path, r.Text)
		}
		return doc, asStr(out["id"])
	}
	s.planDoc, s.planChunk = mk("/proj/rfcs/plan", "Plan", "the first draft of the plan")
	s.guideDoc, s.guideChunk = mk("/proj/docs/guide", "Guide", "how to use the plan")
	if _, r := docExec(t, d, ctx, `{"op":"update_chunk","scope":"user","id":"`+s.planChunk+`","revision":1,"body":"the second draft of the plan"}`); r.IsError {
		t.Fatalf("update_chunk: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"add_tags","scope":"user","document_id":"`+s.planDoc+`","tags":["area/plans"]}`); r.IsError {
		t.Fatalf("add_tags: %s", r.Text)
	}
	if _, r := docExec(t, d, ctx, `{"op":"link_chunks","scope":"user","from_id":"`+s.planChunk+`","to_id":"`+s.guideChunk+`","kind":"documented_by"}`); r.IsError {
		t.Fatalf("link_chunks: %s", r.Text)
	}
	return s
}

func copyExec(t *testing.T, d *Document, ctx context.Context, body string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, body)
	if r.IsError {
		t.Fatalf("%s: %s", body, r.Text)
	}
	return out
}

// copiedID is the new id the answer gives for a source document.
func copiedID(t *testing.T, out map[string]any, src string) string {
	t.Helper()
	docs, _ := out["documents"].([]any)
	for _, e := range docs {
		m := e.(map[string]any)
		if m["id"] == src {
			return asStr(m["new_id"])
		}
	}
	t.Fatalf("document %s is not in the answer: %v", src, out)
	return ""
}

func cleanExport(t *testing.T, d *Document, ctx context.Context, scope, docID string) string {
	t.Helper()
	out := copyExec(t, d, ctx, `{"op":"export_md","scope":"`+scope+`","document_id":"`+docID+`","include_metadata":false}`)
	return asStr(out["markdown"])
}

func TestCopyDocuments_CarriesTheSetIntoTheTargetScope(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)

	out := copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/proj","dry_run":false}`)
	if refused, _ := out["refused"].([]any); len(refused) != 0 {
		t.Fatalf("nothing should be refused: %v", refused)
	}
	plan, guide := copiedID(t, out, s.planDoc), copiedID(t, out, s.guideDoc)
	if plan == "" || plan == s.planDoc || guide == "" || guide == s.guideDoc {
		t.Fatalf("a copy mints new ids: plan %q guide %q", plan, guide)
	}

	// The same text, at the same names, in the tenant scope.
	for _, c := range []struct{ src, dst, path string }{{s.planDoc, plan, "/proj/rfcs/plan"}, {s.guideDoc, guide, "/proj/docs/guide"}} {
		if a, b := cleanExport(t, d, ctx, "user", c.src), cleanExport(t, d, ctx, "tenant", c.dst); a != b {
			t.Errorf("the copy's text differs from its source:\n%s\n---\n%s", a, b)
		}
		got := copyExec(t, d, ctx, `{"op":"get_document","scope":"tenant","path":"`+c.path+`"}`)
		if asStr(got["document_id"]) != c.dst {
			t.Errorf("%s in the tenant scope names %v, want the copy %s", c.path, got["document_id"], c.dst)
		}
	}

	// The relation between the two copies came along, re-pointed at the copies.
	edges := copyExec(t, d, ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+plan+`","direction":"out"}`)
	list, _ := edges["edges"].([]any)
	if len(list) != 1 {
		t.Fatalf("the copy should carry the one relation between the documents, got %v", edges)
	}
	e := list[0].(map[string]any)
	if e["kind"] != "documented_by" || e["to_document_id"] != guide {
		t.Errorf("the carried relation should join the two copies: %v", e)
	}

	// History, tags and the copied_from record.
	pc := copyExec(t, d, ctx, `{"op":"get_document","scope":"tenant","id":"`+plan+`"}`)
	part := chunkTitled(t, d, ctx, "tenant", plan, "Plan part")
	hist := copyExec(t, d, ctx, `{"op":"history","scope":"tenant","id":"`+part+`"}`)
	if revs, _ := hist["revisions"].([]any); len(revs) < 2 {
		t.Errorf("the body history should come along (two revisions), got %v", hist)
	}
	tags := copyExec(t, d, ctx, `{"op":"list_tags","scope":"tenant","document_id":"`+plan+`"}`)
	if b, _ := json.Marshal(tags); !strings.Contains(string(b), "area/plans") {
		t.Errorf("the document's tags should come along: %v", tags)
	}
	root := copyExec(t, d, ctx, `{"op":"get_chunk","scope":"tenant","id":"`+asStr(pc["root_chunk_id"])+`"}`)
	from, _ := root["fields"].(map[string]any)["copied_from"].(map[string]any)
	if from["scope"] != "user" || from["document_id"] != s.planDoc {
		t.Errorf("the copy's root should record where it came from, got %v", root["fields"])
	}

	// The source is untouched.
	if got := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","path":"/proj/rfcs/plan"}`); asStr(got["document_id"]) != s.planDoc {
		t.Errorf("the source should keep its name: %v", got)
	}
}

func TestCopyDocuments_DryRunIsTheDefaultAndWritesNothing(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)

	out := copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/proj"}`)
	if out["dry_run"] != true {
		t.Fatalf("dry_run should default to true: %v", out)
	}
	counts := out["counts"].(map[string]any)
	if counts["documents"] != float64(2) || counts["statements"] != float64(1) || counts["revisions"] == float64(0) {
		t.Errorf("the dry run should count what a copy would carry: %v", counts)
	}
	if got := copiedID(t, out, s.planDoc); got != "" {
		t.Errorf("a dry run mints no ids, got %q", got)
	}
	if _, r := docExec(t, d, ctx, `{"op":"get_document","scope":"tenant","path":"/proj/rfcs/plan"}`); !r.IsError {
		t.Error("a dry run must write nothing to the target scope")
	}
}

func TestCopyDocuments_ARelationLeavingTheSetStaysBehind(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)

	out := copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant","document_ids":["`+s.planDoc+`"],"dry_run":false}`)
	dangling, _ := out["dangling"].([]any)
	if len(dangling) != 1 || dangling[0].(map[string]any)["outside"] != "to" || out["dangling_count"] != float64(1) {
		t.Fatalf("the relation to the guide, outside the set, should be listed as dangling: %v", out)
	}
	plan := copiedID(t, out, s.planDoc)
	edges := copyExec(t, d, ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+plan+`"}`)
	if list, _ := edges["edges"].([]any); len(list) != 0 {
		t.Errorf("a relation to a document left behind is not copied: %v", edges)
	}
	src := copyExec(t, d, ctx, `{"op":"get_edges","scope":"user","document_id":"`+s.planDoc+`"}`)
	if list, _ := src["edges"].([]any); len(list) != 1 {
		t.Errorf("the source keeps its relation: %v", src)
	}
}

func TestCopyDocuments_RefusesByNameAndCopiesTheRest(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	subjectRoot := seedSubjectWithFact(t, d, ctx, "user", "dave", "person:dave", "Dave runs the shop")
	subject := copyExec(t, d, ctx, `{"op":"get_chunk","scope":"user","id":"`+subjectRoot+`"}`)
	subjectDoc := asStr(subject["document_id"])
	// A name the tenant scope already holds.
	copyExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Taken","path":"/proj/docs/guide"}`)

	out := copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant",
		"document_ids":["`+s.planDoc+`","`+s.guideDoc+`","`+subjectDoc+`","nope"],"dry_run":false}`)
	codes := map[string]string{}
	for _, r := range out["refused"].([]any) {
		m := r.(map[string]any)
		codes[asStr(m["document_id"])] = asStr(m["code"])
	}
	want := map[string]string{s.guideDoc: copyPathCollision, subjectDoc: copyIsSubject, "nope": copyNotFound}
	for id, code := range want {
		if codes[id] != code {
			t.Errorf("document %s: refused as %q, want %q (all: %v)", id, codes[id], code, codes)
		}
	}
	if copiedID(t, out, s.planDoc) == "" {
		t.Error("the plan, refused for nothing, should still be copied")
	}
}

func TestCopyDocuments_SameScopeNeedsAPathMap(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	s := seedCopyDocs(t, d, ctx)
	if _, r := docExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"user","document_ids":["`+s.planDoc+`"]}`); !r.IsError || !strings.Contains(r.Text, "path_map") {
		t.Fatalf("a same-scope copy without path_map must be refused, got %s", r.Text)
	}
	out := copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"user","under_path":"/proj",
		"path_map":{"/proj":"/archive/proj","/proj/rfcs":"/archive/rfcs"},"dry_run":false}`)
	got := copyExec(t, d, ctx, `{"op":"get_document","scope":"user","path":"/archive/rfcs/plan"}`)
	if asStr(got["document_id"]) != copiedID(t, out, s.planDoc) {
		t.Errorf("the longest prefix should win: /proj/rfcs/plan → /archive/rfcs/plan, got %v", got)
	}
	got = copyExec(t, d, ctx, `{"op":"get_document","scope":"user","path":"/archive/proj/docs/guide"}`)
	if asStr(got["document_id"]) != copiedID(t, out, s.guideDoc) {
		t.Errorf("/proj/docs/guide → /archive/proj/docs/guide, got %v", got)
	}
}

func TestCopyDocuments_TheTargetScopeNeedsItsGrant(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	s := seedCopyDocs(t, d, ctx)
	// No tenant grant on this run.
	if _, r := docExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant","document_ids":["`+s.planDoc+`"],"dry_run":false}`); !r.IsError {
		t.Fatal("a copy into a scope the caller may not write must be refused")
	}
}

func TestCopyDocuments_CarriesImageAssets(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	ctx = grantedTenantCtx(ctx)
	s := seedCopyDocs(t, d, ctx)
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff, 0xfe, 0x01}
	img := copyExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+s.guideDoc+`","title":"Diagram"}`)
	if _, r := docExec(t, d, ctx, `{"op":"set_asset","scope":"user","id":"`+asStr(img["id"])+`","media_type":"image/png","data":"`+base64.StdEncoding.EncodeToString(png)+`"}`); r.IsError {
		t.Fatalf("set_asset: %s", r.Text)
	}
	out := copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant","document_ids":["`+s.guideDoc+`"],"dry_run":false}`)
	if out["counts"].(map[string]any)["assets"] != float64(1) {
		t.Fatalf("one asset should be copied: %v", out["counts"])
	}
	diagram := chunkTitled(t, d, ctx, "tenant", copiedID(t, out, s.guideDoc), "Diagram")
	key, _, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	mt, data, ok := d.readAssetRow(ctx, key, diagram)
	if !ok || mt != "image/png" || string(data) != string(png) {
		t.Errorf("the image bytes should come across unchanged: ok=%v type=%q bytes=%v", ok, mt, data)
	}
}

func TestCopyDocuments_OnPostgres(t *testing.T) {
	d, ctx := renamePostgresFixture(t)
	ctx = tools.UpdateRunPolicy(ctx, func(rp *tools.RunPolicy) {
		rp.Memory = tools.MemoryPolicyValue{AllowedScopes: []string{"user", "tenant"}}
		rp.SqlMem = tools.SqlMemPolicyValue{AllowedScopes: []string{"user", "tenant"}}
	})
	tkey, _, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.docSQL().DropScope(context.Background(), tkey); err != nil {
		t.Fatalf("DropScope (stale tenant): %v", err)
	}
	t.Cleanup(func() { _, _ = d.docSQL().DropScope(context.Background(), tkey) })
	s := seedCopyDocs(t, d, ctx)
	// bytea is where a string-typed copy would corrupt the bytes.
	png := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff, 0xfe, 0x01, '\\', 'x'}
	img := copyExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+s.guideDoc+`","title":"Diagram"}`)
	if _, r := docExec(t, d, ctx, `{"op":"set_asset","scope":"user","id":"`+asStr(img["id"])+`","media_type":"image/png","data":"`+base64.StdEncoding.EncodeToString(png)+`"}`); r.IsError {
		t.Fatalf("set_asset: %s", r.Text)
	}
	out := copyExec(t, d, ctx, `{"op":"copy_documents","scope":"user","to_scope":"tenant","under_path":"/proj","dry_run":false}`)
	plan := copiedID(t, out, s.planDoc)
	if _, data, ok := d.readAssetRow(ctx, tkey, chunkTitled(t, d, ctx, "tenant", copiedID(t, out, s.guideDoc), "Diagram")); !ok || string(data) != string(png) {
		t.Errorf("the image bytes should come across unchanged on postgres: ok=%v bytes=%v", ok, data)
	}
	if a, b := cleanExport(t, d, ctx, "user", s.planDoc), cleanExport(t, d, ctx, "tenant", plan); a != b {
		t.Errorf("the copy's text differs on postgres:\n%s\n---\n%s", a, b)
	}
	edges := copyExec(t, d, ctx, `{"op":"get_edges","scope":"tenant","document_id":"`+plan+`","direction":"out"}`)
	if list, _ := edges["edges"].([]any); len(list) != 1 {
		t.Errorf("the relation should come along on postgres: %v", edges)
	}
}

// chunkTitled finds a document's chunk by its title.
func chunkTitled(t *testing.T, d *Document, ctx context.Context, scope, docID, title string) string {
	t.Helper()
	out := copyExec(t, d, ctx, `{"op":"query_chunks","scope":"`+scope+`","sql":"SELECT id FROM chunks WHERE document_id = '`+docID+`' AND title = '`+title+`'"}`)
	rows, _ := out["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("document %s should have one chunk titled %q, got %v", docID, title, out)
	}
	return asStr(rows[0].([]any)[0])
}
