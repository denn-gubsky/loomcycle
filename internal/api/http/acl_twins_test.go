package http

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

func twinCall(t *testing.T, srv *Server, ctx context.Context, family, input string) connector.ToolResult {
	t.Helper()
	var (
		res connector.ToolResult
		err error
	)
	switch family {
	case connector.ACLFamilyGroupDef:
		res, err = srv.GroupDef(ctx, json.RawMessage(input))
	case connector.ACLFamilyAclDef:
		res, err = srv.AclDef(ctx, json.RawMessage(input))
	case connector.ACLFamilyAclBinding:
		res, err = srv.AclBinding(ctx, json.RawMessage(input))
	case connector.ACLFamilyAuthz:
		res, err = srv.Authz(ctx, json.RawMessage(input))
	default:
		t.Fatalf("no twin for %q", family)
	}
	if err != nil {
		t.Fatalf("%s %s: %v", family, input, err)
	}
	return res
}

func textCode(text string) string {
	var b struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(text), &b)
	return b.Code
}

// Every op the table lists reaches an operation: none falls through to the
// dispatcher's "listed but not dispatched" refusal or comes back unknown.
func TestACLTwins_DispatchEveryOpInTheTable(t *testing.T) {
	srv, _, _ := aclMgmtServer(t, authz.ModeCompat)
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	seen := 0
	for _, family := range connector.ACLManagementFamilies() {
		for _, name := range connector.ACLManagementOps(family) {
			seen++
			res := twinCall(t, srv, op, family, `{"op":"`+name+`"}`)
			if c := textCode(res.Text); c == "unknown_op" || strings.Contains(res.Text, "not dispatched") {
				t.Errorf("%s op %s: %s", family, name, res.Text)
			}
		}
		if res := twinCall(t, srv, op, family, `{"op":"no_such_op"}`); textCode(res.Text) != "unknown_op" {
			t.Errorf("%s op no_such_op: %s, want unknown_op", family, res.Text)
		}
	}
	if seen < 20 {
		t.Fatalf("the op table lists only %d ops", seen)
	}
}

// A twin answers what its route answers: the same rows, the same decision,
// the same JSON.
func TestACLTwins_AnswerWhatTheRouteAnswers(t *testing.T) {
	srv, ts, _ := aclMgmtServer(t, authz.ModeCompat)
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]}]`
	for _, w := range []struct{ family, input string }{
		{"groupdef", `{"op":"create","name":"support","description":"tier 1"}`},
		{"groupdef", `{"op":"add_member","name":"support","kind":"user","id":"alice"}`},
		{"acldef", `{"op":"create","name":"no-shell","rules":` + rules + `}`},
		{"aclbinding", `{"op":"create","acl":"no-shell","subject_kind":"group","subject_id":"support"}`},
	} {
		if res := twinCall(t, srv, op, w.family, w.input); res.IsError {
			t.Fatalf("%s %s: %s", w.family, w.input, res.Text)
		}
	}
	for _, c := range []struct {
		family, input, method, path, body string
	}{
		{"groupdef", `{"op":"list"}`, "GET", "/v1/_groups", ""},
		{"groupdef", `{"op":"get","name":"support"}`, "GET", "/v1/_groups/support", ""},
		{"acldef", `{"op":"list"}`, "POST", "/v1/_acldef", `{"op":"list"}`},
		{"acldef", `{"op":"get","name":"no-shell"}`, "POST", "/v1/_acldef", `{"op":"get","name":"no-shell"}`},
		{"acldef", `{"op":"names"}`, "GET", "/v1/_acldef/names", ""},
		{"aclbinding", `{"op":"list"}`, "GET", "/v1/_aclbinding", ""},
		{"authz", `{"op":"evaluation","subject":{"id":"alice"},"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`,
			"POST", "/v1/_authz/evaluation", `{"subject":{"id":"alice"},"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`},
	} {
		res := twinCall(t, srv, op, c.family, c.input)
		code, body := bearerCall(t, ts, operatorBearer, c.method, c.path, c.body)
		if res.IsError || code != 200 {
			t.Errorf("%s %s: twin %s / route %d %s", c.family, c.input, res.Text, code, body)
			continue
		}
		var twin, route any
		if err := json.Unmarshal([]byte(res.Text), &twin); err != nil {
			t.Fatalf("twin text is not JSON: %s", res.Text)
		}
		if err := json.Unmarshal([]byte(body), &route); err != nil {
			t.Fatalf("route body is not JSON: %s", body)
		}
		if !reflect.DeepEqual(twin, route) {
			t.Errorf("%s %s:\n twin  %s\n route %s", c.family, c.input, res.Text, body)
		}
	}
}

