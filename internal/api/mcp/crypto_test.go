package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// cryptoCapturingConnector records what a `crypto` call reaches the connector
// with — the input and the principal and identity on its ctx — and answers
// with result/err.
type cryptoCapturingConnector struct {
	*mockConnector
	calls     int
	input     string
	principal auth.Principal
	hasPrinc  bool
	identity  tools.RunIdentityValue
	result    connector.ToolResult
	err       error
}

func (c *cryptoCapturingConnector) Crypto(ctx context.Context, in json.RawMessage) (connector.ToolResult, error) {
	c.calls++
	c.input = string(in)
	c.principal, c.hasPrinc = auth.PrincipalFromContext(ctx)
	c.identity = tools.RunIdentity(ctx)
	return c.result, c.err
}

const cryptoArgs = `{"op":"encrypt","key":"field-key","plaintext":"4111 1111 1111 1111","context":"customers/42/card"}`

const cryptoCallFrame = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"crypto","arguments":` + cryptoArgs + `}}` + "\n"

// TestCryptoMCP_ATenantOperatorReachesTheConnector — a substrate:tenant
// session lists `crypto` and its call reaches connector.Crypto as sent, with
// the session's principal on ctx (the connector takes the tenant and user
// keys from it) and the identity mcpPrincipalCtx stamps from it.
func TestCryptoMCP_ATenantOperatorReachesTheConnector(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	if _, listed := listedTools(t, ctx)["crypto"]; !listed {
		t.Fatal("crypto is not listed for a tenant operator")
	}
	cc := &cryptoCapturingConnector{mockConnector: &mockConnector{}, result: connector.ToolResult{Text: `{"ciphertext":"lc1.x"}`}}
	srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
	resps, _ := driveServerCtx(t, srv, ctx, cryptoCallFrame)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("responses = %+v", resps)
	}
	if cc.calls != 1 {
		t.Fatalf("connector called %d times, want 1", cc.calls)
	}
	var sent, want any
	_ = json.Unmarshal([]byte(cc.input), &sent)
	_ = json.Unmarshal([]byte(cryptoArgs), &want)
	if a, b := mustJSON(t, sent), mustJSON(t, want); a != b {
		t.Errorf("the connector received %s, want %s", a, b)
	}
	if !cc.hasPrinc || cc.principal.TenantID != "acme" || cc.principal.Subject != "alice" {
		t.Errorf("the connector saw principal %+v (present %v), want the session's acme/alice", cc.principal, cc.hasPrinc)
	}
	if cc.identity.TenantID != "acme" || cc.identity.UserID != "alice" {
		t.Errorf("the connector saw identity %+v, want acme/alice from the principal", cc.identity)
	}
	if !strings.Contains(string(resps[0].Result), `lc1.x`) {
		t.Errorf("the result does not carry the tool's answer: %s", resps[0].Result)
	}
}

// TestCryptoMCP_OnlyAnOperatorMayCallIt — the tool uses the TENANT's keys, so
// it is an operator's, like POST /v1/_crypto: a member token (which reaches
// /v1/_mcp without substrate:tenant, whatever else it holds) must not see or
// call it — a member who could decrypt with the tenant's keys could read
// whatever the tenant sealed. Nor may an isolated user (its session is for its
// own credentials only), or a credential held to the run plane. A refused call
// never reaches the connector.
func TestCryptoMCP_OnlyAnOperatorMayCallIt(t *testing.T) {
	for _, c := range []struct {
		name      string
		principal *auth.Principal
		allowed   bool
		wantScope string
	}{
		{"no principal (the stdio operator)", nil, true, ""},
		{"an admin", &auth.Principal{TenantID: "acme", Subject: "root", Scopes: []string{auth.ScopeAdmin}}, true, ""},
		{"a tenant operator", &auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}, true, ""},
		{"a member that may create and read runs", &auth.Principal{TenantID: "acme", Subject: "bob",
			Scopes: []string{auth.ScopeRunsCreate, auth.ScopeRunsRead, auth.ScopeChannelPublish, auth.ScopeChannelRead}}, false, auth.ScopeTenant},
		{"a member with no scope", &auth.Principal{TenantID: "acme", Subject: "bob"}, false, auth.ScopeTenant},
		{"an isolated user", &auth.Principal{TenantID: "acme", Subject: "carol", Scopes: []string{auth.ScopeUser}}, false, auth.ScopeTenant},
		{"a tenant operator held to the run plane", &auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant},
			Restrictions: auth.Restrictions{Agents: []string{"score"}}}, false, ""},
		{"an admin held to the run plane", &auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeAdmin},
			Restrictions: auth.Restrictions{Agents: []string{"score"}}}, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			if c.principal != nil {
				ctx = auth.WithPrincipal(ctx, *c.principal)
			}
			if _, listed := listedTools(t, ctx)["crypto"]; listed != c.allowed {
				t.Errorf("crypto listed = %v, want %v", listed, c.allowed)
			}
			cc := &cryptoCapturingConnector{mockConnector: &mockConnector{}, result: connector.ToolResult{Text: `{"ciphertext":"lc1.x"}`}}
			srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
			resps, _ := driveServerCtx(t, srv, ctx, cryptoCallFrame)
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
				t.Fatalf("a refused call: error %+v after %d connector calls, want forbidden and none", resps[0].Error, cc.calls)
			}
			if c.wantScope != "" && !strings.HasSuffix(resps[0].Error.Message, "requires "+c.wantScope) {
				t.Errorf("the refusal says %q, want it to name %s", resps[0].Error.Message, c.wantScope)
			}
		})
	}
}

// rateSpentError stands in for the server's rate refusal: runner.ErrRateLimited
// to errors.Is, carrying how long to wait.
type rateSpentError struct{ wait time.Duration }

func (e *rateSpentError) Error() string {
	return "rate_limited: starting runs too often; retry in 7s"
}
func (e *rateSpentError) Is(target error) bool      { return target == runner.ErrRateLimited }
func (e *rateSpentError) RetryDelay() time.Duration { return e.wait }

// TestCryptoMCP_ASpentRateIsAToolError — a call past the token's rate comes
// back as a failed tool result, not a transport error: it says the token's
// rate is spent, is classified as retryable, and says when to retry.
func TestCryptoMCP_ASpentRateIsAToolError(t *testing.T) {
	cc := &cryptoCapturingConnector{mockConnector: &mockConnector{}, err: &rateSpentError{wait: 7 * time.Second}}
	srv := New(Config{Connector: cc, Logf: func(string, ...any) {}})
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}})
	resps, _ := driveServerCtx(t, srv, ctx, cryptoCallFrame)
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
	text := res.Content[0].Text
	if !strings.Contains(text, "rate") || !strings.Contains(text, "spent") || !strings.Contains(text, "retry in 7s") {
		t.Errorf("the refusal %q does not say the token's rate is spent and when to retry", text)
	}
	var se loommcp.StructuredError
	if err := json.Unmarshal(res.StructuredContent, &se); err != nil {
		t.Fatalf("structured content %s: %v", res.StructuredContent, err)
	}
	if se.ErrorCategory != string(tools.CategoryTransient) || !se.IsRetryable {
		t.Errorf("classified %+v, want a retryable transient refusal", se)
	}
	if se.RetryAfterSeconds == nil || *se.RetryAfterSeconds != 7 {
		t.Errorf("retryAfterSeconds = %v, want 7", se.RetryAfterSeconds)
	}
}

// TestCryptoMCP_SchemaIsTheToolsPlusTenant — the advertised schema is the
// Crypto tool's own (every property, the op enum, op required) with one
// addition: the admin-only top-level tenant the operator API takes.
func TestCryptoMCP_SchemaIsTheToolsPlusTenant(t *testing.T) {
	var got, base struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	var schema json.RawMessage
	for _, td := range toolDescriptors() {
		if td.Name == "crypto" {
			schema = td.InputSchema
		}
	}
	if schema == nil {
		t.Fatal("no crypto tool in the MCP surface")
	}
	if err := json.Unmarshal(schema, &got); err != nil {
		t.Fatalf("crypto schema is not valid JSON: %v", err)
	}
	if err := json.Unmarshal((&builtin.Crypto{}).InputSchema(), &base); err != nil {
		t.Fatal(err)
	}
	if len(base.Properties) < 20 {
		t.Fatalf("parsed only %d builtin properties — the schema shape changed", len(base.Properties))
	}
	for name, prop := range base.Properties {
		if !bytes.Equal(compactJSON(t, got.Properties[name]), compactJSON(t, prop)) {
			t.Errorf("property %q differs from the builtin's: %s", name, got.Properties[name])
		}
	}
	if len(got.Properties) != len(base.Properties)+1 {
		t.Errorf("schema has %d properties, want the builtin's %d plus tenant", len(got.Properties), len(base.Properties))
	}
	var tenant struct {
		Type        string `json:"type"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(got.Properties["tenant"], &tenant); err != nil || tenant.Type != "string" ||
		!strings.Contains(strings.ToLower(tenant.Description), "admin") {
		t.Errorf("tenant property = %s, want an admin-only string", got.Properties["tenant"])
	}
	if strings.Join(got.Required, ",") != "op" {
		t.Errorf("required = %v, want [op] (tenant stays optional)", got.Required)
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	if raw == nil {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.Bytes()
}
