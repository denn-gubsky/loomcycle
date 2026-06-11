package http

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/pause"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// pauseGate is the per-run loop.PauseGate implementation (RFC X / F41). It
// wraps the runtime pause Manager + store so the loop can cooperatively park
// at an iteration boundary when a runtime-wide pause is in effect, keeping
// loop.go free of pause/store imports.
type pauseGate struct {
	mgr   *pause.Manager
	store store.Store
	runID string
}

// pauseStatePersistTimeout bounds the store write that records a run's
// pause_state. The write rides a non-cancellable ctx (it must land even if the
// run's own ctx is racing cancellation), so a deadline prevents a wedged store
// from blocking park/unpark forever.
const pauseStatePersistTimeout = 5 * time.Second

// PauseRequested reports whether a runtime pause is in effect — the loop's
// cheap top-of-iteration check. State() is the hot-path-safe accessor
// (lock-free in single-replica mode, 1s-cached in cluster mode).
func (g *pauseGate) PauseRequested() bool {
	return g.mgr != nil && !g.mgr.State().AcceptsNewRuns()
}

// Park persists pause_state='paused', blocks until the runtime resumes (or the
// run ctx is cancelled), then restores 'running'. The Manager's BeginPark
// re-checks state under its lock so a run that raced a concurrent resume
// returns immediately without parking.
func (g *pauseGate) Park(ctx context.Context) error {
	resume, shouldPark := g.mgr.BeginPark(g.runID)
	if !shouldPark {
		return nil
	}
	// Persist 'paused' to the store BEFORE marking the run parked in the
	// barrier: Pause() only treats a run as quiesced once MarkParked fires, so
	// this ordering guarantees finalizePause / snapshot (which read the store)
	// see the 'paused' row by the time the barrier releases.
	//
	// If the durable write FAILS we still block (the run stops executing — the
	// quiesce we want) but do NOT MarkParked: the barrier must never count a
	// run as paused when its store row isn't durably 'paused', or
	// paused_runs_count / snapshot would disagree with the store. Pause then
	// reports it as not-yet-parked (a warning), so the operator won't snapshot
	// an inconsistent state.
	if err := g.setPauseState(ctx, store.PauseStatePaused); err != nil {
		log.Printf("pause: persist paused for run %s failed: %v — parking without barrier credit", g.runID, err)
	} else {
		g.mgr.MarkParked(g.runID)
	}
	defer func() {
		g.mgr.EndPark(g.runID)
		// Resume() also bulk-flips paused→running (covers restored/orphaned
		// runs with no live loop); this per-run flip is idempotent with it.
		_ = g.setPauseState(context.Background(), store.PauseStateRunning)
	}()
	select {
	case <-resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// setPauseState writes runs.pause_state under a bounded, non-cancellable ctx so
// the marker survives a run-ctx cancellation racing the pause. Returns the
// store error so Park can decide whether the run earned barrier credit.
func (g *pauseGate) setPauseState(parent context.Context, state string) error {
	if g.store == nil || g.runID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), pauseStatePersistTimeout)
	defer cancel()
	return g.store.SetRunPauseState(ctx, g.runID, state)
}

// newPauseGate builds a loop.PauseGate for runID and registers the run with the
// pause barrier, returning a deregister func to defer at the loop site. Returns
// (nil, no-op) when pause isn't wired (no manager / no store) — the loop then
// skips pausing entirely.
func (s *Server) newPauseGate(runID string) (loop.PauseGate, func()) {
	if s.pauseMgr == nil || s.store == nil || runID == "" {
		return nil, func() {}
	}
	s.pauseMgr.RegisterRun(runID)
	return &pauseGate{mgr: s.pauseMgr, store: s.store, runID: runID}, func() {
		s.pauseMgr.DeregisterRun(runID)
	}
}

// runtimePaused reports whether new runs should be rejected (a pause is in
// flight). The admission gate (RFC X / F41) — the help doc already promises
// "new /v1/runs return 503 while pausing/paused".
func (s *Server) runtimePaused() bool {
	return s.pauseMgr != nil && !s.pauseMgr.State().AcceptsNewRuns()
}

// rejectIfPausedHTTP writes a 503 and returns true when the runtime is paused,
// so an HTTP run-admission handler can `if s.rejectIfPausedHTTP(w) { return }`
// BEFORE acquiring a concurrency slot.
func (s *Server) rejectIfPausedHTTP(w http.ResponseWriter) bool {
	if !s.runtimePaused() {
		return false
	}
	writeJSONError(w, http.StatusServiceUnavailable, "runtime_paused",
		"runtime is paused (snapshot quiesce); retry after resume")
	return true
}

// pausedRunErr returns runner.ErrRuntimePaused when the runtime is paused, for
// the runner.RunOnce admission path (gRPC / webhook / A2A / scheduler). nil
// when not paused.
func (s *Server) pausedRunErr() error {
	if s.runtimePaused() {
		return runner.ErrRuntimePaused
	}
	return nil
}
