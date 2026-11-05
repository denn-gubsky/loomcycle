package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/api/memberpath"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The off-run halves of the document read-deny doors (security re-audit
// 2026-11-04, B7e / D15; RFC FY Part G).

// documentSet allows reading and writing every document, then applies deny.
func documentSet(t *testing.T, subject string, deny authz.Rule) *authz.Effective {
	t.Helper()
	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: "A", ID: subject},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"document:*", "memory:*", "sqlmem:*"}},
			{Effect: authz.Allow, Actions: []string{"delete"}, Resources: []string{"document:*"}},
			deny,
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

func deleteMemoryKey(t *testing.T, s *Server, p auth.Principal, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("DELETE /v1/_memory/scopes/{scope}/{scope_id}/keys/{key...}", s.handleDeleteMemoryEntry)
	req := httptest.NewRequest(http.MethodDelete, path, nil).WithContext(auth.WithPrincipal(context.Background(), p))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestMemoryDocRowDelete_OffRunRouteAsksTheDocumentACL: DELETE on a memory key
// asks what the Memory tool's delete asks. A fact homed in a document the
// caller may not delete is refused; one in a document it may not read is
// answered as a missing key is (204), and stays.
func TestMemoryDocRowDelete_OffRunRouteAsksTheDocumentACL(t *testing.T) {
	c := newACLSearchCorpus(t)
	path := "/v1/_memory/scopes/user/alice/keys/doc.chunk:" + c.fact
	stays := func(what string) {
		t.Helper()
		if _, err := c.s.store.MemoryGet(context.Background(), "A", store.MemoryScopeUser, "alice", "doc.chunk:"+c.fact); err != nil {
			t.Errorf("%s: the fact body should still be there: %v", what, err)
		}
	}
	// Each case starts from the fact body in place, so a case cannot pass on a
	// row an earlier one removed.
	reseed := func() {
		t.Helper()
		v, _ := json.Marshal("supplier audit fact from the report")
		if err := c.s.store.MemorySetProvenance(context.Background(), "A", store.MemoryScopeUser, "alice", "doc.chunk:"+c.fact, v, 0,
			store.MemoryProvenance{Origin: "consolidator", Class: "decision"}); err != nil {
			t.Fatal(err)
		}
	}
	for name, scopes := range map[string][]string{"member": {auth.ScopeRunsCreate}, "tenant operator": {auth.ScopeTenant}} {
		reseed()
		noDelete := auth.Principal{TenantID: "A", Subject: "bob", Scopes: scopes,
			Effective: documentSet(t, "bob", authz.Rule{Effect: authz.Deny, Actions: []string{"delete"}, Resources: []string{"document:user:/qg/reports/**"}})}
		if rec := deleteMemoryKey(t, c.s, noDelete, path); rec.Code != http.StatusForbidden {
			t.Errorf("%s denied delete: %d %s, want 403", name, rec.Code, rec.Body.String())
		}
		stays(name + " denied delete")

		reseed()
		noRead := auth.Principal{TenantID: "A", Subject: "bob", Scopes: scopes,
			Effective: documentSet(t, "bob", authz.Rule{Effect: authz.Deny, Actions: []string{"read"}, Resources: []string{"document:user:/qg/reports/**"}})}
		missing := deleteMemoryKey(t, c.s, noRead, "/v1/_memory/scopes/user/alice/keys/doc.chunk:nope")
		if rec := deleteMemoryKey(t, c.s, noRead, path); rec.Code != missing.Code || rec.Body.String() != missing.Body.String() {
			t.Errorf("%s denied read: %d %s, want the missing key's answer %d %s", name, rec.Code, rec.Body.String(), missing.Code, missing.Body.String())
		}
		stays(name + " denied read")
	}
	reseed()
	admin := auth.Principal{TenantID: "A", Subject: "op", Scopes: []string{auth.ScopeAdmin}}
	if rec := deleteMemoryKey(t, c.s, admin, path); rec.Code != http.StatusNoContent {
		t.Errorf("an admin with no set removes the fact body: %d %s", rec.Code, rec.Body.String())
	}
}

// documentMaintenanceRoutes rewrite a scope's index, map or units, or spend the
// operator's embedder or model per row.
var documentMaintenanceRoutes = []string{
	"/v1/_document/reindex",
	"/v1/_document/derive_units",
	"/v1/_document/describe_images",
	"/v1/_document/project?refit=true&dry_run=false",
	"/v1/_document/project",
}

// TestMaintenanceRoutes_AreNotOnTheMemberPath: a tenant member reaches none of
// the document maintenance routes; the three that only rewrite or spend are
// carved out of the member path by name, so re-gating /v1/_document/* to the
// tenant scope later cannot open them; and the projection handler refuses a
// refit to anyone below the operator plane, which is what keeps a refit an
// operator's if the projection itself is ever opened to members.
func TestMaintenanceRoutes_AreNotOnTheMemberPath(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	seedToken(t, st, "lct_member", "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, time.Time{})
	for _, path := range documentMaintenanceRoutes {
		if reached, code := runMW(t, s, http.MethodPost, path, "lct_member"); reached || code != http.StatusForbidden {
			t.Errorf("member POST %s: code=%d reached=%v, want 403 and not reached", path, code, reached)
		}
	}
	for _, path := range documentMaintenanceRoutes[:3] {
		if !memberpath.CarvedOut(http.MethodPost, path) {
			t.Errorf("CarvedOut(POST %s) = false, want the operator's", path)
		}
	}
	// The projection without a refit is what a member may run if the route is
	// opened (decision D7): it is not carved out by name.
	if memberpath.CarvedOut(http.MethodPost, "/v1/_document/project") {
		t.Error("CarvedOut(POST /v1/_document/project) = true; only a refit is the operator's")
	}

	// The handler's own refit check, asked directly (the route gate is above).
	srv, _, _ := projectFixture(t, true)
	member := auth.Principal{TenantID: "", Subject: "alice", Scopes: []string{auth.ScopeRunsCreate}}
	operator := auth.Principal{TenantID: "", Subject: "op", Scopes: []string{auth.ScopeTenant}}
	for _, c := range []struct {
		who   auth.Principal
		query string
		want  bool // refused with 403
	}{
		{member, "&refit=true", true},
		{member, "&refit=1&dry_run=false", true},
		{member, "", false},
		{member, "&refit=false", false},
		{operator, "&refit=true", false},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/_document/project?scope=user"+c.query, nil).
			WithContext(auth.WithPrincipal(context.Background(), c.who))
		srv.handleDocumentProject(rec, req)
		if got := rec.Code == http.StatusForbidden; got != c.want {
			t.Errorf("%s project%s: %d %s, want refused=%v", c.who.Subject, c.query, rec.Code, rec.Body.String(), c.want)
		}
	}
}
