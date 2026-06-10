package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
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

	// PoolStatsFn, when non-nil, returns the current pgxpool
	// connection stats (total, acquired, idle). Captured by
	// readWithRetry just before the post-notify re-read so the
	// subscribe-empty race diagnostic log can correlate retry fires
	// with pool exhaustion or connection-reuse anomalies.
	//
	// Wired by main.go when the configured store is a Postgres store
	// (via type assertion on store.Store). SQLite + test builds leave
	// it nil; the diagnostic log path then reports zeros for the pool
	// fields and the retry behavior is unchanged.
	PoolStatsFn func() (total, acquired, idle int32)

	// truncWarned dedupes the F22 wait_ms-truncation advisory to once per
	// channel name (per process). A subscriber whose wait_ms exceeds
	// LongPollCapMS re-subscribes every cap interval, so logging on every
	// subscribe would spam. Keyed by channel name; zero-value ready (the
	// Channel tool is always used by pointer, so the sync.Map is never copied).
	truncWarned sync.Map
}

// shouldWarnTruncation reports whether a wait_ms truncation on `channel` should
// be logged now — true only the FIRST time per channel (per process), so the
// F22 advisory does not spam on every re-subscribe. Concurrency-safe.
func (c *Channel) shouldWarnTruncation(channel string) bool {
	_, dup := c.truncWarned.LoadOrStore(channel, struct{}{})
	return !dup
}

const channelDescription = `Persistent inter-agent message bus. ` +
	`Publish JSON payloads to a named channel; subscribe to drain new messages with cursor-based at-least-once delivery. ` +
	`Operations: publish, subscribe, ack, peek, list_channels, await, broadcast. ` +
	`Channel ACLs are operator-configured; the tool refuses ops on channels not in this agent's publish/subscribe allowlists. ` +
	`Scope (agent / user / global) is set by the operator per channel; cursor isolation matches that scope. ` +
	`await is a fan-in barrier across MULTIPLE channels (wait for any / all / at_least N messages, or a timeout) — the complement to ` +
	`Agent.parallel_spawn (which joins sub-agents); use await to join independent producers (scheduler / webhook / separately-spawned agents). ` +
	`await is non-committing (detection only) — it never advances cursors, so subscribe/ack exactly what you process. ` +
	`broadcast is the symmetric fan-OUT: publish one payload to MULTIPLE channels in a single call (e.g. ping N workers to start). ` +
	`Both await and broadcast cap at 32 channels and refuse the whole op if any channel fails its ACL (no partial broadcast).`

