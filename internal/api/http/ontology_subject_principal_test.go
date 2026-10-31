package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// addUser registers subject as a user of tenant with the given status.
func addUser(t *testing.T, s *Server, tenant, subject, status string) {
	t.Helper()
	if err := s.store.UserCreate(context.Background(), store.UserRow{TenantID: tenant, Subject: subject, AccessMode: "tenant", Status: status}); err != nil {
		t.Fatal(err)
	}
}

// postBind drives POST /v1/_ontology/subjects/principal as the principal on ctx.
func (s *Server) postBind(t *testing.T, ctx context.Context, tenant, subject, principal string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"subject": subject, "principal": principal})
	req := httptest.NewRequest(http.MethodPost, "/v1/_ontology/subjects/principal?tenant="+tenant, strings.NewReader(string(body))).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.handleOntologySubjectPrincipal(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// boundPrincipal lists the natural keys of the tenant subjects bound to subject.
func (s *Server) boundPrincipal(t *testing.T, mi memInject, subject string) []string {
	t.Helper()
	bound, err := s.ontologyDocument().TenantSubjectsBoundTo(s.ontologyDocCtx(context.Background(), mi), subject)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, b := range bound {
		out = append(out, b.NaturalKey)
	}
	return out
}

// TestOntologyAPI_BindSubjectPrincipalBindsAndClears (RFC FR §5, P4): the route
// binds a subject named by natural key to a user of the tenant, which an erasure of
// that user then finds, and clears it with an empty principal.
func TestOntologyAPI_BindSubjectPrincipalBindsAndClears(t *testing.T) {
	s, mi := ontologyFixture(t)
	dup, _, _ := seedMergeSubjects(t, s, mi)
	addUser(t, s, mi.Tenant, "u-ada", "active")
	ctx := context.Background()

	code, out := s.postBind(t, ctx, mi.Tenant, "person:ada-king", "u-ada")
	if code != http.StatusOK || out["principal"] != "u-ada" || out["changed"] != true || out["document_id"] != dup ||
		out["natural_key"] != "person:ada-king" || out["path"] != "/facts/ada-king" || out["tenant"] != mi.Tenant {
		t.Fatalf("bind = %d %v", code, out)
	}
	if got := s.boundPrincipal(t, mi, "u-ada"); len(got) != 1 || got[0] != "person:ada-king" {
		t.Errorf("subjects bound to u-ada = %v", got)
	}
	if code, out := s.postBind(t, ctx, mi.Tenant, dup, "u-ada"); code != http.StatusOK || out["changed"] != false {
		t.Errorf("binding again as bound = %d %v, want 200 unchanged", code, out)
	}
	code, out = s.postBind(t, ctx, mi.Tenant, dup, "")
	if code != http.StatusOK || out["principal"] != "" || out["previous"] != "u-ada" || out["changed"] != true {
		t.Fatalf("clear = %d %v", code, out)
	}
	if got := s.boundPrincipal(t, mi, "u-ada"); len(got) != 0 {
		t.Errorf("still bound after the clear: %v", got)
	}
}

// TestOntologyAPI_BindNeedsARegisteredUserOfTheTenant: the Document leaves "is this a
// user" to the transport, so the connector checks it before anything is written. A
// user of the tenant is a row in its users table — pending and disabled included —
// and nothing else: not a subject that only ran, not a user of another tenant.
func TestOntologyAPI_BindNeedsARegisteredUserOfTheTenant(t *testing.T) {
	s, mi := ontologyFixture(t)
	dup, surv, _ := seedMergeSubjects(t, s, mi)
	addUser(t, s, "globex", "u-other", "active")
	addUser(t, s, mi.Tenant, "u-pending", store.UserStatusPending)
	addUser(t, s, mi.Tenant, "u-disabled", "disabled")
	ctx := context.Background()

	for _, p := range []string{"u-ghost", "u-other"} {
		code, out := s.postBind(t, ctx, mi.Tenant, dup, p)
		if code != http.StatusNotFound || out["code"] != "principal_not_found" {
			t.Errorf("bind to %s = %d %v, want 404 principal_not_found", p, code, out)
		}
		if got := s.boundPrincipal(t, mi, p); len(got) != 0 {
			t.Errorf("a refused bind to %s was written: %v", p, got)
		}
	}
	if code, out := s.postBind(t, ctx, mi.Tenant, dup, "u-pending"); code != http.StatusOK {
		t.Errorf("bind to a pending user = %d %v", code, out)
	}
	if code, out := s.postBind(t, ctx, mi.Tenant, surv, "u-disabled"); code != http.StatusOK {
		t.Errorf("bind to a disabled user = %d %v", code, out)
	}
}

// TestOntologyAPI_BindRefusalsCarryTheirCodes: each refusal is its own status and
// code; a conflict names the subject that holds the binding.
func TestOntologyAPI_BindRefusalsCarryTheirCodes(t *testing.T) {
	s, mi := ontologyFixture(t)
	dup, surv, report := seedMergeSubjects(t, s, mi)
	addUser(t, s, mi.Tenant, "u-ada", "active")
	addUser(t, s, mi.Tenant, "u-eve", "active")
	ctx := context.Background()

	for _, tc := range []struct {
		name, subject, principal string
		status                   int
		code                     string
	}{
		{"missing subject", "", "u-ada", http.StatusBadRequest, "invalid_request"},
		{"unknown key", "person:nobody", "u-ada", http.StatusNotFound, "subject_not_found"},
		{"not a subject", report, "u-ada", http.StatusBadRequest, "not_a_subject"},
	} {
		code, out := s.postBind(t, ctx, mi.Tenant, tc.subject, tc.principal)
		if code != tc.status || out["code"] != tc.code {
			t.Errorf("%s: %d %v, want %d %s", tc.name, code, out, tc.status, tc.code)
		}
	}
	if code, out := s.postBind(t, ctx, mi.Tenant, dup, "u-ada"); code != http.StatusOK {
		t.Fatalf("bind = %d %v", code, out)
	}
	code, out := s.postBind(t, ctx, mi.Tenant, surv, "u-ada")
	if msg, _ := out["error"].(string); code != http.StatusConflict || out["code"] != "binding_conflict" || !strings.Contains(msg, "person:ada-king") {
		t.Errorf("a second subject for u-ada = %d %v, want 409 binding_conflict naming person:ada-king", code, out)
	}
	code, out = s.postBind(t, ctx, mi.Tenant, dup, "u-eve")
	if msg, _ := out["error"].(string); code != http.StatusConflict || out["code"] != "binding_conflict" || !strings.Contains(msg, "u-ada") {
		t.Errorf("rebinding a bound subject = %d %v, want 409 binding_conflict naming the current binding", code, out)
	}
}

// TestOntologyAPI_BindIsForOperators (RFC FR §9): who a subject is decides what an
// erasure finds, so a member, an isolated user and a token held to the run plane
// are refused by the connector itself; a tenant operator acts in its own tenant
// only; and one whose rules keep it from writing the subject's document is refused.
func TestOntologyAPI_BindIsForOperators(t *testing.T) {
	s, mi := ontologyFixture(t)
	dup, _, _ := seedMergeSubjects(t, s, mi)
	addUser(t, s, mi.Tenant, "u-ada", "active")
	for what, p := range map[string]auth.Principal{
		"a member":             {TenantID: mi.Tenant, Subject: "m", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}},
		"an isolated user":     {TenantID: mi.Tenant, Subject: "u", Scopes: []string{auth.ScopeUser}},
		"a run-plane operator": {TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Restrictions: auth.Restrictions{Agents: []string{"judge"}}},
	} {
		_, err := s.BindSubjectPrincipal(auth.WithPrincipal(context.Background(), p), connector.BindSubjectPrincipalRequest{Subject: dup, Principal: "u-ada"})
		if connector.SubjectBindCode(err) != "forbidden" {
			t.Errorf("%s: err = %v, want forbidden", what, err)
		}
	}
	if got := s.boundPrincipal(t, mi, "u-ada"); len(got) != 0 {
		t.Fatalf("a refused caller bound the subject: %v", got)
	}

	eff, err := authz.Compile(authz.Input{Subject: authz.Subject{TenantID: mi.Tenant, ID: "o"},
		Bindings: []authz.Binding{{ACL: "a", SubjectKind: authz.SubjectTenant}},
		ACLs: map[string]authz.ACLVersion{"a": {ACL: authz.ACL{Name: "a", Rules: []authz.Rule{
			{Effect: authz.Allow, Actions: []string{"read", "write"}, Resources: []string{"memory:*", "sqlmem:*"}},
			{Effect: authz.Allow, Actions: []string{"read", "write", "link", "propose"}, Resources: []string{"document:*"}},
			{Effect: authz.Deny, Actions: []string{"write"}, Resources: []string{"document:tenant:/facts/**"}},
		}}}}, Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	noWrite := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}, Effective: eff})
	if code, out := s.postBind(t, noWrite, mi.Tenant, dup, "u-ada"); code != http.StatusForbidden || out["code"] != "forbidden" {
		t.Errorf("an operator who may not write the subject = %d %v, want 403 forbidden", code, out)
	}

	op := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: mi.Tenant, Subject: "o", Scopes: []string{auth.ScopeTenant}})
	if res, err := s.BindSubjectPrincipal(op, connector.BindSubjectPrincipalRequest{Tenant: "elsewhere", Subject: dup, Principal: "u-ada"}); err != nil || res.Tenant != mi.Tenant {
		t.Errorf("a tenant operator naming another tenant: %+v %v, want its own tenant's bind", res, err)
	}
}
