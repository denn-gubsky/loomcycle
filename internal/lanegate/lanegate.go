// Package lanegate puts a provider's call lane around every call made to it.
//
// A provider's max_concurrent counts model CALLS: a slot is taken just before
// a call and given back when its stream ends, so a run holds nothing while it
// executes a tool, awaits a sub-agent or waits for its operator. The cap is
// only as good as its coverage, so the gate is not something a call site opts
// into: the provider registry hands out every driver already wrapped (see
// cmd/loomcycle), and the loop, compaction, the LLM gateway and every other
// holder of a registry provider is counted without knowing it.
//
// A wrapper hides whatever its driver implements beyond providers.Provider.
// Callers reach those through providers.As, never a type assertion.
//
// Not every model call goes through a registry provider. A service that makes
// its calls on a driver of its own (the memory reranker, the unit generator) is
// wrapped with the same Wrap under the provider id it resolved to; a decision
// model and an embedder speak protocols of their own and have their own
// wrappers (services.go). Every one of them takes its slot through acquire.
package lanegate

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/teamrun"
)

// kind is the lane every Provider.Call takes. The other model kinds (decision,
// embedder) do not go through Provider.Call: see services.go.
const kind = config.ModelKindChat

type backgroundKey struct{}

// WithBackground marks the calls made under ctx as maintenance work
// (consolidation, reindexing). A lane serves them after live runs, with the
// share its background_share guarantees.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx carries the WithBackground mark.
func IsBackground(ctx context.Context) bool {
	background, _ := ctx.Value(backgroundKey{}).(bool)
	return background
}

type queueBoundKey struct{}

// WithQueueBound marks the calls made under ctx as requests that hold no run
// slot — sent directly, outside any run — so nothing else bounds how many can
// wait. With n already waiting, a call that would have to wait is refused with
// *concurrency.ErrProviderConcurrencyExhausted instead of queueing.
func WithQueueBound(ctx context.Context, n int) context.Context {
	if n < 0 {
		n = 0
	}
	return context.WithValue(ctx, queueBoundKey{}, n)
}

// Provider is a providers.Provider whose calls take the scheduler's lane for
// its provider id. Build one with Wrap.
type Provider struct {
	inner providers.Provider
	id    string
	sched *concurrency.CallScheduler
	// floor is this wrapper's built-in bound while no lane is configured for
	// its provider (WrapWithFloor); nil for none.
	floor *concurrency.Lane
}

// Wrap returns inner gated by sched under the provider id it is registered as
// (the `providers:` key — not inner.ID(), which is the driver's own name for
// itself). The gate is looked up per call, so a cap added or removed by a
// config reload applies to the next call of a provider wrapped before it.
// A nil sched gates nothing.
func Wrap(id string, inner providers.Provider, sched *concurrency.CallScheduler) *Provider {
	return &Provider{inner: inner, id: id, sched: sched}
}

// WrapWithFloor is Wrap for a SERVICE's own driver (the memory reranker): while
// the operator has configured no chat lane for the provider, its calls are
// bounded at FloorCap in flight instead of not at all. A registry provider is
// never wrapped this way — an uncapped provider's chat calls stay uncapped.
func WrapWithFloor(id string, inner providers.Provider, sched *concurrency.CallScheduler) *Provider {
	return &Provider{inner: inner, id: id, sched: sched, floor: newFloor()}
}

// Unwrap returns the wrapped provider (providers.Unwrapper).
func (p *Provider) Unwrap() providers.Provider { return p.inner }

func (p *Provider) ID() string                           { return p.inner.ID() }
func (p *Provider) Capabilities() providers.Capabilities { return p.inner.Capabilities() }
func (p *Provider) Probe(ctx context.Context) error      { return p.inner.Probe(ctx) }
func (p *Provider) ListModels(ctx context.Context) ([]string, error) {
	return p.inner.ListModels(ctx)
}

// RunDone drops the lease the run holds on this provider's lanes
// (providers.RunEnder). A lease outlives the call that earned it so the run's
// next call lands on a warm prompt cache; a run that has ended has no next
// call, and its slot must not sit reserved for the rest of the hold.
func (p *Provider) RunDone(runID string) {
	p.sched.Done(runID)
	providers.RunDone(p.inner, runID)
}

