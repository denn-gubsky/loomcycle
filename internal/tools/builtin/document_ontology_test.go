package builtin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// ontologyDocFixture imports a nested ontology document and returns its path.
//
// import_md turns heading depth into chunk depth, which is exactly the authoring path
// an operator uses, so the tree under test is built the way a real one is rather than
// assembled row by row.
func ontologyDocFixture(t *testing.T, d *Document, ctx context.Context, md string) string {
	t.Helper()
	mj, _ := json.Marshal(md)
	out, r := docExec(t, d, ctx, `{"op":"import_md","scope":"user","markdown":`+string(mj)+`}`)
	if r.IsError {
		t.Fatalf("import_md: %s", r.Text)
	}
	id, _ := out["document_id"].(string)
	if id == "" {
		t.Fatalf("import_md returned no document_id: %v", out)
	}
	path := "/test/ontology-" + id
	pj, _ := json.Marshal(path)
	_, r = docExec(t, d, ctx,
		`{"op":"set_path","scope":"user","id":"`+id+`","path":`+string(pj)+`}`)
	if r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}
	return path
}

// termsByName indexes a term slice for assertion.
func termsByName(terms []ontologyTermForTest) map[string]ontologyTermForTest {
	m := map[string]ontologyTermForTest{}
	for _, tm := range terms {
		m[tm.Name] = tm
	}
	return m
}

// ontologyTermForTest mirrors the fields under assertion, keeping the test readable
// without importing the memory package's full term.
type ontologyTermForTest struct {
	Name   string
	Parent string
	Fields []string
}

func readTerms(t *testing.T, d *Document, ctx context.Context, path string) []ontologyTermForTest {
	t.Helper()
	got, err := d.OntologyTermsFromTree(ctx, "user", path)
	if err != nil {
		t.Fatalf("OntologyTermsFromTree: %v", err)
	}
	out := make([]ontologyTermForTest, 0, len(got))
	for _, g := range got {
		out = append(out, ontologyTermForTest{Name: g.Name, Parent: g.Parent, Fields: g.Fields})
	}
	return out
}

// TestOntologyTree_NestedChunkIsASubclass is the whole point of reading chunks.
//
// This FAILS on the pre-change reader, and not by a near miss: `ParseOntologyMarkdown`
// matched only "## ", so `incident` was recognised as neither a term nor a title and
// vanished with no error and nothing in the panel.
func TestOntologyTree_NestedChunkIsASubclass(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, `# Tenant Ontology

## event
- `+"`occurred_at`"+` — when it happened

### incident
- `+"`severity`"+` — how bad

## project
- `+"`status`"+`
`)

	byName := termsByName(readTerms(t, d, ctx, path))

	inc, ok := byName["incident"]
	if !ok {
		t.Fatalf("the nested type was dropped — have %v", byName)
	}
	if inc.Parent != "event" {
		t.Errorf("incident.Parent = %q, want %q", inc.Parent, "event")
	}
	if got := strings.Join(inc.Fields, ","); got != "severity" {
		t.Errorf("incident fields = %q, want severity", got)
	}
	// A root stays a root, and the DOCUMENT TITLE is not an entity: including it
	// would invent a type nobody declared and make everything its subclass.
	if ev := byName["event"]; ev.Parent != "" {
		t.Errorf("event.Parent = %q, want root", ev.Parent)
	}
	if _, bad := byName["Tenant Ontology"]; bad {
		t.Error("the document's root chunk was read as an entity")
	}
	if len(byName) != 3 {
		t.Errorf("want exactly event/incident/project, got %v", byName)
	}
}

// TestOntologyTree_HeadingInsideABodyIsAComment is the distinction that justifies
// reading the data model instead of the Markdown.
//
// Under a heading rule an operator documenting their type accidentally declares one.
// Under the chunk rule a heading inside a body is prose, and only a genuine child
// CHUNK is a subclass — a difference that is invisible in the exported Markdown and
// unambiguous in the tree.
func TestOntologyTree_HeadingInsideABodyIsAComment(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	// One chunk, whose body happens to contain a heading-looking line. Built with
	// create_chunk rather than import_md precisely because import_md would promote
	// that line to a chunk — the point is a body that carries one.
	out, r := docExec(t, d, ctx,
		`{"op":"create_document","scope":"user","title":"Tenant Ontology","body":""}`)
	if r.IsError {
		t.Fatalf("create_document: %s", r.Text)
	}
	docID, _ := out["document_id"].(string)
	root, _ := out["root_chunk_id"].(string)
	body := "- `status`\n\n### Notes on naming\nPrefer lowercase.\n"
	bj, _ := json.Marshal(body)
	_, r = docExec(t, d, ctx, `{"op":"create_chunk","scope":"user","document_id":"`+docID+
		`","parent_id":"`+root+`","title":"project","body":`+string(bj)+`}`)
	if r.IsError {
		t.Fatalf("create_chunk: %s", r.Text)
	}
	path := "/test/ontology-comment"
	_, r = docExec(t, d, ctx,
		`{"op":"set_path","scope":"user","id":"`+docID+`","path":"`+path+`"}`)
	if r.IsError {
		t.Fatalf("set_path: %s", r.Text)
	}

	byName := termsByName(readTerms(t, d, ctx, path))
	if _, bad := byName["Notes on naming"]; bad {
		t.Error("a heading inside a body was read as an entity — that is documentation")
	}
	p, ok := byName["project"]
	if !ok {
		t.Fatalf("project missing — have %v", byName)
	}
	if got := strings.Join(p.Fields, ","); got != "status" {
		t.Errorf("project fields = %q, want status", got)
	}
}

// TestOntologyTree_DepthBeyondTheCapIsFlattenedNotDropped.
//
// The cap bounds prompt growth, but a cap that DELETES an entity would reintroduce
// the exact silent loss this reader was written to end — one level further down,
// where it would be harder to notice.
func TestOntologyTree_DepthBeyondTheCapIsFlattenedNotDropped(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	path := ontologyDocFixture(t, d, ctx, `# Tenant Ontology

## l1

### l2

#### l3

##### l4

###### l5
`)

	byName := termsByName(readTerms(t, d, ctx, path))
	for _, want := range []string{"l1", "l2", "l3", "l4", "l5"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("%s was dropped by the depth cap — have %v", want, byName)
		}
	}
	// The chain is bounded at four levels: l4 is the deepest allowed, so l5 lands
	// BESIDE it rather than below it. The assertion is on the parent because that is
	// what the cap actually changes — a cap that only stalled a counter would leave
	// the reported tree unbounded while claiming otherwise.
	if got := byName["l4"].Parent; got != "l3" {
		t.Errorf("l4.Parent = %q, want l3", got)
	}
	if got := byName["l5"].Parent; got != "l3" {
		t.Errorf("l5.Parent = %q, want l3 (flattened to the cap, a sibling of l4)", got)
	}
}

// TestOntologyTree_AbsentDocumentIsNotAnError: no tenant layer, not a failure. The
// caller renders the base seed, and a 500 here would take down every run whose prompt
// mentions the ontology.
func TestOntologyTree_AbsentDocumentIsNotAnError(t *testing.T) {
	d, ctx, _ := documentFixture(t)
	terms, err := d.OntologyTermsFromTree(ctx, "user", "/test/nope")
	if err != nil {
		t.Fatalf("absent document should read as empty, got %v", err)
	}
	if len(terms) != 0 {
		t.Errorf("want no terms, got %v", terms)
	}
}
