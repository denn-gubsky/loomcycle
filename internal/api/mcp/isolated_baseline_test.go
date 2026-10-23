package mcp

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
)

// isolatedBaselineSet compiles the built-in isolated list as the resolver
// binds it to an isolated (substrate:user) principal.
func isolatedBaselineSet(t *testing.T) *authz.Effective {
	t.Helper()
	bl := authz.Baselines()
	eff, err := authz.Compile(authz.Input{
		Subject:  authz.Subject{TenantID: "acme", ID: "ivy"},
		Bindings: []authz.Binding{{ACL: authz.BaselineIsolated, SubjectKind: authz.SubjectTenant}},
		ACLs:     map[string]authz.ACLVersion{authz.BaselineIsolated: {ACL: bl[authz.BaselineIsolated]}},
		Now:      time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return eff
}

// The built-in isolated list must admit, in enforce mode, what the isolated
// user's scope gate already admits over MCP: the session itself (`use
// transport:mcp`) and each self-service tool. With neither, enforce refused an
// isolated user every MCP tool — the session it is let in for (RFC CN's
// credential self-service) included. The list names the tools one by one,
// so a tool added to userSelfServiceTools without its baseline grant fails
// here instead of being silently unreachable in enforce mode.
func TestIsolatedBaseline_AdmitsTheMCPSelfServiceSurfaceInEnforce(t *testing.T) {
	eff := isolatedBaselineSet(t)
	ctx := authz.WithTransport(authz.WithMode(context.Background(), authz.ModeEnforce), "mcp")

	session, governed := authz.HTTPSurface("POST", "/v1/_mcp")
	if !governed {
		t.Fatal("POST /v1/_mcp is expected to be ACL-governed")
	}
	if res := authz.EvaluateSurface(ctx, eff, session, true); !res.Allowed {
		t.Errorf("enforce: an isolated user may not open the MCP session (%s %s:%s): %+v", session.Action, session.Type, session.Name, res.Decision)
	}

	tools := make([]string, 0, len(userSelfServiceTools))
	for name := range userSelfServiceTools {
		tools = append(tools, name)
	}
	sort.Strings(tools)
	for _, name := range tools {
		scopes := []string{auth.ScopeUser}
		if need, ok := toolRequiredScope[name]; ok {
			scopes = append(scopes, need)
		}
		ivy := auth.Principal{TenantID: "acme", Subject: "ivy", Scopes: scopes, Effective: eff}
		if !principalMayCallTool(auth.WithPrincipal(ctx, ivy), name) {
			t.Errorf("enforce: an isolated user may not call its self-service tool %q", name)
		}
	}
}

// Narrowing, not widening: the grant is the self-service tools by name, so a
// tenant-confinable tool an isolated user's scope gate refuses stays refused by
// the baseline too (enforce asks the engine alone).
func TestIsolatedBaseline_GrantsNoOtherMCPTool(t *testing.T) {
	eff := isolatedBaselineSet(t)
	ctx := authz.WithTransport(authz.WithMode(context.Background(), authz.ModeEnforce), "mcp")
	for name := range tenantConfinableTools {
		if userSelfServiceTools[name] {
			continue
		}
		if res := authz.EvaluateSurface(ctx, eff, authz.MCPToolSurface(name), true); res.Allowed {
			t.Errorf("the isolated baseline allows invoke mcp-tool:%s, which an isolated user's scope gate refuses", name)
		}
	}
}
