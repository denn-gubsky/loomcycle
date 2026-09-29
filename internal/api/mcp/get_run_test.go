package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// callGetRun drives one get_run call and returns its result.
func callGetRun(t *testing.T, mc *mockConnector, args string) loommcp.CallToolResult {
	t.Helper()
	srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_run","arguments":` + args + `}}`,
	}, "\n") + "\n"
	resps, _ := driveServer(t, srv, in)
	var res loommcp.CallToolResult
	if err := json.Unmarshal(resps[1].Result, &res); err != nil {
		t.Fatalf("unmarshal call result: %v", err)
	}
	return res
}

// A run id names one run; an agent id names that agent's latest, which for a
// team walk (`team:<name>`) is only one of many.
func TestServer_GetRun_ByRunIDReadsThatRun(t *testing.T) {
	mc := &mockConnector{
		getRunByRunIDResult: connector.Run{RunID: "r_walk_1", AgentID: "team:triage", Status: "running", AwaitedState: "review"},
	}
	res := callGetRun(t, mc, `{"run_id":"r_walk_1"}`)
	if res.IsError {
		t.Fatalf("get_run by run_id errored: %v", res.Content)
	}
	if got, _ := mc.getRunByRunIDArg.Load().(string); got != "r_walk_1" {
		t.Errorf("GetRunByRunID saw %q, want r_walk_1", got)
	}
	if mc.getRunAgentID.Load() != nil {
		t.Error("a run_id read also went through the agent-id read")
	}
	var got connector.Run
	if err := json.Unmarshal([]byte(res.Content[0].Text), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RunID != "r_walk_1" || got.AwaitedState != "review" {
		t.Errorf("result = %+v, want the run read by its id", got)
	}
}

func TestServer_GetRun_ByAgentIDStillReadsTheLatest(t *testing.T) {
	mc := &mockConnector{getRunResult: connector.Run{AgentID: "a_x", RunID: "r_x"}}
	if res := callGetRun(t, mc, `{"agent_id":"a_x"}`); res.IsError {
		t.Fatalf("get_run by agent_id errored: %v", res.Content)
	}
	if got, _ := mc.getRunAgentID.Load().(string); got != "a_x" {
		t.Errorf("GetRun saw %q, want a_x", got)
	}
	if mc.getRunByRunIDArg.Load() != nil {
		t.Error("an agent_id read also went through the run-id read")
	}
}

// Both, or neither, is a validation refusal before any read: with both there
// is no telling which run the caller meant.
func TestServer_GetRun_BothOrNeitherIsAToolError(t *testing.T) {
	for _, args := range []string{`{"run_id":"r_1","agent_id":"a_1"}`, `{}`} {
		mc := &mockConnector{}
		res := callGetRun(t, mc, args)
		if !res.IsError {
			t.Errorf("get_run %s: want a tool error", args)
		}
		if !strings.Contains(res.Content[0].Text, "exactly one of run_id or agent_id") {
			t.Errorf("get_run %s: error %q does not say what to send", args, res.Content[0].Text)
		}
		if mc.getRunAgentID.Load() != nil || mc.getRunByRunIDArg.Load() != nil {
			t.Errorf("get_run %s reached the connector", args)
		}
	}
}
