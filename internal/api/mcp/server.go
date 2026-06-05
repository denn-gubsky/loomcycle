// Package mcp implements loomcycle as an MCP (Model Context Protocol)
// server. This is the v0.8.15 capstone: external orchestrators
// (Claude Code via stdio, future HTTP-MCP service consumers) drive
// loomcycle through standard MCP — alternate front-end to /v1/*.
//
// The server CONSUMES the connector.Connector interface and the
// runner.Runner interface from internal/runner. All business logic
// lives in the canonical Connector implementation (*lchttp.Server);
// this package is purely a wire-translation layer.
//
// Transport: stdio in v0.8.15. HTTP Streamable transport lands in
// v0.8.15.x; the dispatch handlers don't change for HTTP — only the
// read/write loop wraps an http.Handler instead of os.Stdin/Stdout.
//
// Architecture:
//
//	stdin →  serve() reads JSON-RPC frames line-by-line
//	    →  routeRequest() dispatches by method:
//	         - initialize / initialized / tools/list / tools/call
//	    →  toolHandlers[name](ctx, sess, args) returns *mcp.CallToolResult
//	    →  writeFrame() emits the JSON-RPC response to stdout
//	stdout ← notifications/loomcycle/run_event during long-running
//	         tools/call invocations (when session opted in via
//	         initialize.capabilities.loomcycle.runEvents=true)
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	loommcp "github.com/denn-gubsky/loomcycle/internal/tools/mcp"
)

// Config configures the MCP server.
type Config struct {
	// Connector is the canonical business-logic surface. Required —
	// every tool handler dispatches through it.
	Connector connector.Connector
	// Runner is the callback-driven loop driver used when the client
	// opts into run-event streaming (Connector.SpawnRun is blocking-
	// only; streaming needs OnEvent callbacks). May be nil; without
	// it, spawn_run still works but ignores the runEvents capability.
	Runner runner.Runner
	// Store is needed for the dynamic-agent TTL sweeper (started by
	// the caller via main.go's worker block). The MCP server itself
	// doesn't write to Store directly — Connector handles that.
	Store store.Store
	// Logf is the log sink. nil → standard library log.Printf.
	Logf func(format string, v ...any)
	// ServerName / ServerVersion are surfaced in the initialize
	// response so adapters can log which loomcycle they're talking
	// to. Empty fallbacks are fine.
	ServerName    string
	ServerVersion string

	// MaxConcurrentCalls bounds how many long-running tool calls
	// (spawn_run, subscribe_channel, peek_channel, stream_user_run_states)
	// may execute concurrently on the stdio dispatch loop. Cheap/control
	// calls (list_*, get_run, cancel_run, …) are never bounded, so they
	// stay responsive even when every slot is occupied. <= 0 → default
	// (defaultMaxConcurrentCalls). Only consulted by Serve (stdio); the
	// HTTP transport is already concurrent at the http.Server level.
	MaxConcurrentCalls int

	// SpawnRunTimeoutMS is the operator default for the spawn_run
	// TRANSPORT timeout (RFC P): how long a spawn_run MCP call may block
	// before loomcycle cancels the run and returns a status:"timeout"
	// result instead of hanging. A caller's per-call timeout_ms narrows
	// this. <= 0 → disabled (the call blocks until the run finishes on
	// its own run_timeout_seconds budget). This is a transport bound,
	// distinct from the run's wall-clock budget.
	SpawnRunTimeoutMS int
}

// Server reads MCP JSON-RPC frames from stdin and writes responses +
// notifications to stdout. One Server per stdio connection (so one
// per loomcycle-as-MCP process). HTTP transport (v0.8.15.x) will
// instantiate one Server per HTTP connection.
type Server struct {
	cfg Config

	// session carries per-connection state — capability flags etc.
	// For stdio there's exactly one session for the process lifetime;
	// initialized as not-yet-handshaked.
	session *Session

	// writeMu serialises writes to stdout — JSON-RPC frames must be
	// emitted atomically (one frame per line). Tool handlers and the
	// notification path both acquire this before writing.
	writeMu sync.Mutex

	// sem bounds concurrently-executing long-running tool calls on the
	// stdio Serve loop (see Config.MaxConcurrentCalls). Acquired INSIDE
	// the per-call goroutine so the read loop never blocks. Buffered to
	// the cap; nil for the HTTP transport's disposable *Server (it never
	// calls Serve, so it never touches sem).
	sem chan struct{}
	// wg tracks in-flight tools/call goroutines so Serve can flush their
	// responses before returning (clean shutdown on stdin EOF).
	wg sync.WaitGroup
}

