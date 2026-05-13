// Package cancel implements the in-memory registry that maps an
// agent_id to a context.CancelCauseFunc, so an external HTTP request
// can cancel a still-running loop.
//
// Why in-memory: the registry holds a goroutine-safe mapping that
// vanishes when the loomcycle process exits. Persistent recording
// (status=running rows in the store) is the durable source of truth;
// the registry is the live-cancellation surface bolted on top. A
// process restart loses the cancelFns but leaves the DB intact, which
// is the correct shape — the runs they referenced were torn down by
// the same restart.
//
// Trust + scope:
//   - Bearer-only at the HTTP layer (matches the existing pattern).
//     Per-user authorization is the host application's responsibility.
//   - agent_id uniqueness is ENFORCED here, not at the DB level —
//     historical rows can share an agent_id (a caller may legitimately
//     reuse it after the previous run terminated). Two simultaneously
//     active runs sharing one agent_id is a programming error and is
//     refused via ErrInUse.
//
// Lifecycle:
//
//	┌────────────────────────────────────────────────────────────────┐
//	│ HTTP req in →  Server creates session+run → Register(agentID)  │
//	│                          │                                     │
//	│                          ▼                                     │
//	│              ┌─ loop.Run(runCtx) ──────────────┐                │
//	│              │  (subloops inherit runCtx —     │                │
//	│              │   parent cancel cascades)       │                │
//	│              └─────────────────────────────────┘                │
//	│                          │                                     │
//	│                          ▼                                     │
//	│              FinishRun (writes terminal status)                 │
//	│                          │                                     │
//	│                          ▼                                     │
//	│              Deregister(agentID)  ← `defer` so even panics      │
//	│                                     deregister cleanly         │
//	└────────────────────────────────────────────────────────────────┘
//
// Cancel from a separate request:
//
//	POST /v1/agents/<id>/cancel
//	↓
//	Registry.Cancel("a_id", "user_clicked_stop")
//	↓
//	1. cancelFn(ErrCancelledByAPI) — runCtx becomes cancelled with cause
//	2. Walk children: each direct child's cancelFn fires too
//	   (recursively descends because each child's cancelFn ALSO triggers
//	   ITS subtree's ctx cascade)
//	3. Loop iterations exit on next ctx check; FinishRun sees the cause
//	   and writes RunCancelled (rather than RunFailed) via the cause-
//	   aware logic in server.finishRun.
package cancel

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrCancelledByAPI is the cause attached to runCtx when the cancel
// API is invoked. The HTTP server's finishRun checks this via
// `errors.Is(context.Cause(runCtx), ErrCancelledByAPI)` and writes
// status=cancelled rather than the default failed.
//
// Distinguishing API-cancel from client-disconnect (which produces
// context.Canceled with no cause) matters: the former is a deliberate
// terminal state worth recording; the latter is a transport hiccup
// that should still mark the run as failed (or completed if the loop
// happened to finish first).
var ErrCancelledByAPI = errors.New("cancelled by api")

// ErrInUse is returned by Register when an agent_id is already mapped
// to a still-active run. The HTTP layer surfaces this as a 409.
var ErrInUse = errors.New("agent_id already registered")

// Entry is one row in the registry — the live cancel handle plus the
// metadata needed for cascade lookups and observability.
type Entry struct {
	AgentID       string
	RunID         string
	SessionID     string
	UserID        string
	ParentAgentID string
	StartedAt     time.Time
	cancelFn      context.CancelCauseFunc
}

// CancelResult is what Cancel returns: whether the cancel actually
// fired, the reason text recorded, and the agent_ids of every direct
// or transitive child that was cancelled as part of the cascade.
//
// Idempotency: a Cancel of an already-deregistered agent_id returns
// {Cancelled: false, Cascaded: nil} — the caller's HTTP response
// becomes "already terminated" with whatever status the store records.
type CancelResult struct {
	Cancelled bool
	Reason    string
	Cascaded  []string
}

// Registry maps agent_id → live cancel handle. Safe for concurrent
// use by the HTTP handler goroutines AND the run goroutines that
// deregister on completion.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// NewRegistry returns an empty registry. The HTTP server constructs
// one at boot and threads it through every handler that creates a run.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[string]Entry)}
}

// Register adds an agent_id → entry mapping. Returns ErrInUse if the
// agent_id is already mapped to an active run. The caller MUST call
// Deregister when the run terminates (typically via `defer` so even
// panics clean up).
//
// The cancelFn passed in must be the result of context.WithCancelCause
// — Cancel invokes it with ErrCancelledByAPI as the cause.
func (r *Registry) Register(e Entry, cancelFn context.CancelCauseFunc) error {
	if e.AgentID == "" {
		return errors.New("registry: agent_id is required")
	}
	if cancelFn == nil {
		return errors.New("registry: cancelFn is required")
	}
	e.cancelFn = cancelFn
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[e.AgentID]; exists {
		return ErrInUse
	}
	r.entries[e.AgentID] = e
	return nil
}

// Deregister removes an agent_id from the registry. Idempotent — a
// deregister of an unknown id is a no-op (typically the run was
// already cancelled and the cancel path removed the entry).
func (r *Registry) Deregister(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, agentID)
}

