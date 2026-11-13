package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// RFC GL P2 — Document op=lineage: a chunk's sections, document and folders up
// to the root of the Path tree, each with its brief, in one call.

func lineageOf(t *testing.T, d *Document, ctx context.Context, id string) map[string]any {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"lineage","scope":"tenant","id":"`+id+`"}`)
	if r.IsError {
		t.Fatalf("lineage %s: %s", id, r.Text)
	}
	return out
}

// folderPaths returns one path entry's folders as "path=brief" (brief "" when
// none), nearest first.
func folderPaths(t *testing.T, entry any) []string {
	t.Helper()
	var out []string
	for _, f := range entry.(map[string]any)["folders"].([]any) {
		m := f.(map[string]any)
		b, _ := m["brief"].(string)
		out = append(out, m["path"].(string)+"="+b)
	}
	return out
}

func TestDocumentLineage_NestedSectionReachesTheRootWithBriefs(t *testing.T) {
	statementsBothTiers(t, func(t *testing.T, d *Document, base context.Context) {
		ctx := annOperator(base)
		c := seedAnnCorpus(t, d, ctx) // /kb/widgets/guide: root ── Setup ── Step one
		annWrite(t, d, ctx, `"id":"`+c.setup+`"`, "How to set the widget up")
		annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
		folderWrite(t, d, ctx, "/kb/widgets", "Everything about widgets")
		folderWrite(t, d, ctx, "/kb", "The whole knowledge base of the team")

		got := lineageOf(t, d, ctx, c.step1)
		secs := got["sections"].([]any)
		if len(secs) != 2 {
			t.Fatalf("sections = %v, want Step one then Setup", secs)
		}
		s0, s1 := secs[0].(map[string]any), secs[1].(map[string]any)
		if s0["id"] != c.step1 || s0["title"] != "Step one" || s0["brief"] != nil {
			t.Fatalf("nearest section = %v, want Step one with no brief", s0)
		}
		if s1["id"] != c.setup || s1["title"] != "Setup" || s1["brief"] != "How to set the widget up" {
			t.Fatalf("enclosing section = %v", s1)
		}
		doc := got["document"].(map[string]any)
		if doc["document_id"] != c.doc || doc["root_chunk_id"] != c.root || doc["title"] != "Widget guide" ||
			doc["type"] != "Guide" || doc["brief"] != "A guide to the widget" {
			t.Fatalf("document = %v", doc)
		}
		paths := got["paths"].([]any)
		if len(paths) != 1 || paths[0].(map[string]any)["path"] != "/kb/widgets/guide" {
			t.Fatalf("paths = %v", paths)
		}
		want := []string{"/kb/widgets=Everything about widgets", "/kb=The whole knowledge base of the team", "/="}
		if f := folderPaths(t, paths[0]); strings.Join(f, "|") != strings.Join(want, "|") {
			t.Fatalf("folders = %v, want %v", f, want)
		}
		if _, ok := got["sections_truncated"]; ok {
			t.Fatalf("a shallow chunk reported truncated sections: %v", got)
		}
	})
}

func TestDocumentLineage_RootChunkHasNoSections(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	got := lineageOf(t, d, ctx, c.root)
	if secs := got["sections"].([]any); len(secs) != 0 {
		t.Fatalf("root chunk sections = %v, want none", secs)
	}
	if doc := got["document"].(map[string]any); doc["document_id"] != c.doc {
		t.Fatalf("document = %v", doc)
	}
	if f := folderPaths(t, got["paths"].([]any)[0]); strings.Join(f, "|") != "/kb/widgets=|/kb=|/=" {
		t.Fatalf("folders without annotations = %v", f)
	}
}

// A chunk the caller may not read, and one that does not exist, answer as
// get_chunk answers them: lineage reveals nothing get_chunk does not.
func TestDocumentLineage_UnreadableOrMissingChunkAnswersAsGetChunk(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedAnnCorpus(t, d, annOperator(base))
	member := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb/widgets/guide"}}))
	for _, id := range []string{c.step1, "0000000000000000000000000000dead"} {
		_, get := docExec(t, d, member, `{"op":"get_chunk","scope":"tenant","id":"`+id+`"}`)
		_, lin := docExec(t, d, member, `{"op":"lineage","scope":"tenant","id":"`+id+`"}`)
		if !get.IsError || !lin.IsError {
			t.Fatalf("%s: get_chunk error=%v, lineage error=%v; want both refused", id, get.IsError, lin.IsError)
		}
		if want := strings.ReplaceAll(get.Text, "get_chunk", "lineage"); lin.Text != want {
			t.Fatalf("%s: lineage answered\n%s\nget_chunk answered\n%s", id, lin.Text, get.Text)
		}
		if strings.Contains(lin.Text, "Widget guide") || strings.Contains(lin.Text, c.doc) {
			t.Fatalf("the refusal names the document: %s", lin.Text)
		}
	}
}

// A folder whose annotation covers a document the caller may not read keeps its
// entry and loses its brief; the caller's own document keeps its brief.
func TestDocumentLineage_FolderBriefWithheldUnlessEveryCoveredDocumentIsReadable(t *testing.T) {
	d, base, _ := documentFixture(t)
	op := annOperator(base)
	c := seedFolderCorpus(t, d, op)
	folderWrite(t, d, op, "/kb/products", "Product guides and pricing")
	root, _ := docExec(t, d, op, `{"op":"get_document","scope":"tenant","id":"`+c.widgets+`"}`)
	rootID := root["root_chunk_id"].(string)

	if f := folderPaths(t, lineageOf(t, d, op, rootID)["paths"].([]any)[0]); f[0] != "/kb/products=Product guides and pricing" {
		t.Fatalf("operator folders = %v, want the brief", f)
	}
	member := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb/products/pricing"}}))
	got := lineageOf(t, d, member, rootID)
	if f := folderPaths(t, got["paths"].([]any)[0]); strings.Join(f, "|") != "/kb/products=|/kb=|/=" {
		t.Fatalf("member folders = %v, want /kb/products listed without its brief", f)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "Product guides") {
		t.Fatalf("the withheld brief leaked: %s", raw)
	}
	if doc := got["document"].(map[string]any); doc["brief"] != "About the Widget guide" {
		t.Fatalf("member's own document brief = %v", doc["brief"])
	}
}

// A folder Path ls would not list is not in the chain.
func TestDocumentLineage_FolderTheCallerCannotListIsLeftOut(t *testing.T) {
	d, base, _ := documentFixture(t)
	c := seedAnnCorpus(t, d, annOperator(base))
	member := tenantDocCtx(base, aclSet(t, authz.Rule{Effect: authz.Deny, Actions: []string{"read"},
		Resources: []string{"document:tenant:/kb"}}))
	got := lineageOf(t, d, member, c.step1)
	if f := folderPaths(t, got["paths"].([]any)[0]); strings.Join(f, "|") != "/kb/widgets=|/=" {
		t.Fatalf("member folders = %v, want /kb left out", f)
	}
}

// Every Path name gets its own chain, in sort order; past the bound the rest
// are cut and the answer says so.
func TestDocumentLineage_EachPathNameHasItsChainUpToTheBound(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	if _, r := docExec(t, d, ctx, `{"op":"set_path","scope":"tenant","id":"`+c.doc+`","path":"/archive/guide"}`); r.IsError {
		t.Fatal(r.Text)
	}
	got := lineageOf(t, d, ctx, c.notes)
	paths := got["paths"].([]any)
	if len(paths) != 2 {
		t.Fatalf("paths = %v, want both names", paths)
	}
	if p := paths[0].(map[string]any)["path"]; p != "/archive/guide" {
		t.Fatalf("first path = %v, want /archive/guide (sort order)", p)
	}
	if f := folderPaths(t, paths[0]); strings.Join(f, "|") != "/archive=|/=" {
		t.Fatalf("/archive/guide folders = %v", f)
	}
	if f := folderPaths(t, paths[1]); strings.Join(f, "|") != "/kb/widgets=|/kb=|/=" {
		t.Fatalf("/kb/widgets/guide folders = %v", f)
	}
	if _, ok := got["paths_truncated"]; ok {
		t.Fatal("two names reported truncated")
	}
	for i := 0; i < maxLineagePaths; i++ {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"set_path","scope":"tenant","id":%q,"path":"/more/name%d"}`, c.doc, i)); r.IsError {
			t.Fatal(r.Text)
		}
	}
	got = lineageOf(t, d, ctx, c.notes)
	if n := len(got["paths"].([]any)); n != maxLineagePaths || got["paths_truncated"] != true {
		t.Fatalf("paths = %d truncated=%v, want %d and truncated", n, got["paths_truncated"], maxLineagePaths)
	}
}

