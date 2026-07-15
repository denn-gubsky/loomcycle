// provider_gate.go — RFC BF P2b run-admission wiring for the per-provider
// concurrency gate (internal/concurrency.ProviderGates).
//
// Admission ORDER is load-bearing: acquire the per-provider slot BEFORE the
// global execution slot. A run blocked on a full per-provider cap must NOT
// already hold a global slot, otherwise a capped provider's overflow would
// starve runs targeting OTHER (uncapped) providers of global concurrency. An
// uncapped provider acquires a noop and goes straight to the global slot — zero
// overhead, no starvation. See the acquire sites in server.go (RunOnce /
// handleRuns / handleMessages), resume.go, and llm_gateway.go.
package http

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// providerFallbackAcquireTimeout bounds how long a mid-run provider fallback
// waits to acquire the NEW provider's gate slot. Short on purpose: a mid-run
// block waiting on a saturated downstream cap is worse than briefly exceeding
// that cap, so on timeout the run proceeds UNCAPPED for the swapped provider
// (see providerSlot.swap). Long enough to admit past a brief burst.
const providerFallbackAcquireTimeout = 2 * time.Second

// providerSlot is the per-provider concurrency slot a single run currently
// occupies (RFC BF P2b). The initial slot is acquired at admission; the
// fallbackForRun reResolve closure calls swap() to move the slot when the run
// fails over to another provider, so the gate counters track the provider
// actually in use.
//
// Single-goroutine per run: acquisition, every swap (driven by loop.Run's
// synchronous ReResolve callback), and the deferred release all execute in the
// run's own goroutine, so no mutex is needed. The release is idempotent
// (Semaphore.Acquire wraps its release in sync.Once, and noop is trivially
// idempotent), so a defer + a swap that already released the old slot is safe.
type providerSlot struct {
	release    func()
	providerID string
	// noop marks a P2c deadlock carve-out holder: an ANCESTOR in this run tree
	// already holds providerID's gate slot, so this run runs UNGATED (acquiring
	// again would make the ancestor await its own descendant behind the same cap).
	// A noop holder never acquired a gate — swap() and releaseCurrent() are inert
	// on it, and it deliberately stays ungated even across a mid-run fallback.
	noop bool
}

// releaseCurrent releases whatever provider slot the holder currently owns.
// Deferred at each admission site; safe on a nil holder or before population
// (resume.go creates the holder empty and fills it inside its goroutine).
func (ps *providerSlot) releaseCurrent() {
	if ps == nil || ps.release == nil {
		return
	}
	ps.release()
}

// swap releases the current provider's gate slot and acquires newProviderID's,
// under a SHORT bounded timeout. On timeout/backpressure it proceeds UNCAPPED (a
// noop release) and logs a WARN — never blocks the mid-run fallback nor fails it
// (RFC BF P2b Deliverable 4). No-op when the holder is nil (sub-agent /
// interactive-detached runs, which never own a swappable slot) or when the
// resolver returned the same provider (a same-provider/different-model retry
// keeps its slot — releasing then re-acquiring could hand our only slot to a
// competitor). A P2c carve-out holder (ps.noop) also does NOT acquire the
// fallback target's gate: that run is deliberately ungated because an ancestor
// holds its original provider, and it must stay ungated across a fallback —
// acquiring here could reintroduce the parent-awaiting-its-own-descendant
// deadlock the carve-out exists to prevent (the run's ctx held-set is fixed at
// admission, so a slot taken now can't be published to the descendants that
// would then queue behind it). The cost is a bounded cap-escape: if the fallback
// target is itself capped and NOT ancestor-held, this run runs ungated on it and
// may briefly exceed that provider's max_concurrent. That's logged (below) so the
// over-subscription is visible rather than silent (RFC BF review finding); true
// enforcement across the carve-out+fallback boundary would need a mutable,
// subtree-scoped held-set, deferred as a larger change.
func (ps *providerSlot) swap(ctx context.Context, gates *concurrency.ProviderGates, newProviderID string) {
	if ps == nil || newProviderID == ps.providerID {
		return
	}
	if ps.noop {
		// Carve-out holder stays ungated across the fallback (deadlock-avoidance,
		// see the doc comment). Surface the escape when the target is a cap this
		// run now evades and no ancestor holds it — a same-provider or
		// ancestor-held target is not an escape (the cap is already respected).
		if gates.Has(newProviderID) && !holdsProviderSlot(ctx, newProviderID) {
			log.Printf("provider-gate: carve-out run failed over %s->%s and stays UNGATED on capped %s (deadlock-avoidance); its max_concurrent may be briefly exceeded",
				ps.providerID, newProviderID, newProviderID)
		}
		ps.providerID = newProviderID
		return
	}
	// Free the old provider's slot FIRST so a run queued on it can proceed; this
	// also means we never hold two provider slots at once (no cross-gate
	// deadlock).
	ps.releaseCurrent()

	swapCtx, cancel := context.WithTimeout(ctx, providerFallbackAcquireTimeout)
	defer cancel()
	rel, err := gates.Acquire(swapCtx, newProviderID)
	if err != nil {
		log.Printf("provider-gate: fallback %s->%s could not acquire the new slot within %s (%v); proceeding UNCAPPED for %s",
			ps.providerID, newProviderID, providerFallbackAcquireTimeout, err, newProviderID)
		ps.release = func() {}
		ps.providerID = newProviderID
		return
	}
	ps.release = rel
	ps.providerID = newProviderID
}

