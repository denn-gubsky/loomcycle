package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// TestUserSelfServiceTools_SubsetOfTenantConfinable pins the RFC CN invariant that
// an isolated user's allowlist is a strict subset of a tenant session's surface —
// every entry must also be tenant-confinable and dispatchable, so the isolated
// tier can never reach a tool a tenant session cannot.
func TestUserSelfServiceTools_SubsetOfTenantConfinable(t *testing.T) {
	for name := range userSelfServiceTools {
		if !tenantConfinableTools[name] {
			t.Errorf("userSelfServiceTools has %q which is not tenant-confinable (must be a subset)", name)
		}
		if _, ok := handlersByName[name]; !ok {
			t.Errorf("userSelfServiceTools has %q which is not dispatchable (stale entry)", name)
		}
	}
}

// TestPrincipalMayCallTool_IsolatedUser: an isolated substrate:user session may
// call ONLY the RFC CN self-service tools (credentialdef) — not the other
// tenant-confinable tools (document/agentdef/memory/spawn_run) and not admin tools.
func TestPrincipalMayCallTool_IsolatedUser(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(),
		auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeUser}})

	if !principalMayCallTool(ctx, "credentialdef") {
		t.Error("isolated user must be allowed credentialdef (RFC CN self-service)")
	}
	denied := []string{"document", "agentdef", "memory", "spawn_run", "channel", "path", "operatortokendef", "restore_snapshot"}
	for _, name := range denied {
		if principalMayCallTool(ctx, name) {
			t.Errorf("isolated user must NOT be allowed %q (only self-service tools)", name)
		}
	}
}

// credCapturingConnector records the CredentialDef input actually forwarded to the
// tool, embedding mockConnector for every other Connector method.
type credCapturingConnector struct {
	*mockConnector
	lastInput string
	called    bool
}

func (c *credCapturingConnector) CredentialDef(_ context.Context, in json.RawMessage) (connector.ToolResult, error) {
	c.lastInput = string(in)
	c.called = true
	return connector.ToolResult{Text: `{"stored":true}`}, nil
}

// TestHandleCredentialDef_ConfinesIsolatedToUserScope drives handleCredentialDef
// through the two principal classes: an isolated user's omitted scope reaches the
// tool as scope=user and its scope=tenant is refused before the tool; a tenant
// principal is unconstrained (scope=tenant passes through untouched).
func TestHandleCredentialDef_ConfinesIsolatedToUserScope(t *testing.T) {
	call := func(scopes []string, body string) (fwd string, called, isErr bool) {
		cc := &credCapturingConnector{mockConnector: &mockConnector{}}
		env := &handlerEnv{connector: cc}
		ctx := auth.WithPrincipal(context.Background(),
			auth.Principal{TenantID: "acme", Subject: "alice", Scopes: scopes})
		res, err := handleCredentialDef(ctx, env, json.RawMessage(body))
		if err != nil {
			t.Fatalf("handleCredentialDef returned a Go error: %v", err)
		}
		return cc.lastInput, cc.called, res.IsError
	}

	// Isolated user, omitted scope → the tool receives scope=user.
	fwd, called, isErr := call([]string{auth.ScopeUser}, `{"op":"create","name":"telegram","value":"x"}`)
	if !called || isErr {
		t.Fatalf("isolated omitted: called=%v isErr=%v, want tool called ok", called, isErr)
	}
	if !strings.Contains(fwd, `"scope":"user"`) {
		t.Errorf("isolated omitted scope not pinned to user before the tool: %s", fwd)
	}

	// Isolated user, scope=tenant → refused (tool-error), tool NOT called.
	fwd, called, isErr = call([]string{auth.ScopeUser}, `{"op":"create","name":"t","scope":"tenant","value":"x"}`)
	if !isErr {
		t.Error("isolated scope=tenant: want an IsError tool result")
	}
	if called {
		t.Error("isolated scope=tenant reached the tool; the guard should refuse first")
	}

	// Tenant principal, scope=tenant → unconstrained (forwarded untouched).
	fwd, called, isErr = call([]string{auth.ScopeTenant}, `{"op":"create","name":"shared","scope":"tenant","value":"x"}`)
	if !called || isErr {
		t.Fatalf("tenant scope=tenant: called=%v isErr=%v, want tool called ok", called, isErr)
	}
	if !strings.Contains(fwd, `"scope":"tenant"`) {
		t.Errorf("tenant principal's scope=tenant was altered: %s", fwd)
	}
}
