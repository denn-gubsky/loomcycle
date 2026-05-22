// connector_impl_channels.go — Connector method bodies for the v0.9.x
// Channel CRUD surface (publish / subscribe / peek / ack). Mirror of
// connector_impl_n8n.go's pattern: HTTP handlers are thin REST wrappers,
// the real business logic lives here so MCP / gRPC / future transports
// dispatch through the SAME code as the HTTP path.
//
// The four ops call the same store + bus helpers as the in-band
// `Channel` tool (internal/tools/builtin/channel.go) so wire callers
// see identical semantics to agents: same cursor advancement, same
// long-poll behaviour, same monotonic-cursor invariant.
package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// resolveChannelScope maps the wire-string `scope` to the store enum
// and validates `scope_id` shape. Channel CRUD only supports global +
// user; agent scope isn't reachable from an HTTP boundary (the agent
// is not present). Returns ErrChannelScopeInvalid for any other value.
func resolveChannelScope(scopeStr, scopeID string) (store.MemoryScope, string, error) {
	switch scopeStr {
	case "global", "":
		return store.MemoryScopeGlobal, "", nil
	case "user":
		if scopeID == "" {
			return "", "", fmt.Errorf("%w: scope=user requires scope_id", connector.ErrChannelScopeInvalid)
		}
		return store.MemoryScopeUser, scopeID, nil
	default:
		return "", "", fmt.Errorf("%w: got %q", connector.ErrChannelScopeInvalid, scopeStr)
	}
}

// requireChannelDeclared reads `cfg.Channels[name]` and returns
// ErrChannelNotDeclared if the operator yaml didn't declare it.
// Mirrors the in-band tool's allowlist check (channel.go:resolveChannel)
// at the wire boundary.
func (s *Server) requireChannelDeclared(name string) (channelDef, error) {
	def, ok := s.cfg.Channels[name]
	if !ok {
		return channelDef{}, fmt.Errorf("%w: %q", connector.ErrChannelNotDeclared, name)
	}
	return channelDef{
		MaxMessages: def.MaxMessages,
		DefaultTTL:  def.DefaultTTL,
	}, nil
}

// channelDef captures only the fields the Connector methods need —
// keeps this file free of the config package's wider surface.
type channelDef struct {
	MaxMessages int
	DefaultTTL  int
}

