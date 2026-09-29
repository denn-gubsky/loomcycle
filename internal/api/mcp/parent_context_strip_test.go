package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// forgedParentContextArg carries, besides the caller's own tracking fields,
// every field the runtime stamps on the runs a team walk spawns.
const forgedParentContextArg = `{"root_agent_run_id":"r_root","function_key":"fk","tier_at_run":"pro",` +
	`"walk_id":"r_victim_walk","wave_id":"wav_victim","wave_index":3,"state":"s_victim","state_visit":5,` +
	`"board_scope":"user","board_chunk_id":"c_victim","board_document_id":"d_victim"}`

var callerOnlyParentContext = store.ParentContext{RootAgentRunID: "r_root", FunctionKey: "fk", TierAtRun: "pro"}

const spawnSegments = `"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]`

// TestServer_SpawnRun_DropsRuntimeParentContextFields: a spawn_run / spawn_runs
// caller cannot place its run inside a team walk or on a board — the walk and
// board fields are dropped before the run is handed on, on the blocking path,
// the streaming path and every batch child, while the caller's own fields
// survive.
func TestServer_SpawnRun_DropsRuntimeParentContextFields(t *testing.T) {
	check := func(t *testing.T, where string, got *store.ParentContext) {
		t.Helper()
		if got == nil || *got != callerOnlyParentContext {
			t.Errorf("%s parent_context = %+v, want only the caller's fields %+v", where, got, callerOnlyParentContext)
		}
	}

	t.Run("spawn_run blocking", func(t *testing.T) {
		mc := &mockConnector{spawnResult: connector.SpawnRunResult{AgentID: "a", RunID: "r", Status: "completed"}}
		srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
		in := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa",` + spawnSegments + `,"parent_context":` + forgedParentContextArg + `}}}`,
		}, "\n") + "\n"
		driveServer(t, srv, in)
		stored, _ := mc.spawnReq.Load().(connector.SpawnRunRequest)
		check(t, "connector", stored.ParentContext)
	})

	t.Run("spawn_run streaming", func(t *testing.T) {
		fr := &fakeRunner{agentID: "a_1", runID: "r_1", sessionID: "s_1"}
		srv := New(Config{Connector: &mockConnector{}, Runner: fr, Logf: func(string, ...any) {}})
		in := strings.Join([]string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{"loomcycle":{"runEvents":true}},"clientInfo":{"name":"t","version":"1"}}}`,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"qa",` + spawnSegments + `,"parent_context":` + forgedParentContextArg + `}}}`,
		}, "\n") + "\n"
		resps, _ := driveServer(t, srv, in)
		check(t, "runner", fr.lastInput.ParentContext)
		if len(resps) == 2 && strings.Contains(string(resps[1].Result), "r_victim_walk") {
			t.Errorf("the spawn_run result echoes the forged walk: %s", resps[1].Result)
		}
	})

	t.Run("spawn_runs", func(t *testing.T) {
		mc := &mockConnector{batchResult: connector.BatchSpawnResult{}}
		srv := New(Config{Connector: mc, Logf: func(string, ...any) {}})
		in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_runs","arguments":{"spawns":[` +
			`{"agent":"a",` + spawnSegments + `,"parent_context":` + forgedParentContextArg + `},` +
			`{"agent":"b",` + spawnSegments + `,"parent_context":` + forgedParentContextArg + `}]}}}` + "\n"
		driveServer(t, srv, in)
		stored, _ := mc.batchReq.Load().(connector.BatchSpawnRequest)
		if len(stored.Spawns) != 2 {
			t.Fatalf("connector saw %d spawns, want 2", len(stored.Spawns))
		}
		for i, sp := range stored.Spawns {
			check(t, fmt.Sprintf("child[%d]", i), sp.ParentContext)
		}
	})
}
