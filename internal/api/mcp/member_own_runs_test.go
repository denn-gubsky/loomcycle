package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
)

func memberPrincipalCtx(t *testing.T, tenant, subject string) context.Context {
	t.Helper()
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	return auth.WithPrincipal(context.Background(), auth.Principal{TenantID: tenant, Subject: subject, Scopes: scopes})
}

// A member (non-isolated) watching another user's run transitions is refused
// before the connector is reached, with the opaque answer HTTP gives; its own
// user streams.
func TestStreamUserRunStates_AMemberCannotWatchAnotherUser(t *testing.T) {
	mc := &mockConnector{}
	env := &handlerEnv{connector: mc, logger: discardLogger()}
	member := memberPrincipalCtx(t, "acme", "bob")
	res, err := handleStreamUserRunStates(member, env, json.RawMessage(`{"user_id":"alice","timeout_ms":50}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("a member streamed another user's run transitions")
	}
	if mc.lastStreamReq.UserID != "" {
		t.Errorf("the connector was reached for a refused stream: %+v", mc.lastStreamReq)
	}
	if res, err := handleStreamUserRunStates(member, env, json.RawMessage(`{"user_id":"bob","timeout_ms":50}`)); err != nil || res.IsError {
		t.Fatalf("member streaming its own user: %v %+v", err, res)
	}
}
