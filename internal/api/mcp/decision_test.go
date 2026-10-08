package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// decisionCapturingConnector records the input and the identity a `decision`
// call reaches the connector with.
type decisionCapturingConnector struct {
	*mockConnector
	input    string
	identity tools.RunIdentityValue
	calls    int
}

func (c *decisionCapturingConnector) Decision(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	c.input = string(in)
	c.identity = tools.RunIdentity(ctx)
	c.calls++
	return connector.ToolResult{Text: `{"model":"decide","answers":{}}`}, nil
}

const decisionArgs = `{"state":{"ticket":"charged twice"},"questions":{"urgent":{"type":"noul","instructions":"Reply within the hour?"}}}`

func listedTools(t *testing.T, ctx context.Context) map[string]loommcp.ToolDescriptor {
	t.Helper()
	srv := New(Config{Connector: &mockConnector{}, Logf: func(string, ...any) {}})
	resps, _ := driveServerCtx(t, srv, ctx, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n")
	if len(resps) != 1 {
		t.Fatalf("got %d responses, want 1", len(resps))
	}
	var result loommcp.ToolsListResult
	if err := json.Unmarshal(resps[0].Result, &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]loommcp.ToolDescriptor{}
	for _, td := range result.Tools {
		out[td.Name] = td
	}
	return out
}

// TestDecisionMCP_GatedLikeStartingARun — `decision` is listed and callable for
// exactly the principals `spawn_run` is: a tenant token, not only an admin, and
// not an isolated user's self-service session. A refused principal's call never
// reaches the connector.
func TestDecisionMCP_GatedLikeStartingARun(t *testing.T) {
	for _, c := range []struct {
		name    string
		scopes  []string
		allowed bool
	}{
		{"a tenant token", []string{auth.ScopeTenant}, true},
		{"an admin token", []string{auth.ScopeAdmin}, true},
		{"an isolated user", []string{auth.ScopeUser}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: c.scopes})
			if got := principalMayCallTool(ctx, "spawn_run"); got != c.allowed {
				t.Fatalf("spawn_run allowed = %v for this principal; the case no longer describes the run gate", got)
			}
			_, listed := listedTools(t, ctx)["decision"]
			if listed != c.allowed {
				t.Errorf("decision listed = %v, want %v", listed, c.allowed)
			}
			cc := &decisionCapturingConnector{mockConnector: &mockConnector{}}
			srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
			in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"decision","arguments":` + decisionArgs + `}}` + "\n"
			resps, _ := driveServerCtx(t, srv, ctx, in)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			if c.allowed {
				if resps[0].Error != nil || cc.calls != 1 {
					t.Fatalf("an allowed call: error %+v after %d connector calls", resps[0].Error, cc.calls)
				}
				return
			}
			if resps[0].Error == nil || resps[0].Error.Code != mcpErrForbidden || cc.calls != 0 {
				t.Errorf("a refused call: error %+v after %d connector calls, want forbidden and none", resps[0].Error, cc.calls)
			}
		})
	}
}

// TestDecisionMCP_DispatchesUnderTheCallersPrincipal — the arguments reach the
// tool as sent, and the call carries the caller's own tenant and subject, which
// is what the provider key is resolved for.
func TestDecisionMCP_DispatchesUnderTheCallersPrincipal(t *testing.T) {
	cc := &decisionCapturingConnector{mockConnector: &mockConnector{}}
	srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"decision","arguments":` + decisionArgs + `}}` + "\n"
	resps, _ := driveServerCtx(t, srv, ctx, in)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("responses = %+v", resps)
	}
	if cc.identity.TenantID != "acme" || cc.identity.UserID != "alice" {
		t.Errorf("the call ran as tenant %q user %q, want acme / alice", cc.identity.TenantID, cc.identity.UserID)
	}
	var sent, want any
	_ = json.Unmarshal([]byte(cc.input), &sent)
	_ = json.Unmarshal([]byte(decisionArgs), &want)
	if a, b := mustJSON(t, sent), mustJSON(t, want); a != b {
		t.Errorf("the tool received %s, want %s", a, b)
	}
	if !strings.Contains(string(resps[0].Result), `\"model\":\"decide\"`) {
		t.Errorf("the result does not carry the tool's answer: %s", resps[0].Result)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestDecisionMCP_DescriptionAndSchemaAreTheBuiltins — an MCP client reads the
// description as the documentation, and descriptions written twice drift. This
// one is the builtin's own text, with only the help pointer rewritten for the
// transport; the schema is the builtin's bytes.
func TestDecisionMCP_DescriptionAndSchemaAreTheBuiltins(t *testing.T) {
	td, ok := listedTools(t, context.Background())["decision"]
	if !ok {
		t.Fatal("decision is not in the MCP surface")
	}
	tool := &builtin.Decision{}
	body, _, found := strings.Cut(tool.Description(), "Formats and worked examples:")
	if !found {
		t.Fatal("the builtin description no longer ends with its help pointer; update this test")
	}
	if !strings.HasPrefix(td.Description, body) {
		t.Errorf("the MCP description does not begin with the builtin's:\n mcp: %s\ntool: %s", td.Description, body)
	}
	// The pointer names the tool that serves help HERE, not the in-run name.
	if !strings.Contains(td.Description, "`context` tool") || strings.Contains(td.Description, "Context op=help") {
		t.Errorf("the MCP description's help pointer is not this transport's: %s", td.Description)
	}
	var got, want any
	if err := json.Unmarshal(td.InputSchema, &got); err != nil {
		t.Fatalf("the MCP input schema is not JSON: %v", err)
	}
	_ = json.Unmarshal(tool.InputSchema(), &want)
	if mustJSON(t, got) != mustJSON(t, want) {
		t.Errorf("the MCP input schema is not the builtin's: %s", td.InputSchema)
	}
}
