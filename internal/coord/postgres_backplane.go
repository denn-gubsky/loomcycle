package coord

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresBackplane implements Backplane via Postgres LISTEN/NOTIFY.
//
// Architecture:
//
//   - Publish acquires a connection from the *pgxpool.Pool and issues
//     `SELECT pg_notify($1, $2)`. The envelope is `{"r":"<replica>","p":"<b64>"}`.
//
//   - Subscribe dials a fresh `pgx.Connect()` per call — NOT from the
//     pool. A LISTEN connection has to stay blocked in WaitForNotification
//     to receive events; holding a pooled connection out of the pool
//     indefinitely would starve everyone else. The dedicated connection
//     is cheap (~5 MB) and we expect a small number of topics in v1.0
//     (one per Phase 2-6 coordination concern).
//
//   - Self-message filter: Postgres delivers your own NOTIFYs back to
//     you on the same connection. The receive goroutine drops events
//     whose envelope.r matches the publisher replica_id.
//
//   - Reconnect: on connection drop, the subscribe goroutine reconnects
//     with exponential backoff (500ms → 30s, ±20% jitter) and re-issues
//     LISTEN. Events arriving during the window are lost; consumers
//     must be idempotent against this.
//
//   - Payload cap: enforced at Publish (returns ErrPayloadTooLarge).
//     Receive-side does NOT validate — a malformed/oversize NOTIFY
//     payload from a buggy peer is logged and dropped.
//
//   - Topic validation: every Publish/Subscribe checks the topic
//     starts with TopicPrefix ("loomcycle.") and contains no shell-
//     unsafe characters. pgx parameter-substitutes pg_notify args so
//     SQL injection isn't the concern here; the concern is collision
//     with other LISTEN/NOTIFY consumers on the same database.
type PostgresBackplane struct {
	pool      *pgxpool.Pool
	dsn       string
	replicaID string

	mu     sync.Mutex
	closed bool
	// subs tracks each Subscribe call's cancel func by an opaque ID.
	// On goroutine exit the entry is removed; on Close every entry's
	// cancel is fired. Replaced the v0.12.0 review-1 chain pattern,
	// which leaked closure pointers as subscriptions were created and
	// disposed of (review finding #1).
	subs   map[uint64]context.CancelFunc
	nextID uint64
	wg     sync.WaitGroup
}

// PostgresBackplaneConfig is the constructor input.
type PostgresBackplaneConfig struct {
	// Pool is the shared pgxpool used for Publish. Required.
	Pool *pgxpool.Pool

	// DSN is the Postgres connection string used for Subscribe's
	// dedicated connections. Required — pgxpool does not expose the
	// original DSN, so the caller passes it in alongside the pool.
	DSN string

	// ReplicaID is this replica's identity. Used in the envelope
	// header and the self-message filter. Required.
	ReplicaID string
}

// NewPostgresBackplane validates the config and returns a ready-to-use
// backplane. Does not dial — connections are opened lazily by
// Publish / Subscribe.
func NewPostgresBackplane(cfg PostgresBackplaneConfig) (*PostgresBackplane, error) {
	if cfg.Pool == nil {
		return nil, errors.New("coord: pgxpool is required")
	}
	if cfg.DSN == "" {
		return nil, errors.New("coord: DSN is required for Subscribe")
	}
	if err := ValidateReplicaID(cfg.ReplicaID); err != nil {
		return nil, err
	}
	return &PostgresBackplane{
		pool:      cfg.Pool,
		dsn:       cfg.DSN,
		replicaID: cfg.ReplicaID,
		subs:      make(map[uint64]context.CancelFunc),
	}, nil
}

// envelope is the wire shape carried inside the NOTIFY payload.
// Short field names ("r", "p") to maximise the usable payload space
// under the 8000-byte Postgres cap.
type envelope struct {
	R string `json:"r"` // publisher replica_id
	P string `json:"p"` // base64-encoded payload
}

