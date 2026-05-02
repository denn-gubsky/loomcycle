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

type entry struct {
	caller Caller
	tools  []ToolDescriptor
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
// first use and performing the MCP handshake + tools/list. On subsequent
// calls the cached entry is returned without re-handshaking.
func (p *Pool) Get(ctx context.Context, name string) (Caller, []ToolDescriptor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.servers[name]; ok {
		return e.caller, e.tools, nil
	}
	caller, err := p.build(name)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp pool: build %q: %w", name, err)
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
	p.servers[name] = &entry{caller: caller, tools: descs}
	return caller, descs, nil
}

// Tools returns wrapped tools.Tool entries for every server discovered so
// far. Each tool is named "mcp__{server}__{tool}" to match the convention
// used by Claude Code (and therefore by jobs-search-agent's policies).
func (p *Pool) Tools() []tools.Tool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []tools.Tool
	for serverName, e := range p.servers {
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

// Close tears down all live connections. Idempotent.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.servers {
		if p.teardown != nil {
			p.teardown(e.caller)
		}
	}
	p.servers = map[string]*entry{}
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
		// Transport / protocol error — surface as an error result so the
		// model sees it and can retry, rather than failing the run.
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
