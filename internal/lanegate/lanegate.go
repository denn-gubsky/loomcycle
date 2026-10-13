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
// embedder) do not go through Provider.Call.
const kind = config.ModelKindChat

type backgroundKey struct{}

// WithBackground marks the calls made under ctx as maintenance work
// (consolidation, reindexing). A lane serves them after live runs, with the
// share its background_share guarantees.
func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundKey{}, true)
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
}

// Wrap returns inner gated by sched under the provider id it is registered as
// (the `providers:` key — not inner.ID(), which is the driver's own name for
// itself). The gate is looked up per call, so a cap added or removed by a
// config reload applies to the next call of a provider wrapped before it.
// A nil sched gates nothing.
func Wrap(id string, inner providers.Provider, sched *concurrency.CallScheduler) *Provider {
	return &Provider{inner: inner, id: id, sched: sched}
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
func (p *Provider) Call(ctx context.Context, req providers.Request) (<-chan providers.Event, error) {
	if !p.sched.Gated(p.id, kind) {
		return p.inner.Call(ctx, req)
	}
	release, err := p.acquire(ctx, req)
	if err != nil {
		return nil, err
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
	release, err := lp.acquire(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	return lp.inner, release, nil
}

// acquire takes the lane slot for one call.
func (p *Provider) acquire(ctx context.Context, req providers.Request) (release func(), err error) {
	meta, _ := providers.RunMetaFromContext(ctx)
	background, _ := ctx.Value(backgroundKey{}).(bool)
	info := concurrency.CallInfo{
		Owner:      meta.RunID,
		TenantID:   meta.TenantID,
		UserID:     meta.UserID,
		Background: background,
		Model:      providers.ModelIdentity(p.inner, req),
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
	if !bounded {
		return p.sched.Acquire(ctx, p.id, kind, info)
	}
	release, err = p.sched.AcquireBounded(ctx, p.id, kind, info, bound)
	if concurrency.IsQueueFull(err) {
		return nil, &concurrency.ErrProviderConcurrencyExhausted{Provider: p.id, Cap: p.sched.LaneCap(p.id, kind)}
	}
	return release, err
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
