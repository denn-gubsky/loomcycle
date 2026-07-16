// Package turncancel implements the in-memory per-run "turn-scoped cancel"
// registry (RFC BH). It mirrors internal/steer's run-keyed shape: an in-memory
// map (run_id → the currently-armed per-turn cancel func) that vanishes when the
// process exits; the run's transcript is the durable record.
//
// Unlike the whole-run cancel registry (internal/cancel, one func per run for
// the run's lifetime), a turn-cancel token is armed at the START of each loop
// iteration (the model call + tool dispatch) and disarmed at the turn boundary.
// Firing it stops the CURRENT turn — the in-flight generation and the tool calls
// it started — without terminating the run: the loop catches the sentinel cause,
// synthesizes valid history, and parks the interactive run at awaiting_input.
//
// It deliberately does NOT import internal/loop: the caller passes the cancel
// cause (loop.ErrTurnCancelled, optionally wrapped with an operator reason) as
// an opaque error, so this leaf package stays dependency-free. Single-process
// (P1); cross-replica owner-routing by runs.replica_id is P3 (mirror steer).
package turncancel

import (
	"context"
	"sync"
)

// Registry maps a live run_id → its currently-armed per-turn CancelCauseFunc.
// At most one token is armed per run at a time (the loop overwrites it on each
// turn via Arm and clears it via the returned disarm). Safe for concurrent use
// by the run goroutines (Arm/disarm) and the HTTP handler goroutines (Cancel/
// IsArmed).
type Registry struct {
	mu    sync.Mutex
	armed map[string]entry
	// seq is a monotonic generation counter. Each Arm stamps the entry with a
	// fresh gen so a stale disarm (from a turn that has since been re-armed) can
	// be told apart from the live token and made a no-op — a missed disarm can
	// therefore never delete a newer turn's token.
	seq uint64
}

type entry struct {
	cancel context.CancelCauseFunc
	gen    uint64
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{armed: make(map[string]entry)}
}

// Arm registers cancel as run_id's live per-turn token and returns a disarm that
// removes it. Re-arming (the next turn) overwrites the prior token; the prior
// turn's disarm then finds a generation mismatch and no-ops, so a caller that
// forgets to disarm an old turn can never clobber the newer turn's token.
func (r *Registry) Arm(runID string, cancel context.CancelCauseFunc) func() {
	r.mu.Lock()
	r.seq++
	gen := r.seq
	r.armed[runID] = entry{cancel: cancel, gen: gen}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if e, ok := r.armed[runID]; ok && e.gen == gen {
			delete(r.armed, runID)
		}
		r.mu.Unlock()
	}
}

// Cancel fires run_id's armed token with cause and removes it, returning whether
// a token was armed. Removing on fire makes a double-cancel idempotent: the
// second call finds nothing armed and returns false (the handler 409s it), so a
// cancel that races the turn ending / a repeat click can never double-fire.
func (r *Registry) Cancel(runID string, cause error) bool {
	r.mu.Lock()
	e, ok := r.armed[runID]
	if ok {
		delete(r.armed, runID)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	e.cancel(cause)
	return true
}

// IsArmed reports whether run_id currently has an armed per-turn token — i.e. the
// run is mid-turn and turn-cancellable. False once the turn ends (disarmed), the
// token has been fired (Cancel removes it), or the run terminates.
func (r *Registry) IsArmed(runID string) bool {
	r.mu.Lock()
	_, ok := r.armed[runID]
	r.mu.Unlock()
	return ok
}
