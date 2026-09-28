package http

import (
	"context"
	"fmt"
	"log"

	"github.com/denn-gubsky/loomcycle/internal/loop"
	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// detachedLoop is a registered run whose loop must outlive the call that
// started it: an interactive run (parks for operator input) or one held for
// review (waits for a verdict), either of which can take far longer than a
// caller keeps its connection. The goroutine that runs it owns its teardown.
type detachedLoop struct {
	runID   string
	meta    runStateMeta
	loopCtx context.Context
	// runCtx is the run's cancellable ctx, detached from the caller; the
	// finish reads the cancel cause from it.
	runCtx context.Context
	opts   loop.RunOptions
	// emit persists a failure the loop returned, so a client tailing the
	// store sees it.
	emit func(providers.Event)
	// teardown releases what the run registered (steer queue, pause gate,
	// cancel registry entry, span, cancel func). It runs once, after the run
	// has finished — or panicked.
	teardown func()
}

// startDetachedLoop runs d's loop in a goroutine that outlives its caller and
// returns a channel that is closed once the run is finished and torn down.
// Returning (or the caller's ctx ending) does not stop the run: only its
// cancel func, reached through the cancel registry, does.
func (s *Server) startDetachedLoop(d detachedLoop) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Teardown is DEFERRED + panic-guarded: a panic in loop.Run must
		// not (a) crash the process (this goroutine has no other recover —
		// the recoveryMiddleware only wraps the synchronous handler) nor
		// (b) skip deregistration, which would leak the run in the cancel /
		// steer / pause-barrier registries — a leaked pause entry never
		// parks, so every future Pause would time out waiting for a ghost.
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("interactive run %s panicked: %v", d.runID, rec)
				s.finishRunFailedReason(d.runID, fmt.Sprintf("panic: %v", rec), d.meta)
			}
			d.teardown()
		}()
		loopRes, runErr := loop.Run(d.loopCtx, d.opts)
		if runErr != nil {
			// Persist (not stream) the failure so a tailing client sees it.
			d.emit(runErrorEvent(runErr))
		}
		// WithoutCancel: the store write must not ride a runCtx that an
		// API-cancel already cancelled (the cause is still read from runCtx).
		s.finishRunWithCancel(context.WithoutCancel(d.runCtx), d.runCtx, d.runID, loopRes, runErr, d.meta)
	}()
	return done
}
