package http

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/authz"
	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/tools/builtin"
)

// RFC FY Part E — the twins ask what their route asks. The gRPC RPCs and the
// MCP meta-tools reach memory and channels through these shared connector
// operations, whose RPC or meta-tool gate asks only a type-level read; the
// HTTP routes ask a write route `write`, and a channel route the instance it
// names. The operation asks the same here, so a deny holds on every
// transport. On HTTP the route has already asked it (routeGateAnswered).

// connectorQuestions is every question a shared connector operation asks
// beyond its transport gate, for the enforcement census.
var connectorQuestions = []authz.Surface{
	{Action: "write", Type: "memory", Name: authz.AnyInstance},
	{Action: "write", Type: "sqlmem", Name: authz.AnyInstance},
	{Action: "publish", Type: "channel"},
	{Action: "subscribe", Type: "channel"},
	{Action: "ack", Type: "channel"},
}

// connectorAsk asks surface for the principal on ctx and returns the refusal,
// or nil. No principal (open mode, stdio): nothing to ask, as at the gates.
func (s *Server) connectorAsk(ctx context.Context, surface authz.Surface) error {
	if routeGateAnswered(ctx, surface) {
		return nil
	}
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil
	}
	return s.aclAdmit(ctx, p, surface)
}

// memoryOpQuestion is the write question a Memory call asks, or nil for an op
// that only reads. A SQL op writes the SQL keyspace.
func memoryOpQuestion(input json.RawMessage) *authz.Surface {
	var head struct {
		Op string `json:"op"`
	}
	_ = json.Unmarshal(input, &head)
	if !builtin.MemoryOpWrites(head.Op) {
		return nil
	}
	typ := "memory"
	if strings.HasPrefix(head.Op, "sql_") {
		typ = "sqlmem"
	}
	return &authz.Surface{Action: "write", Type: typ, Name: authz.AnyInstance}
}

// channelOpQuestion is the instance question a Channel tool call asks of the
// channel it names — publish, subscribe (peek too) or ack — or nil.
func channelOpQuestion(input json.RawMessage) *authz.Surface {
	var head struct {
		Op      string `json:"op"`
		Channel string `json:"channel"`
	}
	_ = json.Unmarshal(input, &head)
	if head.Channel == "" {
		return nil
	}
	return channelQuestion(head.Op, head.Channel)
}

// channelQuestion is the instance question an op asks of a channel, as its
// HTTP route asks it.
func channelQuestion(op, channel string) *authz.Surface {
	verb := ""
	switch op {
	case "publish":
		verb = "publish"
	case "subscribe", "peek":
		verb = "subscribe"
	case "ack":
		verb = "ack"
	default:
		return nil
	}
	if channel == "" {
		channel = authz.AnyInstance
	}
	return &authz.Surface{Action: verb, Type: "channel", Name: channel}
}

// refusalResult is an ACL refusal as a tool result, in the route's body.
func refusalResult(err error) (connector.ToolResult, bool) {
	ad, ok := err.(*errACLDenied)
	if !ok {
		return connector.ToolResult{}, false
	}
	return aclToolResult(aclReply{403, aclDeniedBody(ad.surface, ad.decision)}), true
}
