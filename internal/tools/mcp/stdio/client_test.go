package stdio

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// TestMain acts as both the test harness AND a fake MCP server. When the
// test binary is run with BE_MCP_SERVER set, it loops on stdin reading
// JSON-RPC requests and writing back canned responses. Tests spawn
// os.Args[0] with that env var to drive a real subprocess through the
// stdio transport — same code path production will hit.
func TestMain(m *testing.M) {
	if mode := os.Getenv("BE_MCP_SERVER"); mode != "" {
		runFakeServer(mode)
		return
	}
	os.Exit(m.Run())
}

// runFakeServer is a tiny MCP server that handles initialize, tools/list,
// and tools/call. The mode env value picks the response set:
//   - "ok"           → ToolEcho returns its input as a text block
//   - "error"        → tools/call returns a JSON-RPC error
//   - "tool_iserror" → tools/call returns a result with isError=true
//   - "slow"         → tools/call sleeps 200ms before responding
//   - "crash"        → server exits 1 immediately after initialize
func runFakeServer(mode string) {
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		// Decode just enough to see method + id.
		var probe struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		// Notifications: no response.
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
					"serverInfo":      map[string]any{"name": "fake", "version": "0.1"},
				},
			})
			if mode == "crash" {
				os.Exit(1)
			}
		case "tools/list":
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result": map[string]any{
					"tools": []map[string]any{
						{
							"name":        "Echo",
							"description": "echoes the input string",
							"inputSchema": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"text": map[string]any{"type": "string"},
								},
							},
						},
					},
				},
			})
		case "tools/call":
			if mode == "slow" {
				time.Sleep(200 * time.Millisecond)
			}
			if mode == "error" {
				_ = enc.Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      *probe.ID,
					"error":   map[string]any{"code": -32603, "message": "synthetic server error"},
				})
				continue
			}
			// Decode args to echo back.
			var p struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(probe.Params, &p)
			var args struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(p.Arguments, &args)

			result := map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "echo: " + args.Text},
				},
			}
			if mode == "tool_iserror" {
				result["isError"] = true
			}
			_ = enc.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      *probe.ID,
				"result":  result,
			})
		}
	}
}

func newClient(t *testing.T, mode string) *Client {
	t.Helper()
	c, err := Spawn(Config{
		Command: os.Args[0],
		Env:     []string{"BE_MCP_SERVER=" + mode},
		OnStderr: func(s string) {
			// Surface server-side errors in test output for debuggability.
			t.Logf("server stderr: %s", s)
		},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestHandshakeAndListTools(t *testing.T) {
	c := newClient(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	init, err := mcp.Initialize(ctx, c, "loomcycle-test", "1.0")
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if init.ServerInfo.Name != "fake" {
		t.Errorf("ServerInfo.Name = %q", init.ServerInfo.Name)
	}

	tools, err := mcp.ListTools(ctx, c)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "Echo" {
		t.Errorf("tools = %+v", tools)
	}
}

func TestCallToolHappyPath(t *testing.T) {
	c := newClient(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	res, err := mcp.CallTool(ctx, c, "Echo", json.RawMessage(`{"text":"hello"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := mcp.JoinTextContent(res); got != "echo: hello" {
		t.Errorf("text = %q", got)
	}
	if res.IsError {
		t.Errorf("IsError unexpectedly true")
	}
}

func TestCallToolReturnsServerError(t *testing.T) {
	c := newClient(t, "error")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	_, err := mcp.CallTool(ctx, c, "Echo", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected JSON-RPC error")
	}
	var rpcErr *mcp.Error
	// errors.As-equivalent for the typed error path — protocol returns
	// the raw *mcp.Error wrapped in a fmt.Errorf "%w".
	if !strings.Contains(err.Error(), "synthetic server error") {
		t.Errorf("error doesn't mention server message: %v", err)
	}
	_ = rpcErr
}

func TestCallToolPropagatesIsError(t *testing.T) {
	c := newClient(t, "tool_iserror")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	res, err := mcp.CallTool(ctx, c, "Echo", json.RawMessage(`{"text":"x"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true")
	}
}

func TestConcurrentCalls(t *testing.T) {
	c := newClient(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}

	const N = 20
	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := json.RawMessage(fmt.Sprintf(`{"text":"msg-%d"}`, i))
			res, err := mcp.CallTool(ctx, c, "Echo", args)
			if err != nil {
				errs <- err
				return
			}
			want := fmt.Sprintf("echo: msg-%d", i)
			if got := mcp.JoinTextContent(res); got != want {
				errs <- fmt.Errorf("got %q, want %q", got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCtxCancelMidCall(t *testing.T) {
	c := newClient(t, "slow")
	initCtx, initCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer initCancel()
	if _, err := mcp.Initialize(initCtx, c, "t", "1"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := mcp.CallTool(ctx, c, "Echo", json.RawMessage(`{"text":"x"}`))
	if err == nil {
		t.Fatal("expected ctx error (server delayed 200ms; ctx 50ms)")
	}
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("error doesn't surface context: %v", err)
	}
}

func TestServerCrashFailsInFlightCalls(t *testing.T) {
	// "crash" mode: server exits after initialize. Subsequent Calls must
	// fail with a transport error rather than hang.
	c := newClient(t, "crash")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := mcp.Initialize(ctx, c, "t", "1"); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Give the child a moment to actually exit.
	time.Sleep(50 * time.Millisecond)

	_, err := mcp.CallTool(ctx, c, "Echo", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected transport error after server crash")
	}
	if !strings.Contains(err.Error(), "exited") && !strings.Contains(err.Error(), "closed") {
		t.Errorf("error doesn't mention server exit: %v", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, err := Spawn(Config{Command: os.Args[0], Env: []string{"BE_MCP_SERVER=ok"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close errored: %v", err)
	}
}
