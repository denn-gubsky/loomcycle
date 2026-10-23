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

// A config-declared list, group and binding (RFC EY §9) reach a run with no
// store row: the member is in the group through the yaml, the yaml binding
// denies Bash, and the run withholds it; the whoami shows the yaml group.
func TestACLConfig_DeclaredBindingReachesTheRun(t *testing.T) {
	cfg := makeBaseConfig()
	cfg.Defaults.Provider = "scripted"
	cfg.Env.AuthToken = "legacy-admin-secret"
	cfg.Env.OperatorTokenPepper = testTokenPepper
	cfg.Agents = map[string]config.AgentDef{"worker": {Model: "stub-model", Tools: []string{"Read", "Bash"}, SystemPrompt: "worker"}}
	cfg.ACLs = map[string]config.ACLDefConfig{"no-shell": {Description: "d", Rules: []map[string]any{{"effect": "deny", "actions": []any{"invoke"}, "resources": []any{"tool:Bash"}}}}}
	cfg.Groups = map[string]config.GroupConfig{"support": {Tenant: "acme", Members: []string{"dev"}}}
	cfg.ACLBindings = []config.ACLBindingConfig{{Tenant: "acme", ACL: "no-shell", Group: "support"}}
	if err := config.ResolveACLConfig(cfg); err != nil {
		t.Fatal(err)
	}
	callBash := []providers.Event{
		{Type: providers.EventToolCall, ToolUse: &providers.ToolUse{ID: "tu_1", Name: "Bash", Input: json.RawMessage(`{"command":"id"}`)}},
		{Type: providers.EventDone, StopReason: "tool_use", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}},
	}
	done := []providers.Event{{Type: providers.EventDone, StopReason: "end_turn", Usage: &providers.Usage{InputTokens: 1, OutputTokens: 1}}}
	st, err := storesqlite.Open(filepath.Join(t.TempDir(), "aclcfg.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.OperatorTokenDefCreate(context.Background(), store.OperatorTokenDefRow{
		DefID: "def_worker", Name: "u-def_worker", TenantID: "acme", Subject: "dev",
		TokenHash: auth.HashToken(testTokenPepper, workerBearer), AllowedScopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead},
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, &stubResolver{p: &scriptedProvider{scripts: [][]providers.Event{callBash}, defaultS: done}}, []tools.Tool{namedStubTool{"Read"}, namedStubTool{"Bash"}}, concurrency.New(8, 8, 5*time.Second), st)
	srv.SetACLResolver(aclres.New(st, authz.ModeCompat, 0))
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)

	if code, body := bearerCall(t, ts, workerBearer, "GET", "/v1/_me", ""); code != 200 || !strings.Contains(body, `"groups":["support"]`) {
		t.Errorf("whoami: %d %s", code, body)
	}
	body := `{"agent":"worker","agent_id":"a_c1","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`
	if code, out := bearerCall(t, ts, workerBearer, "POST", "/v1/runs", body); code != 200 {
		t.Fatalf("status %d: %s", code, out)
	}
	evs := runEvents(t, st, "a_c1")
	if !hasEvent(evs, "capability_inert", `"gate":"acl"`) || hasEvent(evs, "tool_result", "Bash ran") {
		t.Errorf("the yaml deny should withhold Bash; events: %s", aclEventTypes(evs))
	}
	// The management surface lists the config list as read-only context:
	// an operator sees it in the catalog of names only through the store, so
	// the stored view stays empty here, and explain names the yaml list.
	if code, out := bearerCall(t, ts, workerBearer, "POST", "/v1/_authz/explain", `{"action":{"name":"invoke"},"resource":{"type":"tool","id":"Bash"}}`); code != 200 || !strings.Contains(out, `"acl":"no-shell"`) {
		t.Errorf("explain: %d %s", code, out)
	}
}