// A chunk nested deeper than the bound answers with the nearest sections, says
// they were cut, and still names its document.
func TestDocumentLineage_SectionDepthIsBounded(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	doc, leaf := deepDocument(t, d, ctx, maxLineageSections+2)
	got := lineageOf(t, d, ctx, leaf)
	secs := got["sections"].([]any)
	if len(secs) != maxLineageSections || got["sections_truncated"] != true {
		t.Fatalf("sections = %d truncated=%v, want %d and truncated", len(secs), got["sections_truncated"], maxLineageSections)
	}
	if first := secs[0].(map[string]any); first["id"] != leaf {
		t.Fatalf("nearest section = %v, want the chunk itself", first)
	}
	if got["document"].(map[string]any)["document_id"] != doc {
		t.Fatalf("document = %v", got["document"])
	}
}

// deepDocument makes a document whose chunks nest n deep below the root and
// returns it and the deepest chunk.
func deepDocument(t *testing.T, d *Document, ctx context.Context, n int) (doc, leaf string) {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Deep","path":"/deep/doc"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	doc = out["document_id"].(string)
	for i := 0; i < n; i++ {
		p := ""
		if leaf != "" {
			p = fmt.Sprintf(`,"parent_id":%q`, leaf)
		}
		o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q%s,"title":"Level %d"}`, doc, p, i))
		if r.IsError {
			t.Fatal(r.Text)
		}
		leaf = o["id"].(string)
	}
	return doc, leaf
}

// Exactly the bound is the whole chain, not a cut one.
func TestDocumentLineage_ExactlyTheBoundIsNotTruncated(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	_, leaf := deepDocument(t, d, ctx, maxLineageSections)
	got := lineageOf(t, d, ctx, leaf)
	if n := len(got["sections"].([]any)); n != maxLineageSections {
		t.Fatalf("sections = %d, want %d", n, maxLineageSections)
	}
	if _, ok := got["sections_truncated"]; ok {
		t.Fatalf("a chain of exactly %d sections reported truncated", maxLineageSections)
	}
}

// A section and a document whose text changed since their briefs were written
// carry stale; a folder never does, because judging it means rebuilding its
// inputs from its whole subtree on every step.
func TestDocumentLineage_StaleMarksSectionsAndDocumentNotFolders(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	annWrite(t, d, ctx, `"id":"`+c.setup+`"`, "How to set the widget up")
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget")
	folderWrite(t, d, ctx, "/kb/widgets", "Everything about widgets")
	// The document's brief rewritten after the folder's: the folder is stale by
	// get_annotation's measure.
	annWrite(t, d, ctx, `"document_id":"`+c.doc+`"`, "A guide to the widget, again")
	if got := annStale(t, d, ctx, `"level":"path","path":"/kb/widgets"`); !got["path"] {
		t.Fatalf("fixture: the folder is not stale by get_annotation (%v)", got)
	}
	// Step one's text changed after Setup's brief was written, and Notes (which
	// has no brief of its own, so the document's is written from its text) after
	// the document's.
	for _, id := range []string{c.step1, c.notes} {
		if _, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"update_chunk","scope":"tenant","id":%q,"body":"Changed text.","revision":1}`, id)); r.IsError {
			t.Fatalf("update_chunk: %s", r.Text)
		}
	}
	got := lineageOf(t, d, ctx, c.step1)
	setup := got["sections"].([]any)[1].(map[string]any)
	if setup["id"] != c.setup || setup["stale"] != true {
		t.Fatalf("Setup = %v, want stale after Step one changed", setup)
	}
	if doc := got["document"].(map[string]any); doc["brief"] != "A guide to the widget, again" || doc["stale"] != true {
		t.Fatalf("document = %v, want its brief marked stale", doc)
	}
	folder := got["paths"].([]any)[0].(map[string]any)["folders"].([]any)[0].(map[string]any)
	if folder["path"] != "/kb/widgets" || folder["brief"] != "Everything about widgets" {
		t.Fatalf("folder = %v", folder)
	}
	if _, ok := folder["stale"]; ok {
		t.Fatalf("folder entry carries stale: %v", folder)
	}
}

