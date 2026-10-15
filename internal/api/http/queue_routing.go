package http

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
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

// queueMoveFn is the loop's MoveForQueue for a tier-routed run, nil for one
// pinned to a provider or model. At a call boundary after the first, a run
// that holds no slot of its provider's chat lane (its lease was preempted or
// expired, or the lane keeps none) and whose next call there would wait past
// switch_after_ms is resolved again as a run start is — the same cascade, the
// same estimate — and moves if that picks somewhere else. Holding a lease, it
// never moves: the lease is its warm prompt cache, and moving would throw it
// away.
//
// Whether it is queue-aware, and its switch_after_ms, are the run's routing
// block as it is NOW: a retune can switch it on, off or change the threshold
// mid-run (currentQueueRouting). It is read only once the cheap checks say a
// move is in question — no lease held, some wait ahead — so a run whose lane
// is not busy costs no store read.
//
// def is the run's EFFECTIVE definition at start (a per-run `routing` already
// applied), and the arguments are what the run's own start resolved with.
func (s *Server) queueMoveFn(def config.AgentDef, runID, tenantID, userID, agentName, userTier string, restricted bool) func(context.Context, providers.Provider, string) (providers.Provider, string, string, bool) {
	if !routesByTier(def) || s.resolver == nil {
		return nil
	}
	return func(ctx context.Context, current providers.Provider, model string) (providers.Provider, string, string, bool) {
		if s.callSched.HoldsLease(current.ID(), config.ModelKindChat, runID) {
			return nil, "", "", false
		}
		wait := s.estimatedQueueWait(current.ID(), model)
		if wait <= 0 {
			return nil, "", "", false
		}
		routing := s.currentQueueRouting(ctx, runID, def.Routing)
		if routing == nil || !routing.QueueAware || wait <= routing.SwitchAfter() {
			return nil, "", "", false
		}
		eff := def
		eff.Routing = routing
		pid, m, effort, err := s.resolveAgentDef(withRunStart(ctx), eff, tenantID, userID, agentName, userTier, restricted)
		if err != nil || (pid == current.ID() && m == model) {
			return nil, "", "", false
		}
		p, err := s.providers.Get(pid)
		if err != nil {
			return nil, "", "", false
		}
		return p, m, effort, true
	}
}

// currentQueueRouting is the run's routing block as last written to its record
// — at start, or by a retune since — else fallback, the block its definition
// started with.
func (s *Server) currentQueueRouting(ctx context.Context, runID string, fallback *config.AgentRouting) *config.AgentRouting {
	if s.store == nil || runID == "" {
		return fallback
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return fallback
	}
	if rec, ok := decodeRunConfig(run.RunConfig); ok && rec.QueueRouting != nil {
		return rec.QueueRouting
	}
	return fallback
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
