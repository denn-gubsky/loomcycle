package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	loomapi "github.com/denn-gubsky/loomcycle/api"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC GQ P3.4 — reading ontology releases over HTTP.

// releaseRoutePaths are the three release reads, as a request names them.
var releaseRoutePaths = []string{"/v1/_ontology/releases", "/v1/_ontology/releases/1", "/v1/_ontology/releases/diff?from=1"}

// getRelease drives a release read as the principal on ctx, through a mux holding the
// three patterns the server registers (so {seq} and diff resolve as they do there).
func (s *Server) getRelease(t *testing.T, ctx context.Context, target string) (int, map[string]any) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/_ontology/releases", s.handleOntologyReleases)
	mux.HandleFunc("GET /v1/_ontology/releases/diff", s.handleOntologyReleaseDiff)
	mux.HandleFunc("GET /v1/_ontology/releases/{seq}", s.handleOntologyRelease)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil).WithContext(ctx))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// takeTestRelease takes a release of mi's tenant ontology.
func (s *Server) takeTestRelease(t *testing.T, mi memInject, reason string) builtin.OntologyRelease {
	t.Helper()
	rel, err := s.ontologyDocument().TakeOntologyRelease(s.ontologyDocCtx(context.Background(), mi), reason, nil)
	if err != nil {
		t.Fatalf("take a %s release: %v", reason, err)
	}
	return rel
}

// TestOntologyReleasesRoute_MemberIsRefused — releases keep text an edit or an erasure
// removed from the ontology, so they are the operator's: the gate refuses a member on
// each of the three routes and admits the tenant operator, each asks the
// access-control question read document, and the connector refuses a member, an
// isolated user and a run-plane operator itself (the check the gRPC and MCP twins will
// reach past the route's).
func TestOntologyReleasesRoute_MemberIsRefused(t *testing.T) {
	gate, st := tokenAuthServer(t, "legacy")
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	seedToken(t, st, "lct_member", "acme", "bob", memberScopes, time.Time{})
	seedToken(t, st, "lct_operator", "acme", "ops", []string{auth.ScopeTenant}, time.Time{})
	for _, path := range releaseRoutePaths {
		if reached, code := runMW(t, gate, http.MethodGet, path, "lct_member"); reached || code != http.StatusForbidden {
			t.Errorf("a member on GET %s = %d reached=%v, want 403", path, code, reached)
		}
		if reached, code := runMW(t, gate, http.MethodGet, path, "lct_operator"); !reached || code == http.StatusForbidden {
			t.Errorf("the tenant operator on GET %s = %d reached=%v, want admitted", path, code, reached)
		}
		bare, _, _ := strings.Cut(path, "?")
		if sf, ok := authz.HTTPSurface(http.MethodGet, bare); !ok || sf.Action != "read" || sf.Type != "document" {
			t.Errorf("GET %s asks %+v (governed %v), want read document", bare, sf, ok)
		}
	}

	s, mi := ontologyFixture(t)
	installPlanOntology(t, s, mi)
	s.takeTestRelease(t, mi, builtin.OntologyReleaseConfirm)
	for what, p := range map[string]auth.Principal{
		"a member":             {TenantID: mi.Tenant, Subject: "m", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		"an isolated user":     {TenantID: mi.Tenant, Subject: "u", Scopes: []string{auth.ScopeUser}},
		"a run-plane operator": {TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"judge"}}},
	} {
		for _, path := range releaseRoutePaths {
			code, out := s.getRelease(t, auth.WithPrincipal(context.Background(), p), path)
			if code != http.StatusForbidden || out["code"] != "forbidden" {
				t.Errorf("%s on %s: %d %v, want 403 forbidden", what, path, code, out)
			}
		}
	}
}

