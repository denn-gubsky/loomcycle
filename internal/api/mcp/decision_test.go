package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// decisionCapturingConnector records what a `decision` call reaches the
// connector with, and answers with result/err.
type decisionCapturingConnector struct {
	*mockConnector
	input  string
	calls  int
	result connector.ToolResult
	err    error
}

func (c *decisionCapturingConnector) Decision(_ context.Context, in json.RawMessage) (connector.ToolResult, error) {
	c.input = string(in)
	c.calls++
	return c.result, c.err
}

const decisionArgs = `{"state":{"ticket":"charged twice"},"questions":{"urgent":{"type":"noul","instructions":"Reply within the hour?"}}}`

const decisionCallFrame = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"decision","arguments":` + decisionArgs + `}}` + "\n"

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

// TestDecisionMCP_NeedsTheScopeThatCreatesARun — `decision` spends tokens, so
// it is listed and callable only for a principal that may create a run: a
// tenant operator, an admin, a member whose token carries runs:create. A
// member holding only runs:read passes the tenant-confinable allowlist (it is
// not isolated) and is still refused here; an isolated user's MCP session is
// for its own credentials only. A refused principal's call never reaches the
// connector.
func TestDecisionMCP_NeedsTheScopeThatCreatesARun(t *testing.T) {
	for _, c := range []struct {
		name    string
		scopes  []string
		allowed bool
	}{
		{"a tenant operator", []string{auth.ScopeTenant}, true},
		{"an admin", []string{auth.ScopeAdmin}, true},
		{"a member that may create runs", []string{auth.ScopeRunsCreate, auth.ScopeRunsRead}, true},
		{"a member that may only read runs", []string{auth.ScopeRunsRead}, false},
		{"an isolated user", []string{auth.ScopeUser}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: c.scopes})
			if _, listed := listedTools(t, ctx)["decision"]; listed != c.allowed {
				t.Errorf("decision listed = %v, want %v", listed, c.allowed)
			}
			cc := &decisionCapturingConnector{mockConnector: &mockConnector{}, result: connector.ToolResult{Text: `{"model":"decide","answers":{}}`}}
			srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
			resps, _ := driveServerCtx(t, srv, ctx, decisionCallFrame)
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

// TestDecisionMCP_NoPrincipalIsTheOperator — the stdio server and an open-mode
// session carry no principal: the operator, who may call every tool.
func TestDecisionMCP_NoPrincipalIsTheOperator(t *testing.T) {
	if _, listed := listedTools(t, context.Background())["decision"]; !listed {
		t.Error("decision is not listed for the operator")
	}
}

// TestDecisionMCP_PassesTheCallToTheConnector — the arguments reach the
// connector's run-less path as sent, and its answer is the tool's result.
func TestDecisionMCP_PassesTheCallToTheConnector(t *testing.T) {
	cc := &decisionCapturingConnector{mockConnector: &mockConnector{}, result: connector.ToolResult{Text: `{"model":"decide","answers":{}}`}}
	srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	resps, _ := driveServerCtx(t, srv, ctx, decisionCallFrame)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("responses = %+v", resps)
	}
	var sent, want any
	_ = json.Unmarshal([]byte(cc.input), &sent)
	_ = json.Unmarshal([]byte(decisionArgs), &want)
	if a, b := mustJSON(t, sent), mustJSON(t, want); a != b {
		t.Errorf("the connector received %s, want %s", a, b)
	}
	if !strings.Contains(string(resps[0].Result), `\"model\":\"decide\"`) {
		t.Errorf("the result does not carry the tool's answer: %s", resps[0].Result)
	}
}

// TestDecisionMCP_ABudgetRefusalIsAToolError — a caller at a hard token budget
// gets a failed tool result that names the refusal and is classified as one a
// retry cannot fix, not a transport error.
func TestDecisionMCP_ABudgetRefusalIsAToolError(t *testing.T) {
	cc := &decisionCapturingConnector{mockConnector: &mockConnector{},
		err: &connector.TokenLimitError{Info: providers.LimitInfo{Scope: "user", ScopeID: "alice", Severity: "hard", Used: 120, Limit: 100, Message: "user alice is over its monthly token budget"}}}
	srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
	resps, _ := driveServerCtx(t, srv, context.Background(), decisionCallFrame)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("responses = %+v, want one tool result", resps)
	}
	var res loommcp.CallToolResult
	if err := json.Unmarshal(resps[0].Result, &res); err != nil {
		t.Fatal(err)
	}
	if !res.IsError || len(res.Content) != 1 || !strings.Contains(res.Content[0].Text, "token_limit_exceeded") {
		t.Fatalf("result = %+v, want a failed result naming token_limit_exceeded", res)
	}
	var structured struct {
		Category  string `json:"errorCategory"`
		Retryable bool   `json:"isRetryable"`
	}
	_ = json.Unmarshal(res.StructuredContent, &structured)
	if structured.Category == "" || structured.Retryable {
		t.Errorf("structured = %s, want a classified, non-retryable failure", res.StructuredContent)
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
	// A caller with no run has to be told who pays.
	if !strings.Contains(td.Description, "charged to you") {
		t.Errorf("the MCP description does not say who is charged: %s", td.Description)
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
