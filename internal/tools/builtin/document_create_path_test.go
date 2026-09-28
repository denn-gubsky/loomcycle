package builtin

import (
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// docCount lists the scope's documents, so "nothing was created" is checked
// against the store rather than taken from the refusal.
func docCount(t *testing.T, d *Document, ctx contextT) int {
	t.Helper()
	out, r := docExec(t, d, ctx, `{"op":"query_documents","scope":"user"}`)
	if r.IsError {
		t.Fatalf("query_documents: %s", r.Text)
	}
	docs, _ := out["documents"].([]any)
	return len(docs)
}

// Measured on two local models: the title passed as a path segment
// ("/documents/zz-eval Trip plan"). The document was created, the name
// silently failed (path_warning on a success), and each model then created a
// second document. A malformed path is now refused before anything exists.
func TestCreateDocument_AMalformedPathIsRefusedAndNothingIsCreated(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"zz-eval Trip plan","path":"/documents/zz-eval Trip plan"}`)
	if !r.IsError {
		t.Fatalf("a path with spaces was accepted: %s", r.Text)
	}
	if r.Error == nil || r.Error.Category != tools.CategoryValidation {
		t.Errorf("want a validation refusal, got %+v", r.Error)
	}
	if !strings.Contains(r.Text, "Nothing was created") {
		t.Errorf("the refusal does not say nothing was created: %s", r.Text)
	}
	if !strings.Contains(r.Error.Description, `"/documents/zz-eval-Trip-plan"`) {
		t.Errorf("the next step does not offer the corrected path: %q", r.Error.Description)
	}
	if n := docCount(t, d, ctx); n != 0 {
		t.Errorf("%d document(s) left behind by a refused create", n)
	}
}

// A valid path is unchanged, and so is the default one.
func TestCreateDocument_AValidPathStillNamesTheDocument(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	out, r := docExec(t, d, ctx, `{"op":"create_document","scope":"user","title":"Trip plan","path":"/documents/zz-eval-Trip-plan"}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	if out["path"] != "/documents/zz-eval-Trip-plan" || out["path_warning"] != nil {
		t.Errorf("path = %v, path_warning = %v", out["path"], out["path_warning"])
	}
	// docCount must be able to see a document, or "nothing was created" above
	// would pass whatever the store held.
	if n := docCount(t, d, ctx); n != 1 {
		t.Errorf("docCount = %d after one create, want 1", n)
	}
}

// import_md passes its `path` to the same create, so a bad one writes nothing
// there either.
func TestImportMD_AMalformedPathWritesNothing(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	_, r := docExec(t, d, ctx, `{"op":"import_md","scope":"user","path":"/notes/my trip","markdown":"# Trip\n\n## Flights\n\nTBD."}`)
	if !r.IsError || !strings.Contains(r.Text, "invalid path segment") {
		t.Fatalf("want the path refusal, got %s", r.Text)
	}
	if n := docCount(t, d, ctx); n != 0 {
		t.Errorf("%d document(s) left behind by a refused import", n)
	}
}

func TestSuggestDocPath(t *testing.T) {
	for in, want := range map[string]string{
		"/documents/zz-eval Trip plan": "/documents/zz-eval-Trip-plan",
		"/my notes/plan (v2)":          "/my-notes/plan-v2",
		"/a//b ":                       "/a/b",
	} {
		if got := suggestDocPath(in); got != want {
			t.Errorf("suggestDocPath(%q) = %q, want %q", in, got, want)
		}
	}
}
