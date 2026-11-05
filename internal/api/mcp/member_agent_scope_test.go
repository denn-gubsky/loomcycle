package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// agentNameConnector records the agent name each memory / path / document
// call reached the connector with.
type agentNameConnector struct {
	*mockConnector
	reached []string
}

func (c *agentNameConnector) note(ctx context.Context) (connector.ToolResult, error) {
	c.reached = append(c.reached, tools.AgentName(ctx))
	return connector.ToolResult{Text: `{}`}, nil
}
func (c *agentNameConnector) Memory(ctx context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return c.note(ctx)
}
func (c *agentNameConnector) Path(ctx context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return c.note(ctx)
}
func (c *agentNameConnector) Document(ctx context.Context, _ json.RawMessage) (connector.ToolResult, error) {
	return c.note(ctx)
}

// RFC FY Part A: mcpPrincipalCtx stamps one synthetic agent name for every
// principal, so its agent scope is one scope the whole tenant shares. A
// member has no such scope: its agent-scope memory, path and document calls
// are refused before they reach the tool, and its other calls carry no agent
// name. The operator plane keeps its scope.
func TestMCPDataTools_AMemberHasNoSharedAgentScope(t *testing.T) {
	member := auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}}
	op := auth.Principal{TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant}}
	call := func(cc *agentNameConnector, p auth.Principal, tool, args string) bool {
		res, err := handlersByName[tool](auth.WithPrincipal(context.Background(), p), &handlerEnv{connector: cc}, json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s %s: %v", tool, args, err)
		}
		return res.IsError
	}
	for _, c := range []struct{ tool, args string }{
		{"document", `{"op":"query_documents","scope":"agent"}`},
		{"document", `{"op":"move_documents","scope":"user","to_scope":"agent","document_ids":["d"]}`},
		{"path", `{"op":"ls","path":"/"}`}, // the Path tool's default scope is agent
		{"path", `{"op":"mkdir","scope":"agent","path":"/x"}`},
		{"memory", `{"op":"set","scope":"agent","key":"k","value":"v"}`},
	} {
		cc := &agentNameConnector{mockConnector: &mockConnector{}}
		if !call(cc, member, c.tool, c.args) || len(cc.reached) != 0 {
			t.Errorf("member %s %s: not refused before the tool (reached with %q)", c.tool, c.args, cc.reached)
		}
		if call(cc, op, c.tool, c.args) || len(cc.reached) != 1 || cc.reached[0] != operatorAgentName {
			t.Errorf("operator %s %s: reached with %q, want %q", c.tool, c.args, cc.reached, operatorAgentName)
		}
	}
	cc := &agentNameConnector{mockConnector: &mockConnector{}}
	for _, c := range []struct{ tool, args string }{
		{"document", `{"op":"query_documents","scope":"user"}`},
		{"path", `{"op":"ls","scope":"user","path":"/"}`},
		{"memory", `{"op":"get","scope":"user","key":"k"}`},
	} {
		if call(cc, member, c.tool, c.args) {
			t.Errorf("member %s %s: refused", c.tool, c.args)
		}
	}
	if len(cc.reached) != 3 {
		t.Fatalf("member calls reached the tool %d times, want 3", len(cc.reached))
	}
	for _, name := range cc.reached {
		if name != "" {
			t.Errorf("a member's call reached the tool with agent name %q, want none", name)
		}
	}
}