const channelInputSchema = `{
  "type": "object",
  "properties": {
    "op":           {"type": "string", "enum": ["publish","subscribe","ack","peek","list_channels","await","broadcast"], "description": "Which operation to perform."},
    "channel":      {"type": "string", "description": "The channel name (required for publish/subscribe/ack/peek)."},
    "channels":     {"type": "array", "items": {"type": "string"}, "description": "await + broadcast (max 32 channels). await resolves each under the SUBSCRIBE allowlist (fan-in); broadcast under the PUBLISH allowlist (fan-out)."},
    "mode":         {"type": "string", "enum": ["any","all","at_least"], "description": "Await only: any = ≥1 channel has a message; all = every channel has ≥1; at_least = total messages across channels ≥ n. Default any."},
    "n":            {"type": "integer", "description": "Await only: the threshold for mode=at_least (required, >0 for that mode)."},
    "value":        {"description": "publish / broadcast: the JSON payload to append (broadcast sends the same payload to every named channel)."},
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
	Channels    []string        `json:"channels,omitempty"` // await: fan-in set
	Mode        string          `json:"mode,omitempty"`     // await: any|all|at_least
	N           int             `json:"n,omitempty"`        // await: threshold for at_least
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
	case "await":
		return c.execAwait(ctx, policy, in)
	case "broadcast":
		return c.execBroadcast(ctx, policy, in)
	case "":
		return errResult("missing required field: op"), nil
	default:
		return errResult(fmt.Sprintf("unknown op %q (must be one of: publish, subscribe, ack, peek, list_channels, await, broadcast)", in.Op)), nil
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
	def, scope, scopeID, refusal := c.checkPublishACL(ctx, policy, in.Channel)
	if refusal != "" {
		return errResult(refusal), nil
	}
	if verr := validatePublishValue(in.Value, c.MaxValueBytes); verr != "" {
		return errResult("publish: " + verr), nil
	}
	now := time.Now()
	visibleAt, deferred, derr := parsePublishDeliverAt(in.DeliverAt, now)
	if derr != "" {
		return errResult("publish: " + derr), nil
	}
	result, err := c.storeAndNotify(ctx, in.Channel, def, scope, scopeID, in.Value, in.TTL, visibleAt, deferred, now)
	if err != nil {
		return errResult(fmt.Sprintf("publish: %s", err)), nil
	}
	return okJSON(result)
}

// checkPublishACL resolves a channel for publishing + applies the v0.8.6
// system-channel refusals (publisher:system + the reserved _system/
// prefix). Returns a non-empty refusal string when publishing is denied.
// Side-effect-free — shared by publish + broadcast so both enforce the
// identical pre-write gate.
func (c *Channel) checkPublishACL(ctx context.Context, policy tools.ChannelPolicyValue, channel string) (tools.ChannelDef, store.MemoryScope, string, string) {
	def, scope, scopeID, err := c.resolveChannel(ctx, policy, "publish", channel)
	if err != nil {
		return def, scope, scopeID, err.Error()
	}
	// The admin endpoint and internal Go publisher bypass these by going
	// through SystemPublisher / Store.ChannelPublish directly, not this
	// tool layer.
	if def.Publisher == "system" {
		return def, scope, scopeID, fmt.Sprintf("channel %q is `publisher: system` — agents may not publish (use admin endpoint POST /v1/_channels/_system/%s/publish or wait for internal publisher)", channel, channel)
	}
	if strings.HasPrefix(channel, "_system/") {
		return def, scope, scopeID, fmt.Sprintf("channel %q starts with `_system/` (reserved prefix) — agents may not publish to system channels", channel)
	}
	return def, scope, scopeID, ""
}

// validatePublishValue checks the payload is present, valid JSON, and
// within the operator byte cap. Returns "" when OK. Shared by publish +
// broadcast (broadcast validates the single shared payload once).
func validatePublishValue(value json.RawMessage, maxBytes int) string {
	if len(value) == 0 {
		return "missing required field: value"
	}
	if !json.Valid(value) {
		return "value is not valid JSON"
	}
	if maxBytes > 0 && len(value) > maxBytes {
		return fmt.Sprintf("payload (%d bytes) exceeds max %d bytes", len(value), maxBytes)
	}
	return ""
}

// parsePublishDeliverAt interprets the optional deliver_at (v0.8.6). Empty
// or parseable-as-past = visible immediately (deferred=false). Future-dated
// = deferred publish at that instant. Returns a non-empty refusal on a
// malformed RFC3339 string.
func parsePublishDeliverAt(deliverAt string, now time.Time) (visibleAt time.Time, deferred bool, refusal string) {
	if deliverAt == "" {
		return time.Time{}, false, ""
	}
	parsed, err := time.Parse(time.RFC3339, deliverAt)
	if err != nil {
		return time.Time{}, false, fmt.Sprintf("invalid deliver_at %q: %s (expected RFC3339)", deliverAt, err)
	}
	if parsed.After(now) {
		return parsed, true, ""
	}
	return time.Time{}, false, ""
}

// storeAndNotify writes one message to an already-resolved channel, wakes
// subscribers (or arms the deferred-visibility timer), and emits the typed
// audit event — the side-effecting half of a publish, shared by publish +
// broadcast. Returns the per-channel result map (message_id / channel /
// dropped_oldest, + visible_at when deferred).
func (c *Channel) storeAndNotify(ctx context.Context, channel string, def tools.ChannelDef, scope store.MemoryScope, scopeID string, value json.RawMessage, ttl int64, visibleAt time.Time, deferred bool, now time.Time) (map[string]any, error) {
	// TTL precedence: per-message > channel default > none. TTL counts
	// from publish time, not deliver_at — a deferred message never
	// survives beyond its declared TTL.
	ttlSecs := ttl
	if ttlSecs == 0 {
		ttlSecs = int64(def.DefaultTTL)
	}
	var expiresAt time.Time
	if ttlSecs > 0 {
		expiresAt = now.Add(time.Duration(ttlSecs) * time.Second)
	}

	id, dropped, err := c.Store.ChannelPublish(ctx, store.ChannelMessage{
		Channel:           channel,
		Scope:             scope,
		ScopeID:           scopeID,
		Payload:           value,
		ExpiresAt:         expiresAt,
		VisibleAt:         visibleAt, // zero = treated as "now" inside the store
		PublishedByUserID: tools.RunIdentity(ctx).UserID,
	}, def.MaxMessages)
	if err != nil {
		return nil, err
	}
	// Deferred publishes go via the scheduler so long-poll subscribers
	// wake at visible_at. Immediate publishes notify the bus directly.
	if deferred && c.Scheduler != nil {
		c.Scheduler.Schedule(channel, id, visibleAt)
	} else if c.Bus != nil {
		c.Bus.Notify(channel)
	}
	// Typed audit event (v0.8.4 polish): a separate event type so SSE
	// consumers building channel dashboards can filter without parsing
	// every tool_result. PayloadPreview truncated at 200 chars.
	tools.EventEmitter(ctx)(providers.Event{
		Type: providers.EventChannelPublish,
		Channel: &providers.ChannelEventInfo{
			Channel:        channel,
			MessageID:      id,
			Scope:          string(scope),
			ScopeID:        scopeID,
			PayloadBytes:   len(value),
			PayloadPreview: truncateForEvent(string(value), 200),
			DroppedOldest:  dropped,
		},
	})
	result := map[string]any{
		"message_id":     id,
		"channel":        channel,
		"dropped_oldest": dropped, // > 0 indicates max_messages overflow
	}
	if deferred {
		result["visible_at"] = visibleAt.UTC().Format(time.RFC3339Nano)
	}
	return result, nil
}

// execBroadcast is the symmetric fan-OUT to await's fan-in (RFC S shape):
// publish ONE payload to a SET of channels in a single call — e.g. ping N
// workers to start, the producer-side bookend of an await consolidator. A
// dedicated op (not an overload of publish), mirroring how await is its own
// op rather than an overload of subscribe.
//
// Atomic ACL pre-flight: every channel is resolved + system-refusal-checked
// (under the PUBLISH allowlist) BEFORE any write, so one denied channel
// refuses the WHOLE op — no partial broadcast. The shared payload + the
// shared deliver_at are validated once. Writes are then applied per channel;
// a per-channel storage error (rare) is reported in that channel's result
// while the already-published ones stand (there is no cross-channel
// transaction), and the op itself does not error — symmetric with await's
// "timeout returns partials, never an error" posture.
func (c *Channel) execBroadcast(ctx context.Context, policy tools.ChannelPolicyValue, in channelInput) (tools.Result, error) {
	if len(in.Channels) == 0 {
		return errResult("broadcast: missing required field: channels (non-empty list)"), nil
	}
	if len(in.Channels) > maxFanChannels {
		return errResult(fmt.Sprintf("broadcast: too many channels (%d > max %d)", len(in.Channels), maxFanChannels)), nil
	}
	if verr := validatePublishValue(in.Value, c.MaxValueBytes); verr != "" {
		return errResult("broadcast: " + verr), nil
	}
	now := time.Now()
	visibleAt, deferred, derr := parsePublishDeliverAt(in.DeliverAt, now)
	if derr != "" {
		return errResult("broadcast: " + derr), nil
	}

	// Pre-flight: resolve + ACL-check every (deduped) channel BEFORE any
	// write. Any denial refuses the whole op so a broadcast is never partial
	// because of ACLs.
	type target struct {
		name    string
		def     tools.ChannelDef
		scope   store.MemoryScope
		scopeID string
	}
	seen := make(map[string]bool, len(in.Channels))
	targets := make([]target, 0, len(in.Channels))
	for _, name := range in.Channels {
		if seen[name] {
			continue
		}
		seen[name] = true
		def, scope, scopeID, refusal := c.checkPublishACL(ctx, policy, name)
		if refusal != "" {
			return errResult("broadcast: " + refusal), nil
		}
		targets = append(targets, target{name, def, scope, scopeID})
	}

	// Write to each resolved channel. Per-channel storage errors are
	// reported in results (the successful publishes stand).
	results := make([]map[string]any, 0, len(targets))
	published := 0
	for _, t := range targets {
		res, err := c.storeAndNotify(ctx, t.name, t.def, t.scope, t.scopeID, in.Value, in.TTL, visibleAt, deferred, now)
		if err != nil {
			results = append(results, map[string]any{"channel": t.name, "error": err.Error()})
			continue
		}
		published++
		results = append(results, res)
	}
	return okJSON(map[string]any{
		"published": published,
		"failed":    len(targets) - published,
		"results":   results,
	})
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

	// Long-poll pattern: when long-poll is enabled, register the
	// waker BEFORE the initial read so a publish that commits between
	// our check and our wait isn't lost. See Bus.Register doc for
	// the race-free invariant.
	//
	// Without this pre-registration, a concurrent publish in the
	// window between read() returning empty and Bus.Wait() registering
	// its waker fires Notify against an empty waiter slice — the
	// notification vanishes, the subscriber waits the full wait_ms,
	// and the just-published row is silently missed. At x10 circuit
	// scale this races at ~50% (cf. PR #231 analysis); at x1 it's
	// invisible because there's no concurrent publisher.
	longPollEnabled := c.Bus != nil && in.WaitMS > 0 && c.LongPollCapMS > 0
	var waker chan struct{}
	if longPollEnabled {
		waker = c.Bus.Register(in.Channel)
		defer c.Bus.Unregister(in.Channel, waker)
	}

	msgs, next, err := read()
	if err != nil {
		return errResult(fmt.Sprintf("subscribe: %s", err)), nil
	}

	// Long-poll if empty AND caller requested it AND operator allows.
	if len(msgs) == 0 && longPollEnabled {
		wait := in.WaitMS
		if wait > c.LongPollCapMS {
			// F22: the requested wait_ms is silently truncated to the operator
			// cap. Surface it once per channel so an operator notices the
			// long-poll budget being clipped — a subscriber that keeps
			// requesting longer waits re-subscribes every cap interval, burning
			// the agent's max_iterations.
			if c.shouldWarnTruncation(in.Channel) {
				log.Printf("channel %q: subscribe wait_ms=%d truncated to the operator cap %d ms (LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS) — repeated truncation re-subscribes every cap interval and burns the agent's max_iterations; raise the cap if longer waits are intended", in.Channel, in.WaitMS, c.LongPollCapMS)
			}
			wait = c.LongPollCapMS
		}
		t := time.NewTimer(time.Duration(wait) * time.Millisecond)
		defer t.Stop()
		select {
		case <-waker:
			// New publish landed. The publish's Bus.Notify is called
			// AFTER ChannelPublish commits, so by Postgres MVCC the
			// row IS visible to any new transaction. In theory one
			// re-read suffices.
			//
			// In practice, the v0.12.x circuit-stress x50 load test
			// surfaced a small residual rate (~2%) where the re-read
			// immediately after waker fires returns empty despite a
			// confirmed publish committed ~15-20ms earlier. The exact
			// mechanism is unclear — most likely pgxpool / connection-
			// level timing under high concurrency (50+ parallel
			// queries against the same channel_messages partition).
			//
			// Defensive fix: bounded retry. If the re-read sees the
			// row, return immediately (no extra cost). If empty,
			// brief backoff and retry — up to 3 attempts adds at
			// most 30ms in the worst case. The retry pattern also
			// covers any future visibility window we haven't yet
			// characterised at higher scale.
			//
			// Diagnostic capture: stamp the waker-receipt time, the
			// cursor used for the read, and the pool stats. Fed into
			// readWithRetry so the structured log when retry fires
			// carries the data needed to characterise the race (see
			// doc-internal/channel-race-investigation.md for the
			// hypothesis table).
			diag := retryDiagnostics{
				notifyAt:   time.Now(),
				fromCursor: from,
			}
			if c.PoolStatsFn != nil {
				diag.poolTotal, diag.poolAcquired, diag.poolIdle = c.PoolStatsFn()
			}
			msgs, next, err = readWithRetry(read, in.Channel, diag)
			if err != nil {
				return errResult(fmt.Sprintf("subscribe (after wait): %s", err)), nil
			}
		case <-t.C:
			// Timeout — caller gets empty messages and decides.
		case <-ctx.Done():
			// Cancelled — same as timeout from the wire shape.
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

// retryDiagnostics carries the per-call telemetry consumed by
// readWithRetry's structured log lines. Populated by execSubscribe at
// the case <-waker: arm so we can correlate retry fires with the
// publish that woke us:
//
//   - notifyAt: when Bus.Wait returned via the waker. retry_lag_ms in
//     the log = time.Since(notifyAt) at the attempt that found rows.
//     Tests Hypothesis 5 (notify before commit propagates).
//
//   - fromCursor: the cursor used for the initial read. Tests
//     Hypothesis 4 (cursor advanced past the new message) — if the
//     just-published msg_id is at or below this cursor, the empty
//     read is correct, not a race.
//
//   - poolTotal/poolAcquired/poolIdle: pgxpool.Stat() snapshot just
//     before the first re-read attempt. Tests Hypothesis 2 (different
//     pool connection sees an older snapshot) — if AcquiredConns ≈
//     TotalConns and retries are spiking, connection reuse is the
//     likely vector.
//
// Zero-valued fields are safe: nil PoolStatsFn leaves them at 0 and
// the diagnostic log reports them as such. The retry behavior itself
// is unchanged when no PoolStatsFn is wired.
type retryDiagnostics struct {
	notifyAt     time.Time
	fromCursor   string
	poolTotal    int32
	poolAcquired int32
	poolIdle     int32
}

// readWithRetry is the bounded retry wrapper used by execSubscribe
// after Bus.Wait returns via the waker (notify). The publish path
// is commit-then-notify, so MVCC says the row IS visible to any
// subsequent transaction; one read should suffice.
//
// Under heavy concurrency on Postgres (observed at x50 load test in
// 2026-05-26's circuit-stress), an immediate re-read after a confirmed
// publish has occasionally returned empty. The exact mechanism is
// unclear (suspected: pgxpool connection-level state or a Postgres
// snapshot edge case under contention). Until we can characterise it,
// this retry covers it pragmatically: max 3 attempts, 10ms backoff,
// so worst-case 30ms latency added when the retry fires.
//
// When the retry actually saves a circuit it logs a structured line
// carrying the retryDiagnostics fields so operators can correlate the
// signal with publish-side timing in `~/work/loomcycle-internal/
// doc-internal/channel-race-investigation.md`. When even the 3rd
// attempt returns empty, we accept the empty result and let the
// caller decide (current behaviour) — but we log a stronger warning
// with the same fields so the operator notices.
func readWithRetry(read func() ([]store.ChannelMessage, string, error), channel string, diag retryDiagnostics) ([]store.ChannelMessage, string, error) {
	const maxAttempts = 3
	const backoff = 10 * time.Millisecond

	// firstEmptyAt records when attempt 1 returned no rows. Used to
	// compute first_read_lag_us — the time from waker-receipt to the
	// race-affected empty read — which is the diagnostic field that
	// distinguishes H5 (notify before commit propagates) from H2
	// (different pool connection sees older snapshot). H5 produces
	// first_read_lag_us < 1000 (commit window still open at first
	// read); H2 produces first_read_lag_us > 100 (snapshot acquired
	// well after commit but pool reused a stale-snapshot connection).
	var firstEmptyAt time.Time
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		msgs, next, err := read()
		if err != nil {
			return nil, "", err
		}
		if len(msgs) > 0 {
			if attempt > 1 {
				// The retry saved this subscriber from a silent miss.
				// Surface at scale so we can measure the residual rate.
				// recovery_lag_ms = waker → eventually-non-empty read.
				// first_read_lag_us = waker → the empty first read.
				log.Printf("channel %q: subscribe-race-recovered attempt=%d msgs=%d recovery_lag_ms=%d first_read_lag_us=%d from_cursor=%q pool_total=%d pool_acquired=%d pool_idle=%d",
					channel, attempt, len(msgs),
					time.Since(diag.notifyAt).Milliseconds(),
					firstEmptyAt.Sub(diag.notifyAt).Microseconds(),
					diag.fromCursor,
					diag.poolTotal, diag.poolAcquired, diag.poolIdle)
			}
			return msgs, next, nil
		}
		if attempt == 1 {
			firstEmptyAt = time.Now()
		}
		if attempt < maxAttempts {
			time.Sleep(backoff)
		}
	}
	// All retries exhausted. The publish-then-notify ordering says the
	// row SHOULD have been visible by now. Either the notify was for
	// a publish we shouldn't have seen (cursor-past it), or there's
	// a more pathological timing window we haven't yet diagnosed.
	// Return empty and let the caller decide.
	log.Printf("channel %q: subscribe-race-exhausted attempts=%d recovery_lag_ms=%d first_read_lag_us=%d from_cursor=%q pool_total=%d pool_acquired=%d pool_idle=%d — silent miss; investigate",
		channel, maxAttempts,
		time.Since(diag.notifyAt).Milliseconds(),
		firstEmptyAt.Sub(diag.notifyAt).Microseconds(),
		diag.fromCursor,
		diag.poolTotal, diag.poolAcquired, diag.poolIdle)
	return nil, "", nil
}

