package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

const deleteTreeCallFrame = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_run_tree","arguments":{"run_id":"r_x"}}}` + "\n"

// delete_run_tree needs runs:delete: an admin and a tenant operator hold it, a
// member only by explicit grant. A session without it neither lists the tool
// nor reaches the connector with a call. An isolated user's session is
// confined to its own credentials whatever its token was granted.
func TestDeleteRunTreeMCP_NeedsTheRunsDeleteScope(t *testing.T) {
	member, err := auth.GrantableUserScopes("tenant")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		scopes    []string
		allowed   bool
		wantScope string
	}{
		{"a tenant operator", []string{auth.ScopeTenant}, true, ""},
		{"an admin", []string{auth.ScopeAdmin}, true, ""},
		{"a member granted runs:delete", append(append([]string{}, member...), auth.ScopeRunsDelete), true, ""},
		{"a member with its delegated scopes", member, false, auth.ScopeRunsDelete},
		{"a member that may only read runs", []string{auth.ScopeRunsRead}, false, auth.ScopeRunsDelete},
		{"an isolated user", []string{auth.ScopeUser}, false, auth.ScopeRunsDelete},
		// It holds the scope; the tool is outside an isolated session.
		{"an isolated user granted runs:delete", []string{auth.ScopeUser, auth.ScopeRunsDelete}, false, auth.ScopeAdmin},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := auth.WithPrincipal(context.Background(), auth.Principal{TenantID: "acme", Subject: "alice", Scopes: c.scopes})
			if _, listed := listedTools(t, ctx)["delete_run_tree"]; listed != c.allowed {
				t.Errorf("delete_run_tree listed = %v, want %v", listed, c.allowed)
			}
			mc := &mockConnector{}
			srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
			resps, _ := driveServerCtx(t, srv, ctx, deleteTreeCallFrame)
			if len(resps) != 1 {
				t.Fatalf("got %d responses, want 1", len(resps))
			}
			_, called := mc.deleteTreeReq.Load().(connector.DeleteRunTreeRequest)
			if c.allowed {
				if resps[0].Error != nil || !called {
					t.Fatalf("an allowed call: error %+v, connector called = %v", resps[0].Error, called)
				}
				return
			}
			if resps[0].Error == nil || resps[0].Error.Code != mcpErrForbidden || called {
				t.Fatalf("a refused call: error %+v, connector called = %v; want forbidden and no call", resps[0].Error, called)
			}
			if !strings.HasSuffix(resps[0].Error.Message, "requires "+c.wantScope) {
				t.Errorf("the refusal says %q, want it to name %s", resps[0].Error.Message, c.wantScope)
			}
		})
	}
}

func TestDeleteRunTreeMCP_PassesTheIDAndReturnsTheCounts(t *testing.T) {
	n := 2
	mc := &mockConnector{deleteTreeResult: connector.RunTreeDeletion{
		RunID: "r_x", DeletedRuns: 3,
		Counts:     map[string]int{"runs": 3, "events": 12},
		Kept:       map[string]string{"usage_ledger": "kept"},
		NotReached: []connector.NotReached{{Name: "snapshots", Count: &n, Reason: "a copy"}},
	}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})

	call := func(args string) loommcp.CallToolResult {
		t.Helper()
		resps, _ := driveServer(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_run_tree","arguments":`+args+`}}`+"\n")
		if len(resps) != 1 || resps[0].Error != nil {
			t.Fatalf("delete_run_tree %s: %+v", args, resps)
		}
		var res loommcp.CallToolResult
		if err := json.Unmarshal(resps[0].Result, &res); err != nil {
			t.Fatal(err)
		}
		return res
	}

	res := call(`{"run_id":"r_x"}`)
	if got, _ := mc.deleteTreeReq.Load().(connector.DeleteRunTreeRequest); got.RunID != "r_x" || got.SessionID != "" {
		t.Errorf("connector saw %+v, want run_id r_x", got)
	}
	var out connector.RunTreeDeletion
	if res.IsError || json.Unmarshal([]byte(res.Content[0].Text), &out) != nil {
		t.Fatalf("result = %+v, want the deletion as JSON", res)
	}
	if out.DeletedRuns != 3 || out.Counts["events"] != 12 || len(out.NotReached) != 1 || *out.NotReached[0].Count != 2 {
		t.Errorf("result = %+v, want the connector's counts and what it did not reach", out)
	}

	call(`{"session_id":"s_y"}`)
	if got, _ := mc.deleteTreeReq.Load().(connector.DeleteRunTreeRequest); got.SessionID != "s_y" || got.RunID != "" {
		t.Errorf("connector saw %+v, want session_id s_y", got)
	}

	// Exactly one id: refused before the connector, as a tool error.
	mc.deleteTreeReq.Store(connector.DeleteRunTreeRequest{RunID: "untouched"})
	for _, args := range []string{`{}`, `{"run_id":"r_x","session_id":"s_y"}`} {
		if res := call(args); !res.IsError {
			t.Errorf("delete_run_tree %s was accepted", args)
		}
	}
	if got, _ := mc.deleteTreeReq.Load().(connector.DeleteRunTreeRequest); got.RunID != "untouched" {
		t.Errorf("the connector was called for a malformed request: %+v", got)
	}
}

// A live run in the tree is a refusal the caller can act on, not a failure.
func TestDeleteRunTreeMCP_TellsTheCallerToCancelALiveRunFirst(t *testing.T) {
	mc := &mockConnector{deleteTreeErr: statusCodeErr{409, "run r_live is still live (running, awaiting input, or paused); cancel it first"}}
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	resps, _ := driveServer(t, srv, deleteTreeCallFrame)
	var res loommcp.CallToolResult
	if len(resps) != 1 || resps[0].Error != nil || json.Unmarshal(resps[0].Result, &res) != nil {
		t.Fatalf("responses = %+v", resps)
	}
	if !res.IsError || !strings.Contains(res.Content[0].Text, "r_live") || !strings.Contains(string(res.StructuredContent), "cancel_run") {
		t.Errorf("result = %+v (structured %s); want a tool error naming the run and cancel_run", res, res.StructuredContent)
	}
}

type statusCodeErr struct {
	code int
	msg  string
}

func (e statusCodeErr) Error() string   { return e.msg }
func (e statusCodeErr) HTTPStatus() int { return e.code }
