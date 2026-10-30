package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// replaceOntologyWithSections swaps the provisioned template's types for a sectioned
// layout built chunk by chunk, the way an operator editing in the document view does.
// Each ontologyTestChunk becomes a chunk under its parent.
func replaceOntologyWithSections(t *testing.T, s *Server, mi memInject, roots []ontologyTestChunk) {
	t.Helper()
	s.getOntology(t, mi.Tenant) // provisions
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	exec := func(payload map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(payload)
		res, err := doc.Execute(dctx, b)
		if err != nil || res.IsError {
			t.Fatalf("%v: %v %s", payload["op"], err, res.Text)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(res.Text), &out)
		return out
	}
	got := exec(map[string]any{"op": "get_document", "scope": "tenant", "path": meminject.OntologyPath})
	docID, rootID := got["document_id"].(string), got["root_chunk_id"].(string)
	q := exec(map[string]any{"op": "query_chunks", "scope": "tenant",
		"sql": "SELECT id FROM chunks WHERE parent_id = '" + rootID + "'"})
	for _, row := range q["rows"].([]any) {
		exec(map[string]any{"op": "delete_chunk", "scope": "tenant", "id": row.([]any)[0]})
	}
	var create func(parent string, cs []ontologyTestChunk)
	create = func(parent string, cs []ontologyTestChunk) {
		for _, c := range cs {
			out := exec(map[string]any{"op": "create_chunk", "scope": "tenant", "document_id": docID,
				"parent_id": parent, "title": c.title, "body": c.body})
			create(out["id"].(string), c.kids)
		}
	}
	create(rootID, roots)
}

type ontologyTestChunk struct {
	title, body string
	kids        []ontologyTestChunk
}

// TestOntologyAPI_ReportsSectionsAndKeepsDocumentClassesOutOfThePrompt (RFC FO P1a).
//
// The panel read reports the sections, the document-class tree and the grammars; the
// run's prompt gets the chunk classes and never a document class or a section title.
func TestOntologyAPI_ReportsSectionsAndKeepsDocumentClassesOutOfThePrompt(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{
			{title: "Document", body: "- `title` text @required\n", kids: []ontologyTestChunk{
				{title: "Policy", body: "@identity\n", kids: []ontologyTestChunk{
					{title: "grammar:policy-ref", body: "pattern: Policy (?P<n>\\d+)\nresolve: work_key=policy:{n}\n"},
				}},
			}},
			{title: "incident", body: "- `summary` text\n"},
		}},
		{title: "Relations", body: "@section relations\n", kids: []ontologyTestChunk{{title: "cites"}}},
	})
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	got := s.getOntology(t, mi.Tenant)

	if strings.Join(got.Sections, ",") != "classes,relations" {
		t.Errorf("sections = %v", got.Sections)
	}
	if !hasTerm(got.DocumentClasses, "Document") || !hasTerm(got.DocumentClasses, "Policy") || hasTerm(got.DocumentClasses, "incident") {
		t.Errorf("document classes = %v", termNames(got.DocumentClasses))
	}
	if len(got.Grammars) != 1 || got.Grammars[0].Name != "policy-ref" {
		t.Errorf("grammars = %#v", got.Grammars)
	}
	if got.OntologyVersion < 2 {
		t.Errorf("ontology_version = %d, want the confirmed root's revision", got.OntologyVersion)
	}
	for _, bad := range []string{"Classes", "Relations", "cites", "grammar:policy-ref"} {
		if hasTerm(got.Terms, bad) {
			t.Errorf("%q was reported as a class", bad)
		}
	}

	prompt := s.renderOntology(context.Background(), mi)
	if !strings.Contains(prompt, "**incident**") {
		t.Errorf("the chunk class is missing from the prompt:\n%s", prompt)
	}
	for _, bad := range []string{"Document", "Policy", "Relations"} {
		if strings.Contains(prompt, bad) {
			t.Errorf("%q reached the prompt:\n%s", bad, prompt)
		}
	}
	// A relation kind the tenant declares is listed — in the relation block, never as an
	// entity type (RFC FO P1b, decision 10d.2).
	entityBlock, relationBlock, _ := strings.Cut(prompt, "# Relation kinds")
	if strings.Contains(entityBlock, "cites") || !strings.Contains(relationBlock, "**cites**") {
		t.Errorf("cites should appear only in the relation block:\n%s", prompt)
	}
}