// maxFanChannels caps the fan-in (await) / fan-out (broadcast) width so a
// single op can't register an unbounded number of Bus wakers / hold an
// unbounded select, or fan one publish out to an unbounded write set.
const maxFanChannels = 32

// execAwait is the RFC S / F35 fan-in barrier: wait until any / all /
// at_least-N messages are visible across a SET of channels, or a timeout.
// It is the complement to Agent.parallel_spawn (which joins sub-agents) —
// await joins INDEPENDENT producers (scheduler / webhook / separately-
// spawned agents) that parallel_spawn can't reach.
//
// A dedicated op — deliberately NOT an overload of subscribe. subscribe's
// single-channel committed-cursor + auto-commit-on-return is the most
// concurrency-sensitive path in the tool; forking it behind multi-channel
// conditionals would risk regressing it. await keeps subscribe byte-for-
// byte and gives fan-in its own clean contract.
//
// NON-COMMITTING (peek-style): await is detection, not drain. It returns
// each fired channel's next_cursor but NEVER advances the committed
// cursor — auto-committing across N channels on a partial / any / timeout
// result would silently consume messages on channels the agent didn't
// act on. The agent then subscribe/acks exactly what it processes.
//
// Timeout is NOT an error: returns {satisfied:false, timed_out:true} with
// whatever partials accumulated, mirroring subscribe's timeout-returns-empty.
func (c *Channel) execAwait(ctx context.Context, policy tools.ChannelPolicyValue, in channelInput) (tools.Result, error) {
	if len(in.Channels) == 0 {
		return errResult("await: missing required field: channels (non-empty list)"), nil
	}
	if len(in.Channels) > maxFanChannels {
		return errResult(fmt.Sprintf("await: too many channels (%d > max %d)", len(in.Channels), maxFanChannels)), nil
	}
	mode := in.Mode
	if mode == "" {
		mode = "any"
	}
	switch mode {
	case "any", "all", "at_least":
	default:
		return errResult(fmt.Sprintf("await: unknown mode %q (must be one of: any, all, at_least)", mode)), nil
	}
	if mode == "at_least" && in.N <= 0 {
		return errResult("await: mode=at_least requires n > 0"), nil
	}
	limit := in.MaxMessages
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	// Resolve + dedup every channel up front. Each goes through the
	// SUBSCRIBE ACL + scope resolution (resolveChannel side="subscribe"),
	// so await can't read a channel the agent can't subscribe to, and
	// RFC N tenancy is honoured per channel (resolveChannel reads the
	// run's tenant scope).
	type chanState struct {
		name    string
		scope   store.MemoryScope
		scopeID string
		from    string // committed/explicit cursor; NON-advancing
		msgs    []store.ChannelMessage
		next    string
	}
	seen := make(map[string]bool, len(in.Channels))
	states := make([]*chanState, 0, len(in.Channels))
	for _, name := range in.Channels {
		if seen[name] {
			continue
		}
		seen[name] = true
		_, scope, scopeID, err := c.resolveChannel(ctx, policy, "subscribe", name)
		if err != nil {
			return errResult(err.Error()), nil
		}
		from := in.FromCursor
		if from == "" {
			committed, err := c.Store.ChannelCommittedCursor(ctx, name, scope, scopeID)
			if err != nil {
				return errResult(fmt.Sprintf("await: read committed cursor for %q: %s", name, err)), nil
			}
			from = committed
		}
		states = append(states, &chanState{name: name, scope: scope, scopeID: scopeID, from: from})
	}

	readChan := func(st *chanState) error {
		msgs, next, err := c.Store.ChannelSubscribe(ctx, st.name, st.scope, st.scopeID, st.from, limit)
		if err != nil {
			return err
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
			return total >= in.N
		}
		return false
	}

	longPoll := c.Bus != nil && in.WaitMS > 0 && c.LongPollCapMS > 0
	wait := in.WaitMS
	if longPoll && wait > c.LongPollCapMS {
		// F22: surface the cap once per channel-set so an operator notices
		// the long-poll budget being clipped.
		if c.shouldWarnTruncation("await:" + strings.Join(in.Channels, ",")) {
			log.Printf("channel await: wait_ms=%d truncated to the operator cap %d ms (LOOMCYCLE_CHANNELS_LONGPOLL_CAP_MS)", in.WaitMS, c.LongPollCapMS)
		}
		wait = c.LongPollCapMS
	}

	// Register one waker per channel BEFORE the initial read — the same
	// race-free invariant subscribe uses (Bus.Register doc): a publish
	// that commits between our read and our wait can't be lost. Wakers
	// are one-shot (Notify drains the slice), so the woken channel's
	// waker is re-registered after each wake. Final wakers are cleaned by
	// the defer (Unregister is idempotent).
	wakers := make([]chan struct{}, len(states))
	if longPoll {
		for i, st := range states {
			wakers[i] = c.Bus.Register(st.name)
		}
		defer func() {
			for i, w := range wakers {
				if w != nil {
					c.Bus.Unregister(states[i].name, w)
				}
			}
		}()
	}

	// Initial synchronous multi-read. Empty is the EXPECTED state here
	// (no producer has fired yet) — plain read, no readWithRetry (its
	// 3×10ms backoff is for the post-notify MVCC-visibility window, not
	// a legitimately-empty channel).
	for _, st := range states {
		if err := readChan(st); err != nil {
			return errResult(fmt.Sprintf("await: read %q: %s", st.name, err)), nil
		}
	}

	timedOut := false
	if !satisfied() && longPoll {
		timer := time.NewTimer(time.Duration(wait) * time.Millisecond)
		defer timer.Stop()
	loop:
		for {
			// Dynamic-N select over the wakers + timer + ctx. reflect.Select
			// is the idiomatic stdlib dynamic select — no merge goroutines,
			// so nothing can leak.
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
			// A channel waker fired (one-shot — Notify drained it). Re-arm a
			// FRESH waker BEFORE re-reading, preserving subscribe's race-free
			// register-before-read invariant (Bus.Register doc): a publish
			// that lands in the window is then guaranteed to either be caught
			// by the read below (it committed first) or fire the fresh waker
			// (caught next iteration). Re-arming AFTER the read would drop a
			// Notify that fires in the gap between the read and the Register,
			// leaving the loop blocked on a waker that already missed its
			// signal until the timer expires.
			st := states[chosen]
			c.Bus.Unregister(st.name, wakers[chosen])
			wakers[chosen] = c.Bus.Register(st.name)
			// Re-read that channel with the same bounded MVCC-visibility retry
			// subscribe uses (the row committed before Notify, so it IS
			// visible — the retry covers the rare pgxpool snapshot window).
			// `from` is non-advancing, so this returns the full window.
			diag := retryDiagnostics{notifyAt: time.Now(), fromCursor: st.from}
			if c.PoolStatsFn != nil {
				diag.poolTotal, diag.poolAcquired, diag.poolIdle = c.PoolStatsFn()
			}
			msgs, next, err := readWithRetry(func() ([]store.ChannelMessage, string, error) {
				return c.Store.ChannelSubscribe(ctx, st.name, st.scope, st.scopeID, st.from, limit)
			}, st.name, diag)
			if err != nil {
				return errResult(fmt.Sprintf("await: read %q (after wake): %s", st.name, err)), nil
			}
			st.msgs = msgs
			st.next = next
			if satisfied() {
				break loop
			}
		}
	} else if !satisfied() {
		// No long-poll budget (Bus nil / wait<=0): the single synchronous
		// multi-read above is the whole answer; unmet ⇒ timed out.
		timedOut = true
	}

	sat := satisfied()
	fired := make([]string, 0, len(states))
	results := make(map[string]any, len(states))
	total := 0
	for _, st := range states {
		out := make([]map[string]any, 0, len(st.msgs))
		for _, m := range st.msgs {
			out = append(out, map[string]any{
				"id":           m.ID,
				"value":        m.Payload,
				"published_at": m.PublishedAt.UTC().Format(time.RFC3339Nano),
			})
		}
		if len(st.msgs) > 0 {
			fired = append(fired, st.name)
		}
		results[st.name] = map[string]any{
			"messages":    out,
			"next_cursor": st.next,
		}
		total += len(st.msgs)
	}

	return okJSON(map[string]any{
		"satisfied":      sat,
		"timed_out":      timedOut && !sat,
		"mode":           mode,
		"fired":          fired,
		"results":        results,
		"total_messages": total,
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
