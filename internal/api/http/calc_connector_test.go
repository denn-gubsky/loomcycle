package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// calcServer is a server holding only the Calc tool, with the per-token run
// rate limiter on.
func calcServer(t *testing.T) *Server {
	t.Helper()
	s, _ := tokenAuthServer(t, "")
	s.tools = []tools.Tool{&builtin.Calc{}}
	s.runRate = newRunRateLimiter(nil)
	return s
}

// The connector answers with the in-run tool's own result: exact decimal
// arithmetic, not a float sum.
func TestServerCalc_AnswersWithTheToolsResult(t *testing.T) {
	s := calcServer(t)
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob"})
	res, err := s.Calc(ctx, []byte(`{"expr":"0.1 + 0.2"}`))
	if err != nil || res.IsError {
		t.Fatalf("Calc = %+v, %v", res, err)
	}
	if !strings.Contains(res.Text, `"result":0.3`) || !strings.Contains(res.Text, `"exact":true`) {
		t.Errorf("Calc text = %s, want result 0.3 exact", res.Text)
	}
}

// Each call takes one unit of the token's run rate, as Crypto does: a token
// rated at a burst of two gets two answers and then a rate refusal, which
// never reaches the tool.
func TestServerCalc_EachCallTakesOneUnitOfTheTokensRunRate(t *testing.T) {
	s := calcServer(t)
	p := auth.Principal{TenantID: "acme", Subject: "rita",
		Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1, Burst: 2}}}
	ctx := auth.WithPrincipal(context.Background(), p)
	for i := 0; i < 2; i++ {
		if res, err := s.Calc(ctx, []byte(`{"expr":"1 + 1"}`)); err != nil || res.IsError {
			t.Fatalf("call %d within the rate = %+v, %v", i+1, res, err)
		}
	}
	res, err := s.Calc(ctx, []byte(`{"expr":"1 + 1"}`))
	if !errors.Is(err, runner.ErrRateLimited) {
		t.Fatalf("call past the rate = %+v, %v; want runner.ErrRateLimited", res, err)
	}
}

// callCalcOverMCP sends one `calc` call through the MCP server backed by s,
// as principal p, and returns the JSON-RPC error (nil when the call was
// admitted) and the tool result.
func callCalcOverMCP(t *testing.T, s *Server, p auth.Principal, args string) (*loommcp.Error, loommcp.CallToolResult) {
	t.Helper()
	mcpSrv := lcmcp.New(lcmcp.Config{Connector: s, Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithTimeout(auth.WithPrincipal(context.Background(), p), 10*time.Second)
	defer cancel()
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"calc","arguments":` + args + `}}` + "\n"
	var stdout bytes.Buffer
	if err := mcpSrv.Serve(ctx, strings.NewReader(in), &stdout); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp loommcp.Response
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", stdout.String(), err)
	}
	var res loommcp.CallToolResult
	if resp.Error == nil {
		if err := json.Unmarshal(resp.Result, &res); err != nil {
			t.Fatalf("decode tool result: %v", err)
		}
	}
	return resp.Error, res
}

// Over MCP, a tenant operator, a member with no scope and an isolated user
// each get the evaluator's own answer from the calc meta-tool; a token held to
// the run plane is refused before the tool runs.
func TestCalcMCP_TenantMemberAndIsolatedUserGetTheEvaluatorsAnswer(t *testing.T) {
	for _, c := range []struct {
		name string
		p    auth.Principal
	}{
		{"a tenant operator", auth.Principal{TenantID: "acme", Subject: "alice", Scopes: []string{auth.ScopeTenant}}},
		{"a member", auth.Principal{TenantID: "acme", Subject: "bob"}},
		{"an isolated user", auth.Principal{TenantID: "acme", Subject: "carol", Scopes: []string{auth.ScopeUser}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rpcErr, res := callCalcOverMCP(t, calcServer(t), c.p, `{"expr":"0.1 + 0.2"}`)
			if rpcErr != nil || res.IsError || len(res.Content) != 1 {
				t.Fatalf("calc = %+v, %+v; want an answer", rpcErr, res)
			}
			var out struct {
				Result json.Number `json:"result"`
				Exact  bool        `json:"exact"`
			}
			if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil || out.Result != "0.3" || !out.Exact {
				t.Errorf("calc answered %s (%v), want 0.3 exact", res.Content[0].Text, err)
			}
		})
	}
	t.Run("a token held to the run plane", func(t *testing.T) {
		p := auth.Principal{TenantID: "acme", Subject: "dev", Scopes: []string{auth.ScopeTenant},
			Restrictions: auth.Restrictions{Agents: []string{"score"}}}
		rpcErr, res := callCalcOverMCP(t, calcServer(t), p, `{"expr":"0.1 + 0.2"}`)
		if rpcErr == nil {
			t.Fatalf("calc = %+v, want a refusal", res)
		}
	})
}

// A refusal of the expression comes back as the in-run tool's: a failed tool
// result whose text carries the code and hint, with the category in
// structuredContent.
func TestCalcMCP_ARefusalCarriesItsCategoryAndHint(t *testing.T) {
	rpcErr, res := callCalcOverMCP(t, calcServer(t), auth.Principal{TenantID: "acme", Subject: "bob"}, `{"expr":"2**3"}`)
	if rpcErr != nil || !res.IsError || len(res.Content) != 1 {
		t.Fatalf("calc = %+v, %+v; want a failed tool result", rpcErr, res)
	}
	if text := res.Content[0].Text; !strings.Contains(text, `"error":"syntax"`) || !strings.Contains(text, "power is `^`") {
		t.Errorf("refusal text %s, want the syntax code and the power hint", text)
	}
	var se loommcp.StructuredError
	if err := json.Unmarshal(res.StructuredContent, &se); err != nil {
		t.Fatalf("structured content %s: %v", res.StructuredContent, err)
	}
	if se.ErrorCategory != string(tools.CategoryValidation) || !strings.Contains(se.Description, "power is `^`") {
		t.Errorf("structured error = %+v, want a validation refusal with the hint", se)
	}
}

// Each MCP call takes a unit of the token's run rate: past it the call is a
// failed, retryable tool result naming calc.
func TestCalcMCP_PastTheTokensRunRateIsRefused(t *testing.T) {
	s := calcServer(t)
	p := auth.Principal{TenantID: "acme", Subject: "rita",
		Restrictions: auth.Restrictions{Rate: &auth.RateLimit{PerMinute: 1, Burst: 1}}}
	if rpcErr, res := callCalcOverMCP(t, s, p, `{"expr":"1 + 1"}`); rpcErr != nil || res.IsError {
		t.Fatalf("first call = %+v, %+v; want an answer", rpcErr, res)
	}
	rpcErr, res := callCalcOverMCP(t, s, p, `{"expr":"1 + 1"}`)
	if rpcErr != nil || !res.IsError || len(res.Content) != 1 || !strings.HasPrefix(res.Content[0].Text, "calc: this token's rate is spent") {
		t.Fatalf("second call = %+v, %+v; want calc's rate refusal", rpcErr, res)
	}
}