// A parent row in another document (a broken tree) ends the walk: the other
// document's chunks are never reported as sections of this one, and the answer
// says the chain is incomplete.
func TestDocumentLineage_ParentInAnotherDocumentStopsTheWalk(t *testing.T) {
	d, base, _ := documentFixture(t)
	ctx := annOperator(base)
	c := seedAnnCorpus(t, d, ctx)
	other, r := docExec(t, d, ctx, `{"op":"create_document","scope":"tenant","title":"Other","path":"/kb/other"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"tenant","document_id":%q,"title":"Foreign"}`, other["document_id"]))
	if r.IsError {
		t.Fatal(r.Text)
	}
	foreign := o["id"].(string)
	key, _, err := d.resolveScope(ctx, "tenant")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.exec(ctx, key, `UPDATE chunks SET parent_id = ? WHERE id = ?`, foreign, c.setup); err != nil {
		t.Fatal(err)
	}
	got := lineageOf(t, d, ctx, c.step1)
	secs := got["sections"].([]any)
	if len(secs) != 2 || secs[1].(map[string]any)["id"] != c.setup {
		t.Fatalf("sections = %v, want Step one and Setup only", secs)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), foreign) {
		t.Fatalf("the other document's chunk is in the lineage: %s", raw)
	}
	if got["sections_truncated"] != true {
		t.Fatalf("a walk cut by a broken parent must say the chain is incomplete: %v", got)
	}
	if got["document"].(map[string]any)["document_id"] != c.doc {
		t.Fatalf("document = %v", got["document"])
	}
}

