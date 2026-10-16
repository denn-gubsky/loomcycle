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

// queueMoveFn is the loop's MoveForQueue. At a call boundary after the first,
// a tier-routed, queue-aware run that holds no slot of its provider's chat lane
// (its lease was preempted or expired, or the lane keeps none) and whose next
// call there would wait past switch_after_ms is resolved again as a run start
// is — the same cascade, the same estimate — and moves if that picks somewhere
// else. Holding a lease, it never moves: the lease is its warm prompt cache,
// and moving would throw it away.
//
// Where it may move is decided by the run's routing as it is NOW
// (currentRoutedDef): a retune can change its tier, model, provider or effort,
// and switch queue-aware routing on, off or change the threshold mid-run. That
// is why every run gets the hook, a pinned one included: a retune can put it on
// a tier. The record is read only once the cheap checks say a move is in
// question — no lease held, some wait ahead — so a run whose lane is not busy
// costs no store read.
//
// def is the run's EFFECTIVE definition at start and startRouting the routing
// override it started with; the other arguments are what the run's own start
// resolved with.
func (s *Server) queueMoveFn(def config.AgentDef, startRouting *routingOverride, runID, tenantID, userID, agentName, userTier string, restricted bool) func(context.Context, providers.Provider, string) (providers.Provider, string, string, bool) {
	if s.resolver == nil {
		return nil
	}
	startKey := shapeKey(startRouting)
	return func(ctx context.Context, current providers.Provider, model string) (providers.Provider, string, string, bool) {
		if s.callSched.HoldsLease(current.ID(), config.ModelKindChat, runID) {
			return nil, "", "", false
		}
		wait := s.estimatedQueueWait(current.ID(), model)
		if wait <= 0 {
			return nil, "", "", false
		}
		eff, ok := s.currentRoutedDef(ctx, def, startKey, runID, tenantID, agentName)
		if !ok || !routesByTier(eff) {
			return nil, "", "", false
		}
		if eff.Routing == nil || !eff.Routing.QueueAware || wait <= eff.Routing.SwitchAfter() {
			return nil, "", "", false
		}
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

// currentRoutedDef is the definition a run routes by now, read from its record.
//
// While the record's routing override is still the one the run started with,
// that is start — the definition it started on, which a sub-agent's def_id pin
// or inherited overrides already shaped — with the record's routing block, if
// it has one, else start's own. Once a retune changed the override, the run is
// re-derived as the operator turn that adopts the retune derives it
// (reResolveOnOperatorTurnFn): its definition as looked up now, with the record
// applied. Deriving from start instead moved a run retuned to another tier back
// into the tier it started on.
//
// ok is false when the retuned routing no longer holds against the definition,
// and the run stays where it is. A record that cannot be read leaves start as
// it is.
func (s *Server) currentRoutedDef(ctx context.Context, start config.AgentDef, startKey, runID, tenantID, agentName string) (config.AgentDef, bool) {
	if s.store == nil || runID == "" {
		return start, true
	}
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return start, true
	}
	rec, ok := decodeRunConfig(run.RunConfig)
	if !ok {
		return start, true
	}
	if shapeKey(rec.Routing) == startKey {
		if rec.QueueRouting != nil {
			start.Routing = rec.QueueRouting
		}
		return start, true
	}
	def, found := s.lookupAgent(rec.withTeamScope(ctx), tenantID, agentName)
	if !found {
		return config.AgentDef{}, false
	}
	eff, err := s.effectiveDef(ctx, def, runOverrides{Routing: rec.Routing, QueueRouting: rec.QueueRouting})
	if err != nil {
		return config.AgentDef{}, false
	}
	return eff, true
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