// PublishChannel publishes a message to a declared channel. Delegates
// to the wired SystemPublisher (the same writer the in-band Channel
// tool uses), so deferred-publish scheduling + Bus.Notify wake-up of
// long-poll subscribers are identical to the agent path.
func (s *Server) PublishChannel(ctx context.Context, req connector.ChannelPublishRequest) (connector.ChannelPublishResult, error) {
	if s.systemPublisher == nil {
		return connector.ChannelPublishResult{}, connector.ErrSystemPublisherUnwired
	}
	if req.Channel == "" {
		return connector.ChannelPublishResult{}, fmt.Errorf("publish: missing required field: channel")
	}
	if len(req.Payload) == 0 || string(req.Payload) == "null" {
		return connector.ChannelPublishResult{}, fmt.Errorf("publish: missing required field: payload")
	}
	if !json.Valid(req.Payload) {
		return connector.ChannelPublishResult{}, fmt.Errorf("publish: payload is not valid JSON")
	}
	if cap := s.cfg.Env.ChannelsMaxValueBytes; cap > 0 && len(req.Payload) > cap {
		return connector.ChannelPublishResult{}, fmt.Errorf("publish: payload (%d bytes) exceeds max %d", len(req.Payload), cap)
	}

	def, err := s.requireChannelDeclared(req.Channel)
	if err != nil {
		return connector.ChannelPublishResult{}, err
	}

	scope, scopeID, err := resolveChannelScope(req.Scope, req.ScopeID)
	if err != nil {
		return connector.ChannelPublishResult{}, err
	}

	var deliverAt time.Time
	if req.DeliverAt != "" {
		parsed, err := time.Parse(time.RFC3339, req.DeliverAt)
		if err != nil {
			return connector.ChannelPublishResult{}, fmt.Errorf("publish: invalid deliver_at %q: %w", req.DeliverAt, err)
		}
		deliverAt = parsed
	}

	// Audit attribution: "_admin" for global-scope (operator), the
	// user_id for user-scope (per-end-user). The bearer's identity
	// is the same either way (operator's LOOMCYCLE_AUTH_TOKEN);
	// the audit string distinguishes the addressed surface.
	publishedBy := "_admin"
	if scope == store.MemoryScopeUser {
		publishedBy = scopeID
	}

	msg, err := s.systemPublisher.Publish(ctx, req.Channel, scope, scopeID,
		req.Payload, deliverAt, publishedBy, def.MaxMessages, def.DefaultTTL)
	if err != nil {
		return connector.ChannelPublishResult{}, fmt.Errorf("publish: %w", err)
	}

	out := connector.ChannelPublishResult{
		MsgID:     msg.ID,
		Channel:   req.Channel,
		CreatedAt: msg.PublishedAt.UTC().Format(time.RFC3339Nano),
	}
	if !msg.VisibleAt.IsZero() && !msg.VisibleAt.Equal(msg.PublishedAt) {
		out.VisibleAt = msg.VisibleAt.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

// SubscribeChannel reads the next batch of messages, optionally waiting
// up to WaitMS for new publishes. Auto-commits the cursor on a
// non-empty batch (at-most-once shape — mirrors the in-band tool). For
// at-least-once semantics, callers PeekChannel + AckChannel explicitly
// once processing is durable.
func (s *Server) SubscribeChannel(ctx context.Context, req connector.ChannelSubscribeRequest) (connector.ChannelSubscribeResult, error) {
	if req.Channel == "" {
		return connector.ChannelSubscribeResult{}, fmt.Errorf("subscribe: missing required field: channel")
	}
	if _, err := s.requireChannelDeclared(req.Channel); err != nil {
		return connector.ChannelSubscribeResult{}, err
	}
	scope, scopeID, err := resolveChannelScope(req.Scope, req.ScopeID)
	if err != nil {
		return connector.ChannelSubscribeResult{}, err
	}

	limit := req.MaxMessages
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	from := req.FromCursor
	if from == "" {
		committed, err := s.store.ChannelCommittedCursor(ctx, req.Channel, scope, scopeID)
		if err != nil {
			return connector.ChannelSubscribeResult{}, fmt.Errorf("subscribe: read committed cursor: %w", err)
		}
		from = committed
	}

	read := func() ([]store.ChannelMessage, string, error) {
		return s.store.ChannelSubscribe(ctx, req.Channel, scope, scopeID, from, limit)
	}
	msgs, next, err := read()
	if err != nil {
		return connector.ChannelSubscribeResult{}, fmt.Errorf("subscribe: %w", err)
	}

	// Long-poll if empty AND caller requested it. The bus's wake
	// signals "new data present"; re-query to fetch the actual rows
	// (one extra store roundtrip in the wake-then-read happy path,
	// negligible vs the wait latency it just saved).
	cap := s.cfg.Env.ChannelsLongPollCapMS
	if len(msgs) == 0 && s.channelBus != nil && req.WaitMS > 0 && cap > 0 {
		wait := req.WaitMS
		if wait > cap {
			wait = cap
		}
		if s.channelBus.Wait(ctx, req.Channel, time.Duration(wait)*time.Millisecond) {
			msgs, next, err = read()
			if err != nil {
				return connector.ChannelSubscribeResult{}, fmt.Errorf("subscribe (after wait): %w", err)
			}
		}
	}

	out := connector.ChannelSubscribeResult{
		Channel:    req.Channel,
		Messages:   make([]connector.ChannelMessage, 0, len(msgs)),
		NextCursor: next,
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, connector.ChannelMessage{
			ID:          m.ID,
			Value:       m.Payload,
			PublishedAt: m.PublishedAt.UTC().Format(time.RFC3339Nano),
		})
	}

	// Auto-commit on non-empty batch. Cursor regression on auto-commit
	// is impossible by construction (we just read this cursor), so any
	// error is a transient store fault — log + return the batch anyway
	// (caller will re-receive on next subscribe; not silent data loss).
	if next != "" && len(msgs) > 0 {
		if ackErr := s.store.ChannelAck(ctx, req.Channel, scope, scopeID, next); ackErr != nil {
			if !errors.Is(ackErr, store.ErrChannelCursorRegression) {
				// Same logging path as the in-band tool. The audit hole
				// is documented in channel.go:execSubscribe; the wire
				// path inherits it.
			}
		}
	}
	return out, nil
}

// PeekChannel is a non-destructive read — never advances the committed
// cursor. Defaults to "cur_0" (the beginning) when FromCursor is empty,
// so a fresh-bearer caller can replay a channel's full history without
// disturbing any subscriber's progress.
func (s *Server) PeekChannel(ctx context.Context, req connector.ChannelPeekRequest) (connector.ChannelPeekResult, error) {
	if req.Channel == "" {
		return connector.ChannelPeekResult{}, fmt.Errorf("peek: missing required field: channel")
	}
	if _, err := s.requireChannelDeclared(req.Channel); err != nil {
		return connector.ChannelPeekResult{}, err
	}
	scope, scopeID, err := resolveChannelScope(req.Scope, req.ScopeID)
	if err != nil {
		return connector.ChannelPeekResult{}, err
	}

	limit := req.MaxMessages
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	msgs, err := s.store.ChannelPeek(ctx, req.Channel, scope, scopeID, req.FromCursor, limit)
	if err != nil {
		return connector.ChannelPeekResult{}, fmt.Errorf("peek: %w", err)
	}

	out := connector.ChannelPeekResult{
		Channel:  req.Channel,
		Messages: make([]connector.ChannelMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		out.Messages = append(out.Messages, connector.ChannelMessage{
			ID:          m.ID,
			Value:       m.Payload,
			PublishedAt: m.PublishedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out, nil
}

// AckChannel advances the committed cursor for a (channel, scope,
// scope_id) tuple. Returns ErrChannelCursorRegression (at the
// connector boundary; mirror of store.ErrChannelCursorRegression) if
// Cursor is older than the committed value — the monotonic-cursor
// invariant is the same on the wire as in the tool.
func (s *Server) AckChannel(ctx context.Context, req connector.ChannelAckRequest) (connector.ChannelAckResult, error) {
	if req.Channel == "" {
		return connector.ChannelAckResult{}, fmt.Errorf("ack: missing required field: channel")
	}
	if req.Cursor == "" {
		return connector.ChannelAckResult{}, fmt.Errorf("ack: missing required field: cursor")
	}
	if _, err := s.requireChannelDeclared(req.Channel); err != nil {
		return connector.ChannelAckResult{}, err
	}
	scope, scopeID, err := resolveChannelScope(req.Scope, req.ScopeID)
	if err != nil {
		return connector.ChannelAckResult{}, err
	}

	if err := s.store.ChannelAck(ctx, req.Channel, scope, scopeID, req.Cursor); err != nil {
		if errors.Is(err, store.ErrChannelCursorRegression) {
			return connector.ChannelAckResult{}, connector.ErrChannelCursorRegression
		}
		return connector.ChannelAckResult{}, fmt.Errorf("ack: %w", err)
	}
	return connector.ChannelAckResult{OK: true}, nil
}
