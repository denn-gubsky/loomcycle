package http

import (
	"context"
	"encoding/json"
	"errors"
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

// connectorAsk asks surface for the principal on ctx and returns the refusal,
// or nil. Every call site is classified by the enforcement census
// (TestACLEnforcement_ConnectorOperationsAskWhatTheTableSays reads this
// package's source), so a new one cannot ask a pair the table does not
// credit. No principal (open mode, stdio): nothing to ask, as at the gates.
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

// channelOpQuestions is every question a Channel tool call asks, as the HTTP
// route of its op asks it: publish, subscribe/peek, ack and release of the
// channel they name (every channel when none is named), await and broadcast
// the type-level question their routes ask plus each channel they name, and
// list_channels the read of every channel. An op this table does not know is
// refused (errUnknownChannelOp), never let through unasked: a deny must not
// be evaded by naming another op.
func channelOpQuestions(input json.RawMessage) ([]authz.Surface, error) {
	var head struct {
		Op       string   `json:"op"`
		Channel  string   `json:"channel"`
		Channels []string `json:"channels"`
	}
	_ = json.Unmarshal(input, &head)
	switch head.Op {
	case "publish", "subscribe", "peek", "ack", "release":
		return []authz.Surface{*channelQuestion(head.Op, head.Channel)}, nil
	case "list_channels":
		return []authz.Surface{{Action: "subscribe", Type: "channel", Name: authz.AnyInstance}}, nil
	case "await", "broadcast":
		out := []authz.Surface{{Action: head.Op, Type: "channel", Name: authz.AnyInstance}}
		for _, c := range head.Channels {
			if c != "" {
				out = append(out, authz.Surface{Action: head.Op, Type: "channel", Name: c})
			}
		}
		return out, nil
	}
	return nil, errUnknownChannelOp
}

// errUnknownChannelOp refuses a Channel op the question table does not map.
var errUnknownChannelOp = errors.New("channel: op is not one this server asks access-control questions for")

// channelQuestion is the instance question an op asks of a channel, as its
// HTTP route asks it ("" names every channel); nil for an op that names no
// single channel.
func channelQuestion(op, channel string) *authz.Surface {
	verb := ""
	switch op {
	case "publish":
		verb = "publish"
	case "subscribe", "peek":
		verb = "subscribe"
	case "ack":
		verb = "ack"
	case "release", "manage":
		// POST /v1/_channels/{name}/release asks manage, as PATCH and DELETE
		// of the channel and its purge route do.
		verb = "manage"
	default:
		return nil
	}
	if channel == "" {
		channel = authz.AnyInstance
	}
	return &authz.Surface{Action: verb, Type: "channel", Name: channel}
}

// askChannels asks verb (await or broadcast) of every channel — the question
// its HTTP route asks — and of each channel named, so a deny on one channel
// holds for a fan-in or fan-out that includes it.
func (s *Server) askChannels(ctx context.Context, verb string, channels []string) error {
	if err := s.connectorAsk(ctx, authz.Surface{Action: verb, Type: "channel", Name: authz.AnyInstance}); err != nil {
		return err
	}
	for _, c := range channels {
		if c == "" {
			continue
		}
		if err := s.connectorAsk(ctx, authz.Surface{Action: verb, Type: "channel", Name: c}); err != nil {
			return err
		}
	}
	return nil
}

// refusalResult is an ACL refusal as a tool result, in the route's body.
func refusalResult(err error) (connector.ToolResult, bool) {
	ad, ok := err.(*errACLDenied)
	if !ok {
		return connector.ToolResult{}, false
	}
	return aclToolResult(aclReply{403, aclDeniedBody(ad.surface, ad.decision)}), true
}
