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
	// deliver time. publishedByUserID is the audit attribution
	// ("_system" for internal, bearer-user-id for admin endpoint).
	// Returns the persisted ChannelMessage row.
	Publish(ctx context.Context, channel string, scope store.MemoryScope, scopeID string,
		payload json.RawMessage, deliverAt time.Time, publishedByUserID string,
		maxMessages int, defaultTTLSeconds int,
	) (store.ChannelMessage, error)

	// PublishNow is the convenience for "publish immediately" — same
	// as Publish with a zero deliverAt.
	PublishNow(ctx context.Context, channel string, scope store.MemoryScope, scopeID string,
		payload json.RawMessage, publishedByUserID string,
		maxMessages int, defaultTTLSeconds int,
	) (store.ChannelMessage, error)
}

// StorePublisher is the concrete SystemPublisher implementation
// backed by a store.Store. Bus + Scheduler are wired so deferred
// publishes wake long-poll subscribers at visible_at, same as the
// agent-tool path.
type StorePublisher struct {
	Store     store.Store
	Bus       *Bus       // nil disables in-process notification
	Scheduler *Scheduler // nil disables deferred-publish wake-up scheduling
}

// SystemPublisherUserID is the audit-trail sentinel for internal Go
// publishes. Distinguishes loomcycle-authored publishes from agent
// and admin-endpoint ones; never matches a real user_id (underscore
// prefix is reserved namespace).
const SystemPublisherUserID = "_system"

// Publish implements SystemPublisher.
func (p *StorePublisher) Publish(ctx context.Context, channel string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, deliverAt time.Time, publishedByUserID string,
	maxMessages int, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	if p.Store == nil {
		return store.ChannelMessage{}, fmt.Errorf("system publisher: no Store configured")
	}

	now := time.Now()
	var expiresAt time.Time
	if defaultTTLSeconds > 0 {
		expiresAt = now.Add(time.Duration(defaultTTLSeconds) * time.Second)
	}

	var visibleAt time.Time
	deferred := false
	if !deliverAt.IsZero() && deliverAt.After(now) {
		visibleAt = deliverAt
		deferred = true
	}

	msg := store.ChannelMessage{
		Channel:           channel,
		Scope:             scope,
		ScopeID:           scopeID,
		Payload:           payload,
		ExpiresAt:         expiresAt,
		VisibleAt:         visibleAt,
		PublishedByUserID: publishedByUserID,
	}
	id, _, err := p.Store.ChannelPublish(ctx, msg, maxMessages)
	if err != nil {
		return store.ChannelMessage{}, fmt.Errorf("system publisher: %w", err)
	}
	msg.ID = id
	if visibleAt.IsZero() {
		msg.VisibleAt = msg.PublishedAt // approximation for caller
	}

	// Wake subscribers. Deferred publishes go through the scheduler
	// (wakes at visible_at); immediate publishes notify the bus
	// directly (same path as the agent tool's execPublish).
	if deferred && p.Scheduler != nil {
		p.Scheduler.Schedule(channel, id, visibleAt)
	} else if p.Bus != nil {
		p.Bus.Notify(channel)
	}

	// Re-read to surface server-stamped PublishedAt to the caller.
	// (The store has it; we approximated above. A round-trip would
	// be authoritative but adds latency for every publish — the
	// internal callers don't need byte-perfect PublishedAt for the
	// cadence + audit use cases.)
	if msg.PublishedAt.IsZero() {
		msg.PublishedAt = now
	}
	return msg, nil
}

// PublishNow implements SystemPublisher.
func (p *StorePublisher) PublishNow(ctx context.Context, channel string, scope store.MemoryScope, scopeID string,
	payload json.RawMessage, publishedByUserID string,
	maxMessages int, defaultTTLSeconds int,
) (store.ChannelMessage, error) {
	return p.Publish(ctx, channel, scope, scopeID, payload, time.Time{}, publishedByUserID, maxMessages, defaultTTLSeconds)
}
