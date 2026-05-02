package mcp_test

// Tests for the Pool and Tool wrapper. Lives in an _test package so it can
// import the stdio subpackage without an import cycle, and so it exercises
// the public surface of mcp from a real consumer's vantage point.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
	"github.com/denn-gubsky/loomcycle/internal/tools/mcp/stdio"
)

// fakeMCPBinary is the path to the stdio test binary. Tests rely on the
// stdio package's TestMain installing the BE_MCP_SERVER hook — we run the
// stdio test binary from this package's test process by invoking
//
//	go test -c -o /tmp/.../stdio.test ./internal/tools/mcp/stdio
//
// at TestMain time. That's heavy. Cheaper: re-use the *current* test
// binary's BE_MCP_SERVER hook by spawning os.Args[0] with the env. But
// THIS test process doesn't have that hook (its TestMain is the default).
// We therefore declare a TestMain here too that mirrors the stdio one.

func TestMain(m *testing.M) {
	if mode := os.Getenv("BE_MCP_SERVER"); mode != "" {
		runFakeServer(mode)
		return
	}
	os.Exit(m.Run())
}

// runFakeServer is duplicated from the stdio test helper; small enough to
// duplicate rather than build a shared test-helper package just for two
// callers. If a third caller appears, factor out.
func runFakeServer(mode string) {
	dec := json.NewDecoder(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		var probe struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := dec.Decode(&probe); err != nil {
			return
		}
		if probe.ID == nil {
			continue
		}
		switch probe.Method {
		case "initialize":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result": map[string]any{
					"protocolVersion": mcp.ProtocolVersion,
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake-pool", "version": "0.1"},
				},
			})
		case "tools/list":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "web_search",
							"description": "search the web",
							"inputSchema": map[string]any{"type": "object"},
						},
					},
				},
			})
		case "tools/call":
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(probe.Params, &p)
			if mode == "tool_iserror" {
				_ = enc.Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      *probe.ID,
					"result": map[string]any{
						"content": []map[string]any{{"type": "text", "text": "tool failed"}},
						"isError": true,
					},
				})
				continue
			}
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "got: " + p.Name}},
				},
			})
		}
	}
}

func newPoolWithFake(t *testing.T, mode string) *mcp.Pool {
	t.Helper()
	build := func(name string) (mcp.Caller, error) {
		c, err := stdio.Spawn(stdio.Config{
			Command: os.Args[0],
			Env:     []string{"BE_MCP_SERVER=" + mode},
		})
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	teardown := func(c mcp.Caller) {
		if cl, ok := c.(*stdio.Client); ok {
			_ = cl.Close()
		}
	}
	pool := mcp.NewPool(build, teardown)
	t.Cleanup(pool.Close)
	return pool
}

func TestPoolGetSpawnsAndDiscoversTools(t *testing.T) {
	pool := newPoolWithFake(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, descs, err := pool.Get(ctx, "search")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(descs) != 1 || descs[0].Name != "web_search" {
		t.Errorf("descs: %+v", descs)
	}

	tools := pool.Tools()
	if len(tools) != 1 {
		t.Fatalf("tools = %d", len(tools))
	}
	want := "mcp__search__web_search"
	if got := tools[0].Name(); got != want {
		t.Errorf("tool name = %q, want %q", got, want)
	}
}

func TestPoolToolExecuteRoutesToServer(t *testing.T) {
	pool := newPoolWithFake(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := pool.Get(ctx, "search"); err != nil {
		t.Fatal(err)
	}
	tool := pool.Tools()[0]
	res, err := tool.Execute(ctx, json.RawMessage(`{"q":"hi"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(res.Text, "got: web_search") {
		t.Errorf("text = %q", res.Text)
	}
	if res.IsError {
		t.Errorf("IsError true unexpectedly")
	}
}

func TestPoolToolPropagatesIsError(t *testing.T) {
	pool := newPoolWithFake(t, "tool_iserror")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := pool.Get(ctx, "search"); err != nil {
		t.Fatal(err)
	}
	tool := pool.Tools()[0]
	res, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true")
	}
}

func TestPoolGetCachesConnection(t *testing.T) {
	build := newCountingBuild(t, "ok")
	pool := mcp.NewPool(build.fn, func(c mcp.Caller) {
		if cl, ok := c.(*stdio.Client); ok {
			_ = cl.Close()
		}
	})
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := pool.Get(ctx, "search"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pool.Get(ctx, "search"); err != nil {
		t.Fatal(err)
	}
	if build.count != 1 {
		t.Errorf("build called %d times, want 1 (cache miss on second Get)", build.count)
	}
}

func TestPoolGetReturnsBuildError(t *testing.T) {
	pool := mcp.NewPool(
		func(name string) (mcp.Caller, error) {
			return nil, errors.New("synthetic build failure")
		},
		nil,
	)
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := pool.Get(ctx, "broken")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "synthetic build failure") {
		t.Errorf("error doesn't propagate underlying cause: %v", err)
	}
}

type countingBuild struct {
	t     *testing.T
	mode  string
	count int
}

func newCountingBuild(t *testing.T, mode string) *countingBuild {
	return &countingBuild{t: t, mode: mode}
}

func (b *countingBuild) fn(name string) (mcp.Caller, error) {
	b.count++
	c, err := stdio.Spawn(stdio.Config{
		Command: os.Args[0],
		Env:     []string{"BE_MCP_SERVER=" + b.mode},
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}
