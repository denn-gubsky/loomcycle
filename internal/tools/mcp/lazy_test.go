package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// fakeCaller is a Caller that responds to initialize, tools/list, and
// tools/call with canned data, configured per-server in fakeBuild.
type fakeCaller struct {
	server      string
	tools       []ToolDescriptor
	healthyFlag atomic.Bool
}

func newFakeCaller(server string, tools []ToolDescriptor) *fakeCaller {
	c := &fakeCaller{server: server, tools: tools}
	c.healthyFlag.Store(true)
	return c
}

func (c *fakeCaller) Call(_ context.Context, method string, _ any) (json.RawMessage, error) {
	switch method {
	case "initialize":
		body, _ := json.Marshal(map[string]any{
			"protocolVersion": ProtocolVersion,
			"serverInfo":      map[string]any{"name": c.server, "version": "0.1"},
			"capabilities":    map[string]any{},
		})
		return body, nil
	case "tools/list":
		body, _ := json.Marshal(map[string]any{"tools": c.tools})
		return body, nil
	case "tools/call":
		// LazyResolver doesn't directly call this in our tests — the
		// Tool's Execute does. Return an empty content envelope so the
		// mcpTool.Execute path completes cleanly.
		body, _ := json.Marshal(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": "fake-call-result"},
			},
		})
		return body, nil
	}
	return nil, errors.New("fakeCaller: unknown method " + method)
}

func (c *fakeCaller) Notify(_ context.Context, _ string, _ any) error { return nil }
func (c *fakeCaller) Healthy() bool                                   { return c.healthyFlag.Load() }

// buildPlan is the mock build callback for NewPool. It controls
// per-server failure ordering:
//   - serversAlwaysFail: name → true means every build attempt errors.
//   - serversFailOnce  : name → true means the FIRST build errors,
//     subsequent attempts succeed (mirrors the boot-skip → recovery
//     scenario the LazyResolver exists for).
type buildPlan struct {
	mu             sync.Mutex
	failOnce       map[string]bool
	alwaysFail     map[string]bool
	tools          map[string][]ToolDescriptor
	buildCallCount map[string]int
}

func (b *buildPlan) build(_, name string) (Caller, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buildCallCount[name]++
	if b.alwaysFail[name] {
		return nil, errors.New("fakeBuild: " + name + " is permanently broken")
	}
	if b.failOnce[name] {
		// Clear the flag so the next attempt succeeds.
		delete(b.failOnce, name)
		return nil, errors.New("fakeBuild: " + name + " transient failure (will succeed next time)")
	}
	tools, ok := b.tools[name]
	if !ok {
		return nil, errors.New("fakeBuild: no tools registered for " + name)
	}
	return newFakeCaller(name, tools), nil
}

func (b *buildPlan) calls(name string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildCallCount[name]
}

func newBuildPlan() *buildPlan {
	return &buildPlan{
		failOnce:       make(map[string]bool),
		alwaysFail:     make(map[string]bool),
		tools:          make(map[string][]ToolDescriptor),
		buildCallCount: make(map[string]int),
	}
}

// TestLazyResolver_NotMCPName guards against handling tool names that
// don't match the mcp__server__tool shape — those should fall through
// to the dispatcher's standard "tool not found", NOT take a handshake
// detour. Without this, every miss for a built-in tool name (Read,
// Write, etc.) would block on a useless pool.Get.
func TestLazyResolver_NotMCPName(t *testing.T) {
	pool := NewPool(newBuildPlan().build, nil, nil)
	r := NewLazyResolver(pool, &config.Config{MCPServers: map[string]config.MCPServer{"jobs": {}}}, nil, nil, 0)

	for _, name := range []string{"Read", "Write", "WebSearch", "mcp__", "mcp__jobs", "mcp__jobs__"} {
		_, handled := r.Resolve(context.Background(), name, json.RawMessage(`{}`))
		if handled {
			t.Errorf("name=%q: expected handled=false (fall through), got true", name)
		}
	}
}

// TestLazyResolver_ServerNotConfigured covers names that LOOK like MCP
// tool names but reference a server the operator never declared. Same
// fall-through outcome as TestLazyResolver_NotMCPName — the model
// likely hallucinated the name and the standard "tool not found" is
// the right surface.
func TestLazyResolver_ServerNotConfigured(t *testing.T) {
	pool := NewPool(newBuildPlan().build, nil, nil)
	r := NewLazyResolver(pool, &config.Config{MCPServers: map[string]config.MCPServer{"jobs": {}}}, nil, nil, 0)

	_, handled := r.Resolve(context.Background(), "mcp__unknown__doSomething", json.RawMessage(`{}`))
	if handled {
		t.Errorf("expected handled=false for unconfigured server, got true")
	}
}

