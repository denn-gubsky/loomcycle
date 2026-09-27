package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// SystemPublisher is the loomcycle-authoritative publish path for
// `_system/*` channels (v0.8.6). Two callers wire through this
// interface:
//
//   - Internal Go publishers (cadence heartbeats, runtime-state
//     hooks, provider-event publishes) — passes the "_system"
//     sentinel as publishedByUserID.
//   - The admin endpoint POST /v1/_channels/_system/{name}/publish —
//     passes the bearer's resolved user id.
//
// The interface intentionally does NOT enforce the `_system/`
// prefix or the channel's Publisher == "system" constraint:
// callers are trusted (operator yaml + bearer-authed http path).
// The TOOL layer is what gates agent-side publishes; system
// publishers bypass that gate by design.
type SystemPublisher interface {
	// Publish writes a message to a channel with optional future
	// deliver time. tenantID is the caller-authoritative owning tenant
	// (RFC L authority model — derived from the principal / run, never
	// from the wire or model). publishedByUserID is the audit
	// attribution ("_system" for internal, bearer-user-id for admin
	// endpoint). Returns the persisted ChannelMessage row.
	Publish(ctx context.Context, channel, tenantID string, scope store.MemoryScope, scopeID string,
		payload json.RawMessage, deliverAt time.Time, publishedByUserID string,
		maxMessages int, defaultTTLSeconds int,
	) (store.ChannelMessage, error)

	// PublishNow is the convenience for "publish immediately" — same
	// as Publish with a zero deliverAt.
	PublishNow(ctx context.Context, channel, tenantID string, scope store.MemoryScope, scopeID string,
		payload json.RawMessage, publishedByUserID string,
		maxMessages int, defaultTTLSeconds int,
	) (store.ChannelMessage, error)
}

// WriteRequest is one channel message to write. The caller has already
// decided WHERE it goes (tenant, scope) and HOW LONG it lives; whether it is
// delivered, held or deferred is the writer's decision, from the channel's
// definition — a caller never passes a reserved instant.
type WriteRequest struct {
	Channel string
	// TenantID is the tenant the message is written in, caller-authoritative
	// (from the principal or the run, never the wire or the model). The
	// channel's definition is resolved in it.
	TenantID    string
	Scope       store.MemoryScope
	ScopeID     string
	Payload     json.RawMessage
	DeliverAt   time.Time // zero or past: visible now
	ExpiresAt   time.Time // zero: no expiry; the caller applies its TTL precedence
	PublishedBy string    // audit attribution
	MaxMessages int       // 0: the store default
}

// WriteResult is what a write did.
type WriteResult struct {
	Message store.ChannelMessage
	// Dropped is how many of the oldest messages max_messages trimmed.
	Dropped int
	// Held: stored, not delivered until released.
	Held bool
	// Deferred: stored, visible at Message.VisibleAt.
	Deferred bool
}

// WriteDef is what a write needs from the channel's definition.
type WriteDef struct {
	// Hold: store the message but deliver nothing until it is released.
	Hold bool
}

// DefResolver resolves a channel's definition in tenantID for a write. A
// channel declared nowhere resolves to the zero WriteDef (a document's change
// feed, an undeclared `_system/*` channel): it is written as-is. An error is a
// fault reading the definition, and the write is refused — a definition that
// cannot be read must not be written past.
type DefResolver func(ctx context.Context, tenantID, channel string) (WriteDef, error)

// Writer writes channel messages. It is the only path into the store's
// channel_messages table outside the store itself, snapshot restore and the
// channel-hook worker — see writer_census_test.go, which fails the build when
// a new caller writes past it.
type Writer interface {
	Write(ctx context.Context, req WriteRequest) (WriteResult, error)
}

// StorePublisher is the channel writer: the concrete Writer and
// SystemPublisher, backed by a store.Store. Bus + Scheduler are wired so
// deferred publishes wake long-poll subscribers at visible_at.
type StorePublisher struct {
	Store     store.Store
	Bus       *Bus       // nil disables in-process notification
	Scheduler *Scheduler // nil disables deferred-publish wake-up scheduling

	// Defs resolves the channel's definition on every write.
	//
	// The DECISION LIVES HERE, not at the call sites, and that is the point:
	// a hold is a promise that nothing reaches a subscriber, and a promise
	// enforced at five call sites is a promise the sixth one breaks. Every
	// writer — the Channel tool, the scheduler, webhooks, documents,
	// heartbeats, interrupts, the admin endpoints — writes through Write, so
	// the definition is honoured once for all of them.
	//
	// Injected because the channel definition plane (static yaml merged with
	// the runtime substrate, tenant-scoped) lives in the server. nil = no
	// channel is held.
	Defs DefResolver
}