// TestOntologyReleasesRoute_TenantOperatorSeesOwnTenantOnly — a tenant operator naming
// another tenant lists, reads and diffs its own tenant's releases: the other tenant's
// second release is not found in its own, and an admin focusing the other tenant sees
// it.
func TestOntologyReleasesRoute_TenantOperatorSeesOwnTenantOnly(t *testing.T) {
	s, mi := ontologyFixture(t)
	installPlanOntology(t, s, mi)
	other := memInject{Tenant: "globex", UserID: "gina", AgentName: "curator"}
	installPlanOntology(t, s, other)
	own := s.takeTestRelease(t, mi, builtin.OntologyReleaseConfirm)
	s.takeTestRelease(t, other, builtin.OntologyReleaseConfirm)
	foreign := s.takeTestRelease(t, other, builtin.OntologyReleaseCatchUp)
	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}})
	q := "?tenant=" + other.Tenant

	code, out := s.getRelease(t, op, "/v1/_ontology/releases"+q)
	rels, _ := out["releases"].([]any)
	if code != http.StatusOK || out["tenant"] != mi.Tenant || len(rels) != 1 ||
		rels[0].(map[string]any)["content_version"] != own.ContentVersion {
		t.Fatalf("list with %s = %d %v, want its own one release", q, code, out)
	}
	code, out = s.getRelease(t, op, "/v1/_ontology/releases/1"+q)
	if code != http.StatusOK || out["tenant"] != mi.Tenant || dig(out, "release", "reason") != builtin.OntologyReleaseConfirm ||
		!strings.Contains(fmt.Sprint(out["export_md"]), "Provision") {
		t.Errorf("get 1 with %s = %d %v, want its own confirm release", q, code, out)
	}
	for _, path := range []string{fmt.Sprintf("/v1/_ontology/releases/%d?", foreign.Seq), fmt.Sprintf("/v1/_ontology/releases/diff?from=1&to=%d&", foreign.Seq)} {
		if code, out := s.getRelease(t, op, path+"tenant="+other.Tenant); code != http.StatusNotFound || out["code"] != "release_not_found" {
			t.Errorf("%s = %d %v, want 404 release_not_found (the other tenant's release)", path, code, out)
		}
	}
	code, out = s.getRelease(t, op, "/v1/_ontology/releases/diff?from=1&to=current&tenant="+other.Tenant)
	if changes, _ := out["changes"].([]any); code != http.StatusOK || out["tenant"] != mi.Tenant || len(changes) != 0 ||
		dig(out, "to", "current") != true {
		t.Errorf("diff 1..current = %d %v, want its own, unchanged", code, out)
	}

	// An admin's focus reaches the other tenant.
	code, out = s.getRelease(t, context.Background(), fmt.Sprintf("/v1/_ontology/releases/%d%s", foreign.Seq, q))
	if code != http.StatusOK || out["tenant"] != other.Tenant || dig(out, "release", "reason") != builtin.OntologyReleaseCatchUp {
		t.Errorf("an admin's get of the other tenant's release = %d %v", code, out)
	}
}

// TestOntologyReleasesRoute_RefusalsCarryTheirCodes — a seq or a from that is not a
// positive integer is 400 invalid_request; a release that does not exist is 404.
func TestOntologyReleasesRoute_RefusalsCarryTheirCodes(t *testing.T) {
	s, mi := ontologyFixture(t)
	installPlanOntology(t, s, mi)
	s.takeTestRelease(t, mi, builtin.OntologyReleaseConfirm)
	for path, want := range map[string]struct {
		status int
		code   string
	}{
		"/v1/_ontology/releases/0":                    {http.StatusBadRequest, "invalid_request"},
		"/v1/_ontology/releases/x":                    {http.StatusBadRequest, "invalid_request"},
		"/v1/_ontology/releases/diff":                 {http.StatusBadRequest, "invalid_request"},
		"/v1/_ontology/releases/diff?from=1&to=later": {http.StatusBadRequest, "invalid_request"},
		"/v1/_ontology/releases/2":                    {http.StatusNotFound, "release_not_found"},
		"/v1/_ontology/releases/diff?from=2":          {http.StatusNotFound, "release_not_found"},
	} {
		if code, out := s.getRelease(t, context.Background(), path); code != want.status || out["code"] != want.code {
			t.Errorf("%s = %d %v, want %d %s", path, code, out, want.status, want.code)
		}
	}
}

// TestOpenAPISpec_PublishesTheOntologyReleaseRoutes — the three reads are in the
// published contract, with the diff's change kinds as the server writes them.
func TestOpenAPISpec_PublishesTheOntologyReleaseRoutes(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(loomapi.OpenAPISpecYAML, &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v1/_ontology/releases", "/v1/_ontology/releases/{seq}", "/v1/_ontology/releases/diff"} {
		if dig(doc, "paths", p, "get") == nil {
			t.Errorf("api/openapi.yaml has no GET %s", p)
		}
	}
	items := dig(doc, "paths", "/v1/_ontology/releases/diff", "get", "responses", "200", "content", "application/json", "schema", "properties", "changes", "items", "properties")
	if got := fmt.Sprint(dig(items, "change", "enum")); got != "[added removed changed erased]" {
		t.Errorf("change enum = %s", got)
	}
	if got := fmt.Sprint(dig(items, "changed", "items", "enum")); got != "[renamed moved kind status meaning directives]" {
		t.Errorf("changed enum = %s", got)
	}
}
