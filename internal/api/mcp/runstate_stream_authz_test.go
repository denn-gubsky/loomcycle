package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

// TestStreamUserRunStates_TenantScopeFollowsThePrincipal pins which sessions
// get a tenant-confined run-state stream: a tenant principal is scoped to its
// own tenant; open mode, the legacy operator and an admin are not. It mirrors
// the HTTP and gRPC streams so the three transports agree.
func TestStreamUserRunStates_TenantScopeFollowsThePrincipal(t *testing.T) {
	cases := []struct {
		name       string
		ctx        context.Context
		wantScoped bool
		wantTenant string
	}{
		{"open", context.Background(), false, ""},
		{"legacy", auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: "default", Subject: "default", Scopes: []string{auth.ScopeAdmin}, Legacy: true,
		}), false, ""},
		{"admin", auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: "ops", Subject: "root", Scopes: []string{auth.ScopeAdmin},
		}), false, ""},
		{"tenant", auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
		}), true, "acme"},
		{"isolated member, own user", auth.WithPrincipal(context.Background(), auth.Principal{
			TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeUser},
		}), true, "acme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := &mockConnector{}
			env := &handlerEnv{connector: mc, logf: func(string, ...any) {}}
			res, err := handleStreamUserRunStates(tc.ctx, env,
				json.RawMessage(`{"user_id":"alice","timeout_ms":50}`))
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError {
				t.Fatalf("stream refused: %+v", res)
			}
			got := mc.lastStreamReq
			if got.TenantScoped != tc.wantScoped || got.TenantID != tc.wantTenant {
				t.Errorf("connector got TenantScoped=%v TenantID=%q, want %v %q",
					got.TenantScoped, got.TenantID, tc.wantScoped, tc.wantTenant)
			}
		})
	}
}

// TestStreamUserRunStates_IsolatedMemberCannotWatchAnotherUser — an isolated
// member naming a co-tenant's user id is refused before the connector is
// reached, with the same opaque answer HTTP gives.
func TestStreamUserRunStates_IsolatedMemberCannotWatchAnotherUser(t *testing.T) {
	mc := &mockConnector{}
	env := &handlerEnv{connector: mc, logf: func(string, ...any) {}}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeUser},
	})
	res, err := handleStreamUserRunStates(ctx, env, json.RawMessage(`{"user_id":"alice","timeout_ms":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("an isolated member streamed another user's run transitions")
	}
	if mc.lastStreamReq.UserID != "" {
		t.Errorf("the connector was reached for a refused stream: %+v", mc.lastStreamReq)
	}
}
