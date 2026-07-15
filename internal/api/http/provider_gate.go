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
// competitor).
func (ps *providerSlot) swap(ctx context.Context, gates *concurrency.ProviderGates, newProviderID string) {
	if ps == nil || newProviderID == ps.providerID {
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
// (RFC BF P2b). Returns a populated holder on success; on a saturated cap it
// returns the raw *concurrency.ErrProviderConcurrencyExhausted so HTTP handlers
// can hand it to writeQuotaError and RunOnce can reclassify via
// providerAcquireErrToRunner. Uncapped providers get a noop holder with zero
// overhead. Keeps the acquire identical across the admission sites.
func (s *Server) acquireProviderSlot(ctx context.Context, providerID string) (*providerSlot, error) {
	release, err := s.providerGates.Acquire(ctx, providerID)
	if err != nil {
		return nil, err
	}
	return &providerSlot{release: release, providerID: providerID}, nil
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
