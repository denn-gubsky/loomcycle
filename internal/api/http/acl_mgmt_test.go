package http

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/aclres"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	storesqlite "github.com/denn-gubsky/loomcycle/internal/store/sqlite"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

const (
	operatorBearer = "tok-operator" // substrate:tenant in acme
	aliceBearer    = "tok-alice"    // member in acme
	otherOpBearer  = "tok-other-op" // substrate:tenant in another tenant
)

// aclMgmtFixture is a server with a tenant operator and a member of tenant
// acme, an operator of tenant other, and the resolver in the given mode.
func aclMgmtFixture(t *testing.T, mode authz.Mode) (*httptest.Server, store.Store) {
	t.Helper()
	_, ts, st := aclMgmtServer(t, mode)
	return ts, st
}

// aclMgmtServer is aclMgmtFixture with the server itself, for tests that
// call the shared management operations directly.
func aclMgmtServer(t *testing.T, mode authz.Mode) (*Server, *httptest.Server, store.Store) {
	t.Helper()
	return aclMgmtServerWith(t, mode, nil)
}

// aclMgmtServerWith is aclMgmtServer with a hook on the config before the
// server is built (config-declared lists, groups and bindings).
func aclMgmtServerWith(t *testing.T, mode authz.Mode, edit func(*config.Config)) (*Server, *httptest.Server, store.Store) {
	t.Helper()
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Agents = map[string]config.AgentDef{"chat": {Model: "stub-model", Tools: []string{}, SystemPrompt: "chat"}}
	if edit != nil {
		edit(cfg)
	}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "aclmgmt.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, tok := range []struct {
		id, bearer, tenant, subject string
		scopes                      []string
	}{
		{"def_op", operatorBearer, "acme", "ops", []string{auth.ScopeTenant}},
		{"def_alice", aliceBearer, "acme", "alice", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}},
		{"def_other", otherOpBearer, "other", "ops", []string{auth.ScopeTenant}},
	} {
		if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
			DefID: tok.id, Name: "u-" + tok.id, TenantID: tok.tenant, Subject: tok.subject,
			TokenHash: auth.HashToken(testTokenPepper, tok.bearer), AllowedScopes: tok.scopes,
		}); err != nil {
			t.Fatal(err)
		}
	}
	done := []providers.Event{{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}}
	srv := New(cfg, &stubResolver{p: &scriptedProvider{defaultS: done}}, []tools.Tool{}, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, mode, 0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return srv, ts, st
}

func jsonField(t *testing.T, body, field string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	return m[field]
}

