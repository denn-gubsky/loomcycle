package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// forgedParentContextJSON is a caller's parent_context carrying, besides its
// own tracking fields, every field the runtime stamps on the runs a team walk
// spawns — an attempt to place this run inside someone else's walk and on
// someone else's board card.
const forgedParentContextJSON = `{"root_agent_run_id":"r_root","function_key":"fk","tier_at_run":"pro",` +
	`"walk_id":"r_victim_walk","wave_id":"wav_victim","wave_index":3,` +
	`"board_scope":"user","board_chunk_id":"c_victim","board_document_id":"d_victim"}`

var callerOnlyParentContext = store.ParentContext{RootAgentRunID: "r_root", FunctionKey: "fk", TierAtRun: "pro"}

func forgedParentContext() *store.ParentContext {
	pc := callerOnlyParentContext
	pc.WalkID, pc.WaveID, pc.WaveIndex = "r_victim_walk", "wav_victim", 3
	pc.BoardScope, pc.BoardChunkID, pc.BoardDocumentID = "user", "c_victim", "d_victim"
	return &pc
}

// TestParentContext_RuntimeFieldsDroppedAtEveryIngress: a caller that sends
// walk_id / wave_id / wave_index (or the board fields) gets a run whose stored
// parent_context carries none of them, while its own tracking fields survive —
// on every surface that creates a run from caller input. The stored row is what
// the run-state stream and a walk's canvas group by, so it is what is asserted.
func TestParentContext_RuntimeFieldsDroppedAtEveryIngress(t *testing.T) {
	const prompt = `"segments":[{"role":"user","content":[{"type":"trusted-text","text":"hi"}]}]`
	cases := []struct {
		name string
		// start creates the run and returns its id.
		start func(t *testing.T, srv *Server, ts *httptest.Server, st store.Store) string
	}{
		{"POST /v1/runs", func(t *testing.T, _ *Server, ts *httptest.Server, st store.Store) string {
			code, body := do(t, "POST", ts.URL+"/v1/runs",
				`{"agent":"agent","agent_id":"pc-runs","parent_context":`+forgedParentContextJSON+`,`+prompt+`}`)
			if code != http.StatusOK {
				t.Fatalf("POST /v1/runs = %d %s", code, body)
			}
			return runIDByAgentID(t, st, "pc-runs")
		}},
		{"POST /v1/sessions/{id}/messages", func(t *testing.T, _ *Server, ts *httptest.Server, st store.Store) string {
			if code, body := do(t, "POST", ts.URL+"/v1/runs", `{"agent":"agent","agent_id":"pc-seed",`+prompt+`}`); code != http.StatusOK {
				t.Fatalf("seed run = %d %s", code, body)
			}
			seed, err := st.GetRunByAgentID(context.Background(), "pc-seed")
			if err != nil {
				t.Fatal(err)
			}
			code, body := do(t, "POST", ts.URL+"/v1/sessions/"+seed.SessionID+"/messages",
				`{"agent_id":"pc-cont","parent_context":`+forgedParentContextJSON+`,`+prompt+`}`)
			if code != http.StatusOK {
				t.Fatalf("continuation = %d %s", code, body)
			}
			return runIDByAgentID(t, st, "pc-cont")
		}},
		{"POST /v1/runs:batch", func(t *testing.T, _ *Server, ts *httptest.Server, st store.Store) string {
			code, body := do(t, "POST", ts.URL+"/v1/runs:batch",
				`{"spawns":[{"agent":"agent","agent_id":"pc-batch","parent_context":`+forgedParentContextJSON+`,`+prompt+`}]}`)
			if code != http.StatusOK {
				t.Fatalf("runs:batch = %d %s", code, body)
			}
			if strings.Contains(body, "r_victim_walk") {
				t.Errorf("the batch envelope echoes the forged walk: %s", body)
			}
			return runIDByAgentID(t, st, "pc-batch")
		}},
		{"configured run (create + start)", func(t *testing.T, _ *Server, ts *httptest.Server, st store.Store) string {
			c := createDraft(t, ts, `,"parent_context":`+forgedParentContextJSON)
			draft, err := st.GetRunDraft(context.Background(), c.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(draft), "r_victim_walk") || strings.Contains(string(draft), "c_victim") {
				t.Errorf("the stored draft kept the forged fields: %s", draft)
			}
			if code, body := do(t, "POST", ts.URL+"/v1/runs/"+c.RunID+"/start", ""); code != http.StatusOK {
				t.Fatalf("start = %d %s", code, body)
			}
			return c.RunID
		}},
		// gRPC and MCP create a draft through the connector, not handleRuns.
		{"configured run via connector (gRPC / MCP)", func(t *testing.T, srv *Server, _ *httptest.Server, st store.Store) string {
			ctx := context.Background()
			c, err := srv.CreateConfiguredRun(ctx, connector.ConfiguredRunRequest{SpawnRunRequest: connector.SpawnRunRequest{
				Agent: "agent", UserID: "u1", ParentContext: forgedParentContext(),
				Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
			}})
			if err != nil {
				t.Fatalf("CreateConfiguredRun: %v", err)
			}
			if strings.Contains(string(c.Draft), "r_victim_walk") || strings.Contains(string(c.Draft), "c_victim") {
				t.Errorf("the stored draft kept the forged fields: %s", c.Draft)
			}
			if _, err := srv.StartConfiguredRun(ctx, c.RunID, connector.RunSecrets{}); err != nil {
				t.Fatalf("StartConfiguredRun: %v", err)
			}
			return c.RunID
		}},
		// RunOnce is the seam gRPC Run/Continue/SpawnRunBatch, the MCP spawn_run
		// streaming path and every connector SpawnRun share.
		{"RunOnce (gRPC / MCP / connector)", func(t *testing.T, srv *Server, _ *httptest.Server, st store.Store) string {
			err := srv.RunOnce(context.Background(), runner.RunInput{
				Agent: "agent", AgentID: "pc-once", ParentContext: forgedParentContext(),
				Segments: []loop.PromptSegment{{Role: "user", Content: []loop.PromptContentBlock{{Type: "trusted-text", Text: "hi"}}}},
			}, runner.RunCallbacks{})
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			return runIDByAgentID(t, st, "pc-once")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, ts, _, st := configuredServer(t, 4)
			runID := tc.start(t, srv, ts, st)
			run, err := st.GetRun(context.Background(), runID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if run.ParentContext == nil || *run.ParentContext != callerOnlyParentContext {
				t.Errorf("stored parent_context = %+v, want only the caller's fields %+v", run.ParentContext, callerOnlyParentContext)
			}
		})
	}
}

func runIDByAgentID(t *testing.T, st store.Store, agentID string) string {
	t.Helper()
	run, err := st.GetRunByAgentID(context.Background(), agentID)
	if err != nil {
		t.Fatalf("GetRunByAgentID(%s): %v", agentID, err)
	}
	return run.ID
}

// TestSubRun_StillStampsWaveAndBoardFields: the strip is for caller input only.
// The runs a team walk spawns get their walk, wave and board fields from the
// sub-run path, which writes them onto the child's identity directly — so they
// must still reach the stored row.
func TestSubRun_StillStampsWaveAndBoardFields(t *testing.T) {
	srv, _, _, st := configuredServer(t, 4)
	ctx := store.WithWaveTask(context.Background(), store.WaveTask{WalkID: "r_walk", WaveID: "wav_1", Index: 2})
	ctx = store.WithBoardTask(ctx, store.BoardTask{Scope: "user", ChunkID: "c1", DocumentID: "d1"})
	_, _, runID, err := srv.runSubAgent(ctx, "agent", "", "go", "")
	if err != nil {
		t.Fatalf("runSubAgent: %v", err)
	}
	run, err := st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	want := store.ParentContext{WalkID: "r_walk", WaveID: "wav_1", WaveIndex: 2,
		BoardScope: "user", BoardChunkID: "c1", BoardDocumentID: "d1"}
	if run.ParentContext == nil || *run.ParentContext != want {
		t.Errorf("stored parent_context = %+v, want the runtime's stamp %+v", run.ParentContext, want)
	}
}