// Get returns the entry for an agent_id, or zero value + false. Used
// by the GET /v1/agents/{agent_id} endpoint to determine "is this run
// still in flight?" — when present, the answer is yes (the live entry
// has not been deregistered). When absent, the endpoint falls back to
// the store to surface the terminal status.
func (r *Registry) Get(agentID string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[agentID]
	return e, ok
}

// Cancel invokes the cancelFn for agentID and recursively cancels
// every direct child (an entry whose ParentAgentID equals one of the
// already-cancelled ids). The reason is attached as the cancel-cause
// argument so the HTTP server's finishRun can record it as
// runs.stop_reason.
//
// Idempotent: a second Cancel for an already-cancelled agentID returns
// (CancelResult{Cancelled: false}, false). The caller can map this to
// either "200 already done" or "404 unknown" by checking the store.
//
// Concurrency note: the cascade walk takes a single read-lock-then-
// release-then-fire pattern (snapshot under RLock, then call
// cancelFns outside the lock). Holding the lock while invoking
// cancelFns would deadlock if a cancelFn synchronously triggers
// Deregister via run goroutine cleanup.
func (r *Registry) Cancel(agentID, reason string) (CancelResult, bool) {
	cause := ErrCancelledByAPI
	if reason != "" {
		// Wrap the sentinel so context.Cause carries both the type
		// (errors.Is(..., ErrCancelledByAPI) still works) and the
		// human reason.
		cause = &cancelWithReason{reason: reason}
	}

	// Snapshot and fire the requested entry under lock.
	r.mu.Lock()
	root, ok := r.entries[agentID]
	if !ok {
		r.mu.Unlock()
		return CancelResult{}, false
	}
	delete(r.entries, agentID)
	r.mu.Unlock()
	root.cancelFn(cause)

	// Walk children. We iterate over a snapshot so we never hold the
	// lock while invoking cancelFns. New children registered during
	// the cascade are correctly cancelled because the parent's
	// cancelFn already cancelled their parent's runCtx, which they
	// inherit via runSubAgent's ctx threading; the registry walk is a
	// belt-and-braces measure for grandchildren whose parent already
	// deregistered.
	cascaded := []string{}
	queue := []string{agentID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]

		r.mu.Lock()
		var children []Entry
		for id, e := range r.entries {
			if e.ParentAgentID == parent {
				children = append(children, e)
				delete(r.entries, id)
			}
		}
		r.mu.Unlock()

		for _, c := range children {
			c.cancelFn(cause)
			cascaded = append(cascaded, c.AgentID)
			queue = append(queue, c.AgentID)
		}
	}

	return CancelResult{Cancelled: true, Reason: reason, Cascaded: cascaded}, true
}

// ListByUser returns a snapshot of every entry whose UserID matches.
// Used by GET /v1/users/{user_id}/agents to surface what's running
// for a user (the store has the terminal-state info; the registry
// has the live in-flight info — together they let the UI reason
// about both).
func (r *Registry) ListByUser(userID string) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if userID == "" {
		return nil
	}
	var out []Entry
	for _, e := range r.entries {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	return out
}

// ListChildren returns direct children of an agent_id from the live
// registry. Recursion is the caller's job (Cancel does this internally
// via its queue walk). Exposed for tests and possible future
// "list-tree" endpoints.
func (r *Registry) ListChildren(parentAgentID string) []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if parentAgentID == "" {
		return nil
	}
	var out []Entry
	for _, e := range r.entries {
		if e.ParentAgentID == parentAgentID {
			out = append(out, e)
		}
	}
	return out
}

// Count returns the number of live entries. Test-only helper; not
// part of the public lifecycle.
func (r *Registry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}

// ListAll returns a snapshot of every live entry regardless of
// user or parent. General-purpose accessor for diagnostics and
// future cross-cutting consumers (the v0.8.x metrics sampler
// already gates on `concurrency.Semaphore.Stats()` for its
// active-runs count; this method exists for richer per-agent
// snapshots a future PR may need). The caller MUST NOT mutate
// the returned slice; treat as read-only. O(n) under RLock; cheap
// for typical run counts (≤ MaxConcurrentRuns).
func (r *Registry) ListAll() []Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Entry, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e)
	}
	return out
}

// cancelWithReason wraps the API-cancel sentinel with a human reason
// so context.Cause carries both. errors.Is(cause, ErrCancelledByAPI)
// continues to identify the cancel as API-originated; cause.Error()
// surfaces the operator-facing reason for inclusion in stop_reason.
type cancelWithReason struct {
	reason string
}

func (c *cancelWithReason) Error() string { return "cancelled by api: " + c.reason }
func (c *cancelWithReason) Is(target error) bool {
	return target == ErrCancelledByAPI
}

// Reason returns the reason text. The HTTP server's finishRun uses
// this to populate runs.stop_reason when the cause is API-cancel.
func (c *cancelWithReason) Reason() string { return c.reason }

// ReasonFromCause extracts the reason text from a context.Cause value
// produced by Cancel. Returns "" for non-API causes (e.g. plain
// context.Canceled from client-disconnect, or a sentinel without a
// reason wrapper).
func ReasonFromCause(cause error) string {
	if cause == nil {
		return ""
	}
	var withReason *cancelWithReason
	if errors.As(cause, &withReason) {
		return withReason.reason
	}
	return ""
}
