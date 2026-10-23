package http

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	lcmcp "github.com/denn-gubsky/loomcycle/internal/api/mcp"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// spawnWithin runs one blocking spawn and fails the test if it has not
// returned within joinHangLimit.
func spawnWithin(t *testing.T, call func() connector.SpawnRunResult) (connector.SpawnRunResult, time.Duration) {
	t.Helper()
	start := time.Now()
	done := make(chan connector.SpawnRunResult, 1)
	go func() { done <- call() }()
	select {
	case res := <-done:
		return res, time.Since(start)
	case <-time.After(joinHangLimit):
		t.Fatalf("the spawn did not return within %s: it waits on a run another request started that never ends", joinHangLimit)
		return connector.SpawnRunResult{}, 0
	}
}

// A blocking spawn_run keyed to a run another request started, and that never
// ends, returns at the server's join bound when its caller set no deadline:
// the run reported running with its ids, and left alone.
func TestSpawnRun_OnAHeldKeyEndsAtTheServerBound(t *testing.T) {
	s, held, _ := heldRunServer(t, 300*time.Millisecond)
	res, took := spawnWithin(t, func() connector.SpawnRunResult {
		r, err := s.SpawnRun(context.Background(), keyed("held", "u1"))
		if err != nil {
			t.Errorf("SpawnRun: %v", err)
		}
		return r
	})
	assertJoinedUnfinished(t, res, held)
	if took > 5*time.Second {
		t.Errorf("returned after %s, want about the 300ms bound", took)
	}
	assertHeldRunUntouched(t, s, held)
}

// The same over MCP spawn_run with no timeout_ms and no operator default:
// the MCP session never cancels, so only the server's bound ends the wait.
func TestMCPSpawnRun_OnAHeldKeyEndsAtTheServerBound(t *testing.T) {
	s, held, _ := heldRunServer(t, 300*time.Millisecond)
	mcpSrv := lcmcp.New(lcmcp.Config{Connector: s, Logger: slog.New(slog.DiscardHandler)})
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"spawn_run","arguments":{"agent":"r","user_id":"u1","idempotency_key":"held","segments":[{"role":"user","content":[{"type":"trusted-text","text":"go"}]}]}}}` + "\n"
	res, _ := spawnWithin(t, func() connector.SpawnRunResult {
		var stdout bytes.Buffer
		if err := mcpSrv.Serve(context.Background(), strings.NewReader(in), &stdout); err != nil {
			t.Errorf("Serve: %v", err)
		}
		var resp loommcp.Response
		var tool loommcp.CallToolResult
		var got connector.SpawnRunResult
		if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
			t.Errorf("decode response %q: %v", stdout.String(), err)
			return got
		}
		if err := json.Unmarshal(resp.Result, &tool); err != nil || len(tool.Content) == 0 {
			t.Errorf("tool result = %s (%v)", resp.Result, err)
			return got
		}
		if err := json.Unmarshal([]byte(tool.Content[0].Text), &got); err != nil {
			t.Errorf("decode result %q: %v", tool.Content[0].Text, err)
		}
		return got
	})
	assertJoinedUnfinished(t, res, held)
	assertHeldRunUntouched(t, s, held)
}

// A caller's own deadline still ends the wait, before a longer server bound,
// and is not reported as the join's timeout.
func TestSpawnRun_ACallerDeadlineEndsTheWaitBeforeTheServerBound(t *testing.T) {
	s, held, _ := heldRunServer(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, took := spawnWithin(t, func() connector.SpawnRunResult {
		r, _ := s.SpawnRun(ctx, keyed("held", "u1"))
		return r
	})
	if !res.Deduplicated || res.RunID != held.RunID || res.Status != "running" {
		t.Fatalf("result = %+v, want the held run reported running, deduplicated", res)
	}
	if took > 5*time.Second {
		t.Errorf("returned after %s, want about the caller's 300ms deadline", took)
	}
	assertHeldRunUntouched(t, s, held)
}

// The server bound off (0): only the caller ends the wait, as before.
func TestSpawnRun_NoServerBoundLeavesTheWaitToTheCaller(t *testing.T) {
	s, held, _ := heldRunServer(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, took := spawnWithin(t, func() connector.SpawnRunResult {
		r, _ := s.SpawnRun(ctx, keyed("held", "u1"))
		return r
	})
	if took < 250*time.Millisecond {
		t.Errorf("returned after %s with no server bound, want it to wait for the caller's deadline", took)
	}
	if strings.Contains(res.Error, "join timed out") {
		t.Errorf("error = %q: no server bound is set, so the join did not time out", res.Error)
	}
	assertHeldRunUntouched(t, s, held)
}