// validateTopic returns nil if t is a valid loomcycle backplane topic.
// Must start with TopicPrefix; the suffix is [A-Za-z0-9_.-]+ to stay
// within shell-safe and LISTEN-quote-safe identifiers.
func validateTopic(t string) error {
	if !strings.HasPrefix(t, TopicPrefix) {
		return fmt.Errorf("%w: topic %q must start with %q", ErrInvalidTopic, t, TopicPrefix)
	}
	suffix := t[len(TopicPrefix):]
	if suffix == "" {
		return fmt.Errorf("%w: topic %q has empty suffix", ErrInvalidTopic, t)
	}
	for _, r := range suffix {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return fmt.Errorf("%w: topic %q has invalid character %q", ErrInvalidTopic, t, r)
		}
	}
	return nil
}

// Publish encodes the payload, sends it via pg_notify, returns.
func (b *PostgresBackplane) Publish(ctx context.Context, topic string, payload []byte) error {
	b.mu.Lock()
	closed := b.closed
	b.mu.Unlock()
	if closed {
		return ErrBackplaneClosed
	}
	if err := validateTopic(topic); err != nil {
		return err
	}
	if len(payload) > MaxPayloadBytes {
		return fmt.Errorf("%w: %d > %d", ErrPayloadTooLarge, len(payload), MaxPayloadBytes)
	}
	env := envelope{
		R: b.replicaID,
		P: base64.StdEncoding.EncodeToString(payload),
	}
	body, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("coord: marshal envelope: %w", err)
	}
	// Final wire size check — base64 + envelope JSON overhead might
	// push us past the 8000-byte cap even when payload is under
	// MaxPayloadBytes. Cap the marshaled size strictly to 7900 to keep
	// the publisher errors deterministic.
	if len(body) > 7900 {
		return fmt.Errorf("%w: envelope %d bytes (payload was %d)", ErrPayloadTooLarge, len(body), len(payload))
	}
	if _, err := b.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, topic, string(body)); err != nil {
		return fmt.Errorf("coord: pg_notify %s: %w", topic, err)
	}
	return nil
}

// Subscribe dials a dedicated LISTEN connection and starts a goroutine
// that pumps events onto the returned channel. The channel closes when
// ctx is cancelled.
//
// Buffer policy: 64-item buffer. Slow subscribers cause silent drops
// — all Phase 2+ consumers re-check authoritative state in the DB on
// receipt, so a missed event becomes a delayed-but-eventually-correct
// operation rather than a correctness bug.
func (b *PostgresBackplane) Subscribe(ctx context.Context, topic string) (<-chan Event, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, ErrBackplaneClosed
	}
	b.mu.Unlock()
	if err := validateTopic(topic); err != nil {
		return nil, err
	}
	out := make(chan Event, 64)
	subCtx, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	subID := b.nextID
	b.nextID++
	b.subs[subID] = cancel
	b.wg.Add(1)
	b.mu.Unlock()
	go func() {
		defer func() {
			// Always release the context (go vet flags WithCancel
			// callers that don't), then remove from the tracking map
			// so Close doesn't walk dead entries.
			cancel()
			b.mu.Lock()
			delete(b.subs, subID)
			b.mu.Unlock()
			close(out)
			b.wg.Done()
		}()
		b.runSubscribeLoop(subCtx, topic, out)
	}()
	return out, nil
}

