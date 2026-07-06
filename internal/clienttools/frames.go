// Package clienttools is the runtime side of RFC BC — client-executed tools
// over a persistent connection (local tool host). A client (the loomboard
// Chrome extension, the Tauri desktop app) opens a WebSocket to loomcycle,
// registers the tools it can execute on the user's machine (browser DOM, local
// FS, shell), and loomcycle routes a matching agent tool call to that
// connection — delegate-and-block — returning the client's reply as an ordinary
// tools.Result.
//
// This package owns the per-principal connection registry + the invoke↔result
// correlation. It is transport-shaped but transport-agnostic: the WebSocket
// read/write lives in the HTTP handler (internal/api/http), which feeds inbound
// result frames to Conn.DeliverResult and provides the outbound write closure —
// so this package needs no WebSocket dependency and is unit-testable with a
// plain fake sender. The dispatch FallbackFunc that turns an agent's
// `client:<tool>` call into an Invoke lives in resolver.go.
package clienttools

import "encoding/json"

// Frame type discriminators on the /v1/client-tools WebSocket (RFC BC §4).
const (
	FrameHello   = "hello"    // client→server: register provided tools
	FrameHelloOK = "hello_ok" // server→client: ack + accepted names
	FrameInvoke  = "invoke"   // server→client: a tool call to run locally
	FrameResult  = "result"   // client→server: the reply to an invoke
	FramePing    = "ping"     // heartbeat (either direction)
	FramePong    = "pong"     // heartbeat reply
)

// ToolSchema is one client-provided tool as advertised in a hello frame. The
// name is the bare tool name (e.g. "browser.read_page"); the runtime exposes it
// to agents under the `client:` prefix (client:browser.read_page).
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// HelloFrame is the client→server registration on connect. Re-sending it
// replaces the prior tool set (idempotent reconnect).
type HelloFrame struct {
	Type   string       `json:"type"`
	Client string       `json:"client,omitempty"` // e.g. "loomboard-ext/0.2"
	Tools  []ToolSchema `json:"tools"`
}

// HelloOKFrame is the server→client ack naming the accepted tool names.
type HelloOKFrame struct {
	Type         string   `json:"type"`
	ConnectionID string   `json:"connection_id"`
	Accepted     []string `json:"accepted"`
}

// InvokeFrame is a server→client tool call. Input is the tool's JSON input,
// already validated shape-wise against the registered input_schema server-side.
type InvokeFrame struct {
	Type    string          `json:"type"`
	CallID  string          `json:"call_id"`
	RunID   string          `json:"run_id,omitempty"`
	AgentID string          `json:"agent_id,omitempty"`
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input"`
}

// ResultFrame is the client→server reply to an invoke. ok:false + error becomes
// a tool error the agent sees; ok:true + output becomes the tool result content.
type ResultFrame struct {
	Type   string          `json:"type"`
	CallID string          `json:"call_id"`
	OK     bool            `json:"ok"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// TypeOf peeks a frame's discriminator without fully decoding it — the handler
// read-loop uses it to route inbound frames (result vs pong).
func TypeOf(raw []byte) string {
	var env struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &env)
	return env.Type
}
