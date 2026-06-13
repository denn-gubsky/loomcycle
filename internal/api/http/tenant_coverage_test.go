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
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestTenantReachableReads_AreOpaqueCrossTenant is the maintained inventory of
// the read routes a NON-ADMIN tenant token can reach (scope runs:read /
// channel:read), each re-verified in one place to stay tenant-gated. It's the
// "enforced invariant" as a guard: Go's ServeMux exposes no route patterns to
// introspect, so the list is maintained by hand — when you add a
// tenant-reachable read route, add a row here and it must be opaque to a
// cross-tenant caller.
//
// Full tenant-reachable surface and where each gate lives:
//
//	GET  /v1/agents/{agent_id}                       tenantStore.GetRunByAgentID   (this table + TestTenantStore_GetRunOpaqueCrossTenant)
//	GET  /v1/runs/{run_id}/stream                    tenantStore.GetRun            (handleRunStream; same accessor as GetRun above)
//	GET  /v1/sessions/{id}/transcript                tenantStore.GetSession        (this table + TestHandleTranscript_RejectsCrossTenant)
//	POST /v1/sessions/{id}/messages                  tenantStore.GetSession        (this table + TestHandleMessages_RejectsCrossTenantSession)
//	POST /v1/runs (continuation)                     sessionOwnershipOK            (openOrCreateSessionAndRun)
//	POST /v1/runs/{run_id}/input  (steer)            sessionOwnershipOK            (handleRunInput; fails open on store error by design)
//	POST /v1/runs/{run_id}/compact                   tenantStore.GetRun            (compactRunWithSource)
//	GET  /v1/runs/{run_id}/interrupts                tenantStore.InterruptListByRun (this table + TestHandleListRunInterrupts_RejectsCrossTenant)
//	GET  /v1/users/{user_id}/interrupts              tenantStore.InterruptListByUser (this table + TestHandleListUserInterrupts_RejectsCrossTenant)
//	GET  /v1/users/{user_id}/agents/stream           StreamUserRunStates tenant filter (TestConnector_StreamUserRunStates_TenantScopedDropsCrossTenant)
//	*    /v1/users/{user_id}/channels/{name}/*        requirePrincipalOwnsPathUser  (TestHandleUserChannelPeek_RejectsCrossSubject)
//
// The /v1/_* admin surfaces are OUT of scope (requiredScopeFor gates them at
// substrate:admin; super-admin sees all tenants by design).
func TestTenantReachableReads_AreOpaqueCrossTenant(t *testing.T) {
	s, st := tokenAuthServer(t, "")
	ctx := context.Background()

	// Seed a tenant-A (acme) session + run + interrupt owned by user "alice".
	sess, err := st.CreateSession(ctx, "acme", "agentx", "alice")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	run, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{AgentID: "ag_cov", UserID: "alice", TenantID: "acme"})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := st.InterruptCreate(ctx, store.InterruptRow{
		InterruptID: store.MintInterruptID(time.Now()), RunID: run.ID, UserID: "alice",
		Question: "secret?", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InterruptCreate: %v", err)
	}

	withPrincipal := func(r *http.Request, tenant, subject string) *http.Request {
		return r.WithContext(auth.WithPrincipal(r.Context(), auth.Principal{
			TenantID: tenant, Subject: subject, Scopes: []string{auth.ScopeRunsRead},
		}))
	}
	totalOf := func(rr *httptest.ResponseRecorder) int {
		var body struct {
			Total int `json:"total"`
		}
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		return body.Total
	}

	type route struct {
		name string
		// fire issues the request as the given (tenant, subject) principal.
		fire func(tenant, subject string) *httptest.ResponseRecorder
		// crossOK asserts the tenant-B (evil) response is opaque (no leak).
		crossOK func(t *testing.T, rr *httptest.ResponseRecorder)
		// ownOK asserts the tenant-A (acme) response is NOT the opaque refusal.
		ownOK func(t *testing.T, rr *httptest.ResponseRecorder)
	}

	routes := []route{
		{
			name: "GET /v1/agents/{agent_id}",
			fire: func(tenant, subject string) *httptest.ResponseRecorder {
				r := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/agents/ag_cov", nil), tenant, subject)
				r.SetPathValue("agent_id", "ag_cov")
				rr := httptest.NewRecorder()
				s.handleGetAgent(rr, r)
				return rr
			},
			crossOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusNotFound {
					t.Errorf("cross-tenant agent read = %d, want 404 (opaque)", rr.Code)
				}
			},
			ownOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusOK {
					t.Errorf("own-tenant agent read = %d, want 200", rr.Code)
				}
			},
		},
		{
			name: "GET /v1/sessions/{id}/transcript",
			fire: func(tenant, subject string) *httptest.ResponseRecorder {
				r := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/sessions/"+sess.ID+"/transcript", nil), tenant, subject)
				r.SetPathValue("id", sess.ID)
				rr := httptest.NewRecorder()
				s.handleTranscript(rr, r)
				return rr
			},
			crossOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusNotFound {
					t.Errorf("cross-tenant transcript = %d, want 404 (opaque)", rr.Code)
				}
			},
			ownOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code == http.StatusNotFound {
					t.Error("own-tenant transcript got opaque 404")
				}
			},
		},
		{
			name: "POST /v1/sessions/{id}/messages (cross-tenant gate only)",
			fire: func(tenant, subject string) *httptest.ResponseRecorder {
				body := `{"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]}`
				r := withPrincipal(httptest.NewRequest(http.MethodPost, "/v1/sessions/"+sess.ID+"/messages", strings.NewReader(body)), tenant, subject)
				r.SetPathValue("id", sess.ID)
				r.Header.Set("Content-Type", "application/json")
				rr := httptest.NewRecorder()
				s.handleMessages(rr, r)
				return rr
			},
			crossOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusNotFound {
					t.Errorf("cross-tenant continuation = %d, want 404 (opaque)", rr.Code)
				}
			},
			// own-tenant proceeds past the gate into run setup (needs a provider),
			// so we only assert it did NOT get the ownership 404.
			ownOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code == http.StatusNotFound {
					t.Error("own-tenant continuation got opaque 404 (gate rejected a legit caller)")
				}
			},
		},
		{
			name: "GET /v1/runs/{run_id}/interrupts",
			fire: func(tenant, subject string) *httptest.ResponseRecorder {
				r := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/runs/"+run.ID+"/interrupts?status=all", nil), tenant, subject)
				r.SetPathValue("run_id", run.ID)
				rr := httptest.NewRecorder()
				s.handleListRunInterrupts(rr, r)
				return rr
			},
			crossOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusOK || totalOf(rr) != 0 {
					t.Errorf("cross-tenant run-interrupts = (%d, total=%d), want (200, 0)", rr.Code, totalOf(rr))
				}
			},
			ownOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusOK || totalOf(rr) != 1 {
					t.Errorf("own-tenant run-interrupts = (%d, total=%d), want (200, 1)", rr.Code, totalOf(rr))
				}
			},
		},
		{
			name: "GET /v1/users/{user_id}/interrupts",
			fire: func(tenant, subject string) *httptest.ResponseRecorder {
				r := withPrincipal(httptest.NewRequest(http.MethodGet, "/v1/users/alice/interrupts?status=all", nil), tenant, subject)
				r.SetPathValue("user_id", "alice")
				rr := httptest.NewRecorder()
				s.handleListUserInterrupts(rr, r)
				return rr
			},
			crossOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusOK || totalOf(rr) != 0 {
					t.Errorf("cross-tenant user-interrupts = (%d, total=%d), want (200, 0)", rr.Code, totalOf(rr))
				}
			},
			ownOK: func(t *testing.T, rr *httptest.ResponseRecorder) {
				if rr.Code != http.StatusOK || totalOf(rr) != 1 {
					t.Errorf("own-tenant user-interrupts = (%d, total=%d), want (200, 1)", rr.Code, totalOf(rr))
				}
			},
		},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			rt.crossOK(t, rt.fire("evil", "mallory")) // cross-tenant: must be opaque
			rt.ownOK(t, rt.fire("acme", "alice"))     // own-tenant: must resolve
		})
	}
}
