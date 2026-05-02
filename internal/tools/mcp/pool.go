package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Pool manages MCP server connections — at most one Caller per server name
// at a time. Connections are spawned lazily on first use and re-spawned on
// the next call after a crash. Concurrent in-flight calls multiplex over
// the shared connection (the underlying transport handles the JSON-RPC ID
// demuxing).
//
// The Pool is intentionally shared across sessions and tenants: per-tenant
// auth is passed through tool arguments, not by spawning per-tenant
// children. This keeps memory bounded — n active sessions doesn't mean
// n × m MCP child processes.
type Pool struct {
	mu       sync.Mutex
	servers  map[string]*entry
	build    func(name string) (Caller, error)
	teardown func(c Caller) // Close hook for the concrete transport
}

// entry is one server slot. Once created it lives in p.servers until Pool.Close
// or until init failure removes it. The ready channel is closed exactly once,
// when init has finished (success or failure). Concurrent Get callers block
// on ready (with ctx) instead of holding p.mu across the network round-trip.
type entry struct {
	ready  chan struct{} // closed when caller/tools/initErr are final
	caller Caller
	tools  []ToolDescriptor
	err    error // non-nil means init failed and the entry is unusable
}

// NewPool creates a pool. build is called the first time a server is needed;
// it should spawn the transport (e.g. stdio.Spawn) and return its Caller.
// teardown is called on Pool.Close for each live entry; pass a function
// that closes the underlying transport.
func NewPool(build func(name string) (Caller, error), teardown func(c Caller)) *Pool {
	return &Pool{
		servers:  make(map[string]*entry),
		build:    build,
		teardown: teardown,
	}
}

// Get returns the Caller for a named server, spawning the connection on
// first use and performing the MCP handshake + tools/list. Concurrent Get
// callers for the same name share one in-flight init; concurrent Get
// callers for different names don't block each other.
func (p *Pool) Get(ctx context.Context, name string) (Caller, []ToolDescriptor, error) {
	// Phase 1 (fast path): check the map under the lock. If the entry
	// exists and is ready, return immediately. If it exists but is still
	// initialising (some other goroutine got here first), wait on its
	// ready channel without holding p.mu.
	p.mu.Lock()
	e, exists := p.servers[name]
	if !exists {
		e = &entry{ready: make(chan struct{})}
		p.servers[name] = e
	}
	p.mu.Unlock()

	if exists {
		// Wait for the in-progress init (or already-finished entry).
		select {
		case <-e.ready:
			return e.caller, e.tools, e.err
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}

	// Phase 2: we created the entry; we're responsible for initialising it.
	// Drop the lock during build/initialize/list; other goroutines wait on
	// e.ready. On failure, remove the entry from the map so a retry can
	// re-attempt rather than seeing a permanently-dead slot.
	caller, descs, err := p.initEntry(ctx, name)
	if err != nil {
		p.mu.Lock()
		// Only remove if it's still our entry (paranoid against a Close
		// having already cleared the map).
		if cur, ok := p.servers[name]; ok && cur == e {
			delete(p.servers, name)
		}
		p.mu.Unlock()
		e.err = err
		close(e.ready)
		return nil, nil, err
	}
	e.caller = caller
	e.tools = descs
	close(e.ready)
	return caller, descs, nil
}

// initEntry runs build → Initialize → ListTools without any pool locks held.
// On failure the caller (Get) removes the half-built entry from the map.
func (p *Pool) initEntry(ctx context.Context, name string) (Caller, []ToolDescriptor, error) {
	caller, err := p.build(name)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp pool: build %q: %w", name, err)
	}
	if caller == nil {
		return nil, nil, fmt.Errorf("mcp pool: build %q returned nil caller without error", name)
	}
	if _, err := Initialize(ctx, caller, "loomcycle", "0.3"); err != nil {
		if p.teardown != nil {
			p.teardown(caller)
		}
		return nil, nil, fmt.Errorf("mcp pool: handshake %q: %w", name, err)
	}
	descs, err := ListTools(ctx, caller)
	if err != nil {
		if p.teardown != nil {
			p.teardown(caller)
		}
		return nil, nil, fmt.Errorf("mcp pool: tools/list %q: %w", name, err)
	}
	return caller, descs, nil
}

// Tools returns wrapped tools.Tool entries for every server that has
// finished initialising. Each tool is named "mcp__{server}__{tool}" to match
// the convention used by Claude Code (and therefore by jobs-search-agent's
// policies). Entries that are still initialising or that failed init are
// skipped.
func (p *Pool) Tools() []tools.Tool {
	p.mu.Lock()
	// Snapshot the entry pointers so we can safely iterate the ready
	// channels without holding p.mu (ready is closed under no lock).
	entries := make(map[string]*entry, len(p.servers))
	for name, e := range p.servers {
		entries[name] = e
	}
	p.mu.Unlock()

	var out []tools.Tool
	for serverName, e := range entries {
		select {
		case <-e.ready:
			if e.err != nil {
				continue
			}
		default:
			// Still initialising — skip; caller can ask again later.
			continue
		}
		for _, td := range e.tools {
			out = append(out, &mcpTool{
				server:     serverName,
				toolName:   td.Name,
				descriptor: td,
				caller:     e.caller,
			})
		}
	}
	return out
}

// Close tears down all live connections. Idempotent. Skips entries that
// are still initialising or that failed init (no caller to tear down).
func (p *Pool) Close() {
	p.mu.Lock()
	entries := p.servers
	p.servers = map[string]*entry{}
	p.mu.Unlock()

	for _, e := range entries {
		select {
		case <-e.ready:
			if e.err == nil && p.teardown != nil {
				p.teardown(e.caller)
			}
		default:
			// Still initialising — leave it alone; the goroutine that
			// is initing it will close ready and observe the empty map
			// on next Tools() / Get(). Its caller (if any) will be torn
			// down when GC sees no remaining references.
		}
	}
}

// mcpTool wraps an MCP server-side tool descriptor as a loomcycle tools.Tool
// so the dispatcher can route to it the same way it routes to built-ins.
type mcpTool struct {
	server     string
	toolName   string
	descriptor ToolDescriptor
	caller     Caller
}

func (t *mcpTool) Name() string {
	return "mcp__" + sanitiseServerName(t.server) + "__" + t.toolName
}

func (t *mcpTool) Description() string { return t.descriptor.Description }

func (t *mcpTool) InputSchema() json.RawMessage {
	if len(t.descriptor.InputSchema) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return t.descriptor.InputSchema
}

func (t *mcpTool) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	res, err := CallTool(ctx, t.caller, t.toolName, input)
	if err != nil {
		// Distinguish two failure modes:
		//   1. ctx cancellation — the run is going away; the model should
		//      not see a "tool failed, retry" message because the next
		//      iteration's provider call will also fail with ctx.Err and
		//      tear down the run. Surface as a hard error so the dispatcher
		//      reports it once instead of feeding a confusing tool_result
		//      back to the model.
		//   2. Genuine transport/protocol failure — IsError result so the
		//      model can self-correct (e.g. retry with different args).
		if ctx.Err() != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	return tools.Result{
		Text:    JoinTextContent(res),
		IsError: res.IsError,
	}, nil
}

// sanitiseServerName replaces characters that aren't valid in tool names —
// MCP server names allow hyphens, but tool names in our model dispatcher
// should be conservative. We replace `-` with `_` to match Claude Code's
// `mcp__brave-search__...` → it actually keeps hyphens in the segment, so
// we mirror that. Nothing to sanitise; helper is a no-op stub for now.
func sanitiseServerName(name string) string {
	return strings.ReplaceAll(name, " ", "_")
}
