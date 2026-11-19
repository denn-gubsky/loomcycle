package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

const attachBearer = "worker-attach-bearer" // test fixture, not a secret

// RFC GW Q2: a workers:attach token sits on a machine that runs model-chosen
// commands, so through the real authMiddleware it reaches NO route the mux
// registers — not the scrape, not whoami, not the MCP transport, not
// self-revocation — in compat and in enforce mode alike.
func TestWorkersAttach_ReachesNoRoute(t *testing.T) {
	for _, mode := range []*authz.Mode{nil, ptrMode(authz.ModeEnforce)} {
		s, st := tokenAuthServer(t, "legacy-secret")
		seedToken(t, st, attachBearer, "acme", "worker-1", []string{auth.ScopeWorkersAttach}, time.Time{})
		if mode != nil {
			s.SetACLResolver(aclres.New(st, *mode, 0))
		}
		for _, r := range eqRoutes(t) {
			reached, code := runMW(t, s, r.method, r.path, attachBearer)
			if reached || code != http.StatusForbidden {
				t.Errorf("mode %v: %s %s: reached=%v code=%d, want refused 403", mode, r.method, r.pattern, reached, code)
			}
		}
		// Named explicitly so a change to the route enumeration cannot drop them.
		for _, c := range []struct{ method, path string }{
			{"GET", "/metrics"}, {"GET", "/v1/_me"}, {"GET", "/v1/runs"}, {"POST", "/v1/runs"},
			{"POST", "/v1/_mcp"}, {"POST", "/v1/chat/completions"}, {"POST", "/v1/_auth/token/revoke"},
			{"POST", "/v1/_operatortokendef"},
		} {
			if reached, code := runMW(t, s, c.method, c.path, attachBearer); reached || code != http.StatusForbidden {
				t.Errorf("mode %v: %s %s: reached=%v code=%d, want refused 403", mode, c.method, c.path, reached, code)
			}
		}
	}
}

func ptrMode(m authz.Mode) *authz.Mode { return &m }

// A worker credential is no person and gets the own-identity carve-out on
// neither side of it.
func TestOwnIdentityRequest_RefusesAWorkerAttacher(t *testing.T) {
	r := httptest.NewRequest("GET", "/v1/_users/worker-1/identities", nil)
	r.Pattern = "GET /v1/_users/{subject}/identities"
	r.SetPathValue("subject", "worker-1")
	member := auth.Principal{TenantID: "acme", Subject: "worker-1", Scopes: []string{auth.ScopeRunsRead}}
	if !ownIdentityRequest(member, r) {
		t.Fatal("the fixture is wrong: a member's own identity request is not admitted")
	}
	worker := auth.Principal{TenantID: "acme", Subject: "worker-1", Scopes: []string{auth.ScopeWorkersAttach}}
	if ownIdentityRequest(worker, r) {
		t.Error("a workers:attach credential was admitted to an identity route")
	}
}

// A worker credential does not sign in to the Web UI: the login is refused
// with a reason, audited, and leaves no session behind.
func TestUILogin_RefusesAWorkersAttachToken(t *testing.T) {
	s, st, sink := sessionTestServer(t, "legacy-secret")
	seedToken(t, st, attachBearer, "acme", "worker-1", []string{auth.ScopeWorkersAttach}, time.Time{})
	rec := doLogin(t, s, loginReq(attachBearer))
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "worker_credential_refused") {
		t.Fatalf("status = %d body = %s, want 403 worker_credential_refused", rec.Code, rec.Body.String())
	}
	if ev := lastEvent(t, sink, "auth_login_failed"); ev.AuthReason != "worker_credential" || ev.ActorSubject != "worker-1" {
		t.Errorf("auth_login_failed = %+v", ev)
	}
	if n := sessionRows(t, st); n != 0 {
		t.Errorf("%d session rows after a refused login, want 0", n)
	}
}
