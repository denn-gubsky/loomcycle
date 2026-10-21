package http

import (
	"context"
	"fmt"
	"log"
	"sync"

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
		opts := d.opts
		var loopErr lastErrorEvent
		opts.OnEvent = loopErr.watch(opts.OnEvent)
		loopRes, runErr := loop.Run(d.loopCtx, opts)
		if runErr != nil {
			// Persist the failure so a tailing client sees it — unless the
			// loop's own last error event already says exactly this, as it
			// does for most failures (a cancel mid-turn among them): a second
			// copy showed the failure twice. A classified copy still goes out,
			// since it carries the category the loop's event lacks.
			if ev := runErrorEvent(runErr); ev.ErrorInfo != nil || !loopErr.is(ev.Error) {
				d.emit(ev)
			}
		}
		// WithoutCancel: the store write must not ride a runCtx that an
		// API-cancel already cancelled (the cause is still read from runCtx).
		s.finishRunWithCancel(context.WithoutCancel(d.runCtx), d.runCtx, d.runID, loopRes, runErr, d.meta)
	}()
	return done
}

// lastErrorEvent remembers the text of the last error event a run emitted.
// The loop may emit from more than one goroutine.
type lastErrorEvent struct {
	mu   sync.Mutex
	text string
	seen bool
}

// watch wraps emit to record each error event before passing it on.
func (l *lastErrorEvent) watch(emit func(providers.Event)) func(providers.Event) {
	return func(ev providers.Event) {
		if ev.Type == providers.EventError {
			l.mu.Lock()
			l.text, l.seen = ev.Error, true
			l.mu.Unlock()
		}
		if emit != nil {
			emit(ev)
		}
	}
}

// is reports whether the last error event emitted said text.
func (l *lastErrorEvent) is(text string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seen && l.text == text
}

// forwardWhileAttached wraps a run's forward to its caller so that it stops
// once the caller has gone. detach returns only when no delivery is in flight,
// so after it returns fwd is never called again — which is what lets a caller
// return while something still emits through the run: a detached run that goes
// on, or a background child winding down after its parent's end. net/http
// forbids writing to a ResponseWriter after its handler returns, and a gRPC
// stream is dead after its handler returns.
func forwardWhileAttached(fwd func(providers.Event)) (forward func(providers.Event), detach func()) {
	var mu sync.Mutex
	attached := true
	forward = func(ev providers.Event) {
		mu.Lock()
		defer mu.Unlock()
		if attached {
			fwd(ev)
		}
	}
	detach = func() {
		mu.Lock()
		attached = false
		mu.Unlock()
	}
	return forward, detach
}
