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

// LazyResolver implements tools.FallbackFunc for "MCP server was skipped
// at boot, but is now reachable" recovery.
//
// Why it exists. Loomcycle's startup loop in cmd/loomcycle/main.go
// initialises every configured MCP server with a 30-second budget. If
// a peer is slow / unreachable / broken when loomcycle boots, the
// server's tools never get added to the global s.tools registry. Per-
// run dispatchers then can't find them — agents see "tool not found"
// even after the peer recovers, until loomcycle itself is restarted.
//
// In a server environment where peers (jobs-search-web, other MCP
// services) restart independently of loomcycle, this is constant
// operational pain.
//
// What it does. Wired as the Dispatcher's FallbackFunc, Resolve runs
// when a tool name is missing from the static dispatcher map:
//
//  1. Parse the name as `mcp__<server>__<tool>`. If it's not that
//     shape, return (zero, false) so the dispatcher emits its standard
//     "tool not found" — preserves existing semantics for non-MCP misses.
//  2. Look up <server> in the configured-servers set. If unknown,
//     same fall-through (operator never declared it; the model
//     probably hallucinated the name).
//  3. Cache hit on the resolver's internal map? Execute the cached Tool
//     and return.
//  4. Cache miss: attempt one fresh pool.Get for the server (the pool
//     handles concurrent-stampede coordination internally — the worst
//     case is the calling goroutine waits for an in-flight init).
//     On success, cache every tool the server exposes (after applying
//     the operator's per-server allowed_tools filter) and dispatch the
//     requested one. On failure, surface a clear error to the model
//     naming the server's last-known unreachability reason.
//
// What it does NOT do.
//   - Does not mutate s.tools (the global registry). Lazy-resolved
//     tools live only in this resolver's memo. The model already knew
//     the tool name (from its agent prompt or earlier turns); the
//     fallback hands the call to the right Tool. Tools that need to be
//     ADVERTISED in the spec list to a fresh model would still need
//     boot-time registration. That's an orthogonal v1.x concern.
//   - Does not periodically re-attempt skipped servers. Recovery
//     happens only when an agent actually needs a tool from the
//     server. This avoids background traffic to peers that may
//     genuinely be down. A future background-probe loop is fine to
//     stack on top — it would call Resolve preemptively for known
//     server names.
type LazyResolver struct {
	pool         *Pool
	serverConfig map[string]ServerCfg

	// onResolve, if non-nil, is called once per server when its tools
	// are first registered via the lazy path. Used for operator-visible
	// "mcp[%s]: lazy-registered N tools after agent call (was skipped at
	// boot)" log lines.
	onResolve func(server string, count int)

	// handshakeTimeout caps the per-call Get budget. The pool's own
	// init has no internal timeout (relies on ctx); if the peer is
	// slow but recoverable this gives a definite ceiling on how long
	// the agent's tool call blocks. Default is 10s.
	handshakeTimeout time.Duration

	mu         sync.Mutex
	registered map[string]map[string]tools.Tool // server → tool name → Tool
}

// ServerCfg is the subset of an MCP server's yaml config that the
// resolver needs to reproduce boot-time tool registration faithfully.
// Only AllowedTools today; if the boot path grows more per-server
// knobs (e.g. per-server pool size already lives on Pool), they
// should join here.
type ServerCfg struct {
	AllowedTools []string
}

// NewLazyResolver builds a resolver. configs MUST cover every server
// the operator yaml declares — Resolve uses configs as the membership
// check for "is this name plausibly an MCP tool we should try?". A
// server that successfully registered at boot can still appear in
// configs; the resolver simply won't be reached for it (its tools are
// in the dispatcher's static map).
//
// onResolve is optional; pass log.Printf for production. handshakeTimeout
// of 0 falls back to a sane default.
func NewLazyResolver(pool *Pool, configs map[string]ServerCfg, onResolve func(string, int), handshakeTimeout time.Duration) *LazyResolver {
	if handshakeTimeout <= 0 {
		handshakeTimeout = 10 * time.Second
	}
	return &LazyResolver{
		pool:             pool,
		serverConfig:     configs,
		onResolve:        onResolve,
		handshakeTimeout: handshakeTimeout,
		registered:       make(map[string]map[string]tools.Tool),
	}
}

