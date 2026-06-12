// Package steer implements the in-memory per-run input registry for operator
// "steering" — unsolicited instructions injected into an in-flight run's
// conversation between iterations (Claude-Code-style interjection).
//
// It mirrors internal/cancel: an in-memory map (run_id → live handle) that
// vanishes when the process exits; the run's transcript (persisted user_input
// events) is the durable record. Two differences from cancel/interruption:
//   - Direction: cancel + interruption are agent- or lifecycle-initiated;
//     steering is OPERATOR-initiated and asynchronous (the operator produces
//     while the loop consumes).
//   - Mechanism: a buffered channel per run (not a one-shot bus.Wait), so the
//     producing HTTP handler is decoupled from the consuming loop goroutine
//     and ordering + multiple queued messages are preserved.
//
// Keyed by run_id (not agent_id): a steering message targets one in-flight
// run, and the loop knows its run_id. Cross-replica delivery (a Push that
// lands on a replica not running the target) is the ClusterSteerer seam —
// nil here = single-replica (local miss → ErrRunNotFound).
package steer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrRunNotFound is returned by Push when no in-flight run is registered for
// the run_id (and, in cluster mode, no replica owns it). HTTP maps it to 404.
var ErrRunNotFound = errors.New("steer: no in-flight run for run_id")

// ErrQueueFull is returned by Push when the run's buffer is full — the
// operator is outpacing the loop's drain. HTTP maps it to 429 (retry).
var ErrQueueFull = errors.New("steer: run input queue full")

// KindCompact marks a Message as a context-compaction control rather than an
// operator steering turn (RFC: interactive context compaction). The loop, on
// draining a compact Message, REPLACES its in-memory conversation with a
// summary pair built from Text instead of appending Text as a user turn. The
// empty Kind ("") is the default — an ordinary steering/continuation message.
const KindCompact = "compact"

// Message is one operator-injected steering instruction (Kind == "") or a
// control message (e.g. KindCompact). Carried over the same per-run queue so
// the loop applies it at its next iteration / park boundary.
type Message struct {
	Text       string
	Source     string // "api" | "webui" — resolved at the auth boundary, never the wire
	EnqueuedAt time.Time
	// Kind discriminates the message. "" = an operator turn (append Text as a
	// user message). KindCompact = replace the conversation with a summary
	// built from Text. New kinds extend this without changing the queue plumbing.
	Kind string
	// KeepN / KeepFirst accompany a KindCompact control: keep the last KeepN
	// messages verbatim and pin the first user turn when KeepFirst. Computed by
	// the server (snapped to a clean boundary) so the loop applies them verbatim.
	KeepN     int
	KeepFirst bool
}

// Entry is the live handle for one run's steering queue.
type Entry struct {
	RunID     string
	AgentID   string
	SessionID string
	UserID    string
	ch        chan Message
	// parked reports whether the run is currently parked at end_turn awaiting
	// operator input (an interactive run between turns) vs. actively mid-turn.
	// A pointer so the value-copied Entry returned by Get still observes
	// updates. Flipped by SetParked (driven by the run's awaiting_input /
	// resume events); read by IsParked for the compaction boundary gate.
	parked *atomic.Bool
}

// ClusterSteerer is the cross-replica fallback (mirror of
// cancel.ClusterCanceller). When set, a Push that misses the local map
// delegates to PushRemote, which routes by runs.replica_id over the backplane.
// Nil = single-replica mode; a local miss returns ErrRunNotFound directly.
// The interface lives here so this package stays free of internal/coord.
type ClusterSteerer interface {
	PushRemote(ctx context.Context, runID string, m Message) (delivered bool, found bool, err error)
}

// Registry maps run_id → buffered steering channel. Safe for concurrent use
// by the HTTP handler goroutines and the run goroutines that deregister on
// completion.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry
	cap     int
	cluster ClusterSteerer // nil in single-replica mode
}

