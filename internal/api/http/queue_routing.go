package http

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

// Queue-aware routing (RFC EI P4): an agent with `routing: {queue_aware: true}`
// is sent, when its run starts, past a tier candidate whose provider is busy
// enough that the first call would wait longer than `switch_after_ms`.
//
// It acts only where a run STARTS — a new run, a session continuation, a
// sub-agent — and nowhere else resolveAgentDef is called: a resume, a retune, a
// compaction summary or a report of the effective config re-resolve a run that
// already has its place, and moving it there, or reporting a choice that
// depends on the moment's load, is not what the field asks for. The run then
// stays where it landed, as the RFC requires.

// runStartKey marks the ctx of a run-start resolution.
type runStartKey struct{}

// withRunStart marks ctx as resolving a run that is about to start.
func withRunStart(ctx context.Context) context.Context {
	return context.WithValue(ctx, runStartKey{}, true)
}

func isRunStart(ctx context.Context) bool {
	v, _ := ctx.Value(runStartKey{}).(bool)
	return v
}

// estimatedQueueWait is how long a run's first call on (provider, model) would
// wait for its provider's chat lane: the scheduler's estimate from the lane's
// state and the model's median call time. 0 when the provider caps nothing or
// the model has no measured call yet — an unmeasured model is never passed
// over on a guess.
func (s *Server) estimatedQueueWait(provider, model string) time.Duration {
	callMs := s.throughput.MedianCallMs(provider, model)
	return s.callSched.EstimatedWait(provider, config.ModelKindChat, model, time.Duration(callMs)*time.Millisecond)
}
