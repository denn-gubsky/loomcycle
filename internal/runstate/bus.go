// Package runstate implements an in-process pub/sub for run-state
// transition events. The SSE handler GET /v1/users/{user_id}/agents/stream
// subscribes; the five finishRun* paths plus the run-creation moment
// publish.
//
// This package is intentionally a sibling to channels.Bus rather than
// a reuse of it — the channels Bus is a "presence of new data" tap
// (waiters block, get woken, re-query the store). Run-state events
// need to carry their payload to the subscriber directly so the SSE
// stream can emit the run_id + status delta without a follow-up DB
// read; the natural shape is per-subscriber buffered channels.
//
// Cross-process delivery is OUT OF SCOPE for v0.9.x (same punt as
// channels.Bus). Multi-replica HA work can swap the backplane for
// Postgres LISTEN/NOTIFY or Redis pub/sub without changing the
// Subscribe / Publish surface.
//
// Concurrency: Bus is safe for concurrent Publish + Subscribe calls.
// Each subscription holds its own buffered channel; the publisher
// fan-outs with a non-blocking send so a stalled subscriber never
// blocks a finishRun write. Buffer overflow increments DroppedEvents
// on the subscription and is logged at unsubscribe time.
package runstate

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/coord"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RunStateEvent is the payload published on every run state transition.
// Fields mirror the SSE frame the handler will emit.
type RunStateEvent struct {
	RunID         string    `json:"run_id"`
	AgentID       string    `json:"agent_id"`
	Agent         string    `json:"agent"`
	UserID        string    `json:"user_id"`
	ParentAgentID string    `json:"parent_agent_id,omitempty"`
	Status        string    `json:"status"`
	StopReason    string    `json:"stop_reason,omitempty"`
	Error         string    `json:"error,omitempty"`
	TS            time.Time `json:"ts"`
	// ParentContext echoes the run's opaque tracking lineage (v0.12.x)
	// on each state transition, so a subscriber to the user agents
	// stream learns which root request a finishing sub-agent belongs to
	// without a follow-up fetch. Nil when the run carried no context.
	ParentContext *store.ParentContext `json:"parent_context,omitempty"`
}

// subscription is one active subscriber's state.
type subscription struct {
	userID  string
	ch      chan RunStateEvent
	dropped int64 // atomic counter incremented on buffer overflow
}

// DroppedEvents returns the cumulative number of events that were
// dropped because the subscriber's buffer was full. Useful for the
// SSE handler to surface as diagnostic info on the close frame.
func (s *subscription) DroppedEvents() int64 {
	return atomic.LoadInt64(&s.dropped)
}

// Bus is the in-process run-state pub/sub. v0.12.3 Phase 4 adds an
// optional backplane fanout: when SetBackplane is called, every local
// Publish ALSO publishes on `loomcycle.runstate` so remote replicas'
// SubscribeBackplane goroutine wakes their local subscribers. The
// PostgresBackplane self-filter prevents the originating replica from
// receiving its own NOTIFY.
type Bus struct {
	mu         sync.Mutex
	byUser     map[string][]*subscription
	bufferSize int

	// bp is the cluster-mode backplane. Nil = single-replica mode
	// (v0.11.x behavior unchanged — local-only publish).
	bp coord.Backplane
}

// NewBus returns a fresh, empty bus. Single instance per loomcycle
// process; injected into *http.Server.
//
// bufferSize is the per-subscription channel capacity. 64 is a
// reasonable default — a healthy SSE consumer drains in <1ms; 64
// covers ~64 in-flight runs between drains before the publisher
// starts dropping for that subscriber.
func NewBus() *Bus {
	return &Bus{
		byUser:     make(map[string][]*subscription),
		bufferSize: 64,
	}
}

// Subscription is the public handle returned by Subscribe. The
// subscriber reads RunStateEvents from C and calls Close() when done.
type Subscription struct {
	bus *Bus
	sub *subscription
	C   <-chan RunStateEvent
}

// Close unregisters the subscription. Idempotent.
func (s *Subscription) Close() {
	if s == nil || s.bus == nil {
		return
	}
	s.bus.unsubscribe(s.sub)
	s.bus = nil
}

// DroppedEvents returns the cumulative number of events dropped
// because the subscription's buffer was full. Safe to call at any
// time, including after Close.
func (s *Subscription) DroppedEvents() int64 {
	if s == nil || s.sub == nil {
		return 0
	}
	return s.sub.DroppedEvents()
}

