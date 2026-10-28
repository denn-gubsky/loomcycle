package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// The tenant policy is enforced on USE, not only by the sweep a policy change
// runs: credentials issued before a config-declared policy loaded (there is
// no change event), a sweep that failed half-way, or a replica that never ran
// it must still stop working on their next request.

// A token issued through gate B before a config policy allowing only gate A
// existed is refused on its next request, with no sweep.
func TestTenantPolicyOnUse_ConfigPolicyRefusesAnEarlierIssuedToken(t *testing.T) {
	s, st, _ := policyServer(t, nil)
	ctx := context.Background()
	seedUser(t, st, "acme", "alice", "isolated", "active")
	issued, _, err := s.issueGateToken(ctx, "acme", "alice", "paste", 0, auth.Restrictions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.resolvePrincipal(ctx, issued); !ok {
		t.Fatal("precondition: the issued token authenticates")
	}
	// The policy arrives with config (a restart, a reload): no PUT, no sweep.
	s.cfg().ResolvedAuth.TenantPolicies = map[string][]string{"acme": {"local"}}
	s.tenantPolicyCache.flush()
	if _, ok := s.resolvePrincipal(ctx, issued); ok {
		t.Fatal("a token issued through an unlisted gate still authenticates under the config policy")
	}
	// Even from the token cache.
	s.EnableTokenCache(time.Hour)
	if _, ok := s.resolvePrincipal(ctx, issued); ok {
		t.Fatal("a cached resolution let the token through")
	}
}

// A Web UI session from gate B is refused once the policy lists only A.
func TestTenantPolicyOnUse_ConfigPolicyRefusesAnEarlierSession(t *testing.T) {
	s, st, _ := policyServer(t, nil)
	c := mustSession(t, s, acmeMemberToken(t, s, st, "alice"))
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusOK {
		t.Fatalf("precondition: session = %d", code)
	}
	s.cfg().ResolvedAuth.TenantPolicies = map[string][]string{"acme": {"local"}}
	s.tenantPolicyCache.flush()
	if code, reached := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized || reached {
		t.Fatalf("a paste session under a local-only policy = %d reached=%v", code, reached)
	}
}

type failingSweepStore struct{ store.Store }

func (failingSweepStore) TenantAuthPolicyRevokeSessions(context.Context, string, []string, time.Time) (int, error) {
	return 0, errDiskFull
}

// A sweep that fails leaves nothing usable: the stored policy refuses the
// session and the issued token on their next request regardless.
func TestTenantPolicyOnUse_FailedSweepLeavesNothingUsable(t *testing.T) {
	s, st, _ := policyServer(t, nil)
	ctx := context.Background()
	c := mustSession(t, s, acmeMemberToken(t, s, st, "alice"))
	issued, _, err := s.issueGateToken(ctx, "acme", "alice", "paste", 0, auth.Restrictions{})
	if err != nil {
		t.Fatal(err)
	}
	s.store = failingSweepStore{Store: st}
	req := adminReq(http.MethodPut, "/v1/_auth/tenant-policies/acme", `{"gates":["local"]}`)
	req.SetPathValue("tenant", "acme")
	rec := httptest.NewRecorder()
	s.handlePutTenantPolicy(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("PUT with a failing sweep = %d %s", rec.Code, rec.Body.String())
	}
	if code, _ := through(s, sessionReq(http.MethodGet, "/v1/_me", c)); code != http.StatusUnauthorized {
		t.Fatalf("the session survived a failed sweep: %d", code)
	}
	if _, ok := s.resolvePrincipal(ctx, issued); ok {
		t.Fatal("the issued token survived a failed sweep")
	}
}

// A token no gate issued (an operator or service token, a hand-minted user
// token) is the operator's; a tenant policy does not touch it as a bearer.
func TestTenantPolicyOnUse_TokenNotIssuedByAGateIsUnaffected(t *testing.T) {
	s, st, _ := policyServer(t, map[string][]string{"acme": {"local"}})
	ctx := context.Background()
	hand := acmeMemberToken(t, s, st, "alice")
	seedToken(t, st, "lct_acme_service_token_value", "acme", "svc", []string{auth.ScopeTenant}, time.Time{})
	for _, tok := range []string{hand, "lct_acme_service_token_value"} {
		if _, ok := s.resolvePrincipal(ctx, tok); !ok {
			t.Fatalf("a token without issued_by_gate was refused by the tenant policy")
		}
	}
}