// Resolve satisfies tools.FallbackFunc. See type-level docstring for the
// full state machine.
func (r *LazyResolver) Resolve(ctx context.Context, name string, input json.RawMessage) (tools.Result, bool) {
	server, ok := r.parseServer(name)
	if !ok {
		return tools.Result{}, false
	}
	cfg, configured := r.serverConfig[server]
	if !configured {
		return tools.Result{}, false
	}

	// Cache fast path.
	r.mu.Lock()
	if reg, ok := r.registered[server]; ok {
		t, hit := reg[name]
		r.mu.Unlock()
		if hit {
			return executeAndWrap(ctx, t, input), true
		}
		// Server resolved but doesn't expose this specific tool.
		// Definitive (handled=true) so the model sees a clear error
		// instead of attempting another lazy-resolve cycle.
		return tools.Result{
			Text:    fmt.Sprintf("tool not found: %s (mcp server %q is registered but does not expose this tool name)", name, server),
			IsError: true,
		}, true
	}
	r.mu.Unlock()

	// Cache miss. Attempt handshake with a per-call ceiling. The pool's
	// internal entry/ready coordination means concurrent calls for the
	// same server share one handshake; we just block on the same channel.
	hsCtx, cancel := context.WithTimeout(ctx, r.handshakeTimeout)
	defer cancel()
	_, descs, err := r.pool.Get(hsCtx, server)
	if err != nil {
		return tools.Result{
			Text:    fmt.Sprintf("tool not found: %s (mcp server %q unreachable: %v — verify the peer is healthy and retry)", name, server, summariseErr(err)),
			IsError: true,
		}, true
	}

	// Apply the operator's per-server filter; build the tool map.
	filtered := ApplyAllowedToolsFilter(descs, cfg.AllowedTools)
	toolMap := make(map[string]tools.Tool, len(filtered))
	for _, d := range filtered {
		t := NewTool(r.pool, server, d)
		toolMap[t.Name()] = t
	}

	// Publish to the cache. Even if another goroutine resolved the same
	// server while we were calling Get, overwriting is safe — the tool
	// map is built from the same pool/descs and Tool instances are
	// stateless wrappers. Last writer wins; no harm.
	r.mu.Lock()
	r.registered[server] = toolMap
	r.mu.Unlock()

	if r.onResolve != nil {
		r.onResolve(server, len(toolMap))
	}

	t, hit := toolMap[name]
	if !hit {
		return tools.Result{
			Text:    fmt.Sprintf("tool not found: %s (mcp server %q is now reachable but does not expose this tool name; check the operator's allowed_tools filter)", name, server),
			IsError: true,
		}, true
	}
	return executeAndWrap(ctx, t, input), true
}

// parseServer extracts the server segment from `mcp__<server>__<tool>`.
// Returns ("", false) if the name doesn't match the prefix shape OR if
// the tool segment is empty (e.g. "mcp__jobs__"). The server segment
// itself is matched against the configured set in Resolve, so this
// helper does no further validation beyond shape.
func (r *LazyResolver) parseServer(name string) (string, bool) {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	rest := name[len(prefix):]
	// Find the "__" separator between server and tool.
	idx := strings.Index(rest, "__")
	if idx <= 0 {
		return "", false
	}
	server := rest[:idx]
	toolName := rest[idx+2:]
	if toolName == "" {
		return "", false
	}
	// Normalise via the same helper Tool.Name() uses, so a
	// "brave search" config (with a space) would match the
	// "mcp__brave_search__..." names the model sees.
	if sanitiseServerName(server) != server {
		// Names emitted by mcpTool.Name() always go through
		// sanitiseServerName, so an incoming `name` with non-
		// sanitised characters can't have come from us. Reject.
		return "", false
	}
	return server, true
}

// executeAndWrap runs a Tool's Execute and converts a Go error into
// the standard error-shaped Result the dispatcher expects. Mirrors the
// equivalent block in Dispatcher.Execute so callers of FallbackFunc
// see the same surface as native dispatcher hits.
func executeAndWrap(ctx context.Context, t tools.Tool, input json.RawMessage) tools.Result {
	res, err := t.Execute(ctx, input)
	if err != nil {
		return tools.Result{Text: err.Error(), IsError: true}
	}
	return res
}

// summariseErr trims long error strings (e.g. an HTML 500-page body)
// to a 200-char tail so the model gets a useful hint without 9 KB of
// peer-side output appearing in tool_result.text.
func summariseErr(err error) string {
	s := err.Error()
	const maxLen = 200
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…(truncated)"
}
