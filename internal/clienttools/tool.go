package clienttools

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// ToolPrefix namespaces client-tools in an agent's `tools` allowlist + the tool
// names the model sees — mirroring the `mcp__` convention. A client-registered
// tool "browser.read_page" is granted/called as "client:browser.read_page".
const ToolPrefix = "client:"

// Candidates returns the tools.Tool set to advertise for a principal — one
// delegating adapter per tool its live connections provide (empty when nothing
// is connected). The HTTP server merges these into a run's candidate tools so
// the model sees them; filterTools then narrows to the agent's `client:*` grant.
//
// Design note: unlike dynamic MCP tools (which need a lazy Dispatcher fallback
// for un-enumerated servers that require a handshake), a client-tool needs no
// handshake — it's advertised-or-absent from the in-memory registry — so it is a
// plain advertised tools.Tool whose Execute delegates. No FallbackFunc is
// involved: an advertised name lives in the dispatcher's tool map, so
// Dispatcher.Execute calls this Execute directly.
func Candidates(reg *Registry, key PrincipalKey, timeout time.Duration) []tools.Tool {
	if reg == nil {
		return nil
	}
	provided := reg.Provides(key)
	out := make([]tools.Tool, 0, len(provided))
	for _, sch := range provided {
		out = append(out, toolAdapter{reg: reg, schema: sch, timeout: timeout})
	}
	return out
}

// toolAdapter presents one client-registered tool as a tools.Tool and, on
// Execute, routes the call to the live connection (delegate-and-block).
type toolAdapter struct {
	reg     *Registry
	schema  ToolSchema
	timeout time.Duration
}

func (t toolAdapter) Name() string        { return ToolPrefix + t.schema.Name }
func (t toolAdapter) Description() string { return t.schema.Description }

func (t toolAdapter) InputSchema() json.RawMessage {
	if len(t.schema.InputSchema) == 0 {
		return json.RawMessage(`{"type":"object"}`)
	}
	return t.schema.InputSchema
}

// Execute delegates to the user's live connection and blocks for the reply,
// bounded by the configured timeout (run-cancel also unblocks it). The
// (tenant, subject) routing key comes from RunIdentity — authoritative, never
// the tool input — so a run can only ever reach its own user's machine.
func (t toolAdapter) Execute(ctx context.Context, input json.RawMessage) (tools.Result, error) {
	rid := tools.RunIdentity(ctx)
	key := PrincipalKey{Tenant: rid.TenantID, Subject: rid.UserID}

	timeout := t.timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := t.reg.Invoke(ictx, key, t.schema.Name, input, InvokeMeta{RunID: rid.RootRunID, AgentID: rid.AgentID})
	switch {
	case err == nil:
		if !res.OK {
			msg := res.Error
			if msg == "" {
				msg = "the client reported an error running " + t.Name()
			}
			return tools.Result{Text: msg, IsError: true}, nil
		}
		return tools.Result{Text: renderOutput(res.Output)}, nil
	case errors.Is(err, ErrNoClient):
		return tools.Result{Text: "no client connection is available to run " + t.Name() + " (the providing client is not connected)", IsError: true}, nil
	case errors.Is(err, ErrClientDisconnected):
		return tools.Result{Text: "the client disconnected while running " + t.Name(), IsError: true}, nil
	case errors.Is(err, context.DeadlineExceeded):
		return tools.Result{Text: t.Name() + " timed out waiting for the client to respond", IsError: true}, nil
	default:
		return tools.Result{Text: "client tool error: " + err.Error(), IsError: true}, nil
	}
}

// renderOutput turns the client's JSON output into tool_result text: a JSON
// string is unwrapped to its value; any other JSON is passed through as-is.
func renderOutput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

var _ tools.Tool = toolAdapter{}
