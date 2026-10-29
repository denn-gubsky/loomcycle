package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	meminject "github.com/denn-gubsky/loomcycle/internal/memory"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// legacyPrincipalCtx is a request authenticated by LOOMCYCLE_AUTH_TOKEN, exactly as the
// auth middleware stamps it.
func legacyPrincipalCtx() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true,
	})
}

func (s *Server) ontologyAs(t *testing.T, ctx context.Context, method, body string) ontologyResponse {
	t.Helper()
	req := httptest.NewRequest(method, "/v1/_ontology", strings.NewReader(body)).WithContext(ctx)
	rec := httptest.NewRecorder()
	if method == http.MethodGet {
		s.handleOntology(rec, req)
	} else {
		s.handleOntologySetStatus(rec, req)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("%s /v1/_ontology = %d: %s", method, rec.Code, rec.Body.String())
	}
	var got ontologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestOntology_LegacyTokenPageEditorAndRunsShareOneOntology crosses the seam the bug sat
// on.
//
// A LOOMCYCLE_AUTH_TOKEN request is the principal tenant "default", and so is every run
// it starts and every off-run Document write (the Web UI document browser). The
// ontology page resolved an unfocused admin to "" instead. SQL Memory maps both to the
// same database, so the page saw the classes' TITLES — but chunk bodies and Path names
// are keyed by the raw tenant, so it read every class with no fields, and runs, looking
// up /memory/ontology under "default", provisioned and read a second, draft document.
// An operator's edits and confirm never reached a run.
func TestOntology_LegacyTokenPageEditorAndRunsShareOneOntology(t *testing.T) {
	s, _ := ontologyFixture(t)
	legacy := legacyPrincipalCtx()

	page := s.ontologyAs(t, legacy, http.MethodGet, "")
	if page.DocumentID == "" {
		t.Fatal("the page did not provision the ontology")
	}

	// Author a class the way the Web UI document browser does: through the off-run
	// Document route, whose context is built from the same principal.
	req := httptest.NewRequest(http.MethodPost, "/v1/_document", nil).WithContext(legacy)
	editorCtx := withTenantDocGrants(s.substrateBrowseCtxFn(req)(legacy))
	doc := &builtin.Document{Store: s.store, SqlMem: s.sqlMem}
	in, _ := json.Marshal(map[string]any{"op": "create_chunk", "scope": "tenant",
		"document_id": page.DocumentID, "parent_id": page.RootChunkID,
		"title": "service", "body": "- `owner` — who runs it\n"})
	if res, err := doc.Execute(editorCtx, in); err != nil || res.IsError {
		t.Fatalf("create_chunk through the editor's context: %v %s", err, res.Text)
	}

	confirmed := s.ontologyAs(t, legacy, http.MethodPost, `{"status":"confirmed"}`)
	var service *meminject.OntologyTerm
	for i := range confirmed.Terms {
		if confirmed.Terms[i].Name == "service" {
			service = &confirmed.Terms[i]
		}
	}
	if service == nil {
		t.Fatalf("the page does not show the class the editor wrote: %v", termNames(confirmed.Terms))
	}
	if strings.Join(service.Fields, ",") != "owner" {
		t.Errorf("the page reads the editor's class with fields %v, want [owner] — its body is keyed under another tenant", service.Fields)
	}

	// And a run started with the same token is told about it: the run's tenant is what
	// applyPrincipal stamps, and the prompt is rendered for that tenant.
	runTenant, _ := s.applyPrincipal(legacy, "", "")
	prompt := s.renderOntology(context.Background(), memInject{Tenant: runTenant})
	if !strings.Contains(prompt, "**service** — owner") {
		t.Errorf("a legacy-token run does not get the confirmed class (run tenant %q):\n%s", runTenant, prompt)
	}
}

// TestOntology_ExplicitTenantFocusStillWins: an admin naming a tenant reads that
// tenant's ontology, not its own.
func TestOntology_ExplicitTenantFocusStillWins(t *testing.T) {
	s, _ := ontologyFixture(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/_ontology?tenant=acme", nil).WithContext(legacyPrincipalCtx())
	rec := httptest.NewRecorder()
	s.handleOntology(rec, req)
	var got ontologyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tenant != "acme" {
		t.Errorf("tenant = %q, want the focused acme", got.Tenant)
	}
}
