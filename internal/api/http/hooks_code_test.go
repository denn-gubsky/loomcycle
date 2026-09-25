package http

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/hooks"
	"github.com/denn-gubsky/loomcycle/internal/hooks/codehook"
)

// A code body is refused unless the operator enabled code hooks, and the
// refusal says how.
func TestConnector_RegisterHook_ACodeBodyNeedsCodeHooksEnabled(t *testing.T) {
	s := minimalServer(t)
	_, err := s.RegisterHook(t.Context(), connector.RegisterHookRequest{Owner: "ops", Name: "g", Phase: "pre", Code: `function hook(ev) {}`})
	if !errors.Is(err, connector.ErrHookInvalidRegistration) || !strings.Contains(err.Error(), "LOOMCYCLE_CODE_HOOKS_ENABLED") {
		t.Fatalf("err = %v", err)
	}
	if len(s.hookRegistry.List()) != 0 {
		t.Error("the refused hook was registered")
	}
}

// With code hooks enabled a broken body is refused at registration, a good one
// is registered over POST /v1/hooks and listed with its body.
func TestHooksAPI_ACodeBodyIsCompiledAtRegistration(t *testing.T) {
	s := minimalServer(t)
	s.SetCodeHookRunner(codehook.New(nil))
	if _, err := s.RegisterHook(t.Context(), connector.RegisterHookRequest{Owner: "ops", Name: "g", Phase: "pre", Code: `function run() {}`}); !errors.Is(err, connector.ErrHookInvalidRegistration) {
		t.Fatalf("a body with no hook(ev): err = %v", err)
	}

	body := `{"owner":"ops","name":"g","phase":"pre","code":"function hook(ev) { return {decision: \"deny\", reason: \"no\"}; }"}`
	rec := httptest.NewRecorder()
	s.handleRegisterHook(rec, httptest.NewRequest("POST", "/v1/hooks", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	got := s.hookRegistry.List()
	if len(got) != 1 || !strings.Contains(got[0].Code, "hook(ev)") {
		t.Fatalf("registered %+v", got)
	}
	raw, _ := json.Marshal(got[0])
	if !strings.Contains(string(raw), `"code":`) {
		t.Errorf("the listed hook omits its body: %s", raw)
	}
}

// Installing the cluster-mode registry keeps code hooks running: the new
// dispatcher gets the runner too, whichever is wired first.
func TestServer_SetHookRegistryKeepsTheCodeRunner(t *testing.T) {
	s := minimalServer(t)
	s.SetCodeHookRunner(codehook.New(nil))
	reg := hooks.NewRegistry()
	s.SetHookRegistry(reg)
	if _, err := reg.Register(&hooks.Hook{Owner: "ops", Name: "g", Phase: hooks.PhasePre, Code: `function hook(ev) { return {decision: "deny", reason: "ran"}; }`}); err != nil {
		t.Fatal(err)
	}
	out := s.hookDispatcher.RunPre(t.Context(), hooks.Identity{Agent: "a"}, hooks.ToolCall{ID: "t1", Name: "Read"})
	if out.Deny == nil || out.Deny.Text != "ran" {
		t.Fatalf("outcome = %+v, want the code body to have run", out)
	}
}

// A tenant operator's code body is refused unless the operator also set
// LOOMCYCLE_CODE_HOOKS_TENANTS, and the refusal says so; an operator-global
// code hook needs only LOOMCYCLE_CODE_HOOKS_ENABLED. A body's memory is not
// bounded, and substrate:tenant may register hooks, so one tenant's body could
// exhaust the whole shared server.
func TestConnector_RegisterHook_ATenantCodeBodyNeedsTheTenantOptIn(t *testing.T) {
	s := minimalServer(t)
	s.SetCodeHookRunner(codehook.New(nil))
	body := `function hook(ev) {}`
	tenant := auth.WithPrincipal(t.Context(), auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}})

	_, err := s.RegisterHook(tenant, connector.RegisterHookRequest{Owner: "t", Name: "g", Phase: "pre", Code: body})
	if !errors.Is(err, connector.ErrHookInvalidRegistration) || !strings.Contains(err.Error(), "LOOMCYCLE_CODE_HOOKS_TENANTS=1") {
		t.Fatalf("tenant without the opt-in: err = %v", err)
	}
	if n := len(s.hookRegistry.List()); n != 0 {
		t.Fatalf("the refused hook was registered (%d hooks)", n)
	}
	if _, err := s.RegisterHook(t.Context(), connector.RegisterHookRequest{Owner: "op", Name: "g", Phase: "pre", Code: body}); err != nil {
		t.Fatalf("an operator-global code hook: %v", err)
	}

	cfg := *s.cfg()
	cfg.Env.CodeHooksTenants = true
	s.cfgHolder = config.NewHolder(&cfg)
	if _, err := s.RegisterHook(tenant, connector.RegisterHookRequest{Owner: "t", Name: "g", Phase: "pre", Code: body}); err != nil {
		t.Fatalf("tenant with the opt-in: %v", err)
	}
}