// acquireProviderSlot reserves the per-provider concurrency slot for providerID
// (RFC BF P2b/P2c). Returns a populated holder on success; on a saturated cap it
// returns the raw *concurrency.ErrProviderConcurrencyExhausted so HTTP handlers
// can hand it to writeQuotaError and RunOnce can reclassify via
// providerAcquireErrToRunner. Uncapped providers get a noop holder with zero
// overhead. Keeps the acquire identical across the admission sites.
//
// P2c deadlock carve-out: when an ANCESTOR in this run tree already holds
// providerID's gate slot (holdsProviderSlot), this run runs UNGATED — a noop
// holder, no gate touched. Gating it would let a fan-out parent await its own
// descendant queued behind the same cap (a self-deadlock). At the top-level
// admission sites the held-set is always empty, so the carve-out never fires and
// behavior is byte-identical to P2b; it fires only for sub-agents whose provider
// their parent already holds.
//
// A run that DOES take a real slot must run its protected work under
// heldSlotCtx(ctx, slot) so descendants see providerID as held (that stamp is a
// separate call, not folded in here, so the interactive-detach path — which
// releases its slot at hand-off — can opt out).
func (s *Server) acquireProviderSlot(ctx context.Context, providerID string) (*providerSlot, error) {
	if holdsProviderSlot(ctx, providerID) {
		return &providerSlot{noop: true}, nil
	}
	release, err := s.providerGates.Acquire(ctx, providerID)
	if err != nil {
		return nil, err
	}
	return &providerSlot{release: release, providerID: providerID}, nil
}

// heldSlotCtx augments ctx with slot's provider in the ancestor-held set (P2c)
// so descendants spawned under the returned ctx take the deadlock carve-out for
// that provider. It stamps ONLY when the slot is a real CAPPED holder that
// persists for the loop it guards:
//   - a nil / carve-out (noop) / empty holder holds no fresh gate → ctx unchanged;
//   - an UNCAPPED provider has no gate at all → nothing for a descendant to skip,
//     so ctx is returned unchanged, preserving P2b's zero-overhead-when-uncapped.
//
// Callers wrap the ctx passed to loop.Run with this. A run whose slot is released
// before its loop runs (the interactive-detached top-level run) deliberately does
// NOT call this — its descendants gate normally because nothing is held.
func (s *Server) heldSlotCtx(ctx context.Context, slot *providerSlot) context.Context {
	if slot == nil || slot.noop || slot.providerID == "" {
		return ctx
	}
	if !s.providerGates.Has(slot.providerID) {
		return ctx
	}
	return withHeldProviderSlot(ctx, slot.providerID)
}

// providerAcquireErrToRunner maps a provider-gate acquire failure to the runner
// sentinel vocabulary so RunOnce-driven surfaces (gRPC / MCP / connector /
// scheduler / webhook) classify it uniformly. A saturated cap → 429/Resource
// Exhausted; anything else (ctx cancel while queued) → ErrInternal, mirroring
// how RunOnce already treats a non-backpressure global-slot error.
func providerAcquireErrToRunner(err error) error {
	if concurrency.IsProviderConcurrencyExhausted(err) {
		return fmt.Errorf("%w: %v", runner.ErrProviderConcurrencyExhausted, err)
	}
	return fmt.Errorf("%w: %v", runner.ErrInternal, err)
}
