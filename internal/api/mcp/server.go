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
}

// New constructs an MCP Server. Caller drives it by calling Serve.
func New(cfg Config) *Server {
	if cfg.Logf == nil {
		cfg.Logf = defaultLogf
	}
	if cfg.ServerName == "" {
		cfg.ServerName = "loomcycle"
	}
	return &Server{
		cfg:     cfg,
		session: NewSession(),
	}
}

// Serve runs the read-dispatch-write loop until stdin closes or
// ctx is done. Returns nil on clean stdin EOF; non-nil on read /
// write errors. Each line on stdin is one JSON-RPC frame (per MCP's
// stdio framing — newline-delimited JSON, no header).
//
// Frames are dispatched SEQUENTIALLY per the MCP initialization
// contract: initialize must complete before any tool call, and the
// initialized notification must arrive before other requests are
// honored. Serialising the entire dispatch satisfies this without
// per-method gating. Concurrent tools/call (long-running spawn_run
// vs short list_runs on the same connection) is a v0.8.15.x or v0.9.x
// optimisation; the writeMu is retained for the notification path
// where in-flight spawn_run emits notifications while reading the
// next frame is desired — but those notifications come from the
// handler goroutine, which is the same goroutine that's dispatching.
//
// In practice MCP clients (Claude Code first) send one request at a
// time and wait for the response, so sequential dispatch matches
// real-world usage.
func (s *Server) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	// MCP messages can be large (tool inputs, agent system prompts).
	// Default 64 KB is too small. Use 4 MB ceiling — matches the HTTP
	// server's effective request-size budget.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		// Copy the line — scanner reuses its buffer on next Scan().
		line := append([]byte(nil), scanner.Bytes()...)
		if len(line) == 0 {
			continue
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		s.handleFrame(ctx, line, stdout)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("mcp: read stdin: %w", err)
	}
	return nil
}

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
		s.cfg.Logf("mcp: decode probe: %v", err)
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
		connector: s.cfg.Connector,
		runner:    s.cfg.Runner,
		session:   s.session,
		notify:    s.makeNotifier(stdout, req.ID),
		logf:      s.cfg.Logf,
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
}

func defaultLogf(format string, v ...any) {
	log.Printf("mcp: "+format, v...)
}
