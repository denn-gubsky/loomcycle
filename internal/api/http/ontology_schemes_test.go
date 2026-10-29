package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

func schemesLayout(render bool) []ontologyTestChunk {
	scheme := ""
	if render {
		scheme = "@render\n"
	}
	return []ontologyTestChunk{
		{title: "Classes", body: "@section classes\n", kids: []ontologyTestChunk{{title: "incident", body: "- `summary` text\n"}}},
		{title: "Schemes", body: "@section schemes\n", kids: []ontologyTestChunk{
			{title: "topics", body: scheme, kids: []ontologyTestChunk{
				{title: "Risk Management", body: "@notation RM\n", kids: []ontologyTestChunk{{title: "outsourcing"}}},
			}},
			{title: "file-plan", kids: []ontologyTestChunk{{title: "compliance", body: "@retention 10y\n@acl compliance\n"}}},
		}},
	}
}

// TestOntologyAPI_ConfirmMirrorsConceptTagsAndReportsStaleness (RFC FO P1c-1).
//
// Confirming applies the mirror; a concept edited afterwards shows as stale on the read;
// confirming again — allowed on a confirmed ontology — re-applies it.
func TestOntologyAPI_ConfirmMirrorsConceptTagsAndReportsStaleness(t *testing.T) {
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, schemesLayout(false))

	draft := s.getOntology(t, mi.Tenant)
	if len(draft.Schemes) != 2 || len(draft.SchemeMirrorStale) != 3 {
		t.Fatalf("before confirm: schemes %d, stale %v", len(draft.Schemes), draft.SchemeMirrorStale)
	}
	got := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if got.SchemeMirror == nil || got.SchemeMirror.Added != 3 || len(got.SchemeMirrorStale) != 0 {
		t.Fatalf("confirm should mirror all three concepts: %#v stale %v", got.SchemeMirror, got.SchemeMirrorStale)
	}
	var plan meminject.OntologyConcept
	for _, sc := range got.Schemes {
		for _, c := range sc.Concepts {
			if c.Tag == "file-plan/compliance" {
				plan = c
			}
		}
	}
	if plan.Retention != "10y" || plan.ACL != "compliance" {
		t.Errorf("file-plan policy not reported: %#v", plan)
	}

	// Rename a concept in the document: stale until confirmed again.
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	dctx := s.ontologyDocCtx(context.Background(), mi)
	var outsourcing string
	for _, sc := range got.Schemes {
		for _, c := range sc.Concepts {
			if c.Name == "outsourcing" {
				outsourcing = c.ChunkID
			}
		}
	}
	g, _ := doc.Execute(dctx, ontologyJSON(map[string]any{"op": "get_chunk", "scope": "tenant", "id": outsourcing}))
	var chunk map[string]any
	_ = json.Unmarshal([]byte(g.Text), &chunk)
	if res, _ := doc.Execute(dctx, ontologyJSON(map[string]any{"op": "update_chunk", "scope": "tenant", "id": outsourcing,
		"title": "vendor outsourcing", "revision": chunk["revision"]})); res.IsError {
		t.Fatal(res.Text)
	}
	if stale := s.getOntology(t, mi.Tenant).SchemeMirrorStale; len(stale) != 1 || stale[0] != "topics/risk-management/vendor-outsourcing" {
		t.Errorf("a renamed concept should read as stale: %v", stale)
	}
	again := s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if len(again.SchemeMirrorStale) != 0 || again.SchemeMirror == nil || again.SchemeMirror.Removed != 1 {
		t.Errorf("re-confirming should re-apply: %#v stale %v", again.SchemeMirror, again.SchemeMirrorStale)
	}
}

// TestOntology_OnlyRenderMarkedSchemesReachThePrompt.
func TestOntology_OnlyRenderMarkedSchemesReachThePrompt(t *testing.T) {
	withoutSchemes := func() string {
		s, mi := ontologyFixture(t)
		replaceOntologyWithSections(t, s, mi, schemesLayout(false)[:1])
		s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
		return s.renderOntology(context.Background(), mi)
	}()
	s, mi := ontologyFixture(t)
	replaceOntologyWithSections(t, s, mi, schemesLayout(false))
	s.setOntologyStatus(t, mi.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	if got := s.renderOntology(context.Background(), mi); got != withoutSchemes {
		t.Errorf("an unrendered scheme changed the prompt:\n got %q\nwant %q", got, withoutSchemes)
	}
	s2, mi2 := ontologyFixture(t)
	replaceOntologyWithSections(t, s2, mi2, schemesLayout(true))
	s2.setOntologyStatus(t, mi2.Tenant, meminject.OntologyConfirmedStatus, http.StatusOK)
	got := s2.renderOntology(context.Background(), mi2)
	if !strings.HasPrefix(got, withoutSchemes) || !strings.Contains(got, "# Concept schemes") || !strings.Contains(got, "- Risk Management (RM)") {
		t.Errorf("a @render scheme should be listed after the unchanged blocks:\n%s", got)
	}
	if strings.Contains(got, "compliance") {
		t.Errorf("the unrendered file plan leaked into the prompt:\n%s", got)
	}
}

func ontologyJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
