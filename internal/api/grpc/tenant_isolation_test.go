package grpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/api/grpc/loomcyclepb"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/cancel"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// These regression tests close the gRPC cross-tenant isolation gap: the gRPC
// handlers authenticate + enforce scope but historically did NO tenant/subject
// row-filtering (that lived only in the HTTP handlers), so a scoped principal
// could read/stream/act across tenants over gRPC that HTTP folds away. Each
// test drives the handler directly with an auth.Principal-bearing ctx (the same
// ctx the interceptor stamps in the live path).

func tenantTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st := newTestStore(t)
	adapter := New(Config{Store: st, CancelReg: cancel.NewRegistry()})
	return adapter, st
}

// seedRun creates a session + run in the given tenant and returns the agent id.
func seedRun(t *testing.T, st store.Store, tenant, user, agentID string) string {
	t.Helper()
	ctx := context.Background()
	sess, err := st.CreateSession(ctx, tenant, "default", user)
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", tenant, err)
	}
	if _, err := st.CreateRun(ctx, sess.ID, store.RunIdentity{
		AgentID: agentID, UserID: user, TenantID: tenant,
	}); err != nil {
		t.Fatalf("CreateRun(%s): %v", tenant, err)
	}
	return agentID
}

func scopedCtx(tenant, subject string, scopes ...string) context.Context {
	return auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes})
}

// TestGrpcGetAgent_CrossTenantFoldsNotFound: a tenant-acme principal must not
// read a tenant-evil run's metadata over gRPC — it folds to NotFound (the
// opaque posture HTTP handleGetAgent gets from tenantStore.GetRunByAgentID).
func TestGrpcGetAgent_CrossTenantFoldsNotFound(t *testing.T) {
	adapter, st := tenantTestServer(t)
	seedRun(t, st, "evil", "mallory", "a_evil")
	seedRun(t, st, "acme", "alice", "a_acme")

	acme := scopedCtx("acme", "alice", auth.ScopeRunsRead)

	if _, err := adapter.GetAgent(acme, &loomcyclepb.GetAgentRequest{AgentId: "a_evil"}); status.Code(err) != codes.NotFound {
		t.Errorf("cross-tenant GetAgent code=%s, want NotFound", status.Code(err))
	}
	if _, err := adapter.GetAgent(acme, &loomcyclepb.GetAgentRequest{AgentId: "a_acme"}); err != nil {
		t.Errorf("own-tenant GetAgent: %v, want ok", err)
	}
	// An admin principal crosses tenants by design.
	admin := scopedCtx("acme", "ops", auth.ScopeAdmin)
	if _, err := adapter.GetAgent(admin, &loomcyclepb.GetAgentRequest{AgentId: "a_evil"}); err != nil {
		t.Errorf("admin GetAgent cross-tenant: %v, want ok", err)
	}
}

// TestGrpcGetTranscript_CrossTenantFoldsNotFound: a cross-tenant session's
// transcript folds to NotFound (a transcript exposes the whole conversation).
func TestGrpcGetTranscript_CrossTenantFoldsNotFound(t *testing.T) {
	adapter, st := tenantTestServer(t)
	ctx := context.Background()
	evilSess, _ := st.CreateSession(ctx, "evil", "default", "mallory")
	acmeSess, _ := st.CreateSession(ctx, "acme", "default", "alice")

	acme := scopedCtx("acme", "alice", auth.ScopeRunsRead)
	if _, err := adapter.GetTranscript(acme, &loomcyclepb.GetTranscriptRequest{SessionId: evilSess.ID}); status.Code(err) != codes.NotFound {
		t.Errorf("cross-tenant GetTranscript code=%s, want NotFound", status.Code(err))
	}
	if _, err := adapter.GetTranscript(acme, &loomcyclepb.GetTranscriptRequest{SessionId: acmeSess.ID}); err != nil {
		t.Errorf("own-tenant GetTranscript: %v, want ok", err)
	}
}

// TestGrpcListUserAgents_DropsCrossTenant: when the same user_id has runs in
// two tenants, a scoped principal sees only its own tenant's rows.
func TestGrpcListUserAgents_DropsCrossTenant(t *testing.T) {
	adapter, st := tenantTestServer(t)
	// Same user id "shared" owns a run under each tenant.
	seedRun(t, st, "evil", "shared", "a_evil_shared")
	seedRun(t, st, "acme", "shared", "a_acme_shared")

	acme := scopedCtx("acme", "shared", auth.ScopeRunsRead)
	resp, err := adapter.ListUserAgents(acme, &loomcyclepb.ListUserAgentsRequest{UserId: "shared"})
	if err != nil {
		t.Fatalf("ListUserAgents: %v", err)
	}
	for _, a := range resp.GetAgents() {
		if a.GetAgentId() == "a_evil_shared" {
			t.Errorf("scoped ListUserAgents leaked a cross-tenant run: %s", a.GetAgentId())
		}
	}
	if len(resp.GetAgents()) != 1 || resp.GetAgents()[0].GetAgentId() != "a_acme_shared" {
		t.Errorf("scoped list = %v, want only a_acme_shared", resp.GetAgents())
	}
}

// TestGrpcChannelScope_ConfinedToSubjectAndAdmin locks the channel-scope gate:
// a non-admin may touch only its own user scope; another subject folds to
// NotFound and the operator global scope is admin-only.
func TestGrpcChannelScope_ConfinedToSubjectAndAdmin(t *testing.T) {
	tenantAlice := scopedCtx("acme", "alice", auth.ScopeChannelRead, auth.ScopeChannelPublish)

	// Own user scope — allowed.
	if err := principalMayUseChannelScope(tenantAlice, "user", "alice"); err != nil {
		t.Errorf("own user scope denied: %v", err)
	}
	// Another subject — opaque NotFound.
	if err := principalMayUseChannelScope(tenantAlice, "user", "bob"); status.Code(err) != codes.NotFound {
		t.Errorf("cross-subject user scope code=%s, want NotFound", status.Code(err))
	}
	// Operator global scope — admin-only → PermissionDenied for a tenant token.
	if err := principalMayUseChannelScope(tenantAlice, "global", ""); status.Code(err) != codes.PermissionDenied {
		t.Errorf("global scope for tenant token code=%s, want PermissionDenied", status.Code(err))
	}
	if err := principalMayUseChannelScope(tenantAlice, "", ""); status.Code(err) != codes.PermissionDenied {
		t.Errorf("default(global) scope for tenant token code=%s, want PermissionDenied", status.Code(err))
	}
	// Admin bypasses all of it.
	admin := scopedCtx("acme", "ops", auth.ScopeAdmin)
	for _, sc := range []struct{ scope, id string }{{"user", "bob"}, {"global", ""}} {
		if err := principalMayUseChannelScope(admin, sc.scope, sc.id); err != nil {
			t.Errorf("admin denied scope=%q id=%q: %v", sc.scope, sc.id, err)
		}
	}
}
