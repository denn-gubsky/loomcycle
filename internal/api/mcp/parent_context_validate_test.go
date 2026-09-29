package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// TestServer_SpawnRun_RefusesOverLongParentContextField: spawn_run and
// spawn_runs answer an over-long parent_context.function_key with a tool error
// naming the field, before the connector sees the request, and pass a
// well-formed one on. The MCP half of the every-transport table in the gRPC
// package (TestParentContext_OverLongFieldRefusedOnEveryTransport); this package
// cannot import the real server, and the check it pins runs before the
// connector, so a mock connector is enough.
func TestServer_SpawnRun_RefusesOverLongParentContextField(t *testing.T) {
	pc := func(n int) string {
		b, _ := json.Marshal(map[string]string{"function_key": strings.Repeat("k", n)})
		return string(b)
	}
	surfaces := []struct {
		name string
		args func(pc string) string
		// reached reports whether the connector was handed the request.
		reached func(mc *mockConnector) bool
	}{
		{"spawn_run", func(pc string) string {
			return `{"agent":"qa",` + spawnSegments + `,"parent_context":` + pc + `}`
		}, func(mc *mockConnector) bool { return mc.spawnReq.Load() != nil }},
		{"spawn_runs", func(pc string) string {
			return `{"spawns":[{"agent":"qa",` + spawnSegments + `,"parent_context":` + pc + `}]}`
		}, func(mc *mockConnector) bool { return mc.batchReq.Load() != nil }},
	}
	call := func(t *testing.T, name, args string) (loommcp.CallToolResult, *mockConnector) {
		t.Helper()
		mc := &mockConnector{spawnResult: connector.SpawnRunResult{AgentID: "a", RunID: "r", Status: "completed"}}
		srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
		resps, _ := driveServer(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+name+`","arguments":`+args+`}}`+"\n")
		if len(resps) != 1 {
			t.Fatalf("got %d responses, want 1", len(resps))
		}
		var res loommcp.CallToolResult
		if err := json.Unmarshal(resps[0].Result, &res); err != nil {
			t.Fatalf("unmarshal %s result: %v (%s)", name, err, resps[0].Result)
		}
		return res, mc
	}
	for _, tool := range surfaces {
		t.Run(tool.name, func(t *testing.T) {
			res, mc := call(t, tool.name, tool.args(pc(257)))
			if !res.IsError || len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "parent_context.function_key") {
				t.Errorf("257-byte function_key = isError %v %+v, want a tool error naming the field", res.IsError, res.Content)
			}
			if tool.reached(mc) {
				t.Error("the connector was handed a request with an over-long field")
			}
			res, mc = call(t, tool.name, tool.args(pc(256)))
			if res.IsError || !tool.reached(mc) {
				t.Errorf("256-byte function_key = isError %v %+v (connector reached %v), want it passed on", res.IsError, res.Content, tool.reached(mc))
			}
		})
	}
}
