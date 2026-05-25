// Package coord is loomcycle's v0.12.0 multi-replica coordination
// substrate.
//
// One backplane interface, one impl in v1.0:
//
//   - Backplane is a publish/subscribe abstraction over an inter-
//     replica signal channel. Phase 1 wires it up; Phases 2-7 build
//     cross-replica cancel / pause / bus-fanout / quota signals on
//     top of it.
//
//   - PostgresBackplane (postgres_backplane.go) implements Backplane
//     via Postgres LISTEN/NOTIFY. Zero new infra dep over what
//     loomcycle already needs for cluster mode (Postgres is required;
//     SQLite refuses to start when LOOMCYCLE_REPLICA_ID is set).
//
// The package also owns the ReplicaStore — read/write access to the
// `replicas` heartbeat table that backs the /healthz cluster view.
// Kept in this package (not the store package) because it's tied to
// the multi-replica posture: when LOOMCYCLE_REPLICA_ID is unset, no
// code in this package runs.
//
// Topic namespace: every backplane topic is prefixed `loomcycle.` so
// loomcycle's NOTIFY traffic doesn't collide with any other LISTEN
// consumer sharing the same Postgres instance. Currently used:
//
//	loomcycle.cancel   — Phase 3
//	loomcycle.pause    — Phase 4
//	loomcycle.runstate — Phase 4 (RunState bus fanout)
//	loomcycle.channel  — Phase 4 (Channel bus fanout)
//	loomcycle.quota    — Phase 2 (observability only)
//	loomcycle.hook     — Phase 6 (hook registry invalidation)
//
// Phase 1 ships the substrate with no live publishers / subscribers
// in the wider runtime — only the package's own tests exercise the
// wire roundtrip. Later phases add the producers/consumers.
package coord

import (
	"context"
	"errors"
)

// Event is one cross-replica notification.
type Event struct {
	// Topic is the loomcycle. … namespace string the publisher emitted on.
	Topic string

	// Payload is the publisher-supplied bytes. Coord does not interpret
	// the payload; consumers parse it (usually JSON).
	Payload []byte

	// PublisherReplicaID identifies the originating replica. Subscribers
	// receive events from all replicas including themselves; the
	// PostgresBackplane filters self-messages before delivering on the
	// subscriber channel, but PublisherReplicaID stays populated for
	// audit/log purposes.
	PublisherReplicaID string
}

// Backplane is the cross-replica pub/sub abstraction.
//
// Implementations are expected to be safe for concurrent use by
// multiple goroutines. Subscribe may dial a fresh connection per
// call (PostgresBackplane does this); callers should treat Subscribe
// as moderately expensive and Subscribe once per long-lived subscriber.
type Backplane interface {
	// Publish broadcasts payload on topic to every subscribed replica
	// (including this one — implementations filter self-messages).
	// Returns ErrPayloadTooLarge if the payload exceeds the impl's cap
	// (Postgres NOTIFY caps at 8000 bytes; PostgresBackplane enforces
	// 7800 for envelope headroom).
	Publish(ctx context.Context, topic string, payload []byte) error

	// Subscribe returns a receive-only channel that delivers Events
	// for topic. The channel closes when ctx is cancelled or the
	// Backplane is Closed. Self-messages (payloads published by this
	// replica) are filtered out before delivery.
	//
	// Buffer policy: subscriber channels have a fixed buffer; on
	// overflow the implementation drops events silently. Every Phase
	// 2+ consumer of these signals must be idempotent against missed
	// events (cancel re-checks DB state on miss; sweepers re-run next
	// tick) — the backplane is "best-effort hint, source-of-truth in DB."
	Subscribe(ctx context.Context, topic string) (<-chan Event, error)

	// Close shuts the backplane down. Outstanding Subscribe channels
	// close as their goroutines drain. Idempotent.
	Close() error
}

// Sentinel errors. Consumers branch on these via errors.Is.
var (
	// ErrPayloadTooLarge — caller's payload exceeds the impl's wire
	// cap. PostgresBackplane: 7800 bytes (200-byte margin under the
	// Postgres 8000-byte NOTIFY hard cap for the envelope header).
	ErrPayloadTooLarge = errors.New("coord: payload exceeds backplane wire cap")

	// ErrBackplaneClosed — Publish / Subscribe called after Close.
	ErrBackplaneClosed = errors.New("coord: backplane is closed")

	// ErrInvalidTopic — topic name failed validation (empty, contains
	// shell-unsafe chars, doesn't start with the loomcycle. prefix).
	ErrInvalidTopic = errors.New("coord: invalid topic name")
)

// TopicPrefix is the namespace every loomcycle backplane topic carries.
// Implementations enforce this on Publish + Subscribe so non-loomcycle
// LISTEN/NOTIFY consumers on the same database don't collide.
const TopicPrefix = "loomcycle."

// MaxPayloadBytes is the largest payload PostgresBackplane will accept.
// 200-byte margin under the Postgres 8000-byte NOTIFY hard cap for the
// JSON envelope header (replica_id field, base64 padding overhead).
const MaxPayloadBytes = 7800
