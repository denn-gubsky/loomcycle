// modelgate.go — the cap on how many DIFFERENT models compute at once on one
// provider.
//
// Some hosts fall over when several models run at the same moment (a GPU box
// that faults, or thrashes loading and evicting weights). A lane cannot express
// that: it counts calls, and three calls to one model are fine where three
// calls to three models are not. The gate counts distinct models instead: any
// number of calls to a model already in flight, and at most maxModels models.
package concurrency

import (
	"context"
	"sync"
	"time"
)

// ModelGateStats is a point-in-time snapshot of a ModelGate.
type ModelGateStats struct {
	InFlight map[string]int // model -> calls in flight; nil when none
	Waiting  int
}

// gateWaiter is one queued Enter.
type gateWaiter struct {
	model string
	since time.Time
	ch    chan struct{} // closed on admission
	// admitted is written under ModelGate.mu by the admitter; a cancelled
	// waiter reads it under the same lock (see Lane's laneWaiter.granted).
	admitted bool
}

// ModelGate caps the distinct models in flight on one provider. The zero value
// is not usable; build one with NewModelGate. A nil *ModelGate admits
// everything.
type ModelGate struct {
	mu      sync.Mutex
	max     int
	maxWait time.Duration

	inFlight map[string]int
	waiters  []*gateWaiter // FIFO
}

// NewModelGate builds a gate. maxModels <= 0 disables it. maxWait > 0 bounds
// how long the oldest waiter can be overtaken by calls to models already in
// flight (see Enter); maxWait <= 0 leaves that unbounded.
func NewModelGate(maxModels int, maxWait time.Duration) *ModelGate {
	return &ModelGate{max: maxModels, maxWait: maxWait, inFlight: map[string]int{}}
}

// Enter blocks until a call to model may compute and returns the leave for it.
// Call leave when the call ends; it is idempotent.
//
// A call is admitted at once when its model is already in flight, or when
// fewer than maxModels models are. Otherwise it waits, first come first
// served, with no timeout: only ctx ends the wait, with ctx.Err(). When a
// model's last call leaves, the oldest waiter's model is admitted together
// with every other waiter for that same model.
//
// Left at that, a model with steady traffic would never drain and a waiting
// model would never get in. So once the oldest waiter has waited maxWait, new
// calls to models in flight queue behind it too: the models in flight run
// down, and the waiting model gets its turn.
//
// A disabled gate (maxModels <= 0) and a call with no model ("") are not
// counted at all.
func (g *ModelGate) Enter(ctx context.Context, model string) (leave func(), err error) {
	leave, _, err = g.enter(ctx, model, -1, nil)
	return leave, err
}

// enter is Enter, also reporting whether the call was counted. An uncounted
// call gets the shared noop leave, which the scheduler need not wrap.
//
// maxWaiting >= 0 bounds the queue for a caller that holds no admission slot
// (see Lane.AcquireBounded): with that many already waiting, a call that would
// have to wait gets *QueueFullError instead. < 0 is no bound.
//
// onQueued, when set, is called once the call has queued, before it blocks
// (see CallInfo.OnQueued).
func (g *ModelGate) enter(ctx context.Context, model string, maxWaiting int, onQueued func(CallQueueInfo)) (leave func(), counted bool, err error) {
	if g == nil || model == "" {
		return noopRelease, false, nil
	}
	g.mu.Lock()
	if g.max <= 0 {
		g.mu.Unlock()
		return noopRelease, false, nil
	}
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return nil, false, err
	}
	if g.admissible(model) {
		g.inFlight[model]++
		g.mu.Unlock()
		return g.leaveFn(model), true, nil
	}
	if maxWaiting >= 0 && len(g.waiters) >= maxWaiting {
		n := len(g.waiters)
		g.mu.Unlock()
		return nil, false, &QueueFullError{Queued: n, Limit: maxWaiting}
	}
	w := &gateWaiter{model: model, since: time.Now(), ch: make(chan struct{})}
	g.waiters = append(g.waiters, w)
	pos := len(g.waiters)
	g.mu.Unlock()
	if onQueued != nil {
		onQueued(CallQueueInfo{Reason: CallQueueReasonModel, Position: pos})
	}

	select {
	case <-w.ch:
		return g.leaveFn(model), true, nil
	case <-ctx.Done():
		g.mu.Lock()
		if w.admitted {
			// Admitted while giving up: give the place back.
			g.exit(model)
		} else {
			g.unqueue(w)
			// The waiter that left may have been the one everybody behind it
			// was held for.
			g.admit()
		}
		g.mu.Unlock()
		return nil, false, ctx.Err()
	}
}

// enabled reports whether the gate caps anything right now.
func (g *ModelGate) enabled() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max > 0
}

// SetLimit changes the cap and the anti-starvation bound in place (config
// reload). Raising or disabling the cap admits waiters at once; lowering it
// never interrupts a call in flight, the excess models drain. Calls that
// entered while the gate was disabled were not counted, so for as long as they
// run a newly enabled gate can see fewer models than really compute.
func (g *ModelGate) SetLimit(maxModels int, maxWait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.max = maxModels
	g.maxWait = maxWait
	g.admit()
}

// Stats returns a snapshot of the gate.
func (g *ModelGate) Stats() ModelGateStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := ModelGateStats{Waiting: len(g.waiters)}
	if len(g.inFlight) > 0 {
		out.InFlight = make(map[string]int, len(g.inFlight))
		for m, n := range g.inFlight {
			out.InFlight[m] = n
		}
	}
	return out
}

// admissible reports whether a NEW call to model may go ahead of the queue.
// Caller holds mu.
func (g *ModelGate) admissible(model string) bool {
	if g.inFlight[model] > 0 {
		return !g.starving()
	}
	// A new model never overtakes the queue. (A free place with waiters left
	// cannot outlast admit, which runs on every change; the check keeps that
	// from depending on it.)
	return len(g.waiters) == 0 && len(g.inFlight) < g.max
}

// starving reports whether the oldest waiter has been overtaken for long
// enough. Caller holds mu.
func (g *ModelGate) starving() bool {
	return g.maxWait > 0 && len(g.waiters) > 0 && time.Since(g.waiters[0].since) >= g.maxWait
}

func (g *ModelGate) leaveFn(model string) func() {
	left := false
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		if left {
			return
		}
		left = true
		g.exit(model)
	}
}

// exit ends one counted call to model. Caller holds mu.
func (g *ModelGate) exit(model string) {
	if g.inFlight[model] > 1 {
		g.inFlight[model]--
		return
	}
	delete(g.inFlight, model)
	g.admit()
}

// admit lets the head of the queue in for as long as it fits, each time with
// every other waiter for the same model. Caller holds mu.
func (g *ModelGate) admit() {
	for len(g.waiters) > 0 {
		model := g.waiters[0].model
		if g.max > 0 && g.inFlight[model] == 0 && len(g.inFlight) >= g.max {
			return
		}
		kept := g.waiters[:0]
		for _, w := range g.waiters {
			if w.model != model {
				kept = append(kept, w)
				continue
			}
			g.inFlight[model]++
			w.admitted = true
			close(w.ch)
		}
		// Clear the tail so admitted waiters are not kept alive by the array.
		for i := len(kept); i < len(g.waiters); i++ {
			g.waiters[i] = nil
		}
		g.waiters = kept
	}
}

// unqueue removes a waiter that gave up while still queued. Caller holds mu.
func (g *ModelGate) unqueue(w *gateWaiter) {
	for i, x := range g.waiters {
		if x == w {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			return
		}
	}
}