func TestACLMgmt_OperatorAuthorsGroupsListsAndBindings(t *testing.T) {
	ts, _ := aclMgmtFixture(t, authz.ModeCompat)
	call := func(bearer, method, path, body string) (int, string) {
		return bearerCall(t, ts, bearer, method, path, body)
	}

	// A group, a nested group, a member; the get shows members and containment.
	if code, body := call(operatorBearer, "POST", "/v1/_groups", `{"name":"support","description":"tier 1"}`); code != 201 {
		t.Fatalf("create group: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_groups", `{"name":"support-l2"}`); code != 201 {
		t.Fatalf("create group: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_groups/support/members", `{"kind":"group","id":"support-l2"}`); code != 201 {
		t.Fatalf("nest: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_groups/support-l2/members", `{"kind":"user","id":"alice"}`); code != 201 {
		t.Fatalf("add member: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_groups/support-l2/members", `{"kind":"group","id":"support"}`); code != 400 || !strings.Contains(body, "nested inside itself") {
		t.Errorf("a cycle should be refused: %d %s", code, body)
	}
	code, body := call(operatorBearer, "GET", "/v1/_groups/support-l2", "")
	if code != 200 || !strings.Contains(body, `"member_id":"alice"`) || !strings.Contains(body, `"contained_in":["support","support-l2"]`) {
		t.Errorf("get group: %d %s", code, body)
	}
	// An ACL: validate, create, list (with the built-ins), names, get.
	rules := `[{"effect":"deny","actions":["invoke"],"resources":["tool:Bash"]},{"effect":"allow","actions":["run"],"resources":["agent:chat/*"]}]`
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"validate","rules":`+rules+`}`); code != 200 || !strings.Contains(body, `"valid":true`) {
		t.Errorf("validate: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"no-shell","rules":`+rules+`,"description":"d"}`); code != 201 || !strings.Contains(body, `"version":1`) {
		t.Fatalf("create acl: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"bad","rules":[{"effect":"allow","actions":["read"],"resources":["tool:*"]}]}`); code != 400 {
		t.Errorf("an invalid list should be refused: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"builtin:admin","rules":`+rules+`}`); code != 400 || !strings.Contains(body, "built-in") {
		t.Errorf("a built-in name should be refused: %d %s", code, body)
	}
	code, body = call(operatorBearer, "POST", "/v1/_acldef", `{"op":"list"}`)
	if code != 200 || !strings.Contains(body, `"name":"no-shell"`) || !strings.Contains(body, `"name":"builtin:member"`) {
		t.Errorf("list: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "GET", "/v1/_acldef/names", ""); code != 200 || !strings.Contains(body, `["no-shell"]`) {
		t.Errorf("names: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"catalog"}`); code != 200 || !strings.Contains(body, `"type":"agent"`) {
		t.Errorf("catalog: %d %s", code, body)
	}
	// A binding to the group; alice's whoami shows her groups; the deny
	// reaches her (evaluate as herself) and explain names the rule.
	code, body = call(operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"no-shell","subject_kind":"group","subject_id":"support"}`)
	if code != 201 {
		t.Fatalf("bind: %d %s", code, body)
	}
	bindingID, _ := jsonField(t, body, "binding_id").(string)
	if code, body := call(aliceBearer, "GET", "/v1/_me", ""); code != 200 || !strings.Contains(body, `"groups":["support","support-l2"]`) {
		t.Errorf("alice whoami: %d %s", code, body)
	}
	code, body = call(aliceBearer, "POST", "/v1/_authz/evaluation", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 || !strings.Contains(body, `"decision":false`) || !strings.Contains(body, `"outcome":"deny"`) || !strings.Contains(body, `"acl":"no-shell"`) {
		t.Errorf("alice evaluate Bash: %d %s", code, body)
	}
	code, body = call(aliceBearer, "POST", "/v1/_authz/evaluation", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Read"}}`)
	if code != 200 || !strings.Contains(body, `"decision":true`) {
		t.Errorf("alice evaluate Read: %d %s", code, body)
	}
	code, body = call(aliceBearer, "POST", "/v1/_authz/explain", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 || !strings.Contains(body, `"why":"matched"`) || !strings.Contains(body, `"applied_bindings"`) {
		t.Errorf("alice explain: %d %s", code, body)
	}
	// Batch with deny_on_first_deny stops at the deny.
	code, body = call(aliceBearer, "POST", "/v1/_authz/evaluations", `{"action":{"name":"invoke"},"evaluations":[{"resource":{"type":"tool","id":"Read"}},{"resource":{"type":"tool","id":"Bash"}},{"resource":{"type":"tool","id":"Write"}}],"options":{"evaluations_semantic":"deny_on_first_deny"}}`)
	if code != 200 || strings.Count(body, `"decision":true`) != 1 || !strings.Contains(body, `"outcome":"no_match"`) {
		t.Errorf("batch: %d %s", code, body)
	}
	// The operator may ask about alice; alice may not ask about the operator.
	code, body = call(operatorBearer, "POST", "/v1/_authz/evaluation", `{"subject":{"id":"alice"},"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 || !strings.Contains(body, `"outcome":"deny"`) {
		t.Errorf("operator about alice: %d %s", code, body)
	}
	if code, _ := call(aliceBearer, "POST", "/v1/_authz/evaluation", `{"subject":{"id":"ops"},"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`); code != 403 {
		t.Errorf("alice about the operator: %d", code)
	}
	// Simulate a draft: a list allowing Bash does not beat the deny; retiring
	// the binding in the store does, on the next call (ttl 0).
	code, body = call(operatorBearer, "POST", "/v1/_authz/simulate", `{"subject":{"id":"alice"},"acls":[{"name":"shell-ok","rules":[{"effect":"allow","actions":["invoke"],"resources":["tool:Bash"]}]}],"bindings":[{"acl":"shell-ok","subject_kind":"user","subject_id":"alice"}],"requests":[{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}]}`)
	if code != 200 || !strings.Contains(body, `"changed":false`) {
		t.Errorf("simulate (deny overrides the draft allow): %d %s", code, body)
	}
	if code, body := call(operatorBearer, "DELETE", "/v1/_aclbinding/"+bindingID, ""); code != 200 {
		t.Errorf("retire binding: %d %s", code, body)
	}
	code, body = call(aliceBearer, "POST", "/v1/_authz/evaluation", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`)
	if code != 200 || !strings.Contains(body, `"decision":true`) {
		t.Errorf("after retiring the binding: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "GET", "/v1/_aclbinding?include_retired=1", ""); code != 200 || !strings.Contains(body, `"retired_at"`) {
		t.Errorf("list retired: %d %s", code, body)
	}
	// Retire the list; a put brings it back with a new version.
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"retire","name":"no-shell"}`); code != 200 {
		t.Errorf("retire acl: %d %s", code, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_acldef", `{"op":"create","name":"no-shell","rules":`+rules+`}`); code != 201 || !strings.Contains(body, `"version":2`) {
		t.Errorf("re-create: %d %s", code, body)
	}
}

func TestACLMgmt_MembersAndOtherTenantsAreConfined(t *testing.T) {
	ts, _ := aclMgmtFixture(t, authz.ModeCompat)
	call := func(bearer, method, path, body string) (int, string) {
		return bearerCall(t, ts, bearer, method, path, body)
	}
	if code, body := call(operatorBearer, "POST", "/v1/_groups", `{"name":"support"}`); code != 201 {
		t.Fatalf("create group: %d %s", code, body)
	}
	// A member is refused every management route (operator controls).
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/_groups", ""}, {"POST", "/v1/_groups", `{"name":"x"}`},
		{"POST", "/v1/_acldef", `{"op":"list"}`}, {"GET", "/v1/_acldef/names", ""},
		{"GET", "/v1/_aclbinding", ""}, {"POST", "/v1/_aclbinding", `{"acl":"x","subject_kind":"tenant"}`},
	} {
		if code, _ := call(aliceBearer, c.method, c.path, c.body); code != 403 {
			t.Errorf("member %s %s: %d, want 403", c.method, c.path, code)
		}
	}
	// A member may still ask about itself.
	if code, _ := call(aliceBearer, "POST", "/v1/_authz/evaluation", `{"action":{"name":"run"},"resource":{"type":"agent","id":"chat"}}`); code != 200 {
		t.Errorf("member self-evaluate: %d", code)
	}
	// Another tenant's operator sees none of acme's rows and cannot reach them.
	if code, body := call(otherOpBearer, "GET", "/v1/_groups", ""); code != 200 || strings.Contains(body, "support") {
		t.Errorf("other tenant list: %d %s", code, body)
	}
	if code, _ := call(otherOpBearer, "GET", "/v1/_groups/support", ""); code != 404 {
		t.Errorf("other tenant get: %d, want 404", code)
	}
	if code, _ := call(otherOpBearer, "DELETE", "/v1/_groups/support", ""); code != 404 {
		t.Errorf("other tenant delete: %d, want 404", code)
	}
	// The ?tenant= focus is the admin's; a tenant operator stays in its own.
	if code, body := call(otherOpBearer, "GET", "/v1/_groups?tenant=acme", ""); code != 200 || strings.Contains(body, "support") {
		t.Errorf("tenant focus by a non-admin: %d %s", code, body)
	}
	if code, body := call("legacy-admin-secret", "GET", "/v1/_groups?tenant=acme", ""); code != 200 || !strings.Contains(body, "support") {
		t.Errorf("admin focus: %d %s", code, body)
	}
}

func TestACLMgmt_UnknownFieldsAndOpsAreRefused(t *testing.T) {
	ts, _ := aclMgmtFixture(t, authz.ModeCompat)
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_groups", `{"name":"x","members":["a"]}`); code != 400 || !strings.Contains(body, "unknown field") {
		t.Errorf("unknown field: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_acldef", `{"op":"promote","name":"x"}`); code != 400 || !strings.Contains(body, "unknown_op") {
		t.Errorf("unknown op: %d %s", code, body)
	}
	if code, body := bearerCall(t, ts, operatorBearer, "POST", "/v1/_aclbinding", `{"acl":"x","subject_kind":"role","subject_id":"y"}`); code != 400 {
		t.Errorf("bad kind: %d %s", code, body)
	}
}
