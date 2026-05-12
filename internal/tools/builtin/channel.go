package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/denn-gubsky/loomcycle/internal/channels"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// Channel is the v0.8.4 built-in tool — persistent inter-agent
// message bus. Five operations dispatched off the `op` field:
//
//	publish        — append a message; ACL gated by yaml channels.publish
//	subscribe      — drain up to N new messages; auto-commits the previous batch's cursor
//	ack            — explicitly commit a cursor (crash-safety: commit BEFORE next read)
//	peek           — non-consuming read; never advances cursor
//	list_channels  — informational; reports the agent's publish/subscribe allowlists
//
// scope_id is RESOLVED SERVER-SIDE based on the agent's run context
// (same shape as Memory's scope resolution — model picks the SCOPE,
// loomcycle picks the SCOPE_ID):
//
//   - scope=agent → yaml agent name from tools.AgentName(ctx)
//   - scope=user  → user_id from tools.RunIdentity(ctx)
//   - scope=global→ fixed "" (one shared cursor for the channel)
//
// The scope comes from the per-channel operator declaration
// (cfg.Channels[name].Scope) — agents do NOT pick scope at publish/
// subscribe time. This is what makes ACLs enforceable: an agent that
// can subscribe to channel X always reads it under the same
// (scope, scope_id) tuple regardless of how it phrases the call.
type Channel struct {
	// Store is the persistence backend. Required.
	Store store.Store

	// Bus is the in-process notification bus. Required when
	// long-poll subscribe is desired; nil disables the long-poll
	// path (subscribe returns immediately with whatever's in
	// storage).
	Bus *channels.Bus

	// Scheduler arms timers for deferred publishes (v0.8.6). When
	// the model passes `deliver_at` in the future, the tool stores
	// the message immediately + asks the scheduler to fire
	// Bus.Notify(channel) at visible_at. Nil scheduler = deferred
	// publishes still land in storage but long-poll subscribers
	// wake only on their periodic budget (no in-process latency
	// optimisation).
	Scheduler *channels.Scheduler

	// MaxValueBytes caps a single publish's payload size. 0 = no
	// cap. Sourced from LOOMCYCLE_CHANNELS_MAX_VALUE_BYTES.
	MaxValueBytes int

	// LongPollCapMS caps the operator-allowed long-poll budget in
	// milliseconds (i.e., the largest wait_ms an agent may request).
	// 0 disables long-poll entirely (subscribe always returns
	// immediately). Sourced from LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS.
	LongPollCapMS int
}

const channelDescription = `Persistent inter-agent message bus. ` +
	`Publish JSON payloads to a named channel; subscribe to drain new messages with cursor-based at-least-once delivery. ` +
	`Operations: publish, subscribe, ack, peek, list_channels. ` +
	`Channel ACLs are operator-configured; the tool refuses ops on channels not in this agent's publish/subscribe allowlists. ` +
	`Scope (agent / user / global) is set by the operator per channel; cursor isolation matches that scope.`

const channelInputSchema = `{
  "type": "object",
  "properties": {
    "op":           {"type": "string", "enum": ["publish","subscribe","ack","peek","list_channels"], "description": "Which operation to perform."},
    "channel":      {"type": "string", "description": "The channel name (required for publish/subscribe/ack/peek)."},
    "value":        {"description": "Publish only: the JSON payload to append."},
    "ttl":          {"type": "integer", "description": "Publish only: per-message TTL in seconds. Absent = channel default."},
    "deliver_at":   {"type": "string", "description": "Publish only (optional): RFC3339 timestamp at which the message becomes deliverable. Absent or in the past = immediate. TTL counts from publish time, NOT deliver_at — size the TTL to cover both the deferral window AND the desired visibility window."},
    "from_cursor":  {"type": "string", "description": "Subscribe/peek only: read starting after this cursor. Absent = since last ack. \"cur_0\" = replay from oldest."},
    "max_messages": {"type": "integer", "description": "Subscribe/peek only: max messages to return (default 10, cap 100)."},
    "wait_ms":      {"type": "integer", "description": "Subscribe only: long-poll budget in ms. 0 = return immediately. Capped by operator config."},
    "cursor":       {"type": "string", "description": "Ack only: the cursor to commit (must be >= currently committed)."}
  },
  "required": ["op"],
  "additionalProperties": false
}`

