package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// calcCapturingConnector counts the `calc` calls that reach the connector and
// answers with result/err.
type calcCapturingConnector struct {
	*mockConnector
	calls  int
	input  string
	result connector.ToolResult
	err    error
}

func (c *calcCapturingConnector) Calc(_ context.Context, in json.RawMessage) (connector.ToolResult, error) {
	c.calls++
	c.input = string(in)
	return c.result, c.err
}

const calcCallFrame = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"calc","arguments":{"expr":"0.1 + 0.2"}}}` + "\n"

// TestCalcMCP_EveryCallerButARunPlaneTokenMayCallIt — calc is a pure function,
// so every class of principal that can open a session reaches it with no
// scope of its own: an admin, a tenant operator, a member whatever its scopes,
// and an isolated user. A credential held to the run plane does not: calc is
// not starting or reading a run. A refused call never reaches the connector.
func TestCalcMCP_EveryCallerButARunPlaneTokenMayCallIt(t *testing.T) {
	for _, c := range []struct {
		name      string
		principal *auth.Principal
		allowed   bool
	}{
		{"no principal (the stdio operator)", nil, true},
		{"an admin", &auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}, true},
		{"a tenant operator", &auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}, true},
		{"a member that may only read runs", &auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsRead}}, true},
		{"a member with no scope", &auth.Principal{TenantID: "acme", Subject: "bob"}, true},
		{"an isolated user", &auth.Principal{TenantID: "acme", Subject: "carol", Scopes: []string{auth.ScopeUser}}, true},
		{"a tenant operator held to the run plane", &auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant},
			Restrictions: auth.Restrictions{Agents: []string{"score"}}}, false},
		{"an admin held to the run plane", &auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeAdmin},
			Restrictions: auth.Restrictions{Agents: []string{"score"}}}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			if c.principal != nil {
				ctx = auth.WithPrincipal(ctx, *c.principal)
			}
			if _, listed := listedTools(t, ctx)["calc"]; listed != c.allowed {
				t.Errorf("calc listed = %v, want %v", listed, c.allowed)
			}
			cc := &calcCapturingConnector{mockConnector: &mockConnector{}, result: connector.ToolResult{Text: `{"result":0.3,"type":"number","exact":true,"text":"0.3"}`}}
			srv := New(Config{Connector: cc, Logger: discardLogger()})
			resps, _ := driveServerCtx(t, srv, ctx, calcCallFrame)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			if c.allowed {
				if resps[0].Error != nil || cc.calls != 1 {
					t.Fatalf("an allowed call: error %+v after %d connector calls", resps[0].Error, cc.calls)
				}
				if cc.input != `{"expr":"0.1 + 0.2"}` {
					t.Errorf("the connector received %s, want the arguments as sent", cc.input)
				}
				return
			}
			if resps[0].Error == nil || resps[0].Error.Code != mcpErrForbidden || cc.calls != 0 {
				t.Fatalf("a refused call: error %+v after %d connector calls, want forbidden and none", resps[0].Error, cc.calls)
			}
		})
	}
}

// TestCalcMCP_ASpentRateIsAToolError — a call past the token's run rate comes
// back as a failed, retryable tool result naming calc and when to retry.
func TestCalcMCP_ASpentRateIsAToolError(t *testing.T) {
	cc := &calcCapturingConnector{mockConnector: &mockConnector{}, err: &rateSpentError{wait: 7 * time.Second}}
	srv := New(Config{Connector: cc, Logger: discardLogger()})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob"})
	resps, _ := driveServerCtx(t, srv, ctx, calcCallFrame)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("responses = %+v, want one tool result", resps)
	}
	var res loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) != 1 {
		t.Fatalf("result = %+v, want one failed text block", res)
	}
	if text := res.Content[0].Text; !strings.HasPrefix(text, "calc: this token's rate is spent") || !strings.Contains(text, "retry in 7s") {
		t.Errorf("the refusal %q does not say calc spent the token's rate and when to retry", text)
	}
	var se loommcp.StructuredError
	if err := json.Unmarshal(res.StructuredContent, &se); err != nil || !se.IsRetryable {
		t.Errorf("structured content %s (%v), want a retryable refusal", res.StructuredContent, err)
	}
}

// TestCalcMCP_DescriptionPointsAtTheContextTool — the builtin's description
// sends the model to `Context op=help`, a tool an MCP client does not have; the
// meta-tool's must send it to the `context` meta-tool instead. Fails if the
// builtin's wording moves away from calcHelpInRun.
func TestCalcMCP_DescriptionPointsAtTheContextTool(t *testing.T) {
	if !strings.Contains((&builtin.Calc{}).Description(), calcHelpInRun) {
		t.Fatalf("the Calc builtin's description no longer contains %q; update calcHelpInRun", calcHelpInRun)
	}
	desc := calcDescription()
	if strings.Contains(desc, "Context op=help") {
		t.Errorf("the calc description still points at the in-run Context tool: %s", desc)
	}
	if !strings.Contains(desc, "call the `context` tool with {\"op\": \"help\", \"topic\": \"Calc\"}") {
		t.Errorf("the calc description does not point at the context meta-tool: %s", desc)
	}
}