// TestLazyResolver_FirstCallTriggersHandshakeAndDispatches is the
// headline scenario: an agent calls a tool from a server that was
// "skipped" at boot. LazyResolver attempts a fresh pool.Get (which
// internally retries build), the build succeeds, the tool is
// registered, and the call dispatches to the underlying mcpTool.
func TestLazyResolver_FirstCallTriggersHandshakeAndDispatches(t *testing.T) {
	plan := newBuildPlan()
	plan.tools["jobs"] = []ToolDescriptor{
		{Name: "getAgentContext", Description: "load context", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	pool := NewPool(plan.build, nil, nil)
	r := NewLazyResolver(pool, &config.Config{MCPServers: map[string]config.MCPServer{"jobs": {}}}, nil, nil, 0)

	res, handled := r.Resolve(context.Background(), "mcp__jobs__getAgentContext", json.RawMessage(`{}`))
	if !handled {
		t.Fatal("expected handled=true after successful lazy resolution")
	}
	if res.IsError {
		t.Fatalf("expected success Result, got IsError=true: %q", res.Text)
	}
	if !strings.Contains(res.Text, "fake-call-result") {
		t.Errorf("expected fake-call-result text, got %q", res.Text)
	}
	if got := plan.calls("jobs"); got != 1 {
		t.Errorf("build called %d times on first lazy resolution, want 1", got)
	}
}

// TestLazyResolver_SecondCallHitsCache verifies that successful
// resolutions are memoised — the second call for any tool from the
// same server must NOT trigger another handshake. Without this, every
// tool call goes through pool.Get which serialises on the entry mutex.
func TestLazyResolver_SecondCallHitsCache(t *testing.T) {
	plan := newBuildPlan()
	plan.tools["jobs"] = []ToolDescriptor{
		{Name: "getAgentContext", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "patchApplication", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	pool := NewPool(plan.build, nil, nil)
	r := NewLazyResolver(pool, &config.Config{MCPServers: map[string]config.MCPServer{"jobs": {}}}, nil, nil, 0)

	for i := 0; i < 5; i++ {
		_, handled := r.Resolve(context.Background(), "mcp__jobs__getAgentContext", json.RawMessage(`{}`))
		if !handled {
			t.Fatalf("iter %d: expected handled=true", i)
		}
	}
	// Even the OTHER tool from the same server should now be in cache —
	// the first resolution registered the entire server's tool map.
	_, handled := r.Resolve(context.Background(), "mcp__jobs__patchApplication", json.RawMessage(`{}`))
	if !handled {
		t.Fatal("expected sibling tool to be cache-resolved")
	}
	if got := plan.calls("jobs"); got != 1 {
		t.Errorf("build called %d times across 6 calls, want 1 (cache miss only on the first)", got)
	}
}

// TestLazyResolver_HandshakeFails covers the recovery path's failure
// case: the operator declared the server, the call comes in, the pool
// build still errors. LazyResolver must return (handled=true) with a
// clear error to the model — falling through (handled=false) would
// produce a generic "tool not found" with no operational signal.
func TestLazyResolver_HandshakeFails(t *testing.T) {
	plan := newBuildPlan()
	plan.alwaysFail["jobs"] = true
	pool := NewPool(plan.build, nil, nil)
	r := NewLazyResolver(pool, &config.Config{MCPServers: map[string]config.MCPServer{"jobs": {}}}, nil, nil, 0)

	res, handled := r.Resolve(context.Background(), "mcp__jobs__getAgentContext", json.RawMessage(`{}`))
	if !handled {
		t.Fatal("expected handled=true on handshake failure (clear error to model)")
	}
	if !res.IsError {
		t.Errorf("expected IsError=true; got success: %q", res.Text)
	}
	if !strings.Contains(res.Text, "unreachable") {
		t.Errorf("expected 'unreachable' in error message; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "jobs") {
		t.Errorf("expected server name 'jobs' in error; got %q", res.Text)
	}
}

// TestLazyResolver_OperatorToolsFilter verifies that the per-
// server tools yaml setting is respected on the lazy path
// just as it would be at boot. Without this, an operator who
// excluded "expensive_tool" from a server's discovered set would
// suddenly see it become callable after a boot-skip recovery.
func TestLazyResolver_OperatorToolsFilter(t *testing.T) {
	plan := newBuildPlan()
	plan.tools["jobs"] = []ToolDescriptor{
		{Name: "safe_tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "expensive_tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	pool := NewPool(plan.build, nil, nil)
	r := NewLazyResolver(pool, &config.Config{
		MCPServers: map[string]config.MCPServer{"jobs": {Tools: []string{"safe_tool"}}},
	}, nil, nil, 0)

	// safe_tool resolves
	_, handled := r.Resolve(context.Background(), "mcp__jobs__safe_tool", json.RawMessage(`{}`))
	if !handled {
		t.Fatal("safe_tool: expected handled=true")
	}
	// expensive_tool is filtered out — server resolved but does not
	// expose this tool name (per the operator's filter).
	res, handled := r.Resolve(context.Background(), "mcp__jobs__expensive_tool", json.RawMessage(`{}`))
	if !handled {
		t.Fatal("expensive_tool: expected handled=true (definitive 'not exposed' error)")
	}
	if !res.IsError {
		t.Fatalf("expensive_tool: expected IsError=true; got %q", res.Text)
	}
	if !strings.Contains(res.Text, "does not expose") {
		t.Errorf("expected 'does not expose' message; got %q", res.Text)
	}
}

// TestLazyResolver_OnResolveCallback verifies the operator-visible
// "lazy-registered N tools" callback fires exactly once per server,
// not on every call to a cache-hit tool.
func TestLazyResolver_OnResolveCallback(t *testing.T) {
	plan := newBuildPlan()
	plan.tools["jobs"] = []ToolDescriptor{
		{Name: "alpha", InputSchema: json.RawMessage(`{"type":"object"}`)},
		{Name: "beta", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	pool := NewPool(plan.build, nil, nil)
	var (
		mu        sync.Mutex
		callbacks []struct {
			server string
			count  int
		}
	)
	r := NewLazyResolver(pool, &config.Config{MCPServers: map[string]config.MCPServer{"jobs": {}}}, nil, func(server string, count int) {
		mu.Lock()
		defer mu.Unlock()
		callbacks = append(callbacks, struct {
			server string
			count  int
		}{server, count})
	}, 0)

	// Drive 3 calls — the callback should fire ONCE for the first one only.
	_, _ = r.Resolve(context.Background(), "mcp__jobs__alpha", json.RawMessage(`{}`))
	_, _ = r.Resolve(context.Background(), "mcp__jobs__beta", json.RawMessage(`{}`))
	_, _ = r.Resolve(context.Background(), "mcp__jobs__alpha", json.RawMessage(`{}`))

	mu.Lock()
	defer mu.Unlock()
	if len(callbacks) != 1 {
		t.Fatalf("callback fired %d times, want 1: %+v", len(callbacks), callbacks)
	}
	if callbacks[0].server != "jobs" || callbacks[0].count != 2 {
		t.Errorf("callback args = (%q, %d), want (jobs, 2)", callbacks[0].server, callbacks[0].count)
	}
}

// TestLazyResolver_ConcurrentCallsCoalesceHandshake verifies that 50
// goroutines hitting the same skipped server simultaneously result in
// a single underlying build call. The pool's existing entry/ready
// coordination handles this; the test is a regression guard against
// future changes to LazyResolver that might bypass that coordination.
func TestLazyResolver_ConcurrentCallsCoalesceHandshake(t *testing.T) {
	plan := newBuildPlan()
	plan.tools["jobs"] = []ToolDescriptor{
		{Name: "getAgentContext", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	pool := NewPool(plan.build, nil, nil)
	r := NewLazyResolver(pool, &config.Config{MCPServers: map[string]config.MCPServer{"jobs": {}}}, nil, nil, 0)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, handled := r.Resolve(context.Background(), "mcp__jobs__getAgentContext", json.RawMessage(`{}`))
			if !handled {
				t.Errorf("expected handled=true under concurrency")
			}
		}()
	}
	wg.Wait()
	if got := plan.calls("jobs"); got != 1 {
		t.Errorf("build called %d times under %d-way concurrent first-touch, want 1", got, N)
	}
}

// TestLazyResolver_DynamicRegistryServerResolves pins the fix for
// dynamically-registered (MCPServerDef) servers: a server absent from the
// static serverConfig but present in the shared DynamicRegistry must still
// resolve its tools. Before the fix the membership gate consulted only
// serverConfig, so a runtime-registered `jobs` server fell through to the
// dispatcher's bare "tool not found" even though the pool could reach it —
// the exact `tool not found: mcp__jobs__postResearchIngest` symptom.
func TestLazyResolver_DynamicRegistryServerResolves(t *testing.T) {
	plan := newBuildPlan()
	plan.tools["jobs"] = []ToolDescriptor{
		{Name: "postResearchIngest", InputSchema: json.RawMessage(`{"type":"object"}`)},
	}
	pool := NewPool(plan.build, nil, nil)
	// serverConfig deliberately omits "jobs" — it lives ONLY in the dynamic
	// registry, as if registered at runtime via `mcpserverdef create`.
	dyn := NewDynamicRegistry()
	dyn.Set(DynamicMCPServerSpec{Name: "jobs", Transport: "http", URL: "http://localhost:3000/api/mcp"})
	r := NewLazyResolver(pool, &config.Config{}, dyn, nil, 0)

	res, handled := r.Resolve(context.Background(), "mcp__jobs__postResearchIngest", json.RawMessage(`{}`))
	if !handled {
		t.Fatal("dynamically-registered server should be handled (pre-fix: fell through → 'tool not found')")
	}
	if res.IsError {
		t.Fatalf("expected success Result, got IsError: %q", res.Text)
	}
	if !strings.Contains(res.Text, "fake-call-result") {
		t.Errorf("expected dispatch to the underlying tool; got %q", res.Text)
	}

	// Control: a server in NEITHER serverConfig NOR the registry still falls
	// through to the dispatcher's generic "tool not found".
	if _, handled2 := r.Resolve(context.Background(), "mcp__ghost__doThing", json.RawMessage(`{}`)); handled2 {
		t.Error("unknown server (not static, not dynamic) must fall through (handled=false)")
	}
}