// In the user scope the access questions carry the scope's owner; a folder the
// caller's rules deny is left out and the rest of the chain is kept.
func TestDocumentLineage_UserScope(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Notes","path":"/a/b/notes"}`)
	if r.IsError {
		t.Fatal(r.Text)
	}
	o, r := docExec(t, d, ctx, fmt.Sprintf(`{"op":"create_chunk","scope":"user","document_id":%q,"title":"Monday"}`, out["document_id"]))
	if r.IsError {
		t.Fatal(r.Text)
	}
	lin := func(ctx context.Context) map[string]any {
		got, r := docExec(t, d, ctx, `{"op":"lineage","scope":"user","id":"`+o["id"].(string)+`"}`)
		if r.IsError {
			t.Fatalf("lineage: %s", r.Text)
		}
		return got
	}
	if f := folderPaths(t, lin(ctx)["paths"].([]any)[0]); strings.Join(f, "|") != "/a/b=|/a=|/=" {
		t.Fatalf("user-scope folders = %v", f)
	}
	got := lin(withACL(ctx, aclSet(t, denyRule("read", "document:user:/a"))))
	if f := folderPaths(t, got["paths"].([]any)[0]); strings.Join(f, "|") != "/a/b=|/=" {
		t.Fatalf("user-scope folders under a deny on /a = %v", f)
	}
	if got["document"].(map[string]any)["title"] != "Notes" {
		t.Fatalf("document = %v", got["document"])
	}
}
