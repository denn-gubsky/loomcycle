package builtin

import (
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// A webhook that maps user_id runs each delivery as the user the body names.
// A member holds the webhook's secret and so chooses the body: it could run
// prompts as any colleague, with that colleague's memory and history. A member
// may not create or fork a webhook that maps user_id; a tenant operator may.
func TestWebhookDef_AMemberCannotMapDeliveriesToAnotherUser(t *testing.T) {
	tool, base, cleanup := webhookDefFixture(t)
	defer cleanup()
	scopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	ctx := tools.WithRunIdentity(base, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme"})
	member := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "alice", Scopes: scopes})
	operator := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})
	mapped := `{` + webhookBase + `,"payload_mapping":{"user_id":"$.user"}}`

	if text, isErr := webhookCreate(t, tool, member, "as-anyone", mapped); !isErr {
		t.Errorf("a member created a webhook that runs as the user a delivery names: %s", text)
	}
	if text, isErr := webhookCreate(t, tool, member, "plain", `{`+webhookBase+`}`); isErr {
		t.Fatalf("a member's webhook without a user mapping: %s", text)
	}
	res, err := tool.Execute(member, json.RawMessage(`{"op":"fork","name":"plain","overlay":{"payload_mapping":{"user_id":"$.user"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("a member forked a webhook to map user_id: %s", res.Text)
	}
	if text, isErr := webhookCreate(t, tool, operator, "op-mapped", mapped); isErr {
		t.Errorf("a tenant operator's webhook mapping user_id: %s", text)
	}
	res, err = tool.Execute(member, json.RawMessage(`{"op":"fork","name":"op-mapped","overlay":{"description":"mine now"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Errorf("a member forked a webhook that maps user_id (inherited): %s", res.Text)
	}
}
