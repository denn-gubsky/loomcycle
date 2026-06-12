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
	"reflect"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/connector"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// channelFanCap bounds the fan-in (await) / fan-out (broadcast) width on
// the wire surface — same cap as the in-band Channel tool's maxFanChannels.
const channelFanCap = 32

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

// requireChannelDeclared resolves `name` against the yaml `cfg.Channels`
// block first, then the runtime channel store. F29: a channel declared at
// runtime (POST /v1/_channels) must be publishable/subscribable like a yaml
// channel — without the store fallback the admin publish/subscribe routes
// 404'd `channel_not_declared` for a perfectly valid runtime channel.
// Returns ErrChannelNotDeclared only when NEITHER source has it. Mirrors the
// in-band tool's allowlist check (channel.go:resolveChannel) and the per-run
// channelPolicyForAgent merge, at the wire boundary.
func (s *Server) requireChannelDeclared(ctx context.Context, name string) (channelDef, error) {
	if def, ok := s.cfg.Channels[name]; ok {
		return channelDef{
			MaxMessages: def.MaxMessages,
			DefaultTTL:  def.DefaultTTL,
		}, nil
	}
	if s.store != nil {
		// exp7 I5: point lookup (not an O(N) ChannelsList scan on every
		// publish/subscribe/peek/ack), and propagate a genuine store fault
		// instead of swallowing it — the old `if err == nil` masked any
		// transient error as a spurious channel_not_declared denial.
		row, err := s.store.ChannelGet(ctx, name)
		switch {
		case err == nil:
			return channelDef{
				MaxMessages: row.MaxMessages,
				DefaultTTL:  row.DefaultTTL,
			}, nil
		case isNotFound(err):
			// fall through to the not-declared signal below
		default:
			return channelDef{}, fmt.Errorf("channel lookup %q: %w", name, err)
		}
	}
	return channelDef{}, fmt.Errorf("%w: %q", connector.ErrChannelNotDeclared, name)
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

	def, err := s.requireChannelDeclared(ctx, req.Channel)
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
	if _, err := s.requireChannelDeclared(ctx, req.Channel); err != nil {
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
	if _, err := s.requireChannelDeclared(ctx, req.Channel); err != nil {
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
	if _, err := s.requireChannelDeclared(ctx, req.Channel); err != nil {
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

// BroadcastChannels publishes one payload to a SET of channels — the
// wire-surface twin of the in-band Channel.broadcast op. Atomic at the
// declare/scope pre-flight: every channel is checked before ANY write, so
// one undeclared/invalid channel refuses the whole op (no partial
// broadcast). Per-channel storage errors after that are reported in that
// channel's result while the successful publishes stand. Goes through the
// SAME systemPublisher as PublishChannel, so deferred-publish scheduling +
// Bus wake-up of waiting subscribers (or awaiters) are identical.
func (s *Server) BroadcastChannels(ctx context.Context, req connector.ChannelBroadcastRequest) (connector.ChannelBroadcastResult, error) {
	if s.systemPublisher == nil {
		return connector.ChannelBroadcastResult{}, connector.ErrSystemPublisherUnwired
	}
	if len(req.Channels) == 0 {
		return connector.ChannelBroadcastResult{}, fmt.Errorf("broadcast: missing required field: channels")
	}
	if len(req.Channels) > channelFanCap {
		return connector.ChannelBroadcastResult{}, fmt.Errorf("broadcast: too many channels (%d > max %d)", len(req.Channels), channelFanCap)
	}
	if len(req.Payload) == 0 || string(req.Payload) == "null" {
		return connector.ChannelBroadcastResult{}, fmt.Errorf("broadcast: missing required field: payload")
	}
	if !json.Valid(req.Payload) {
		return connector.ChannelBroadcastResult{}, fmt.Errorf("broadcast: payload is not valid JSON")
	}
	if cap := s.cfg.Env.ChannelsMaxValueBytes; cap > 0 && len(req.Payload) > cap {
		return connector.ChannelBroadcastResult{}, fmt.Errorf("broadcast: payload (%d bytes) exceeds max %d", len(req.Payload), cap)
	}
	scope, scopeID, err := resolveChannelScope(req.Scope, req.ScopeID)
	if err != nil {
		return connector.ChannelBroadcastResult{}, err
	}
	var deliverAt time.Time
	if req.DeliverAt != "" {
		parsed, perr := time.Parse(time.RFC3339, req.DeliverAt)
		if perr != nil {
			return connector.ChannelBroadcastResult{}, fmt.Errorf("broadcast: invalid deliver_at %q: %w", req.DeliverAt, perr)
		}
		deliverAt = parsed
	}

	// Pre-flight: declare-check every (deduped) channel BEFORE any write.
	type target struct {
		name string
		def  channelDef
	}
	seen := make(map[string]bool, len(req.Channels))
	targets := make([]target, 0, len(req.Channels))
	for _, name := range req.Channels {
		if seen[name] {
			continue
		}
		seen[name] = true
		def, derr := s.requireChannelDeclared(ctx, name)
		if derr != nil {
			return connector.ChannelBroadcastResult{}, derr
		}
		targets = append(targets, target{name, def})
	}

	publishedBy := "_admin"
	if scope == store.MemoryScopeUser {
		publishedBy = scopeID
	}
	out := connector.ChannelBroadcastResult{Results: make([]connector.ChannelBroadcastEntry, 0, len(targets))}
	for _, t := range targets {
		msg, perr := s.systemPublisher.Publish(ctx, t.name, scope, scopeID,
			req.Payload, deliverAt, publishedBy, t.def.MaxMessages, t.def.DefaultTTL)
		if perr != nil {
			out.Failed++
			out.Results = append(out.Results, connector.ChannelBroadcastEntry{Channel: t.name, Error: perr.Error()})
			continue
		}
		out.Published++
		entry := connector.ChannelBroadcastEntry{
			Channel:   t.name,
			MsgID:     msg.ID,
			CreatedAt: msg.PublishedAt.UTC().Format(time.RFC3339Nano),
		}
		if !msg.VisibleAt.IsZero() && !msg.VisibleAt.Equal(msg.PublishedAt) {
			entry.VisibleAt = msg.VisibleAt.UTC().Format(time.RFC3339Nano)
		}
		out.Results = append(out.Results, entry)
	}
	return out, nil
}

// AwaitChannels fans IN across a SET of channels — the wire-surface twin
// of the in-band Channel.await op. NON-committing (detection only): it
// reads via ChannelSubscribe but never advances the committed cursor, so
// the caller subscribe/acks exactly what it processes. Mirrors the in-band
// tool's race-free register-before-read + per-channel re-arm pattern over
// the SAME bus, so a publish/broadcast from any surface wakes it. A
// timeout is NOT an error — returns TimedOut:true with whatever partials
// accumulated.
func (s *Server) AwaitChannels(ctx context.Context, req connector.ChannelAwaitRequest) (connector.ChannelAwaitResult, error) {
	if len(req.Channels) == 0 {
		return connector.ChannelAwaitResult{}, fmt.Errorf("await: missing required field: channels")
	}
	if len(req.Channels) > channelFanCap {
		return connector.ChannelAwaitResult{}, fmt.Errorf("await: too many channels (%d > max %d)", len(req.Channels), channelFanCap)
	}
	mode := req.Mode
	if mode == "" {
		mode = "any"
	}
	switch mode {
	case "any", "all", "at_least":
	default:
		return connector.ChannelAwaitResult{}, fmt.Errorf("await: unknown mode %q (must be one of: any, all, at_least)", mode)
	}
	if mode == "at_least" && req.N <= 0 {
		return connector.ChannelAwaitResult{}, fmt.Errorf("await: mode=at_least requires n > 0")
	}
	scope, scopeID, err := resolveChannelScope(req.Scope, req.ScopeID)
	if err != nil {
		return connector.ChannelAwaitResult{}, err
	}
	limit := req.MaxMessages
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	type chanState struct {
		name string
		from string
		next string
		msgs []store.ChannelMessage
	}
	seen := make(map[string]bool, len(req.Channels))
	states := make([]*chanState, 0, len(req.Channels))
	for _, name := range req.Channels {
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, derr := s.requireChannelDeclared(ctx, name); derr != nil {
			return connector.ChannelAwaitResult{}, derr
		}
		from := req.FromCursor
		if from == "" {
			committed, cerr := s.store.ChannelCommittedCursor(ctx, name, scope, scopeID)
			if cerr != nil {
				return connector.ChannelAwaitResult{}, fmt.Errorf("await: read committed cursor for %q: %w", name, cerr)
			}
			from = committed
		}
		states = append(states, &chanState{name: name, from: from})
	}

	readChan := func(st *chanState) error {
		msgs, next, rerr := s.store.ChannelSubscribe(ctx, st.name, scope, scopeID, st.from, limit)
		if rerr != nil {
			return rerr
		}
		st.msgs = msgs
		st.next = next
		return nil
	}
	satisfied := func() bool {
		nonEmpty, total := 0, 0
		for _, st := range states {
			if len(st.msgs) > 0 {
				nonEmpty++
			}
			total += len(st.msgs)
		}
		switch mode {
		case "any":
			return nonEmpty >= 1
		case "all":
			return nonEmpty == len(states)
		case "at_least":
			return total >= req.N
		}
		return false
	}

	cap := s.cfg.Env.ChannelsLongPollCapMS
	longPoll := s.channelBus != nil && req.WaitMS > 0 && cap > 0
	wait := req.WaitMS
	if longPoll && wait > cap {
		wait = cap
	}

	// Register all wakers BEFORE the initial read (race-free invariant);
	// re-arm the woken channel after each wake (one-shot wakers).
	wakers := make([]chan struct{}, len(states))
	if longPoll {
		for i, st := range states {
			wakers[i] = s.channelBus.Register(st.name)
		}
		defer func() {
			for i, w := range wakers {
				if w != nil {
					s.channelBus.Unregister(states[i].name, w)
				}
			}
		}()
	}
	for _, st := range states {
		if rerr := readChan(st); rerr != nil {
			return connector.ChannelAwaitResult{}, fmt.Errorf("await: read %q: %w", st.name, rerr)
		}
	}

	timedOut := false
	if !satisfied() && longPoll {
		timer := time.NewTimer(time.Duration(wait) * time.Millisecond)
		defer timer.Stop()
	loop:
		for {
			cases := make([]reflect.SelectCase, 0, len(wakers)+2)
			for _, w := range wakers {
				cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(w)})
			}
			timerIdx := len(cases)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timer.C)})
			ctxIdx := len(cases)
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})

			chosen, _, _ := reflect.Select(cases)
			if chosen == timerIdx || chosen == ctxIdx {
				timedOut = true
				break loop
			}
			// Re-arm BEFORE re-reading (register-before-read), then re-read
			// the woken channel — `from` is non-advancing so it returns the
			// full window.
			st := states[chosen]
			s.channelBus.Unregister(st.name, wakers[chosen])
			wakers[chosen] = s.channelBus.Register(st.name)
			if rerr := readChan(st); rerr != nil {
				return connector.ChannelAwaitResult{}, fmt.Errorf("await: read %q (after wake): %w", st.name, rerr)
			}
			if satisfied() {
				break loop
			}
		}
	} else if !satisfied() {
		timedOut = true
	}

	sat := satisfied()
	out := connector.ChannelAwaitResult{
		Mode:    mode,
		Fired:   make([]string, 0, len(states)),
		Results: make(map[string]connector.ChannelAwaitEntry, len(states)),
	}
	for _, st := range states {
		msgs := make([]connector.ChannelMessage, 0, len(st.msgs))
		for _, m := range st.msgs {
			msgs = append(msgs, connector.ChannelMessage{
				ID:          m.ID,
				Value:       m.Payload,
				PublishedAt: m.PublishedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		if len(st.msgs) > 0 {
			out.Fired = append(out.Fired, st.name)
		}
		out.Results[st.name] = connector.ChannelAwaitEntry{Messages: msgs, NextCursor: st.next}
		out.TotalMessages += len(st.msgs)
	}
	out.Satisfied = sat
	out.TimedOut = timedOut && !sat
	return out, nil
}
