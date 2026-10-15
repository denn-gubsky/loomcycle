package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// user_scope_credentials is captured from who writes the schedule, never
// from the overlay: an operator, or the schedule's own user, may have it
// resolve $cred: in that user's scope; anyone else may not. A hook edit keeps
// it only when its editor qualifies too.
func TestScheduleDefTool_UserScopeIsCapturedFromTheAuthor(t *testing.T) {
	tool, tenantAuthor := scheduleBearerFixture(t)
	asUser := func(user string) context.Context {
		return tools.WithRunIdentity(tenantAuthor, tools.RunIdentityValue{AgentID: "a_test", TenantID: "acme", UserID: user})
	}
	admin := auth.WithPrincipal(tenantAuthor, auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeAdmin}})
	overlay := func(extra string) string {
		return `{"agent":"digest","schedule":"0 6 * * *","user_id":"alice"` + extra + `}`
	}
	for _, tc := range []struct {
		name string
		ctx  context.Context
		ov   string
		want bool
	}{
		{"another user", asUser("bob"), overlay(""), false},
		{"another user claiming it", asUser("bob"), overlay(`,"user_scope_credentials":true`), false},
		{"no user", tenantAuthor, overlay(""), false},
		{"the user themselves", asUser("alice"), overlay(""), true},
		{"an admin", admin, overlay(""), true},
	} {
		if text, isErr := scheduleCreate(t, tool, tc.ctx, "s", tc.ov); isErr {
			t.Fatalf("%s: %s", tc.name, text)
		}
		if got := activeScheduleDef(t, tool, "s").UserScopeCredentials; got != tc.want {
			t.Errorf("%s: user_scope_credentials = %t, want %t", tc.name, got, tc.want)
		}
		if _, err := tool.Execute(tc.ctx, json.RawMessage(`{"op":"retire","name":"s"}`)); err != nil {
			t.Fatal(err)
		}
	}

	if text, isErr := scheduleCreate(t, tool, asUser("alice"), "own", overlay("")); isErr {
		t.Fatal(text)
	}
	row, err := tool.Store.ScheduleDefGetActive(context.Background(), "acme", "own")
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Execute(asUser("bob"), json.RawMessage(`{"op":"add_hook","def_id":"`+row.DefID+`","hook":{"kind":"memory.set","scope":"user","key":"k"}}`))
	if err != nil || res.IsError {
		t.Fatalf("add_hook: %v %s", err, res.Text)
	}
	if activeScheduleDef(t, tool, "own").UserScopeCredentials {
		t.Error("a hook edit by another user kept the user scope")
	}
}
