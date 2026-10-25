package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

func approveReq(p auth.Principal, subject string) *http.Request {
	return bodyReq(http.MethodPost, "/v1/_users/"+subject+"/approve", "", p, "subject", subject)
}

// RFC FA §3.7a: a tenant operator approves a pending user of its own
// tenant; the approval is audited and the user's session opens up.
func TestApproveUser_PendingBecomesActiveAndIsAudited(t *testing.T) {
	s, st, sink := identitySessionServer(t, config.AuthIdentityMap{Tenant: "acme", Provision: config.ProvisionAuto,
		NewUser: config.AuthNewUser{AccessMode: "isolated", Status: "pending"}})
	c, out := identityLoginSession(t, s, "500", time.Now())
	if code, reached := through(s, sessionReq(http.MethodGet, "/v1/agents", c)); reached {
		t.Fatalf("pending reached a route: %d", code)
	}
	ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleApproveUser(rec, approveReq(ops, out.Subject))
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if u, _ := st.UserGet(context.Background(), "acme", out.Subject); u.Status != "active" {
		t.Fatalf("status = %q", u.Status)
	}
	if ev := lastEvent(t, sink, "auth_user_approved"); ev.TargetSubject != out.Subject || ev.ActorSubject != "ops" {
		t.Fatalf("audit = %+v", ev)
	}
	if _, reached := through(s, sessionReq(http.MethodGet, "/v1/agents", c)); !reached {
		t.Fatal("approved user still refused")
	}
	rec = httptest.NewRecorder()
	s.handleApproveUser(rec, approveReq(ops, out.Subject))
	if rec.Code != http.StatusConflict {
		t.Fatalf("approving an active user = %d", rec.Code)
	}
}

func TestApproveUser_OtherTenantIsAnOpaqueMiss(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	if err := st.UserCreate(context.Background(), store.UserRow{TenantID: "other", Subject: "p1", AccessMode: "isolated", Status: store.UserStatusPending}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleApproveUser(rec, approveReq(auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}, "p1"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant approve = %d", rec.Code)
	}
	if u, _ := st.UserGet(context.Background(), "other", "p1"); u.Status != store.UserStatusPending {
		t.Fatal("cross-tenant approve changed the user")
	}
}

// The approval is the one way out of pending: a PATCH to active is refused
// (it would skip the audit), a PATCH to disabled (a rejection) is not.
func TestUpdateUser_PendingToActiveNeedsTheApproveRoute(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	for _, sub := range []string{"p1", "p2"} {
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: "isolated", Status: store.UserStatusPending}); err != nil {
			t.Fatal(err)
		}
	}
	ops := auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}}
	rec := httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq(http.MethodPatch, "/v1/_users/p1", `{"status":"active"}`, ops, "subject", "p1"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("patch pending→active = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.handleUpdateUser(rec, bodyReq(http.MethodPatch, "/v1/_users/p2", `{"status":"disabled"}`, ops, "subject", "p2"))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch pending→disabled = %d", rec.Code)
	}
}

// RFC FA §6.1.1: a website's auth:manage credential approves only users at
// the provisioning floor.
func TestApproveUser_ManagerIsHeldToTheFloor(t *testing.T) {
	s, st := tokenAuthServer(t, "legacy")
	ctx := context.Background()
	for sub, mode := range map[string]string{"floor": "isolated", "staff": "tenant"} {
		if err := st.UserCreate(ctx, store.UserRow{TenantID: "acme", Subject: sub, AccessMode: mode, Status: store.UserStatusPending}); err != nil {
			t.Fatal(err)
		}
	}
	site := auth.Principal{TenantID: "x", Subject: "site", Scopes: []string{auth.ScopeAuthManage}}
	req := func(sub string) *http.Request {
		return bodyReq(http.MethodPost, "/v1/_users/"+sub+"/approve?tenant=acme", "", site, "subject", sub)
	}
	rec := httptest.NewRecorder()
	s.handleApproveUser(rec, req("floor"))
	if rec.Code != http.StatusOK {
		t.Fatalf("floor user = %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	s.handleApproveUser(rec, req("staff"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("above-floor user = %d", rec.Code)
	}
}

// The approve route is a tenant-operator route: a member token is refused at
// the scope gate.
func TestApproveUser_RouteNeedsTheTenantScope(t *testing.T) {
	if got := requiredScopeFor(http.MethodPost, "/v1/_users/x/approve"); got != auth.ScopeTenant {
		t.Fatalf("required scope = %q", got)
	}
	if !authManageRoute(http.MethodPost, "/v1/_users/x/approve") || authManageRoute(http.MethodPost, "/v1/_users/x/y/approve") {
		t.Fatal("auth:manage route list")
	}
}
