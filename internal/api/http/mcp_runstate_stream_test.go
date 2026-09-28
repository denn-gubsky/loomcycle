package http

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runstate"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// TestMCPStreamUserRunStates_TenantSessionDropsOtherTenantsRuns drives the MCP
// `stream_user_run_states` tool end to end — the MCP server over the REAL
// connector and run-state bus — as a substrate:tenant session.
//
// A user id is unique only within its tenant, so "alice" in globex is a
// different person from "alice" in acme. The globex event is published FIRST:
// if the MCP handler leaves the stream unscoped, it is the one event collected
// (max_events=1) and the test fails naming it.
func TestMCPStreamUserRunStates_TenantSessionDropsOtherTenantsRuns(t *testing.T) {
	srv, _, cleanup := systemChannelFixture(t)
	defer cleanup()
	bus := runstate.NewBus()
	srv.SetRunStateBus(bus)

	mcpSrv := lcmcp.New(lcmcp.Config{Connector: srv, Logf: func(string, ...any) {}})
	ctx, cancel := context.WithTimeout(auth.WithPrincipal(t.Context(), auth.Principal{
		TenantID: "acme", Subject: "op", Scopes: []string{auth.ScopeTenant},
	}), 5*time.Second)
	defer cancel()

	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"stream_user_run_states","arguments":{"user_id":"alice","max_events":1,"timeout_ms":2000}}}` + "\n"
	var stdout bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- mcpSrv.Serve(ctx, strings.NewReader(in), &stdout) }()

	waitForSubscriber(t, bus)
	bus.Publish(runstate.RunStateEvent{RunID: "r_globex", UserID: "alice", TenantID: "globex", Status: "completed"})
	bus.Publish(runstate.RunStateEvent{RunID: "r_acme", UserID: "alice", TenantID: "acme", Status: "completed"})

	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp loommcp.Response
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", stdout.String(), err)
	}
	var res loommcp.CallToolResult
	if err := json.Unmarshal(resp.Result, &res); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if res.IsError || len(res.Content) == 0 {
		t.Fatalf("tool call failed: %+v", res)
	}
	var out struct {
		Events []connector.RunStateEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(out.Events) != 1 || out.Events[0].RunID != "r_acme" {
		t.Fatalf("a tenant session streamed another tenant's run transitions: got %+v, want only r_acme", out.Events)
	}
}