// Call makes one call to the provider inside its lane: it waits for a slot,
// calls the driver, and gives the slot back when the call is over — at once if
// the driver refused it, otherwise when the driver's stream reports its end
// (done or error) or closes.
//
// A provider nobody capped is called directly: the driver's own channel is
// returned and nothing is allocated.
//
// A call that has to wait reports it on req.OnEvent, the hook the loop gives
// every call for the driver's own retry reports: EventCallQueued before each
// wait, EventCallAdmitted once it has its slot. It is called here, on the
// caller's goroutine, before Call returns — as a driver's retry report is. A
// call made for a run without that hook (a summary, a listwise rerank a tool
// asks for) reports on the run's event sink instead (runReporter).
func (p *Provider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	if p.floor == nil && !p.sched.Gated(p.id, kind) {
		return p.inner.Call(ctx, req)
	}
	report := req.OnEvent
	if report == nil {
		report = runReporter(ctx)
	}
	slot := -1
	release, err := acquire(ctx, p.sched, p.floor, p.id, kind, providers.ModelIdentity(p.inner, req), report, func(n int) { slot = n })
	if err != nil {
		return nil, err
	}
	// The lane slot this call runs on, for a driver that pins its backend's
	// slot to it (providers.LaneSlot).
	if slot >= 0 {
		ctx = providers.WithLaneSlot(ctx, slot)
	}
	in, err := p.inner.Call(ctx, req)
	if err != nil {
		release()
		return nil, err
	}
	// Unbuffered: the consumer's pace reaches the driver exactly as it did
	// without the wrapper.
	out := make(chan providers.Event)
	go forward(ctx, in, out, release)
	return out, nil
}

// Hold takes p's lane slot for ONE call the caller is about to make itself, and
// returns the provider to make it on plus the release for the slot. It is for a
// request that must know at ADMISSION whether its provider can take it — the
// LLM gateway, which answers 429 before any stream opens when the provider's
// queue is at its bound (WithQueueBound on ctx is honoured as Call honours it).
//
// Take it AFTER the global run slot, never before: a run takes the global slot
// and then its lane, and the opposite order here would let a request sit on a
// provider slot while it waited for a global slot held by a run that is waiting
// for that provider slot.
//
// The provider returned is the driver itself: its Call takes no second slot.
// The slot is held until release, not until the stream ends, so the caller
// releases when its request is over. A provider that is not gated is returned
// as is, with a release that does nothing.
func Hold(ctx context.Context, p providers.Provider, req providers.Request) (providers.Provider, func(), error) {
	lp, ok := p.(*Provider)
	if !ok || !lp.sched.Gated(lp.id, kind) {
		return p, func() {}, nil
	}
	release, err := acquire(ctx, lp.sched, nil, lp.id, kind, providers.ModelIdentity(lp.inner, req), nil, nil)
	if err != nil {
		return nil, nil, err
	}
	return lp.inner, release, nil
}

// heldKey marks a ctx whose caller already holds the slot of one (provider,
// kind): the value under it is true.
type heldKey struct{ id, kind string }

// HoldLane is Hold for a call that is NOT a Provider.Call — an embedding or a
// decision, made through this package's WrapEmbedder / WrapDecision. It takes
// the slot of (provider id, kind) for ONE call to model, and returns the ctx to
// make that call under: the wrapper sees the slot is already held and takes no
// second one. The same rules as Hold: take it after the global run slot, never
// before; WithQueueBound on ctx is honoured; release when the request is over.
// An ungated (provider, kind) returns ctx as is and a release that does nothing.
//
// The returned ctx is for that one call. Passed on to other work it would let
// that work's calls of the same kind through uncounted.
func HoldLane(ctx context.Context, sched *concurrency.CallScheduler, id, kind, model string) (context.Context, func(), error) {
	if !sched.Gated(id, kind) {
		return ctx, func() {}, nil
	}
	release, err := acquire(ctx, sched, nil, id, kind, model, nil, nil)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, heldKey{id, kind}, true), release, nil
}