// SystemPublisherUserID is the audit-trail sentinel for internal Go
// publishes. Distinguishes loomcycle-authored publishes from agent
// and admin-endpoint ones; never matches a real user_id (underscore
// prefix is reserved namespace).
const SystemPublisherUserID = "_system"

// Write implements Writer.
func (p *StorePublisher) Write(ctx context.Context, req WriteRequest) (WriteResult, error) {
	if p.Store == nil {
		return WriteResult{}, fmt.Errorf("channel writer: no Store configured")
	}
	// The reserved instants mark a message held; only the writer may set them.
	if store.IsChannelHeld(req.DeliverAt) {
		return WriteResult{}, fmt.Errorf("channel writer: deliver_at %s is a reserved instant", req.DeliverAt.UTC().Format(time.RFC3339))
	}
	var def WriteDef
	if p.Defs != nil {
		d, err := p.Defs(ctx, req.TenantID, req.Channel)
		if err != nil {
			return WriteResult{}, fmt.Errorf("channel writer: resolve %q: %w", req.Channel, err)
		}
		def = d
	}

	now := time.Now()
	var visibleAt time.Time
	deferred := false
	switch {
	case def.Hold:
		// A hold overrides any deliver_at: the message waits for a release,
		// not for a clock.
		visibleAt = store.ChannelHeldVisibleAt()
	case !req.DeliverAt.IsZero() && req.DeliverAt.After(now):
		visibleAt, deferred = req.DeliverAt, true
	}

	msg := store.ChannelMessage{
		Channel:           req.Channel,
		TenantID:          req.TenantID,
		Scope:             req.Scope,
		ScopeID:           req.ScopeID,
		Payload:           req.Payload,
		ExpiresAt:         req.ExpiresAt,
		VisibleAt:         visibleAt, // zero = "now" inside the store
		PublishedByUserID: req.PublishedBy,
	}
	id, dropped, err := p.Store.ChannelPublish(ctx, msg, req.MaxMessages)
	if err != nil {
		return WriteResult{}, fmt.Errorf("channel writer: %w", err)
	}
	msg.ID = id
	// The store stamps the authoritative PublishedAt; this approximation is
	// what callers report (the audit and cadence uses do not need it to the
	// nanosecond, and a re-read would cost every write a round-trip).
	msg.PublishedAt = now
	if visibleAt.IsZero() {
		msg.VisibleAt = now
	}

	// Wake subscribers: a deferred message through the scheduler at
	// visible_at, an immediate one on the bus now. A held message wakes
	// nobody — that is what holding means, and a timer for the reserved
	// instant would be a timer for the year 2200.
	switch {
	case def.Hold:
	case deferred && p.Scheduler != nil:
		p.Scheduler.Schedule(req.Channel, id, visibleAt)
	case p.Bus != nil:
		p.Bus.Notify(req.Channel)
	}
	return WriteResult{Message: msg, Dropped: dropped, Held: def.Hold, Deferred: deferred}, nil
}

// Publish implements SystemPublisher.
func (p *StorePublisher) Publish(ctx context.Context, channel, tenantID string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, deliverAt time.Time, publishedByUserID string,
	maxMessages int, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	var expiresAt time.Time
	if defaultTTLSeconds > 0 {
		expiresAt = time.Now().Add(time.Duration(defaultTTLSeconds) * time.Second)
	}
	res, err := p.Write(ctx, WriteRequest{
		Channel: channel, TenantID: tenantID, Scope: scope, ScopeID: scopeID,
		Payload: payload, DeliverAt: deliverAt, ExpiresAt: expiresAt,
		PublishedBy: publishedByUserID, MaxMessages: maxMessages,
	})
	if err != nil {
		return store.ChannelMessage{}, fmt.Errorf("system publisher: %w", err)
	}
	return res.Message, nil
}

// PublishNow implements SystemPublisher.
func (p *StorePublisher) PublishNow(ctx context.Context, channel, tenantID string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, publishedByUserID string,
	maxMessages int, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	return p.Publish(ctx, channel, tenantID, scope, scopeID, payload, time.Time{}, publishedByUserID, maxMessages, defaultTTLSeconds)
}