// NewRegistry returns an empty registry. perRunCap is the per-run buffer
// depth (<=0 → default 16) — a bound so an operator can't unbounded-buffer
// against a stuck run; a full buffer surfaces as ErrQueueFull → 429.
func NewRegistry(perRunCap int) *Registry {
	if perRunCap <= 0 {
		perRunCap = 16
	}
	return &Registry{entries: make(map[string]Entry), cap: perRunCap}
}

// SetClusterSteerer installs the cross-replica fallback. Called from main.go
// in cluster mode after the backplane is wired; not designed for mid-flight
// swaps.
func (r *Registry) SetClusterSteerer(c ClusterSteerer) {
	r.mu.Lock()
	r.cluster = c
	r.mu.Unlock()
}

// Register installs a steering queue for a run and returns the receive side
// the loop drains plus a deregister func (defer it so even a panic cleans
// up). A run terminating deregisters; a Push after that returns ErrRunNotFound.
func (r *Registry) Register(e Entry) (<-chan Message, func()) {
	ch := make(chan Message, r.cap)
	e.ch = ch
	e.parked = &atomic.Bool{}
	r.mu.Lock()
	r.entries[e.RunID] = e
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		// Guard against clobbering a newer entry that reused the run_id
		// (shouldn't happen — run_ids are unique — but cheap insurance).
		if cur, ok := r.entries[e.RunID]; ok && cur.ch == ch {
			delete(r.entries, e.RunID)
		}
		r.mu.Unlock()
	}
}

// Push enqueues a message for run_id. Returns:
//   - (true, nil)              delivered to the local buffer (or a remote replica)
//   - (false, ErrQueueFull)    local buffer full
//   - (false, ErrRunNotFound)  no local entry and no cluster route
func (r *Registry) Push(ctx context.Context, runID string, m Message) (bool, error) {
	r.mu.RLock()
	e, ok := r.entries[runID]
	cluster := r.cluster
	r.mu.RUnlock()
	if !ok {
		if cluster == nil {
			return false, ErrRunNotFound
		}
		delivered, found, err := cluster.PushRemote(ctx, runID, m)
		if err != nil {
			return false, err
		}
		if !found {
			return false, ErrRunNotFound
		}
		return delivered, nil
	}
	select {
	case e.ch <- m:
		return true, nil
	default:
		return false, ErrQueueFull
	}
}

// PushLocal is Push without cluster delegation — for a cluster subscriber
// dispatching an inbound backplane event to the local registry (mirror of
// cancel.CancelLocal; using Push there would re-broadcast on a local miss).
// found=false on a local miss; delivered=false (found=true) on a full buffer.
func (r *Registry) PushLocal(runID string, m Message) (delivered, found bool) {
	r.mu.RLock()
	e, ok := r.entries[runID]
	r.mu.RUnlock()
	if !ok {
		return false, false
	}
	select {
	case e.ch <- m:
		return true, true
	default:
		return false, true
	}
}

// Get returns the live entry for run_id (and a presence bool). Used by the
// HTTP handler to resolve the run's session for the tenant-ownership gate
// before pushing. The returned Entry's channel is unexported, so callers
// can only read its metadata (RunID/SessionID/UserID/AgentID).
func (r *Registry) Get(runID string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[runID]
	return e, ok
}

// Count returns the number of live entries. Test/diagnostic helper.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// SetParked records whether a registered run is parked at end_turn awaiting
// input. Driven by the run's awaiting_input event (true) and the events that
// signal it resumed work (false). No-op for an unregistered run_id.
func (r *Registry) SetParked(runID string, parked bool) {
	r.mu.RLock()
	e, ok := r.entries[runID]
	r.mu.RUnlock()
	if ok && e.parked != nil {
		e.parked.Store(parked)
	}
}

// IsParked reports whether a LOCALLY-registered run is parked awaiting input —
// the safe boundary for context compaction. False when the run_id is unknown
// locally (terminated, or owned by another replica): the caller treats those
// via the terminal / cross-replica paths, not as "parked here".
func (r *Registry) IsParked(runID string) bool {
	r.mu.RLock()
	e, ok := r.entries[runID]
	r.mu.RUnlock()
	return ok && e.parked != nil && e.parked.Load()
}
