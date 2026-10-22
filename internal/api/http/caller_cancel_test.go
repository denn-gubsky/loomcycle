package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/store"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// assertCallerCancelled checks the row of a run whose caller ended it
// mid-turn: cancelled, with the reason, and no error text. Before the fix the
// provider's stream, closed by the cancelled ctx, read to the loop as a
// finished turn and the row was written completed with an empty stop reason.
func assertCallerCancelled(t *testing.T, st store.Store, runID, reason string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		run, err := st.GetRun(context.Background(), runID)
		if err == nil && store.IsTerminalRunStatus(run.Status) {
			if run.Status != store.RunCancelled || run.StopReason != reason || run.ErrorMsg != "" {
				t.Fatalf("run row = (%q, stop_reason %q, error %q), want (cancelled, %q, no error)", run.Status, run.StopReason, run.ErrorMsg, reason)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not end (status %q, err %v)", runID, run.Status, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A client that disconnects from a non-detached POST /v1/runs while the model
// is mid-turn ends the run cancelled, "client disconnected".
func TestRuns_ClientDisconnectMidTurnRecordsCancelled(t *testing.T) {
	s, _ := newGatedBatchServer(t, 8) // the gate is never fed: the turn stays open
	ts := httptest.NewServer(s.Mux())
	defer ts.Close()

	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/runs", strings.NewReader(
		`{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post run: %v", err)
	}
	defer resp.Body.Close()

	var runID string
	frames := sseLines(t, resp.Body)
	timeout := time.After(3 * time.Second)
	for sawText := false; runID == "" || !sawText; {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before the model's text arrived")
			}
			switch f[0] {
			case "agent":
				var a struct {
					RunID string `json:"run_id"`
				}
				_ = json.Unmarshal([]byte(f[1]), &a)
				runID = a.RunID
			case "text":
				sawText = true
			case "done", "error":
				t.Fatalf("run ended (%s: %s) before the disconnect", f[0], f[1])
			}
		case <-timeout:
			t.Fatal("timed out waiting for the run's text")
		}
	}

	disconnect()
	assertCallerCancelled(t, s.store, runID, "client disconnected")
}

// spawn_run over MCP with a transport timeout_ms that passes mid-turn ends
// the run cancelled, "caller deadline exceeded" — the tool result reports the
// timeout, and the row agrees the run did not complete.
func TestMCPSpawnRun_TransportTimeoutMidTurnRecordsCancelled(t *testing.T) {
	s, _ := newGatedBatchServer(t, 8)
	mcpSrv := lcmcp.New(lcmcp.Config{Connector: s, Logf: func(string, ...any) {}})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"r","user_id":"u1","timeout_ms":300,"segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}}}` + "\n"
	var stdout bytes.Buffer
	if err := mcpSrv.Serve(context.Background(), strings.NewReader(in), &stdout); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resp loommcp.Response
	var tool loommcp.CallToolResult
	var got connector.SpawnRunResult
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		t.Fatalf("decode response %q: %v", stdout.String(), err)
	}
	if err := json.Unmarshal(resp.Result, &tool); err != nil || len(tool.Content) == 0 {
		t.Fatalf("tool result = %s (%v)", resp.Result, err)
	}
	if err := json.Unmarshal([]byte(tool.Content[0].Text), &got); err != nil {
		t.Fatalf("decode result %q: %v", tool.Content[0].Text, err)
	}
	if got.Status != "timeout" || got.RunID == "" {
		t.Fatalf("spawn_run result = %+v, want a timeout with its run_id", got)
	}
	assertCallerCancelled(t, s.store, got.RunID, "caller deadline exceeded")
}

// SpawnRun (the connector every transport's blocking spawn shares) under a
// caller deadline that passes mid-turn: the result and the row both say
// cancelled.
func TestConnectorSpawnRun_CallerDeadlineMidTurnRecordsCancelled(t *testing.T) {
	s, _ := newGatedBatchServer(t, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := s.SpawnRun(ctx, connector.SpawnRunRequest{Agent: "r", UserID: "u1", Segments: oneUserSeg("go")})
	if err != nil {
		t.Fatalf("SpawnRun: %v", err)
	}
	if res.Status != string(store.RunCancelled) || res.RunID == "" {
		t.Fatalf("SpawnRun result = %+v, want cancelled with its run_id", res)
	}
	assertCallerCancelled(t, s.store, res.RunID, "caller deadline exceeded")
}

// The run end's reason for a caller deadline, on both a top-level run and a
// sub-agent: cancelled, not failed "context deadline exceeded".
func TestFinishRunWithCancel_CallerDeadlineRecordsCancelled(t *testing.T) {
	for _, topLevel := range []bool{true, false} {
		srv, runID := newRunForFinishTest(t)
		runCtx, cancelFn := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		cancelFn()
		srv.finishRunWithCancel(context.Background(), runCtx, runID, loop.RunResult{StopReason: "cancelled"}, context.DeadlineExceeded, runStateMeta{RunID: runID, IsTopLevel: topLevel})
		assertCallerCancelled(t, srv.store, runID, "caller deadline exceeded")
	}
}

// An API cancel while the model is mid-turn ends a POST /v1/runs stream with
// exactly one error frame — the loop's own, not a second copy from the
// transport — and the transcript ends with it.
func TestRuns_APICancelMidTurnEndsWithOneErrorFrame(t *testing.T) {
	s, _ := newGatedBatchServer(t, 8)
	ts := httptest.NewServer(s.Mux())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/v1/runs", "application/json", strings.NewReader(
		`{"agent":"r","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}`))
	if err != nil {
		t.Fatalf("post run: %v", err)
	}
	defer resp.Body.Close()

	var agentID, sessionID string
	var types []string
	for f := range sseLines(t, resp.Body) {
		types = append(types, f[0])
		switch f[0] {
		case "agent":
			var a struct {
				AgentID   string `json:"agent_id"`
				SessionID string `json:"session_id"`
			}
			_ = json.Unmarshal([]byte(f[1]), &a)
			agentID, sessionID = a.AgentID, a.SessionID
		case "text":
			if _, err := s.CancelRun(context.Background(), agentID, "stop"); err != nil {
				t.Fatalf("CancelRun: %v", err)
			}
		}
	}
	errorFrames := 0
	for _, typ := range types {
		if typ == "error" {
			errorFrames++
		}
	}
	if errorFrames != 1 || types[len(types)-1] != "error" {
		t.Fatalf("frames = %v, want the stream to end with exactly one error frame", types)
	}
	events, err := s.store.GetTranscript(context.Background(), sessionID)
	if err != nil || len(events) == 0 {
		t.Fatalf("GetTranscript: %d events, err %v", len(events), err)
	}
	if last := events[len(events)-1]; last.Type != "error" {
		t.Fatalf("the transcript ends with %q, want the error event", last.Type)
	}
}