type channelInput struct {
	Op          string          `json:"op"`
	Channel     string          `json:"channel,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	TTL         int64           `json:"ttl,omitempty"`
	DeliverAt   string          `json:"deliver_at,omitempty"`
	FromCursor  string          `json:"from_cursor,omitempty"`
	MaxMessages int             `json:"max_messages,omitempty"`
	WaitMS      int             `json:"wait_ms,omitempty"`
	Cursor      string          `json:"cursor,omitempty"`
}

// Name implements tools.Tool.
func (c *Channel) Name() string { return "Channel" }

// Description implements tools.Tool.
func (c *Channel) Description() string { return channelDescription }

// InputSchema implements tools.Tool.
func (c *Channel) InputSchema() json.RawMessage { return json.RawMessage(channelInputSchema) }

// Execute implements tools.Tool. Dispatches off `op`; ACL + scope
// resolution shared across publish/subscribe/peek/ack.
func (c *Channel) Execute(ctx context.Context, raw json.RawMessage) (tools.Result, error) {
	if c.Store == nil {
		return errResult("Channel tool: not configured (no Store backend)"), nil
	}
	var in channelInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errResult(fmt.Sprintf("invalid input JSON: %s", err)), nil
	}
	policy := tools.ChannelPolicy(ctx)

	switch in.Op {
	case "publish":
		return c.execPublish(ctx, policy, in)
	case "subscribe":
		return c.execSubscribe(ctx, policy, in)
	case "ack":
		return c.execAck(ctx, policy, in)
	case "peek":
		return c.execPeek(ctx, policy, in)
	case "list_channels":
		return c.execListChannels(policy)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: publish, subscribe, ack, peek, list_channels)", in.Op)), nil
	}
}

// resolveChannel returns the operator-declared channel def + the
// effective (scope, scope_id) tuple for THIS agent's run. side is
// "publish" or "subscribe" — used both to check the allowlist and
// to phrase refusal messages.
func (c *Channel) resolveChannel(ctx context.Context, policy tools.ChannelPolicyValue, side, name string) (tools.ChannelDef, store.MemoryScope, string, error) {
	if name == "" {
		return tools.ChannelDef{}, "", "", fmt.Errorf("missing required field: channel")
	}
	def, ok := policy.Channels[name]
	if !ok {
		return tools.ChannelDef{}, "", "", fmt.Errorf("Channel tool: channel %q is not declared in operator config (channels: block)", name)
	}
	var allowed []string
	switch side {
	case "publish":
		allowed = policy.Publish
	case "subscribe":
		allowed = policy.Subscribe
	}
	if !channelAllowed(name, allowed) {
		if len(allowed) == 0 {
			return tools.ChannelDef{}, "", "", fmt.Errorf("Channel tool: this agent has no %s allowlist — add `channels.%s: [%s]` to the agent yaml", side, side, name)
		}
		return tools.ChannelDef{}, "", "", fmt.Errorf("Channel tool: %s not allowed on channel %q (agent allowlist: %v)", side, name, allowed)
	}

	switch def.Scope {
	case "agent":
		agentName := tools.AgentName(ctx)
		if agentName == "" {
			return tools.ChannelDef{}, "", "", fmt.Errorf("Channel tool: channel %q has scope=agent but the run has no agent name", name)
		}
		return def, store.MemoryScopeAgent, agentName, nil
	case "user":
		ident := tools.RunIdentity(ctx)
		if ident.UserID == "" {
			return tools.ChannelDef{}, "", "", fmt.Errorf("Channel tool: channel %q has scope=user but the run has no user_id", name)
		}
		return def, store.MemoryScopeUser, ident.UserID, nil
	case "global":
		// Single shared cursor for the channel — empty scope_id.
		return def, store.MemoryScopeGlobal, "", nil
	default:
		return tools.ChannelDef{}, "", "", fmt.Errorf("Channel tool: channel %q has unknown scope %q (operator config bug)", name, def.Scope)
	}
}

// channelAllowed checks `name` against the allowlist, supporting a
// trailing "/*" prefix wildcard. `findings/*` matches `findings/alpha`
// and `findings/beta` but NOT `findings` itself.
//
// Defense-in-depth: rejects names containing path-traversal patterns
// (`..` or `//`) before the wildcard match. The closed-set check at
// resolveChannel's call site already filters undeclared names — this
// guard ensures that if a future refactor relaxes that check, the
// wildcard can't accidentally grant `findings/*` access to
// `findings/../secret` or `findings//bypass`.
func channelAllowed(name string, allowlist []string) bool {
	name = strings.TrimSpace(name)
	if strings.Contains(name, "..") || strings.Contains(name, "//") {
		return false
	}
	for _, pat := range allowlist {
		pat = strings.TrimSpace(pat)
		if pat == name {
			return true
		}
		if strings.HasSuffix(pat, "/*") {
			prefix := strings.TrimSuffix(pat, "*") // keeps the trailing "/"
			if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
				return true
			}
		}
	}
	return false
}

func (c *Channel) execPublish(ctx context.Context, policy tools.ChannelPolicyValue, in channelInput) (tools.Result, error) {
	def, scope, scopeID, err := c.resolveChannel(ctx, policy, "publish", in.Channel)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if len(in.Value) == 0 {
		return errResult("publish: missing required field: value"), nil
	}
	if !json.Valid(in.Value) {
		return errResult("publish: value is not valid JSON"), nil
	}
	if c.MaxValueBytes > 0 && len(in.Value) > c.MaxValueBytes {
		return errResult(fmt.Sprintf("publish: payload (%d bytes) exceeds max %d bytes", len(in.Value), c.MaxValueBytes)), nil
	}

	// TTL precedence: per-message > channel default > none.
	ttlSecs := in.TTL
	if ttlSecs == 0 {
		ttlSecs = int64(def.DefaultTTL)
	}
	now := time.Now()
	var expiresAt time.Time
	if ttlSecs > 0 {
		// TTL counts from publish time, not deliver_at — preserves
		// total-lifetime semantics (a deferred message never survives
		// beyond its declared TTL). Operator/agent's responsibility
		// to size correctly if they want a meaningful visibility
		// window after a long deferral.
		expiresAt = now.Add(time.Duration(ttlSecs) * time.Second)
	}

	// deliver_at parsing (v0.8.6). Empty / parseable-as-past = visible
	// immediately. Future-dated = deferred publish.
	var visibleAt time.Time
	deferred := false
	if in.DeliverAt != "" {
		parsed, err := time.Parse(time.RFC3339, in.DeliverAt)
		if err != nil {
			return errResult(fmt.Sprintf("publish: invalid deliver_at %q: %s (expected RFC3339)", in.DeliverAt, err)), nil
		}
		if parsed.After(now) {
			visibleAt = parsed
			deferred = true
		}
	}

	id, dropped, err := c.Store.ChannelPublish(ctx, store.ChannelMessage{
		Channel:           in.Channel,
		Scope:             scope,
		ScopeID:           scopeID,
		Payload:           in.Value,
		ExpiresAt:         expiresAt,
		VisibleAt:         visibleAt, // zero = treated as "now" inside the store
		PublishedByUserID: tools.RunIdentity(ctx).UserID,
	}, def.MaxMessages)
	if err != nil {
		if errors.Is(err, store.ErrChannelValueTooLarge) {
			return errResult(fmt.Sprintf("publish: %s", err)), nil
		}
		return errResult(fmt.Sprintf("publish: %s", err)), nil
	}
	// Deferred publishes go via the scheduler so long-poll subscribers
	// wake at visible_at. Immediate publishes notify the bus directly
	// (same as v0.8.4).
	if deferred && c.Scheduler != nil {
		c.Scheduler.Schedule(in.Channel, id, visibleAt)
	} else if c.Bus != nil {
		c.Bus.Notify(in.Channel)
	}
	// Typed audit event (v0.8.4 polish): same payload as the
	// tool_result envelope, but on a separate event type so SSE
	// consumers building channel dashboards can filter by Type
	// without parsing every tool_result JSON. PayloadPreview is
	// truncated at 200 chars — adapters that need the full
	// payload still read it from the tool_result envelope.
	tools.EventEmitter(ctx)(providers.Event{
		Type: providers.EventChannelPublish,
		Channel: &providers.ChannelEventInfo{
			Channel:        in.Channel,
			MessageID:      id,
			Scope:          string(scope),
			ScopeID:        scopeID,
			PayloadBytes:   len(in.Value),
			PayloadPreview: truncateForEvent(string(in.Value), 200),
			DroppedOldest:  dropped,
		},
	})
	result := map[string]any{
		"message_id":     id,
		"channel":        in.Channel,
		"dropped_oldest": dropped, // > 0 indicates max_messages overflow
	}
	if deferred {
		result["visible_at"] = visibleAt.UTC().Format(time.RFC3339Nano)
	}
	return okJSON(result)
}

func (c *Channel) execSubscribe(ctx context.Context, policy tools.ChannelPolicyValue, in channelInput) (tools.Result, error) {
	def, scope, scopeID, err := c.resolveChannel(ctx, policy, "subscribe", in.Channel)
	_ = def
	if err != nil {
		return errResult(err.Error()), nil
	}
	limit := in.MaxMessages
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	// from_cursor precedence: explicit > committed.
	from := in.FromCursor
	if from == "" {
		committed, err := c.Store.ChannelCommittedCursor(ctx, in.Channel, scope, scopeID)
		if err != nil {
			return errResult(fmt.Sprintf("subscribe: read committed cursor: %s", err)), nil
		}
		from = committed
	}

	read := func() ([]store.ChannelMessage, string, error) {
		return c.Store.ChannelSubscribe(ctx, in.Channel, scope, scopeID, from, limit)
	}
	msgs, next, err := read()
	if err != nil {
		return errResult(fmt.Sprintf("subscribe: %s", err)), nil
	}

	// Long-poll if empty AND caller requested it AND operator allows.
	if len(msgs) == 0 && c.Bus != nil && in.WaitMS > 0 && c.LongPollCapMS > 0 {
		wait := in.WaitMS
		if wait > c.LongPollCapMS {
			wait = c.LongPollCapMS
		}
		if c.Bus.Wait(ctx, in.Channel, time.Duration(wait)*time.Millisecond) {
			// New publish landed — re-query (the row is committed).
			msgs, next, err = read()
			if err != nil {
				return errResult(fmt.Sprintf("subscribe (after wait): %s", err)), nil
			}
		}
	}

	// Emit BEFORE the auto-commit ack. Audit-event ordering
	// invariant: "if the cursor committed, the events were
	// emitted." Without this ordering, a transient ack failure
	// could leave messages marked consumed in storage while the
	// audit event was already written — but a transient ack
	// SUCCESS combined with an SSE flush failure could leave
	// messages consumed-but-not-audited, which is the worse gap
	// (operator using events for compliance/billing sees silent
	// holes). Emit first; commit second.
	out := make([]map[string]any, 0, len(msgs))
	emit := tools.EventEmitter(ctx)
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id":           m.ID,
			"value":        m.Payload,
			"published_at": m.PublishedAt.UTC().Format(time.RFC3339Nano),
		})
		// One typed delivery event per message in the returned
		// batch, in delivery order. For replays via cur_0 these
		// fire each time — delivery events count consumption,
		// distinct from EventChannelPublish which counts
		// production.
		emit(providers.Event{
			Type: providers.EventChannelDelivery,
			Channel: &providers.ChannelEventInfo{
				Channel:        in.Channel,
				MessageID:      m.ID,
				Scope:          string(scope),
				ScopeID:        scopeID,
				PayloadBytes:   len(m.Payload),
				PayloadPreview: truncateForEvent(string(m.Payload), 200),
				Cursor:         m.ID,
			},
		})
	}

	// Commit-on-return: when the read returned messages, advance
	// the committed cursor to next (= last message in batch) BEFORE
	// returning. This is the simple at-most-once shape — agents
	// that just call subscribe in a loop march forward without
	// having to track cursors themselves.
	//
	// Agents that want at-least-once (crash safety between
	// "loomcycle returned the batch" and "agent finished
	// processing"): use `peek` to read without advancing, then
	// explicit `ack` once processing is durable. The two-step
	// pattern is documented in docs/TOOLS.md.
	if next != "" {
		if err := c.Store.ChannelAck(ctx, in.Channel, scope, scopeID, next); err != nil {
			// Cursor regression on auto-commit is impossible by
			// construction (we just read this cursor in the same
			// txn, so it's always >= the previous committed value).
			// Any error here is a transient storage problem
			// (Postgres connection drop, sqlite lock contention).
			// We still return the batch to the agent — but log so
			// operators can detect the silent double-delivery that
			// will happen on the next subscribe call: the agent
			// will re-receive this batch because the committed
			// cursor didn't advance.
			if !errors.Is(err, store.ErrChannelCursorRegression) {
				log.Printf("channel %q: subscribe auto-ack failed (next double-delivery expected): %v", in.Channel, err)
			}
		}
	}
	return okJSON(map[string]any{
		"channel":     in.Channel,
		"messages":    out,
		"next_cursor": next,
	})
}

func (c *Channel) execAck(ctx context.Context, policy tools.ChannelPolicyValue, in channelInput) (tools.Result, error) {
	_, scope, scopeID, err := c.resolveChannel(ctx, policy, "subscribe", in.Channel)
	if err != nil {
		return errResult(err.Error()), nil
	}
	if in.Cursor == "" {
		return errResult("ack: missing required field: cursor"), nil
	}
	if err := c.Store.ChannelAck(ctx, in.Channel, scope, scopeID, in.Cursor); err != nil {
		if errors.Is(err, store.ErrChannelCursorRegression) {
			return errResult(fmt.Sprintf("ack: %s", err)), nil
		}
		return errResult(fmt.Sprintf("ack: %s", err)), nil
	}
	return okJSON(map[string]any{"ok": true})
}

func (c *Channel) execPeek(ctx context.Context, policy tools.ChannelPolicyValue, in channelInput) (tools.Result, error) {
	_, scope, scopeID, err := c.resolveChannel(ctx, policy, "subscribe", in.Channel)
	if err != nil {
		return errResult(err.Error()), nil
	}
	limit := in.MaxMessages
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	from := in.FromCursor // peek defaults to cur_0 (replay) when caller omits.
	msgs, err := c.Store.ChannelPeek(ctx, in.Channel, scope, scopeID, from, limit)
	if err != nil {
		return errResult(fmt.Sprintf("peek: %s", err)), nil
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"id":           m.ID,
			"value":        m.Payload,
			"published_at": m.PublishedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return okJSON(map[string]any{
		"channel":  in.Channel,
		"messages": out,
	})
}

func (c *Channel) execListChannels(policy tools.ChannelPolicyValue) (tools.Result, error) {
	return okJSON(map[string]any{
		"publish":   policy.Publish,
		"subscribe": policy.Subscribe,
	})
}

// truncateForEvent caps payload-preview strings at the configured
// byte budget. Adds an ellipsis when truncated so consumers can
// tell the preview is partial. Empty input returns empty (no
// ellipsis on the zero case — keeps the wire shape tidy).
//
// UTF-8 safety: walks runes and stops at the last rune boundary
// that fits within `max` bytes. Naive `s[:max]` byte-slicing
// would split a multi-byte rune that straddles the cut, producing
// invalid UTF-8 — JSON consumers (TS adapter's TextDecoder,
// strict json.Unmarshal) would reject the resulting preview.
// Since ChannelEventInfo is wire-stable from v0.8.4+, byte-slicing
// is not an option.
func truncateForEvent(s string, max int) string {
	if max <= 0 || s == "" {
		return s
	}
	if len(s) <= max {
		return s
	}
	n := 0
	for _, r := range s {
		size := utf8.RuneLen(r)
		if n+size > max {
			break
		}
		n += size
	}
	return s[:n] + "…"
}
