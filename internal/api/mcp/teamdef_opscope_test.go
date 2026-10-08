package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
)

// teamRecorder is the mock connector, recording what reaches TeamDef.
type teamRecorder struct {
	*mockConnector
	mu     sync.Mutex
	inputs []string
}

func (c *teamRecorder) TeamDef(_ context.Context, in json.RawMessage) (connector.ToolResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inputs = append(c.inputs, string(in))
	return connector.ToolResult{Text: "{}"}, nil
}

func (c *teamRecorder) reached() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inputs)
}

const teamRunCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"teamdef","arguments":{"op":"run","name":"solo"}}}` + "\n"

// The stdio launcher has no principal: it is the operator, and its teamdef
// run reaches the connector as it always did.
func TestServer_TeamDefRun_StdioOperatorIsNotHeldToAScope(t *testing.T) {
	conn := &teamRecorder{mockConnector: &mockConnector{}}
	srv := New(Config{Connector: conn, Logf: func(string, ...any) {}})
	resps, _ := driveServerCtx(t, srv, context.Background(), teamRunCall)
	if len(resps) != 1 || resps[0].Error != nil {
		t.Fatalf("stdio teamdef run = %+v", resps)
	}
	if conn.reached() != 1 {
		t.Errorf("the connector was reached %d times, want 1", conn.reached())
	}
}

// A member session without runs:create is refused the run before the
// connector, with the op and the scope named; with it, the call goes through.
func TestServer_TeamDefRun_NeedsRunsCreateOnAMemberSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scopes  []string
		refused bool
	}{
		{"runs:read only", []string{auth.ScopeRunsRead}, true},
		{"no scopes", nil, true},
		{"runs:create", []string{auth.ScopeRunsCreate}, false},
		{"substrate:tenant", []string{auth.ScopeTenant}, false},
		{"substrate:admin", []string{auth.ScopeAdmin}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &teamRecorder{mockConnector: &mockConnector{}}
			srv := New(Config{Connector: conn, Logf: func(string, ...any) {}})
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: tc.scopes})
			resps, _ := driveServerCtx(t, srv, ctx, teamRunCall)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			if !tc.refused {
				if resps[0].Error != nil || conn.reached() != 1 {
					t.Fatalf("error=%+v, connector reached %d times; want the call through", resps[0].Error, conn.reached())
				}
				return
			}
			if resps[0].Error == nil || resps[0].Error.Code != mcpErrForbidden {
				t.Fatalf("teamdef run = %+v / %s, want mcpErrForbidden", resps[0].Error, resps[0].Result)
			}
			if got, want := resps[0].Error.Message, "teamdef: forbidden — op=run requires runs:create"; got != want {
				t.Errorf("refusal = %q, want %q", got, want)
			}
			if conn.reached() != 0 {
				t.Errorf("the connector was reached %d times on a refusal", conn.reached())
			}
		})
	}
}

// The per-op check is teamdef's alone: a tool whose input happens to carry
// op=run is not judged by it.
func TestToolOpMissingScope_OnlyJudgesTeamDef(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "bob", Scopes: []string{auth.ScopeRunsRead}})
	args := json.RawMessage(`{"op":"run","name":"x"}`)
	if op, scope := toolOpMissingScope(ctx, "teamdef", args); op != "run" || scope != auth.ScopeRunsCreate {
		t.Errorf("teamdef: op=%q scope=%q, want run / runs:create", op, scope)
	}
	for name := range tenantConfinableTools {
		if name == "teamdef" {
			continue
		}
		if op, scope := toolOpMissingScope(ctx, name, args); scope != "" {
			t.Errorf("%s: op=%q scope=%q, want it not judged", name, op, scope)
		}
	}
}
