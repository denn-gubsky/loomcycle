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
// It deliberately does NOT import internal/loop: the caller INJECTS a causeFor
// (SetCauseFor(loop.TurnCancelCause)) that builds the cancel cause from an
// operator reason string, so this leaf package stays free of internal/loop AND
// internal/coord. That same reason-string boundary is what lets P3a route a
// cancel cross-replica — the wire carries the reason string, and each replica's
// registry rebuilds the proper loop cause locally via its own causeFor.
//
// Cross-replica owner-routing (P3a) mirrors internal/cancel + internal/steer: on
// a LOCAL MISS, Cancel delegates to a ClusterCanceller (implemented by
// internal/coord.TurnCancelCoordinator) that routes the cancel to the run's
// owning replica by runs.replica_id over the backplane. nil ClusterCanceller =
// single-process mode, byte-identical to P1 (a local miss fires nothing).
package turncancel

import (
	"context"
	"sync"
)

// ClusterCanceller is the cross-replica fallback (mirror of
// cancel.ClusterCanceller / steer.ClusterSteerer). When set, a Cancel that
// misses the local armed map delegates to CancelRemote, which routes the cancel
// by runs.replica_id over the backplane to the run's owning replica. nil =
// single-replica mode; a local miss fires nothing (P1 behaviour, byte-identical).
//
// The wire carries the REASON STRING, not a cause error: this leaf can't build a
// loop cause, and the owning replica rebuilds it locally via its own causeFor.
// found reports whether the owning replica had the run armed and fired it.
//
// The interface lives here (not in internal/coord) so this leaf package stays
// free of the coord dependency — internal/coord implements it on
// TurnCancelCoordinator.
type ClusterCanceller interface {
	CancelRemote(ctx context.Context, runID, reason string) (found bool, err error)
}

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
	// causeFor builds the cancel cause from an operator reason string. Injected
	// (SetCauseFor(loop.TurnCancelCause)) so the leaf stays loop-free; a cause
	// the loop DOESN'T recognize as a turn-cancel would terminate the run, so a
	// nil causeFor makes the fire methods refuse to fire (fail-safe).
	causeFor func(reason string) error
	// cluster is the cross-replica fallback; nil in single-process mode.
	cluster ClusterCanceller
}

type entry struct {
	cancel context.CancelCauseFunc
	gen    uint64
}

// NewRegistry returns an empty registry. Wire SetCauseFor before use (the fire
// methods refuse to fire without it); SetClusterCanceller is cluster-mode only.
func NewRegistry() *Registry {
	return &Registry{armed: make(map[string]entry)}
}

// SetCauseFor installs the reason→cause builder (the server passes
// loop.TurnCancelCause). Called once at server construction; not for mid-flight
// swaps.
func (r *Registry) SetCauseFor(fn func(reason string) error) {
	r.mu.Lock()
	r.causeFor = fn
	r.mu.Unlock()
}

// SetClusterCanceller installs the cross-replica fallback. Called from main.go
// in cluster mode after the backplane is wired; not for mid-flight swaps.
func (r *Registry) SetClusterCanceller(c ClusterCanceller) {
	r.mu.Lock()
	r.cluster = c
	r.mu.Unlock()
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

// Cancel is the handler-facing entry: it fires run_id's armed per-turn token with
// the operator reason and reports whether a cancel fired. On a LOCAL hit it fires
// here (building the proper loop cause via causeFor). On a local miss in cluster
// mode it delegates to the ClusterCanceller, which routes to the owning replica
// by runs.replica_id. Single-process (cluster == nil) → a local miss is
// (false, nil), byte-identical to P1.
func (r *Registry) Cancel(ctx context.Context, runID, reason string) (bool, error) {
	if r.CancelLocal(runID, reason) {
		return true, nil
	}
	r.mu.Lock()
	cluster := r.cluster
	r.mu.Unlock()
	if cluster == nil {
		return false, nil
	}
	return cluster.CancelRemote(ctx, runID, reason)
}

// CancelLocal fires run_id's LOCALLY-armed token with the operator reason and
// removes it, returning whether a token was armed here. It NEVER delegates to the
// cluster (the CancelLocal lesson — a cluster subscriber dispatching an inbound
// backplane event must not re-broadcast on a local miss), so it is what both the
// handler's local fast path and the coordinator's owning-side subscriber call.
//
// Removing on fire makes a double-cancel idempotent: the second call finds
// nothing armed and returns false (the handler 409s it), so a cancel that races
// the turn ending / a repeat click can never double-fire.
func (r *Registry) CancelLocal(runID, reason string) bool {
	r.mu.Lock()
	causeFor := r.causeFor
	if causeFor == nil {
		// Defensive floor — never hit in production (the server wires SetCauseFor
		// at construction). Without a cause builder we can't produce a cause the
		// loop recognizes as a turn-cancel, and firing a wrong cause would
		// TERMINATE the run instead of parking it. Refuse, leaving the token armed.
		r.mu.Unlock()
		return false
	}
	e, ok := r.armed[runID]
	if ok {
		delete(r.armed, runID)
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	e.cancel(causeFor(reason))
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