// defaultMaxConcurrentCalls bounds in-flight long-running tool calls
// when Config.MaxConcurrentCalls is unset. Ample for a single MCP
// client (Claude Code sends roughly one call at a time); the cap exists
// to keep a pathological burst from spawning unbounded in-flight runs,
// not to throttle normal use.
const defaultMaxConcurrentCalls = 16

// New constructs an MCP Server. Caller drives it by calling Serve.
func New(cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = defaultLogf
	}
	if cfg.ServerName == "" {
		cfg.ServerName = "loomcycle"
	}
	maxCalls := cfg.MaxConcurrentCalls
	if maxCalls <= 0 {
		maxCalls = defaultMaxConcurrentCalls
	}
	return &Server{
		cfg:     cfg,
		session: NewSession(),
		sem:     make(chan struct{}, maxCalls),
	}
}

// Serve runs the read-dispatch-write loop until stdin closes or
// ctx is done. Returns nil on clean stdin EOF; non-nil on read /
// write errors. Each line on stdin is one JSON-RPC frame (per MCP's
// stdio framing — newline-delimited JSON, no header).
//
// Dispatch is CONCURRENT for tools/call and INLINE for everything
// else (RFC O). A tools/call request runs on its own goroutine so a
// long-running call (spawn_run blocking on a whole run, a channel
// long-poll, an event wait) can't head-of-line-block the frames the
// client sends behind it — the prior serial loop let one slow call
// freeze every subsequent request, including a cheap list_runs or a
// cancel_run, wedging the whole connection until the process was
// killed. The init handshake stays correctly ordered for free:
// initialize / tools/list / notifications are handled inline on the
// read loop, in arrival order, and a client sends them before any
// tools/call. writeMu keeps concurrent responses + notifications from
// interleaving bytes on stdout.
//
// Long-running tools take a slot in s.sem (acquired inside the
// goroutine, so the read loop never blocks); cheap/control tools run
// unbounded so they stay responsive even when every slot is occupied.
func (s *Server) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	// MCP messages can be large (tool inputs, agent system prompts).
	// Default 64 KB is too small. Use 4 MB ceiling — matches the HTTP
	// server's effective request-size budget.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	// Handlers get a child of ctx so a global shutdown (parent ctx
	// cancel) propagates into in-flight tools/call goroutines.
	handlerCtx, cancelHandlers := context.WithCancel(ctx)
	defer cancelHandlers()

	for scanner.Scan() {
		// Copy the line — scanner reuses its buffer on next Scan().
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) == 0 {
			continue
		}
		if ctx.Err() != nil {
			break
		}

		async, bounded := frameRoute(line)
		if !async {
			// initialize / tools/list / notifications / malformed —
			// fast and ordering-sensitive; handle inline on the read
			// loop (and the HTTP transport reuses handleFrame the same
			// synchronous way).
			s.handleFrame(ctx, line, stdout)
			continue
		}

		// tools/call — dispatch concurrently so it can't HOL-block the
		// next frame.
		s.wg.Add(1)
		go func(fr []byte, bound bool) {
			defer s.wg.Done()
			if bound {
				select {
				case s.sem <- struct{}{}:
					defer func() { <-s.sem }()
				case <-handlerCtx.Done():
					// Global shutdown fired before a slot freed. Respond
					// with an explicit error rather than silently
					// abandoning the call, so the client doesn't wait out
					// its own timeout. Best-effort — stdout may already be
					// tearing down (writeFrame logs + ignores write errors).
					if id, ok := frameRequestID(fr); ok {
						s.writeError(stdout, id, -32603, "server shutting down")
					}
					return
				}
			}
			s.handleFrame(handlerCtx, fr, stdout)
		}(line, bounded)
	}

	scanErr := scanner.Err()
	// Stop reading: wait for in-flight tools/call goroutines to flush
	// their responses before returning, so a caller draining stdout
	// after Serve sees every frame. We do NOT cancel them here on a
	// clean EOF — a client that sent a request then closed stdin still
	// expects the response on stdout. Global shutdown cancellation
	// flows through handlerCtx (child of ctx) instead.
	s.wg.Wait()
	if scanErr != nil {
		return fmt.Errorf("mcp: read stdin: %w", scanErr)
	}
	return ctx.Err()
}

