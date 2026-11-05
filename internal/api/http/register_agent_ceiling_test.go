package http

import (
	"context"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// register_agent writes a definition a run resolves exactly as an AgentDef
// (lookup.Agent), so a member's registration is held to the member ceiling an
// AgentDef create is (RFC FY Part B): it may not grant what it does not hold
// itself. Before, a member's session registered tenant crypto keys, statement
// methods, submit_any evaluation and git push, and could spawn the result.
func TestRegisterAgent_AMemberIsHeldToTheCapabilityCeiling(t *testing.T) {
	h := newReviewHarness(t)
	memberScopes, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	member := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes})
	operator := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	for _, c := range []struct {
		field string
		req   connector.RegisterAgentRequest
	}{
		{"crypto_scopes", connector.RegisterAgentRequest{CryptoScopes: []string{"tenant"}}},
		{"statement_methods", connector.RegisterAgentRequest{Provider: "code-js", StatementMethods: []string{"rule"}}},
		{"evaluation_scopes", connector.RegisterAgentRequest{EvaluationScopes: []string{"submit_any"}}},
		{"git_remote", connector.RegisterAgentRequest{GitRemote: []string{"fetch", "push"}}},
	} {
		for who, ctx := range map[string]context.Context{"member": member, "operator": operator} {
			req := c.req
			req.Name, req.SystemPrompt, req.Tools = "reg-"+c.field+"-"+who, "x", []string{"Read"}
			_, err := h.srv.RegisterAgent(ctx, req)
			if who == "member" && (err == nil || !strings.Contains(err.Error(), c.field)) {
				t.Errorf("a member registered %s: %v", c.field, err)
			}
			if who == "operator" && err != nil {
				t.Errorf("the tenant operator was refused %s: %v", c.field, err)
			}
		}
	}
	// What a member holds itself it may still register.
	if _, err := h.srv.RegisterAgent(member, connector.RegisterAgentRequest{Name: "reg-plain", SystemPrompt: "x", Tools: []string{"Read"}, MemoryScopes: []string{"user"}}); err != nil {
		t.Errorf("a member's plain registration was refused: %v", err)
	}
}

// A registered agent resolves BEFORE the tenant's AgentDefs (lookup.Agent), so
// a member registering the name of an operator's definition would take it
// over for every run of that name, a schedule's included. A non-operator may
// not register a name a definition holds; the operator still may.
func TestRegisterAgent_AMemberCannotShadowADefinedAgent(t *testing.T) {
	h := newReviewHarness(t)
	ctx := context.Background()
	row, err := h.st.AgentDefCreate(ctx, store.AgentDefRow{DefID: "def_linker", Name: "linker", Definition: []byte(`{"system_prompt":"links"}`), TenantID: "acme", OperatorAuthored: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.AgentDefSetActive(ctx, "acme", "linker", row.DefID, "ops"); err != nil {
		t.Fatal(err)
	}
	memberScopes, _ := auth.GrantableUserScopes("tenant")
	member := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "bob", Scopes: memberScopes})
	if _, err := h.srv.RegisterAgent(member, connector.RegisterAgentRequest{Name: "linker", SystemPrompt: "mine", Tools: []string{"Read"}}); err == nil {
		t.Error("a member registered over the tenant's linker definition")
	}
	operator := auth.WithPrincipal(ctx, auth.Principal{TenantID: "acme", Subject: "ops", Scopes: []string{auth.ScopeTenant}})
	if _, err := h.srv.RegisterAgent(operator, connector.RegisterAgentRequest{Name: "linker", SystemPrompt: "ops", Tools: []string{"Read"}}); err != nil {
		t.Errorf("the tenant operator was refused: %v", err)
	}
}
