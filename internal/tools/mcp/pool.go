package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

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
//
// If a cached entry's Caller is no longer Healthy (e.g. the stdio child
// crashed), Get evicts it and re-initialises a fresh entry inline. The
// caller never sees a "permanently dead" slot.
func (p *Pool) Get(ctx context.Context, name string) (Caller, []ToolDescriptor, error) {
	// Phase 1 (fast path): check the map under the lock. If the entry
	// exists, ready, and healthy, return immediately. If it exists but is
	// still initialising, wait on its ready channel without holding p.mu.
	// If it's ready but unhealthy, evict and fall through to a fresh init.
	p.mu.Lock()
	e, exists := p.servers[name]
	if exists {
		select {
		case <-e.ready:
			// Init finished. Check the result.
			if e.err == nil && e.caller != nil && e.caller.Healthy() {
				p.mu.Unlock()
				return e.caller, e.tools, nil
			}
			// Either init failed earlier or the caller is now unhealthy.
			// Evict so the rest of this function creates a fresh entry.
			if e.err == nil && e.caller != nil && p.teardown != nil {
				p.teardown(e.caller)
			}
			delete(p.servers, name)
			exists = false
		default:
			// Still initialising — fall through to wait.
		}
	}
	if !exists {
		e = &entry{ready: make(chan struct{})}
		p.servers[name] = e
	}
	p.mu.Unlock()

	if exists {
		// Wait for the in-progress init.
		select {
		case <-e.ready:
			if e.err == nil && e.caller != nil && e.caller.Healthy() {
				return e.caller, e.tools, nil
			}
			if e.err != nil {
				return nil, nil, e.err
			}
			// Won the race in the most awkward way: it became unhealthy
			// between ready and our re-check. One retry resolves it.
			return p.Get(ctx, name)
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

// GetWithRetry calls Get with exponential backoff on failure: 500ms,
// 1s, 2s, 4s, 8s, 16s capped (cumulative ~32s). Stops on first success
// or when ctx is done. Each retry log line names the attempt + the
// error so operators can see whether the wait is meaningful.
//
// Use case: chicken-and-egg start order. The MCP server lives behind a
// dependency that boots concurrently with loomcycle (e.g. a Next.js
// dev server compiling its `/api/mcp` route on first request). Without
// retry, a slow-starting peer means loomcycle marks the server "skipped"
// and the operator has to restart loomcycle after the peer is up.
func (p *Pool) GetWithRetry(ctx context.Context, name string, logf func(string, ...any)) (Caller, []ToolDescriptor, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	const initialDelay = 500 * time.Millisecond
	const maxDelay = 16 * time.Second
	delay := initialDelay
	attempt := 0
	var lastErr error
	for {
		attempt++
		caller, descs, err := p.Get(ctx, name)
		if err == nil {
			if attempt > 1 {
				logf("mcp[%s]: handshake succeeded on attempt %d", name, attempt)
			}
			return caller, descs, nil
		}
		lastErr = err
		// Done? Either ctx already cancelled before attempt, or it
		// cancelled DURING the Get above. Either way, stop retrying.
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("mcp[%s]: gave up after %d attempt(s): %w", name, attempt, lastErr)
		}
		logf("mcp[%s]: handshake failed (attempt %d): %v — retrying in %s", name, attempt, err, delay)
		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("mcp[%s]: gave up after %d attempt(s): %w", name, attempt, lastErr)
		case <-time.After(delay):
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
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
			out = append(out, NewTool(p, serverName, td))
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

// Evict tears down the named entry if one exists. Used by the v0.9.x
// MCPServerDef substrate when a server is retired or replaced (a new
// version promoted) — the cached client must not keep serving against
// the old URL / bearer / transport metadata.
//
// In-flight tool calls against the evicted entry continue to use the
// caller object they already hold; the underlying transport closes
// gracefully when those calls finish. A subsequent Get(name) finds the
// map empty and triggers a fresh build() — which consults the dynamic
// registry for the up-to-date spec.
//
// Returns true if an entry was evicted (existed in the map).
func (p *Pool) Evict(name string) bool {
	p.mu.Lock()
	e, exists := p.servers[name]
	if exists {
		delete(p.servers, name)
	}
	p.mu.Unlock()
	if !exists {
		return false
	}
	// Only tear down if init completed successfully — half-built entries
	// have their teardown handled by the initEntry failure path.
	select {
	case <-e.ready:
		if e.err == nil && e.caller != nil && p.teardown != nil {
			p.teardown(e.caller)
		}
	default:
		// Still initialising — leave it alone; the in-progress
		// initEntry will close ready and the next Get(name) finds the
		// map empty.
	}
	return true
}

// mcpTool wraps an MCP server-side tool descriptor as a loomcycle tools.Tool
// so the dispatcher can route to it the same way it routes to built-ins.
//
// Critically, the tool resolves its Caller via Pool.Get on every Execute —
// not via a captured pointer at registration time. That way, if the stdio
// child crashes and Pool.Get respawns it, this tool's next call uses the
// new caller automatically, with no re-registration step.
type mcpTool struct {
	server     string
	toolName   string
	descriptor ToolDescriptor
	pool       *Pool
}

// NewTool wraps a single MCP tool descriptor as a tools.Tool. Operators
// that want per-server allowed_tools filtering can call this directly with
// the subset of descriptors they want to expose.
func NewTool(pool *Pool, server string, descriptor ToolDescriptor) tools.Tool {
	return &mcpTool{
		server:     server,
		toolName:   descriptor.Name,
		descriptor: descriptor,
		pool:       pool,
	}
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
	caller, _, err := t.pool.Get(ctx, t.server)
	if err != nil {
		// Pool spawn / handshake failed. ctx cancellation goes through as
		// a hard error; everything else as a recoverable IsError so the
		// model can see "MCP server unavailable" and decide what to do.
		if ctx.Err() != nil {
			return tools.Result{}, err
		}
		return tools.Result{Text: err.Error(), IsError: true}, nil
	}
	res, err := CallTool(ctx, caller, t.toolName, input)
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
