// http_transport.go — HTTP MCP transport (v0.8.15.3+).
//
// loomcycle's stdio MCP transport (server.go's Serve method) handles
// one connection per process via os.Stdin / os.Stdout. The HTTP MCP
// transport handles MANY concurrent connections, each tracked by an
// Mcp-Session-Id header. This file wires the http.Handler that
// implements the Streamable HTTP shape of the MCP 2024-11-05 spec.
//
// Architectural shape (per v0.8.15.x RFC decision C4):
//
//	HTTPHandler (one per loomcycle process)
//	  ├─ Config            (shared with stdio Server; same Connector/Runner)
//	  └─ HTTPSessionStore  (per-session *Session lookup by ID)
//
//	per HTTP request:
//	  ├─ Look up / create *Session
//	  ├─ Construct disposable *Server{cfg, session} (writeMu zero-value = unlocked)
//	  ├─ Construct response writer (jsonWriter or sseWriter based on tool)
//	  └─ Call srv.handleFrame(ctx, body, responseWriter)
//
// The disposable per-request *Server gives us "free" writeMu isolation
// — each HTTP request has its own goroutine, its own writeMu, no
// contention possible.
//
// Same-package access lets us call handleFrame (unexported) without
// new public API on Server.
package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// maxHTTPRequestBodyBytes caps each Streamable HTTP request body.
// Matches the stdio scanner's per-line buffer (4 MB) so the two
// transports have symmetric limits on inbound frame size. Larger
// MCP payloads (e.g., a register_agent with a giant system_prompt
// + many memory_scopes) should still fit comfortably under this.
const maxHTTPRequestBodyBytes = 4 * 1024 * 1024

// HTTPHandler serves MCP over Streamable HTTP. Shared across all HTTP
// MCP sessions on this loomcycle instance; tracks per-session state
// in the embedded HTTPSessionStore.
type HTTPHandler struct {
	cfg      Config
	sessions *HTTPSessionStore
}

// NewHTTPHandler constructs the handler with a fresh session store
// using the default 30-min inactivity TTL. cfg should be the same
// Config passed to New() for the stdio Server — both transports
// share the Connector / Runner / Store / Logf so they dispatch
// through identical business logic.
func NewHTTPHandler(cfg Config) *HTTPHandler {
	if cfg.Logf == nil {
		cfg.Logf = defaultLogf
	}
	if cfg.ServerName == "" {
		cfg.ServerName = "loomcycle"
	}
	return &HTTPHandler{
		cfg:      cfg,
		sessions: NewHTTPSessionStore(0), // 0 → default TTL
	}
}

// Sessions exposes the underlying session store. Used by main.go to
// wire RunHTTPSessionSweeper; tests inspect it directly.
func (h *HTTPHandler) Sessions() *HTTPSessionStore { return h.sessions }

// ServeHTTP implements http.Handler. Routes:
//
//	POST /v1/_mcp                  → dispatch one JSON-RPC frame
//	DELETE /v1/_mcp                → terminate session (Mcp-Session-Id header required)
//	GET /v1/_mcp                   → 405 (no SSE-stream-init shape today)
//
// Authentication is handled by the surrounding middleware chain in
// internal/api/http/server.go (s.authMiddleware) — by the time a
// request reaches this handler, the bearer token has been verified.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePost dispatches a single JSON-RPC request. The protocol shape
// is identical to stdio MCP — we just route the inbound frame through
// the same handleFrame method on a per-request disposable *Server.
func (h *HTTPHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxHTTPRequestBodyBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(body)) > maxHTTPRequestBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds %d-byte limit", maxHTTPRequestBodyBytes))
		return
	}

	// Probe the inbound frame for method + tool name so we can decide
	// (a) whether this is an initialize that needs a fresh session,
	// (b) whether we should reply with SSE (spawn_run + runEvents).
	var probe struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &probe) // probe is best-effort; bad JSON falls through to handleFrame which emits -32700

	sessionID := r.Header.Get("Mcp-Session-Id")
	var sess *Session

	if probe.Method == "initialize" {
		// New session per Streamable HTTP spec. If the client supplied
		// an Mcp-Session-Id, ignore it — initialize always creates a
		// fresh session on the server. (Clients implementing the spec
		// don't send the header on initialize anyway.)
		sess = NewSession()
		sessionID = h.sessions.Create(sess)
	} else {
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest,
				"Mcp-Session-Id header required for non-initialize requests")
			return
		}
		var ok bool
		sess, ok = h.sessions.Get(sessionID)
		if !ok {
			// 404 with a JSON-RPC error body so spec-conforming
			// clients can recover. -32001 is the MCP convention for
			// "session not found or expired" (the spec doesn't fix a
			// code; -32001 is widely used by other MCP servers).
			writeJSONRPCError(w, http.StatusNotFound, -32001, "session not found or expired")
			return
		}
	}

	// Set the Mcp-Session-Id response header BEFORE handleFrame writes
	// the response body. Once the response writer has been written to,
	// header changes are ignored.
	w.Header().Set("Mcp-Session-Id", sessionID)

	// Build a disposable per-request *Server. Same-package access
	// means handleFrame is callable directly. writeMu zero value =
	// unlocked = correct (per-request goroutine, no contention).
	srv := &Server{
		cfg:     h.cfg,
		session: sess,
	}

	// Decide the response writer. Spec allows per-response choice
	// between application/json and text/event-stream. Per RFC C3,
	// SSE only for tools/call where:
	//   - the called tool is spawn_run, AND
	//   - the session opted into runEvents at initialize.
	// Everything else (cancel_run, list_runs, register_agent, builtin
	// wrappers, pause/snapshot, even spawn_run without runEvents) is
	// single-shot application/json.
	useSSE := probe.Method == "tools/call" &&
		probe.Params.Name == "spawn_run" &&
		sess.RunEventsEnabled()

	var responseWriter io.Writer
	if useSSE {
		responseWriter = newSSEWriter(w)
	} else {
		responseWriter = newJSONWriter(w)
	}

	srv.handleFrame(r.Context(), body, responseWriter)
}