// acquire takes the slot for one call of the given kind to model on provider
// id: the one way every wrapper in this package enters a lane, so a chat call, a
// decision and an embedding are identified, queued and refused by the same
// rules.
//
// The run identity comes from the RunMeta the loop stamps on a run's ctx, which
// a tool's ctx inherits — so a call a tool makes on a run's behalf (a Memory
// search's embedding, a Decision) queues as that run. A call made outside any
// run carries none: it is a one-shot in the anonymous bucket.
//
// A ctx from HoldLane already holds this slot: nothing more is taken.
//
// floor is the caller's built-in bound (see FloorCap), nil for none. It applies
// while the operator has configured NO LANE for (id, kind): a configured lane
// replaces it, so it never undercuts a cap the operator chose. The test is the
// lane, not "is anything gated": a provider that sets only max_models_in_flight
// is gated — its calls wait for their model — yet nothing there bounds how MANY
// of them are in flight, so the floor still applies, around the scheduler. With
// neither a floor nor a gate, the release does nothing.
//
// report, when set, is told of each wait (EventCallQueued) and of the slot
// that ends it (EventCallAdmitted); a call that waits for nothing reports
// nothing, and one whose wait ends with ctx reports no admission.
//
// onSlot, when set, is told the number of the provider lane's slot the call
// was granted (concurrency.CallInfo.OnSlot). The floor's slots are this
// service's own, not the provider's, so they are never reported.
func acquire(ctx context.Context, sched *concurrency.CallScheduler, floor *concurrency.Lane, id, kind, model string, report func(providers.Event), onSlot func(int)) (release func(), err error) {
	if held, _ := ctx.Value(heldKey{id, kind}).(bool); held {
		return func() {}, nil
	}
	gated := sched.Gated(id, kind)
	if sched.LaneCap(id, kind) > 0 {
		floor = nil
	}
	if !gated && floor == nil {
		return func() {}, nil
	}
	meta, _ := providers.RunMetaFromContext(ctx)
	info := concurrency.CallInfo{
		Owner:      meta.RunID,
		TenantID:   meta.TenantID,
		UserID:     meta.UserID,
		Background: IsBackground(ctx),
		Model:      model,
	}
	var queuedAt time.Time
	if report != nil {
		info.OnQueued = func(q concurrency.CallQueueInfo) {
			if queuedAt.IsZero() {
				queuedAt = time.Now()
			}
			report(providers.Event{
				Type:      providers.EventCallQueued,
				Admission: &providers.AdmissionInfo{Reason: q.Reason, Position: q.Position, Provider: id, Kind: kind},
			})
		}
		defer func() {
			if err == nil && !queuedAt.IsZero() {
				report(providers.Event{
					Type:      providers.EventCallAdmitted,
					Admission: &providers.AdmissionInfo{WaitMs: time.Since(queuedAt).Milliseconds(), Provider: id, Kind: kind},
				})
			}
		}()
	}
	// Queueing is not working time: the clock bounding the run (a sub-agent's
	// timeout_ms, a team state's) stops while it waits here. Reported to the
	// clock only, never as a review hold — a walk's poll table shows a hold as
	// a person deciding, which a queued call is not.
	if held := teamrun.ClockObserver(ctx); held != nil {
		held(true)
		defer held(false)
	}
	bound, bounded := ctx.Value(queueBoundKey{}).(int)

	// The floor first, then the scheduler: the floor is this service's own, so
	// holding its slot while waiting for the provider's model gate keeps nobody
	// else out of anything.
	releaseFloor := func() {}
	if floor != nil {
		if bounded {
			releaseFloor, err = floor.AcquireBounded(ctx, info, bound)
		} else {
			releaseFloor, err = floor.Acquire(ctx, info)
		}
		if concurrency.IsQueueFull(err) {
			return nil, &concurrency.ErrProviderConcurrencyExhausted{Provider: id, Cap: FloorCap}
		}
		if err != nil {
			return nil, err
		}
	}
	if !gated {
		return releaseFloor, nil
	}
	// A local, not the named result: the closure below must not capture the
	// variable it is itself returned through.
	var releaseSched func()
	schedInfo := info
	schedInfo.OnSlot = onSlot
	if bounded {
		releaseSched, err = sched.AcquireBounded(ctx, id, kind, schedInfo, bound)
	} else {
		releaseSched, err = sched.Acquire(ctx, id, kind, schedInfo)
	}
	if err != nil {
		releaseFloor()
		if concurrency.IsQueueFull(err) {
			return nil, &concurrency.ErrProviderConcurrencyExhausted{Provider: id, Cap: sched.LaneCap(id, kind)}
		}
		return nil, err
	}
	if floor == nil {
		return releaseSched, nil
	}
	// Both are idempotent, so the pair is too.
	return func() { releaseSched(); releaseFloor() }, nil
}