// A twin takes exactly the fields its op takes, as the route takes exactly
// its body's.
func TestACLTwins_RefuseAFieldTheOpDoesNotTake(t *testing.T) {
	srv, _, _ := aclMgmtServer(t, authz.ModeCompat)
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	for _, c := range []struct{ family, input, code string }{
		{"groupdef", `{"op":"create","name":"x","kind":"user"}`, "invalid_body"},
		{"groupdef", `{"op":"list","name":"x"}`, "invalid_body"},
		{"groupdef", `{"op":"get"}`, "missing_field"},
		{"groupdef", `{"op":"remove_member","name":"x","kind":"user"}`, "missing_field"},
		{"aclbinding", `{"op":"delete"}`, "missing_field"},
		{"aclbinding", `{"op":"create","acl":"x","subject_kind":"user","subject_id":"y","rules":[]}`, "invalid_body"},
		{"acldef", `{"op":"list","members":["a"]}`, "invalid_body"},
		{"authz", `{"op":"evaluation","acls":[]}`, "invalid_body"},
		{"authz", `not json`, "invalid_body"},
	} {
		res := twinCall(t, srv, op, c.family, c.input)
		if !res.IsError || textCode(res.Text) != c.code || res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryValidation {
			t.Errorf("%s %s: %+v, want a %s validation refusal", c.family, c.input, res, c.code)
		}
	}
}

// The "tenant" field is the admin's focus, as ?tenant= is on the route: a
// tenant operator naming another tenant stays in its own.
func TestACLTwins_TenantFocusIsTheAdmins(t *testing.T) {
	srv, _, _ := aclMgmtServer(t, authz.ModeCompat)
	acme := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	if res := twinCall(t, srv, acme, "groupdef", `{"op":"create","name":"support"}`); res.IsError {
		t.Fatal(res.Text)
	}
	other := aclCaller(t, srv, "other", "ops", auth.ScopeTenant)
	if res := twinCall(t, srv, other, "groupdef", `{"op":"list","tenant":"acme"}`); res.IsError || strings.Contains(res.Text, "support") {
		t.Errorf("another tenant's operator focusing acme: %s", res.Text)
	}
	if res := twinCall(t, srv, other, "groupdef", `{"op":"get","name":"support","tenant":"acme"}`); textCode(res.Text) != "group_not_found" {
		t.Errorf("another tenant's operator reading acme's group: %s, want group_not_found", res.Text)
	}
	admin := auth.WithPrincipal(context.Background(), auth.Principal{Legacy: true, Scopes: []string{auth.ScopeAdmin}})
	if res := twinCall(t, srv, admin, "groupdef", `{"op":"list","tenant":"acme"}`); res.IsError || !strings.Contains(res.Text, "support") {
		t.Errorf("the admin focusing acme: %s", res.Text)
	}
}

// A refusal carries its category, so a gRPC or MCP client can tell "not
// yours" from "malformed" without parsing the text.
func TestACLTwins_ARefusalIsClassified(t *testing.T) {
	srv, _, _ := aclMgmtServer(t, authz.ModeCompat)
	member := aclCaller(t, srv, "acme", "alice", auth.ScopeRunsCreate, auth.ScopeRunsRead)
	res := twinCall(t, srv, member, "groupdef", `{"op":"list"}`)
	if !res.IsError || textCode(res.Text) != "insufficient_scope" || res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryPermission {
		t.Errorf("a member listing groups: %+v, want a permission refusal", res)
	}
	op := aclCaller(t, srv, "acme", "ops", auth.ScopeTenant)
	res = twinCall(t, srv, op, "groupdef", `{"op":"get","name":"nobody"}`)
	if !res.IsError || res.ErrorInfo == nil || res.ErrorInfo.Category != tools.CategoryBusiness {
		t.Errorf("a missing group: %+v, want a business refusal", res)
	}
}
