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
	"sync"
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
			if mode == "slow-call" {
				time.Sleep(200 * time.Millisecond)
			}
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

// Regression: when ctx fires mid-Execute, the wrapper must surface a hard
// error (non-nil err) rather than dressing the cancellation as a recoverable
// tool failure (Result{IsError:true}, nil). The model would otherwise see a
// confusing "tool errored" message in the transcript before the loop's next
// provider call detects the same ctx and terminates the run.
//
// Test approach: drive an Execute against a slow server with a tight ctx.
// Without the fix, Execute returns (Result{IsError:true, Text:"context..."}, nil).
// With the fix, Execute returns (Result{}, err) so dispatcher's err branch fires.
func TestPoolToolReturnsErrOnCtxCancel(t *testing.T) {
	pool := newPoolWithFake(t, "slow-call")
	initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer initCancel()
	if _, _, err := pool.Get(initCtx, "search"); err != nil {
		t.Fatal(err)
	}
	tool := pool.Tools()[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_, err := tool.Execute(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected non-nil err on ctx cancel; tool dressed cancel as IsError")
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

// Regression: concurrent Get on the same server name must share one
// in-flight init — not block under one lock and not double-spawn.
func TestPoolGetSharesInFlightInit(t *testing.T) {
	build := newCountingBuild(t, "ok")
	pool := mcp.NewPool(build.fn, func(c mcp.Caller) {
		if cl, ok := c.(*stdio.Client); ok {
			_ = cl.Close()
		}
	})
	t.Cleanup(pool.Close)

	const N = 8
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _, err := pool.Get(ctx, "shared")
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Get failed: %v", err)
	}
	if build.count != 1 {
		t.Errorf("build called %d times, want 1 (concurrent Get must share init)", build.count)
	}
}

// Regression: a failed init removes the entry from the map so subsequent
// Get calls can retry, rather than getting back the cached error forever.
func TestPoolGetRetriesAfterInitFailure(t *testing.T) {
	var attempts int
	pool := mcp.NewPool(
		func(name string) (mcp.Caller, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("transient build failure")
			}
			c, err := stdio.Spawn(stdio.Config{
				Command: os.Args[0],
				Env:     []string{"BE_MCP_SERVER=ok"},
			})
			return c, err
		},
		func(c mcp.Caller) {
			if cl, ok := c.(*stdio.Client); ok {
				_ = cl.Close()
			}
		},
	)
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, _, err := pool.Get(ctx, "retry"); err == nil {
		t.Fatal("first Get expected to fail")
	}
	if _, _, err := pool.Get(ctx, "retry"); err != nil {
		t.Errorf("second Get should succeed after failed entry was evicted: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2", attempts)
	}
}

// Regression: Get respects ctx cancellation while waiting on an in-flight
// init started by another goroutine.
func TestPoolGetRespectsCtxWhileWaitingOnPeerInit(t *testing.T) {
	// Use a slow build so the second Get can race the first's init.
	releaseBuild := make(chan struct{})
	pool := mcp.NewPool(
		func(name string) (mcp.Caller, error) {
			<-releaseBuild
			return stdio.Spawn(stdio.Config{
				Command: os.Args[0],
				Env:     []string{"BE_MCP_SERVER=ok"},
			})
		},
		func(c mcp.Caller) {
			if cl, ok := c.(*stdio.Client); ok {
				_ = cl.Close()
			}
		},
	)
	t.Cleanup(func() {
		// Unblock any stuck build before Close.
		select {
		case <-releaseBuild:
		default:
			close(releaseBuild)
		}
		pool.Close()
	})

	// First Get starts an init that hangs on releaseBuild.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = pool.Get(ctx, "slow")
	}()
	// Give it a moment to register the entry.
	time.Sleep(50 * time.Millisecond)

	// Second Get races in with a tight ctx. Must return ctx.Err()
	// before the first Get's init completes.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, _, err := pool.Get(ctx, "slow")
	if err == nil {
		t.Fatal("expected ctx error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}

// Regression: when a cached entry's caller goes unhealthy (e.g. the stdio
// child crashed), the next Pool.Get must evict + respawn rather than handing
// back a dead caller forever.
func TestPoolGetRespawnsAfterUnhealthy(t *testing.T) {
	build := newCountingBuild(t, "ok")
	pool := mcp.NewPool(build.fn, func(c mcp.Caller) {
		if cl, ok := c.(*stdio.Client); ok {
			_ = cl.Close()
		}
	})
	t.Cleanup(pool.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First Get → spawn 1.
	caller1, _, err := pool.Get(ctx, "rs")
	if err != nil {
		t.Fatal(err)
	}
	if !caller1.Healthy() {
		t.Fatal("first caller should be Healthy")
	}

	// Kill the child. After this the cached caller reports !Healthy.
	if cl, ok := caller1.(*stdio.Client); ok {
		_ = cl.Close()
	}
	// Give the readLoop a moment to observe EOF and close doneCh.
	for i := 0; i < 100 && caller1.Healthy(); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if caller1.Healthy() {
		t.Fatal("caller still Healthy after Close — test setup wrong")
	}

	// Next Get must respawn (build called again).
	caller2, _, err := pool.Get(ctx, "rs")
	if err != nil {
		t.Fatalf("Get after crash: %v", err)
	}
	if !caller2.Healthy() {
		t.Fatal("respawned caller should be Healthy")
	}
	if caller2 == caller1 {
		t.Fatal("Get returned the dead caller, not a respawn")
	}
	if build.count != 2 {
		t.Errorf("build called %d times, want 2 (spawn + respawn)", build.count)
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
