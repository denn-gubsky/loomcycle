// callscheduler.go — the lanes and the model gate of every capped provider.
//
// A CallScheduler caps model CALLS, not runs: a slot is held from just before a
// provider call until its stream ends, never while a run executes a tool or
// waits. It is one lookup by provider id that is a zero-allocation noop for a
// provider nobody capped.
// A capped provider has one Lane per model kind it caps (chat, decision,
// embedder — the kind is an opaque key here) and one ModelGate across them.
package concurrency

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// noopRelease is the shared release for a call nothing gates. One package-level
// value keeps the ungated path free of allocations.
var noopRelease = func() {}

// ErrProviderConcurrencyExhausted is the refusal of a request that holds no
// run slot (one sent directly, outside any run) when the queue it would have
// joined on a capped provider is already full. A call made by a run is never
// refused this way: the run was admitted, so its calls wait.
//
// Today the one such request is the document image-describe route, which
// reports it as that image's failure; errclassify reads it as transient.
type ErrProviderConcurrencyExhausted struct {
	Provider string
	// Cap is the provider's max_concurrent for the kind of call refused; 0
	// when it was the cap on models in flight that the request queued behind.
	Cap int
}

func (e *ErrProviderConcurrencyExhausted) Error() string {
	return fmt.Sprintf("provider concurrency exhausted: provider=%s cap=%d", e.Provider, e.Cap)
}

// Code is the refusal's typed identifier.
func (e *ErrProviderConcurrencyExhausted) Code() string { return "provider_concurrency_exhausted" }

// IsProviderConcurrencyExhausted reports whether err is an
// *ErrProviderConcurrencyExhausted.
func IsProviderConcurrencyExhausted(err error) bool {
	var e *ErrProviderConcurrencyExhausted
	return errors.As(err, &e)
}

// ProviderLanesConfig is the call-level caps of one provider.
type ProviderLanesConfig struct {
	// Lanes maps a model kind to its lane. A kind with no entry, or with
	// MaxConcurrent <= 0, is ungated — the same "no cap, no gate" rule as a
	// provider's max_concurrent.
	Lanes map[string]LaneConfig
	// MaxModelsInFlight caps the distinct models computing at once on the
	// provider, across every kind of call — including a kind with no lane.
	// <= 0 = no cap.
	MaxModelsInFlight int
	// ModelSwitchMaxWait is the ModelGate anti-starvation bound.
	ModelSwitchMaxWait time.Duration
	// ExemptKinds are the kinds the model gate leaves out: their calls are
	// neither counted nor held back by it, only by their own lane.
	ExemptKinds []string
}

// ProviderLaneStats is one provider's snapshot.
type ProviderLaneStats struct {
	Lanes  map[string]LaneStats // by kind
	Models ModelGateStats
}

// providerLanes is one provider's lanes and gate. It is immutable once
// published in CallScheduler.byID: Reload builds a new one (reusing the Lane
// and ModelGate values that survive), so a reader needs the lock only for the
// map lookup.
type providerLanes struct {
	lanes map[string]*Lane
	gate  *ModelGate
	// exempt holds the kinds the gate does not apply to.
	exempt map[string]bool
}

// CallScheduler holds the lanes and model gate of every capped provider. A nil
// *CallScheduler gates nothing.
type CallScheduler struct {
	mu   sync.RWMutex
	byID map[string]*providerLanes
}

// NewCallScheduler builds the lanes for cfgs, keyed by provider id. A provider
// with no lane and no models-in-flight cap gets no entry at all.
func NewCallScheduler(cfgs map[string]ProviderLanesConfig) *CallScheduler {
	s := &CallScheduler{}
	s.Reload(cfgs)
	return s
}

// lane returns the lane for (providerID, kind), nil when that kind has none,
// and the provider's model gate, nil when the provider caps nothing at all.
// The gate does not depend on the lane: a provider's cap on models computing
// at once covers every call to it, whichever kinds it also gives a lane —
// except a kind the provider exempts, for which the gate is nil.
func (s *CallScheduler) lane(providerID, kind string) (*Lane, *ModelGate) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.byID[providerID]
	if p == nil {
		return nil, nil
	}
	if p.exempt[kind] {
		return p.lanes[kind], nil
	}
	return p.lanes[kind], p.gate
}

// Gated reports whether a call of the given kind to providerID is capped at
// all right now — by a lane, or by the provider's cap on models in flight. A
// caller uses it to keep an uncapped call free of any bookkeeping; the answer
// can change at the next Reload, and Acquire is correct either way.
func (s *CallScheduler) Gated(providerID, kind string) bool {
	lane, gate := s.lane(providerID, kind)
	return lane != nil || gate.enabled()
}

// LaneCap returns the max_concurrent of the (providerID, kind) lane, 0 when
// that kind has no lane.
func (s *CallScheduler) LaneCap(providerID, kind string) int {
	lane, _ := s.lane(providerID, kind)
	if lane == nil {
		return 0
	}
	return lane.Stats().MaxConcurrent
}

// Acquire blocks until c may make one call of the given kind to providerID and
// returns the release for it. Call release when the call ends; it is
// idempotent. An ungated (provider, kind) — and a nil scheduler — returns a
// shared noop release without allocating.
//
// The model gate is entered BEFORE the lane: a call waiting for its model to
// be admitted must not sit on a lane slot that a call to a model already in
// flight could use. Neither wait times out; only ctx ends one, with ctx.Err().
func (s *CallScheduler) Acquire(ctx context.Context, providerID, kind string, c CallInfo) (release func(), err error) {
	return s.acquire(ctx, providerID, kind, c, -1)
}