// runSubscribeLoop is the reconnect-on-drop driver. Holds a fresh
// pgx.Connect for the lifetime of one LISTEN session; on error,
// backs off and reconnects.
func (b *PostgresBackplane) runSubscribeLoop(ctx context.Context, topic string, out chan<- Event) {
	backoff := 500 * time.Millisecond
	const maxBackoff = 30 * time.Second
	for {
		err := b.subscribeOnce(ctx, topic, out)
		if err == nil {
			return // ctx cancelled cleanly
		}
		if ctx.Err() != nil {
			return
		}
		jittered := jitter(backoff)
		log.Printf("coord: LISTEN %s dropped (%v); reconnecting in %s", topic, err, jittered)
		select {
		case <-ctx.Done():
			return
		case <-time.After(jittered):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// subscribeOnce holds one connection and pumps notifications until
// either ctx is cancelled (returns nil) or an error occurs.
func (b *PostgresBackplane) subscribeOnce(ctx context.Context, topic string, out chan<- Event) error {
	conn, err := pgx.Connect(ctx, b.dsn)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer func() {
		// pgx.Conn.Close sends a Terminate message over TCP. On a
		// dead network with context.Background() the write blocks
		// until the kernel send buffer drains (minutes). Cap it at
		// 3s so reconnect backoff + global Close() can make progress
		// when the peer has gone away (review finding #2).
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = conn.Close(closeCtx)
		closeCancel()
	}()
	// Use Exec because LISTEN takes a literal identifier (not a
	// parameter). The topic is validated by validateTopic above and is
	// safe to splice. Quote with double-quotes per the Postgres
	// identifier-quoting rule, which makes the LISTEN target tolerant
	// of identifiers that would otherwise need escaping (dots in the
	// "loomcycle.foo" topic name require quoting; without quoting,
	// Postgres would lowercase + treat the dot as a separator).
	if _, err := conn.Exec(ctx, fmt.Sprintf(`LISTEN "%s"`, topic)); err != nil {
		return fmt.Errorf("LISTEN: %w", err)
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait: %w", err)
		}
		evt, ok := b.decode(topic, n.Payload)
		if !ok {
			continue
		}
		// Self-filter — drop events we ourselves published.
		if evt.PublisherReplicaID == b.replicaID {
			continue
		}
		select {
		case out <- evt:
		default:
			// Backpressure: drop on overflow. Log at low rate (every
			// drop logs would be noisy; first-drop-per-topic-per-minute
			// would be ideal but adds complexity. Phase 1 logs every
			// drop; if it becomes noisy in practice, rate-limit later).
			log.Printf("coord: subscriber for %s is slow, dropping event from %s", topic, evt.PublisherReplicaID)
		}
	}
}

// decode parses a NOTIFY payload string. Returns ok=false on malformed
// payloads (logged); ok=true with a populated Event on success.
func (b *PostgresBackplane) decode(topic, raw string) (Event, bool) {
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		log.Printf("coord: malformed NOTIFY payload on %s (decode env): %v", topic, err)
		return Event{}, false
	}
	payload, err := base64.StdEncoding.DecodeString(env.P)
	if err != nil {
		log.Printf("coord: malformed NOTIFY payload on %s (decode b64): %v", topic, err)
		return Event{}, false
	}
	return Event{
		Topic:              topic,
		Payload:            payload,
		PublisherReplicaID: env.R,
	}, true
}

// Close shuts down all subscribers and marks the backplane closed.
// Idempotent. Blocks up to 5s waiting for subscriber goroutines to drain.
func (b *PostgresBackplane) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	// Snapshot every live cancel func before releasing the lock.
	// Each cancel triggers its subscriber goroutine to exit, which
	// in turn removes its own entry from b.subs — but we already
	// have the snapshot so the concurrent mutation is fine.
	cancels := make([]context.CancelFunc, 0, len(b.subs))
	for _, c := range b.subs {
		cancels = append(cancels, c)
	}
	b.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Printf("coord: backplane close timed out waiting for subscribers")
	}
	return nil
}

// jitter applies ±20% randomness to d for thundering-herd protection
// on reconnect bursts (multiple replicas waking up after a Postgres
// restart shouldn't all reconnect at the same tick).
func jitter(d time.Duration) time.Duration {
	delta := float64(d) * 0.2
	off := (rand.Float64()*2 - 1) * delta
	return d + time.Duration(off)
}
