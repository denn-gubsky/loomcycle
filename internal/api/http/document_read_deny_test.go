package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
