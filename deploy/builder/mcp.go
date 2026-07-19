package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

const protocolVersion = "2024-11-05"

// rpcResponse mirrors loomcycle's mcp.Response wire shape.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MCPHandler serves the Streamable-HTTP MCP surface (single POST → JSON
// response), matching what loomcycle's internal/tools/mcp/http client expects.
type MCPHandler struct {
	cfg  *Config
	disp *Dispatcher
	ver  string
}

func NewMCPHandler(cfg *Config, disp *Dispatcher, version string) *MCPHandler {
	return &MCPHandler{cfg: cfg, disp: disp, ver: version}
}

func (h *MCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, ok := h.authenticate(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var probe struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeRPCError(w, 0, -32700, "parse error")
		return
	}

	// Notifications (no id) get 202 with no body — loomcycle's Notify only
	// checks the status code.
	if probe.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	id := *probe.ID

	switch probe.Method {
	case "initialize":
		// Hand out a session correlator; loomcycle stores + echoes it. We do
		// not require it on later requests (no per-MCP-session server state).
		w.Header().Set(sessionHeaderName, newSessionCorrelator())
		writeRPCResult(w, id, mustJSON(map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "loomcycle-builder", "version": h.ver},
		}))
	case "tools/list":
		writeRPCResult(w, id, mustJSON(map[string]any{"tools": h.disp.Tools()}))
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(probe.Params, &p); err != nil {
			writeRPCError(w, id, -32602, "invalid params")
			return
		}
		// The run identifiers are attested — read from the headers loomcycle
		// filled (X-Loom-Root-Run / X-Loom-Tenant, RFC BI P2b), never from tool
		// args. Empty when the caller didn't send them.
		c := caller{
			Principal: principal,
			RootRun:   strings.TrimSpace(r.Header.Get("X-Loom-Root-Run")),
			Tenant:    strings.TrimSpace(r.Header.Get("X-Loom-Tenant")),
		}
		text, isErr, cerr := h.disp.Call(r.Context(), c, p.Name, p.Arguments)
		if cerr != nil {
			// Protocol-level fault (unknown tool, malformed args).
			writeRPCError(w, id, -32602, cerr.Error())
			return
		}
		writeRPCResult(w, id, mustJSON(map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": isErr,
		}))
	default:
		writeRPCError(w, id, -32601, "method not found: "+probe.Method)
	}
}

// authenticate checks the bearer token (constant-time) and returns the derived
// principal. In AllowAnon mode every caller shares the "anon" principal.
func (h *MCPHandler) authenticate(r *http.Request) (principal string, ok bool) {
	if h.cfg.AllowAnon {
		return "anon", true
	}
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		return "", false
	}
	// Constant-time comparison via fixed-length digests (mirrors loomcycle's
	// auth-middleware invariant — a length-leaking compare is a side channel).
	got := sha256.Sum256([]byte(tok))
	want := sha256.Sum256([]byte(h.cfg.AuthToken))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		return "", false
	}
	return principalFromToken(tok), true
}

// principalFromToken derives a stable, non-secret owner key from the bearer.
// P1 has a single shared token → a single principal; the store's principal
// check is the seam P2 fills with a per-tenant key from an attested header.
func principalFromToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return "op:" + hex.EncodeToString(sum[:8])
}

const sessionHeaderName = "Mcp-Session-Id"

func newSessionCorrelator() string {
	id, err := newID()
	if err != nil {
		return "sbx-session"
	}
	return id
}

func writeRPCResult(w http.ResponseWriter, id int64, result json.RawMessage) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeRPCError(w http.ResponseWriter, id int64, code int, msg string) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("mcp: encode response: %v", err)
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return b
}