// handleDelete terminates a session client-side. Returns 200 on
// successful termination, 400 if no session ID was supplied, 404 if
// the session doesn't exist. The MCP spec marks DELETE as optional
// (sessions can also expire naturally via the inactivity TTL); we
// support it as a courtesy for well-behaved clients.
func (h *HTTPHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "Mcp-Session-Id header required")
		return
	}
	if _, ok := h.sessions.Get(sessionID); !ok {
		writeJSONError(w, http.StatusNotFound, "session not found or expired")
		return
	}
	h.sessions.Delete(sessionID)
	w.WriteHeader(http.StatusNoContent)
}

// --- Response writers ---
//
// Both jsonWriter and sseWriter receive the byte sequence emitted by
// (*Server).writeFrame: first the marshalled JSON-RPC frame, then a
// trailing newline. jsonWriter just passes both through (Content-Type
// header is set on the first write). sseWriter buffers the JSON,
// then on the trailing newline emits a complete SSE "data: <json>\n\n"
// frame and flushes the response writer.

// jsonWriter wraps a single application/json response. Setting
// Content-Type on first write means we get the right header even for
// error frames written by handleFrame's writeError path.
type jsonWriter struct {
	w    http.ResponseWriter
	once sync.Once
}

func newJSONWriter(w http.ResponseWriter) *jsonWriter { return &jsonWriter{w: w} }

func (j *jsonWriter) Write(b []byte) (int, error) {
	j.once.Do(func() {
		j.w.Header().Set("Content-Type", "application/json")
		j.w.WriteHeader(http.StatusOK)
	})
	return j.w.Write(b)
}

// sseWriter wraps a text/event-stream response. The MCP spec says SSE
// responses carry one JSON-RPC frame per `data:` line, with `\n\n`
// terminating each frame.
//
// (*Server).writeFrame always emits exactly two writes per frame:
//
//	w.Write(rawJSON)
//	w.Write([]byte("\n"))
//
// We buffer the JSON write, then on the trailing newline write we
// emit `"data: " + buf + "\n\n"` and flush. This is deterministic
// because writeFrame holds its own writeMu for the duration of both
// calls — no other goroutine can interleave a write mid-frame.
//
// Streaming use case: a spawn_run with runEvents opted in emits one
// SSE frame per notifications/loomcycle/run_event followed by a final
// SSE frame for the tools/call response. Each frame triggers a Flush
// so the client sees events in real time.
type sseWriter struct {
	w    http.ResponseWriter
	buf  bytes.Buffer
	once sync.Once
}

func newSSEWriter(w http.ResponseWriter) *sseWriter { return &sseWriter{w: w} }

func (s *sseWriter) Write(b []byte) (int, error) {
	s.once.Do(func() {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
		s.w.Header().Set("Connection", "keep-alive")
		s.w.WriteHeader(http.StatusOK)
	})
	// writeFrame's contract is: first write = JSON body, second
	// write = single "\n" byte. We detect the terminator by checking
	// for an exact-1-byte newline write.
	if len(b) == 1 && b[0] == '\n' && s.buf.Len() > 0 {
		// Emit the SSE frame: "data: <json>\n\n" + flush.
		frame := append([]byte("data: "), s.buf.Bytes()...)
		frame = append(frame, '\n', '\n')
		s.buf.Reset()
		if _, err := s.w.Write(frame); err != nil {
			return 0, err
		}
		if f, ok := s.w.(http.Flusher); ok {
			f.Flush()
		}
		return len(b), nil
	}
	// Buffer the JSON body until the terminator arrives.
	return s.buf.Write(b)
}

// --- Helpers ---

// writeJSONError emits `{"error": "<msg>"}` with the given HTTP status.
// Used for transport-level errors (malformed body, missing session)
// that occur BEFORE we know whether to use JSON or SSE framing.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(payload)
}

// writeJSONRPCError emits a JSON-RPC 2.0 error response wrapped in an
// HTTP response. Differs from writeJSONError in that the body is the
// JSON-RPC shape (`{jsonrpc: "2.0", id: 0, error: {code, message}}`)
// — used when the client expects a JSON-RPC frame even on errors that
// the HTTP layer surfaces (e.g., session-not-found).
func writeJSONRPCError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"error":   map[string]any{"code": code, "message": msg},
	})
	_, _ = w.Write(payload)
}

// Compile-time interface assertion: HTTPHandler must satisfy http.Handler.
// Catches signature drift the moment ServeHTTP is renamed or its shape
// changes.
var _ http.Handler = (*HTTPHandler)(nil)