// frameRoute peeks at a JSON-RPC frame to decide stdio dispatch.
// async is true for a tools/call request (run on its own goroutine so
// it can't head-of-line-block later frames). bounded is true when that
// call targets a long-running tool that must take a concurrency slot.
// A best-effort decode failure routes inline (async=false) — the
// authoritative parse + error reporting happens in handleFrame.
func frameRoute(frame []byte) (async, bounded bool) {
	var p struct {
		ID     *int64 `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &p); err != nil {
		return false, false
	}
	if p.ID == nil || p.Method != "tools/call" {
		return false, false
	}
	return true, isLongRunningTool(p.Params.Name)
}

// frameRequestID extracts the JSON-RPC request id from a frame, used to
// address a shutdown error response back to a dropped tools/call. ok is
// false for notifications (no id) or undecodable frames.
func frameRequestID(frame []byte) (int64, bool) {
	var p struct {
		ID *int64 `json:"id"`
	}
	if json.Unmarshal(frame, &p) != nil || p.ID == nil {
		return 0, false
	}
	return *p.ID, true
}

// longRunningTools are the meta-tools whose handler can block for a
// significant or unbounded time — a full run (spawn_run), a long-poll
// (subscribe_channel / peek_channel), or an event wait
// (stream_user_run_states). They take a concurrency slot so a burst
// can't spawn unbounded in-flight work; every other tool (cheap reads
// + control ops like cancel_run / get_run) runs unbounded so it stays
// responsive even when all slots are occupied. Add new blocking tools
// here when they're introduced.
var longRunningTools = map[string]bool{
	"spawn_run":              true,
	"subscribe_channel":      true,
	"peek_channel":           true,
	"stream_user_run_states": true,
}

func isLongRunningTool(name string) bool { return longRunningTools[name] }

// handleFrame decodes one inbound JSON-RPC frame and dispatches to
// the appropriate handler. Requests get a response written back to
// stdout; notifications (no id) get logged + handled but emit no
// response.
func (s *Server) handleFrame(ctx context.Context, frame []byte, stdout io.Writer) {
	// JSON-RPC servers see Requests (id present) and Notifications
	// (no id). Probe for id presence the same way DecodeFrame does
	// for the client side.
	var probe struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		// Malformed frame — emit a best-effort -32700 with id=0 (the
		// "unknown id" convention) so the client gets SOME response
		// instead of waiting forever for a request that never resolves.
		// Without this, a single bad frame from a buggy client stalls
		// the session permanently.
		s.cfg.Logf("mcp: decode probe: %v", err)
		s.writeError(stdout, 0, -32700, "parse error: "+err.Error())
		return
	}
	if probe.ID == nil {
		// Notification — no response. We handle "initialized" + log
		// the rest. (MCP defines a few client-to-server notifications
		// — most loomcycle can safely ignore.)
		var n loommcp.Notification
		if err := json.Unmarshal(frame, &n); err != nil {
			s.cfg.Logf("mcp: decode notification: %v", err)
			return
		}
		if n.Method == "notifications/initialized" {
			s.session.MarkInitialized()
		}
		return
	}

	// Request — must respond.
	var req loommcp.Request
	if err := json.Unmarshal(frame, &req); err != nil {
		s.writeError(stdout, *probe.ID, -32700, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		s.writeError(stdout, req.ID, -32600, "invalid request: jsonrpc must be \"2.0\"")
		return
	}

	switch req.Method {
	case "initialize":
		s.handleInitialize(stdout, req)
	case "tools/list":
		s.handleToolsList(stdout, req)
	case "tools/call":
		s.handleToolsCall(ctx, stdout, req)
	default:
		// MCP -32601 = method not found. Adapters log this and fall
		// through; we don't need to enumerate every spec method we
		// don't yet implement (sampling, elicitation, etc.).
		s.writeError(stdout, req.ID, -32601, "method not found: "+req.Method)
	}
}

// handleInitialize captures the client's capabilities (most
// importantly: did they opt into run-event notifications?) and
// returns our serverInfo + capabilities + protocolVersion.
func (s *Server) handleInitialize(stdout io.Writer, req loommcp.Request) {
	var params struct {
		ProtocolVersion string          `json:"protocolVersion"`
		Capabilities    json.RawMessage `json:"capabilities"`
		ClientInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(stdout, req.ID, -32602, "invalid initialize params: "+err.Error())
		return
	}

	// Parse our v0.8.15-specific capability nesting:
	// capabilities.loomcycle.runEvents = true | false
	var caps struct {
		Loomcycle struct {
			RunEvents bool `json:"runEvents"`
		} `json:"loomcycle"`
	}
	_ = json.Unmarshal(params.Capabilities, &caps) // best-effort; default false
	s.session.SetRunEventsEnabled(caps.Loomcycle.RunEvents)
	s.session.SetClientInfo(params.ClientInfo.Name, params.ClientInfo.Version)

	result := struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}{
		ProtocolVersion: loommcp.ProtocolVersion,
		Capabilities: map[string]any{
			"tools": map[string]any{},
			// Server-side advertised capabilities. Mirrors what we
			// expect from the client side too.
			"loomcycle": map[string]any{
				"runEvents": true,
			},
		},
	}
	result.ServerInfo.Name = s.cfg.ServerName
	result.ServerInfo.Version = s.cfg.ServerVersion

	s.writeResult(stdout, req.ID, result)
}

// handleToolsList returns the static tool descriptor catalogue.
func (s *Server) handleToolsList(stdout io.Writer, req loommcp.Request) {
	descs := toolDescriptors()
	result := loommcp.ToolsListResult{Tools: descs}
	s.writeResult(stdout, req.ID, result)
}

// handleToolsCall is the dispatch for every loomcycle__<tool>
// invocation. Each tool handler returns a *loommcp.CallToolResult
// shaped result; we marshal it as the JSON-RPC response.
func (s *Server) handleToolsCall(ctx context.Context, stdout io.Writer, req loommcp.Request) {
	var params loommcp.CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeError(stdout, req.ID, -32602, "invalid tools/call params: "+err.Error())
		return
	}

	handler, ok := toolHandlerByName(params.Name)
	if !ok {
		s.writeError(stdout, req.ID, -32601, "unknown tool: "+params.Name)
		return
	}

	res, err := handler(ctx, &handlerEnv{
		connector:         s.cfg.Connector,
		runner:            s.cfg.Runner,
		session:           s.session,
		notify:            s.makeNotifier(stdout, req.ID),
		logf:              s.cfg.Logf,
		spawnRunTimeoutMS: s.cfg.SpawnRunTimeoutMS,
	}, params.Arguments)
	if err != nil {
		// Handler returned a Go error (internal failure, not a
		// tool-error). MCP -32603 = internal error.
		s.writeError(stdout, req.ID, -32603, err.Error())
		return
	}
	s.writeResult(stdout, req.ID, res)
}

// writeResult emits a JSON-RPC 2.0 success response.
func (s *Server) writeResult(stdout io.Writer, id int64, result any) {
	raw, err := json.Marshal(result)
	if err != nil {
		s.writeError(stdout, id, -32603, "marshal result: "+err.Error())
		return
	}
	frame := loommcp.Response{JSONRPC: "2.0", ID: id, Result: raw}
	s.writeFrame(stdout, frame)
}

// writeError emits a JSON-RPC 2.0 error response.
func (s *Server) writeError(stdout io.Writer, id int64, code int, message string) {
	frame := loommcp.Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &loommcp.Error{Code: code, Message: message},
	}
	s.writeFrame(stdout, frame)
}

// writeFrame serialises and writes one JSON-RPC frame. Holds writeMu
// so concurrent goroutines (request handlers + notification emitters)
// can't interleave bytes.
func (s *Server) writeFrame(stdout io.Writer, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		s.cfg.Logf("mcp: marshal frame: %v", err)
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := stdout.Write(raw); err != nil {
		s.cfg.Logf("mcp: write stdout: %v", err)
		return
	}
	if _, err := stdout.Write([]byte("\n")); err != nil {
		s.cfg.Logf("mcp: write newline: %v", err)
	}
}

// makeNotifier returns a notifier closure that handlers can call to
// emit notifications/loomcycle/run_event during a long-running
// tools/call. Captures stdout + writeMu via the Server reference.
func (s *Server) makeNotifier(stdout io.Writer, _ int64) NotifyFunc {
	return func(method string, params any) {
		n, err := loommcp.NewNotification(method, params)
		if err != nil {
			s.cfg.Logf("mcp: notify build: %v", err)
			return
		}
		s.writeFrame(stdout, n)
	}
}

// NotifyFunc is the signature passed to handlers for emitting
// server-to-client notifications during long-running calls.
type NotifyFunc func(method string, params any)

// handlerEnv carries the things every tool handler needs. Passing
// one struct keeps handler signatures uniform (matters because we
// register them by name in a map).
type handlerEnv struct {
	connector connector.Connector
	runner    runner.Runner
	session   *Session
	notify    NotifyFunc
	logf      func(format string, v ...any)
	// spawnRunTimeoutMS is the operator-default transport timeout for
	// spawn_run (Config.SpawnRunTimeoutMS); 0 = disabled.
	spawnRunTimeoutMS int
}

func defaultLogf(format string, v ...any) {
	log.Printf("mcp: "+format, v...)
}