// Subscribe registers a new subscriber for events on the given user
// scope. Returns a Subscription whose C channel delivers events until
// Close is called.
//
// An empty userID subscribes to ALL events (admin / operator-side
// debugging). Used by the gRPC streaming RPC when callers pass no
// filter. The HTTP handler always provides a concrete user_id.
func (b *Bus) Subscribe(userID string) *Subscription {
	sub := &subscription{
		userID: userID,
		ch:     make(chan RunStateEvent, b.bufferSize),
	}
	b.mu.Lock()
	b.byUser[userID] = append(b.byUser[userID], sub)
	b.mu.Unlock()
	return &Subscription{bus: b, sub: sub, C: sub.ch}
}

// Publish fans out an event to every matching subscriber.
//
// "Matching" means: the subscriber's userID equals event.UserID, OR
// the subscriber's userID is "" (subscribe-all). Non-blocking send on
// each subscriber — a full buffer increments the subscriber's
// DroppedEvents counter rather than blocking the publisher. This is
// the right trade-off: a stalled SSE consumer must NEVER block a
// finishRun write.
//
// Idempotent + safe to call from any goroutine, including the request
// handler goroutine.
func (b *Bus) Publish(evt RunStateEvent) {
	b.publishLocal(evt)
	// v0.12.3 Phase 4: cluster-mode fanout. Non-blocking — backplane
	// errors are logged but never fail Publish (a local subscriber on
	// THIS replica already received the event; cross-replica delivery
	// is best-effort).
	b.mu.Lock()
	bp := b.bp
	b.mu.Unlock()
	if bp != nil {
		if payload, err := json.Marshal(evt); err == nil {
			if pubErr := bp.Publish(context.Background(), "loomcycle.runstate", payload); pubErr != nil {
				log.Printf("runstate: backplane publish failed: %v", pubErr)
			}
		}
	}
}

// publishLocal is the v0.12.3 fan-out-to-local-subscribers half of
// Publish. Extracted so SubscribeBackplane can call it on remote
// events WITHOUT triggering another backplane publish (which would
// loop). The PostgresBackplane self-filter already prevents the
// originating replica from looping, but the dedicated entry point
// keeps the no-re-publish invariant explicit.
func (b *Bus) publishLocal(evt RunStateEvent) {
	if evt.TS.IsZero() {
		evt.TS = time.Now().UTC()
	}
	b.mu.Lock()
	matches := make([]*subscription, 0, len(b.byUser[evt.UserID])+len(b.byUser[""]))
	matches = append(matches, b.byUser[evt.UserID]...)
	if evt.UserID != "" {
		matches = append(matches, b.byUser[""]...)
	}
	b.mu.Unlock()

	for _, sub := range matches {
		select {
		case sub.ch <- evt:
		default:
			atomic.AddInt64(&sub.dropped, 1)
		}
	}
}

// SetBackplane installs the v0.12.3 Phase 4 cluster-mode fanout.
// When set, every Publish ALSO publishes on `loomcycle.runstate`.
// Nil-safe: calling with nil disables fanout (e.g. for tests).
func (b *Bus) SetBackplane(bp coord.Backplane) {
	b.mu.Lock()
	b.bp = bp
	b.mu.Unlock()
}

// SubscribeBackplane starts a goroutine that listens for remote
// run-state events on `loomcycle.runstate` and fans them out to
// local subscribers via publishLocal (which does NOT re-publish on
// the backplane). Exits on ctx.Done.
func (b *Bus) SubscribeBackplane(ctx context.Context, bp coord.Backplane) error {
	ch, err := bp.Subscribe(ctx, "loomcycle.runstate")
	if err != nil {
		return err
	}
	go func() {
		for evt := range ch {
			var rs RunStateEvent
			if err := json.Unmarshal(evt.Payload, &rs); err != nil {
				log.Printf("runstate: malformed backplane event: %v", err)
				continue
			}
			b.publishLocal(rs)
		}
	}()
	return nil
}

// unsubscribe is called by Subscription.Close. Removes the
// subscription from the per-user slice and closes its channel so a
// goroutine still ranging over C exits cleanly.
func (b *Bus) unsubscribe(sub *subscription) {
	if sub == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.byUser[sub.userID]
	for i, s := range subs {
		if s == sub {
			subs[i] = subs[len(subs)-1]
			subs = subs[:len(subs)-1]
			break
		}
	}
	if len(subs) == 0 {
		delete(b.byUser, sub.userID)
	} else {
		b.byUser[sub.userID] = subs
	}
	// Close exactly once. Buffered sends from a concurrent Publish
	// can race with this close; we accept losing the very last in-
	// flight event on a closing subscription as the cost of a clean
	// close. The drain is fine — the SSE handler is the only reader
	// and it cancels its ctx before calling Close.
	close(sub.ch)
}

// ActiveSubscriberCount returns the total number of active
// subscriptions. Diagnostic / test helper.
func (b *Bus) ActiveSubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, subs := range b.byUser {
		n += len(subs)
	}
	return n
}