// TestOntology_SectionsOnlyDocumentDoesNotFallBackToFlatParsing.
//
// A sectioned document with no classes yields no class from the tree read. That is a
// tree read, not a reason for the flat-Markdown fallback — which knows nothing of
// sections and would have turned "Relations" into an entity type in every prompt.
func TestOntology_SectionsOnlyDocumentDoesNotFallBackToFlatParsing(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Relations", body: "@section relations\n", kids: []ontologyTestChunk{{title: "cites"}}},
	})
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)

	terms, _, notes := s.tenantOntologyTerms(context.Background(), mi)
	if len(terms) != 0 {
		t.Errorf("a sections-only document produced classes %v", termNames(terms))
	}
	if len(notes) != 0 {
		t.Errorf("the flat fallback reported itself: %v", notes)
	}
	prompt, _ := s.applyMemoryInjection(context.Background(), config.AgentDef{SystemPrompt: "{{memory:ontology}}"}, mi)
	if strings.Contains(prompt.SystemPrompt, "Relations") {
		t.Errorf("a section title reached the prompt:\n%s", prompt.SystemPrompt)
	}
}

// TestOntologyAPI_FlatDocumentResponseHasNoSectionFields.
//
// The provisioned template has no @section: none of the RFC FO fields may appear, so
// a section-less tenant's panel response is unchanged.
func TestOntologyAPI_FlatDocumentResponseHasNoSectionFields(t *testing.T) {
	s, mi := ontologyFixture(t)
	req := getOntologyRaw(t, s, mi.Tenant)
	for _, key := range []string{"sections", "document_classes", "grammars", "undeclared_types", "fact_versions",
		"relations", "effective_relations", "adoptable_relations", "statement_versions", "undated_statements",
		"schemes", "scheme_mirror_stale", "scheme_mirror", "instances", "instance_adoption"} {
		if _, has := req[key]; has {
			t.Errorf("a flat ontology's response grew %q", key)
		}
	}
}

func getOntologyRaw(t *testing.T, s *Server, tenant string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/_ontology?tenant="+tenant, nil)
	rec := httptest.NewRecorder()
	s.handleOntology(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/_ontology = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestOntologyTemplate_ProvisionsOnlyItsWorkedExamplesAsClasses.
//
// import_md turns every heading into a chunk and a chunk under the root is a class, so a
// prose sub-heading in the template's introduction became a class named after the
// sentence — and, once an operator confirmed the template, an entity type in every
// extraction prompt. The template's classes are exactly its three worked examples.
func TestOntologyTemplate_ProvisionsOnlyItsWorkedExamplesAsClasses(t *testing.T) {
	s, mi := ontologyFixture(t)
	got := termNames(s.getOntology(t, mi.Tenant).Terms)
	if strings.Join(got, ",") != "project,incident,constraint" {
		t.Errorf("the provisioned template's classes = %v, want exactly project, incident, constraint", got)
	}
}

// TestOntologyAPI_EffectiveExcludesDocumentClasses: `effective` is what an agent is told
// about, so a tenant's document classes are reported apart and never listed there.
func TestOntologyAPI_EffectiveExcludesDocumentClasses(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{
			{title: "Document", body: "- `title` text\n", kids: []ontologyTestChunk{{title: "Policy"}}},
			{title: "incident"},
		}},
	})
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	got := s.getOntology(t, mi.Tenant)
	if hasTerm(got.Effective, "Document") || hasTerm(got.Effective, "Policy") {
		t.Errorf("effective lists document classes: %v", termNames(got.Effective))
	}
	if !hasTerm(got.Effective, "incident") || !hasTerm(got.DocumentClasses, "Policy") {
		t.Errorf("effective %v / document_classes %v", termNames(got.Effective), termNames(got.DocumentClasses))
	}
}