// AcquireBounded is Acquire for a request that holds no admission slot — one a
// caller sends directly, outside any run, so nothing else bounds how many can
// wait. With maxQueued callers already waiting at the model gate or in the
// lane, a call that would have to wait there is refused with *QueueFullError
// instead.
func (s *CallScheduler) AcquireBounded(ctx context.Context, providerID, kind string, c CallInfo, maxQueued int) (release func(), err error) {
	if maxQueued < 0 {
		maxQueued = 0
	}
	return s.acquire(ctx, providerID, kind, c, maxQueued)
}

func (s *CallScheduler) acquire(ctx context.Context, providerID, kind string, c CallInfo, maxQueued int) (release func(), err error) {
	lane, gate := s.lane(providerID, kind)
	if lane == nil && gate == nil {
		return noopRelease, nil
	}
	leave, counted, err := gate.enter(ctx, c.Model, maxQueued, c.OnQueued)
	if err != nil {
		return nil, err
	}
	if lane == nil {
		// No lane for this kind: the call is bound only by the models-in-flight
		// cap (leave is the shared noop when that is off or the call names no
		// model).
		return leave, nil
	}
	rel, err := lane.acquire(ctx, c, maxQueued)
	if err != nil {
		leave()
		return nil, err
	}
	if !counted {
		return rel, nil
	}
	// Both halves are idempotent, so the pair is too. The lane goes first: the
	// slot is what other callers queue for.
	return func() {
		rel()
		leave()
	}, nil
}

// EstimatedWait is how long a new call of kind to model on providerID would
// wait before it computes, judged from the lane's and the model gate's state
// right now and callTime, the typical time one call holds its slot. It is an
// estimate for choosing where a run goes (queue-aware routing), not a promise.
//
//   - A free lane slot waits nothing; a full lane waits for the calls queued
//     ahead and this one, shared across its slots: (queued+1)/max × callTime.
//     A slot held for the run that last used it counts as taken.
//   - A model at its own cap in the lane (LaneConfig.ModelCaps) waits for its
//     own calls instead, whatever the lane's slots: the calls queued for that
//     model ahead and this one, shared across the model's cap:
//     (queued for the model+1)/cap × callTime. When one of them ends, its slot
//     is free for the next, so the lane's other traffic does not add to it.
//   - A gate at its cap that does not have the model in flight adds one
//     callTime: the call waits for a model to drain first.
//
// 0 when nothing caps the call, or callTime is unknown (0): with no measure of
// how long a call takes, a queue's length says nothing about its wait.
func (s *CallScheduler) EstimatedWait(providerID, kind, model string, callTime time.Duration) time.Duration {
	if callTime <= 0 {
		return 0
	}
	lane, gate := s.lane(providerID, kind)
	var wait time.Duration
	if lane != nil {
		if atCap, capN, queued := lane.modelWait(model); atCap {
			wait = time.Duration(queued+1) * callTime / time.Duration(capN)
		} else if st := lane.Stats(); st.MaxConcurrent > 0 && st.Active+st.Reserved >= st.MaxConcurrent {
			wait = time.Duration(st.Queued+1) * callTime / time.Duration(st.MaxConcurrent)
		}
	}
	if gate.blocksNew(model) {
		wait += callTime
	}
	return wait
}

// Done tells every lane that owner's run has ended, dropping its reservations.
func (s *CallScheduler) Done(owner string) {
	if s == nil || owner == "" {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.byID {
		for _, l := range p.lanes {
			l.Done(owner)
		}
	}
}

// Reload applies a new config (config reload). A lane whose (provider, kind)
// survives is reconfigured in place and keeps its calls, reservations and
// queue; so does a surviving provider's model gate. An added lane appears. A
// removed lane is forgotten, not torn down: callers already in it hold their
// own release and its queue still drains as they finish, while new calls go
// ungated.
func (s *CallScheduler) Reload(cfgs map[string]ProviderLanesConfig) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]*providerLanes, len(cfgs))
	for id, cfg := range cfgs {
		old := s.byID[id]
		lanes := make(map[string]*Lane, len(cfg.Lanes))
		for kind, lc := range cfg.Lanes {
			if lc.MaxConcurrent <= 0 {
				continue
			}
			if old != nil && old.lanes[kind] != nil {
				old.lanes[kind].Reconfigure(lc)
				lanes[kind] = old.lanes[kind]
				continue
			}
			lanes[kind] = NewLane(lc)
		}
		// A surviving gate takes the new limit even when the provider ends up
		// with nothing capped: disabling it is what lets its waiters go.
		if old != nil {
			old.gate.SetLimit(cfg.MaxModelsInFlight, cfg.ModelSwitchMaxWait)
		}
		if len(lanes) == 0 && cfg.MaxModelsInFlight <= 0 {
			continue
		}
		p := &providerLanes{lanes: lanes}
		if len(cfg.ExemptKinds) > 0 {
			p.exempt = make(map[string]bool, len(cfg.ExemptKinds))
			for _, k := range cfg.ExemptKinds {
				p.exempt[k] = true
			}
		}
		if old != nil {
			p.gate = old.gate
		} else {
			p.gate = NewModelGate(cfg.MaxModelsInFlight, cfg.ModelSwitchMaxWait)
		}
		next[id] = p
	}
	s.byID = next
}

// Stats returns a snapshot per capped provider, keyed by provider id. Nil when
// nothing is capped.
func (s *CallScheduler) Stats() map[string]ProviderLaneStats {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.byID) == 0 {
		return nil
	}
	out := make(map[string]ProviderLaneStats, len(s.byID))
	for id, p := range s.byID {
		ps := ProviderLaneStats{Lanes: make(map[string]LaneStats, len(p.lanes)), Models: p.gate.Stats()}
		for kind, l := range p.lanes {
			ps.Lanes[kind] = l.Stats()
		}
		out[id] = ps
	}
	return out
}