// runReporter is where a model call made on a run's behalf off the loop's own
// call path reports its wait: the run's event sink, which the run stamps on
// every ctx its tools work under (providers.WithEventSink). Nil outside a run,
// and nil for background work: a consolidation pass or an embedding fill
// started from a run's ctx is not that run's work, and may outlive it.
func runReporter(ctx context.Context) func(providers.Event) {
	if IsBackground(ctx) {
		return nil
	}
	return providers.EventSink(ctx)
}

// FloorCap is the built-in bound on a service's calls in flight when the
// operator has configured no lane for them: decisions per provider, and reranks.
//
// These services had a private limit of this size before their calls moved to
// lanes. A rerank is a prompt of several thousand tokens and they arrive per
// SEARCH; a decision is asked per item being routed or judged. A fan-out of
// agents sends dozens at once, and with no lane configured nothing else would
// stop every one from reaching the provider together. A lane the operator
// configures for that provider and kind replaces the floor entirely.
const FloorCap = 4

// newFloor builds the built-in bound for one service.
func newFloor() *concurrency.Lane {
	return concurrency.NewLane(concurrency.LaneConfig{MaxConcurrent: FloorCap})
}

// forward relays the driver's events to the consumer and releases the slot when
// the call is over. release is idempotent.
//
// The slot goes back BEFORE the event that ends the call is handed on, not when
// the channel closes: a consumer that stops reading after an error (several
// do, and a driver may still send a trailing event) would otherwise pin the
// slot until its ctx ended — and on a cap of 1 its own next call would then
// wait behind itself. It also means that whoever sees the call end sees the
// slot free.
//
// When ctx ends the consumer may be gone, so nothing more is sent; the driver's
// channel is drained until it closes (a driver closes on ctx cancel) and only
// then is the slot released — the host is busy with the call until the driver
// has let go of it.
func forward(ctx context.Context, in <-chan providers.Event, out chan<- providers.Event, release func()) {
	defer close(out)
	defer release()
	for ev := range in {
		if ev.Type == providers.EventDone || ev.Type == providers.EventError {
			release()
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			for range in {
			}
			return
		}
	}
}

// SchedulerConfig turns the providers: block into the call scheduler's config:
// each provider's gated lanes (the max_concurrent shorthand folded into the
// chat lane) and its cap on models in flight. The one translation, used at boot
// and on every config reload, so the two cannot build different lanes from the
// same config.
func SchedulerConfig(provs map[string]config.ProviderConfig) map[string]concurrency.ProviderLanesConfig {
	out := make(map[string]concurrency.ProviderLanesConfig, len(provs))
	for id, pc := range provs {
		lanes := pc.EffectiveLanes()
		if len(lanes) == 0 && pc.MaxModelsInFlight <= 0 {
			continue
		}
		plc := concurrency.ProviderLanesConfig{
			Lanes:              make(map[string]concurrency.LaneConfig, len(lanes)),
			MaxModelsInFlight:  pc.MaxModelsInFlight,
			ModelSwitchMaxWait: time.Duration(pc.ModelSwitchMaxWaitMSOrDefault()) * time.Millisecond,
			ExemptKinds:        pc.ModelsInFlightExemptKinds,
		}
		for k, l := range lanes {
			plc.Lanes[k] = concurrency.LaneConfig{
				MaxConcurrent:   l.MaxConcurrent,
				QuantumCalls:    l.QuantumCalls,
				AffinityHold:    time.Duration(l.AffinityHoldMS) * time.Millisecond,
				BackgroundShare: l.BackgroundShare,
			}
		}
		out[id] = plc
	}
	return out
}
