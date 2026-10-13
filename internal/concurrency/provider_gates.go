// provider_gates.go — RFC BF P2b per-provider concurrency gate.
//
// A ProviderGates holds one counting Semaphore PER provider id. A provider whose
// config sets `max_concurrent > 0` gets a gate that caps in-flight runs to that
// provider (the rest queue in loomcycle); a provider with max_concurrent 0/unset
// gets NO gate and Acquire/Wait are a zero-alloc noop — the common, uncapped
// case. This is the per-provider analogue of the per-user cap (WithPerUserCap):
// it deliberately reuses the same queue/backpressure machinery rather than
// reinventing it.
//
// A gate never times a waiter out. Acquire (a NEW top-level request) is refused
// only when the gate's queue is already full; Wait (work already admitted — a
// sub-agent, a resumed run, a mid-run fallback) is never refused. Cancelling
// admitted work because a queue was slow is never the right answer.
//
// Admission acquires the provider gate BEFORE the global execution slot (see the
// ordering rationale in internal/api/http RunOnce): a run blocked on a full
// per-provider cap must not already hold a global slot, or a saturated provider's
// overflow would starve runs targeting OTHER (uncapped) providers.
package concurrency

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// noopRelease is the shared release func returned for uncapped providers. A
// single package-level value avoids an allocation on the hot (uncapped) path.
var noopRelease = func() {}

// ErrProviderConcurrencyExhausted signals a provider's per-provider concurrency
// cap (max_concurrent) rejected a new run — that provider's queue was full.
// Distinct from BackpressureError (global queue) and
// ErrPerUserQuotaExhausted (per-user cap) because the retry strategy differs: the
// run targets a specific saturated provider, so a caller could retry a different
// tier/provider immediately rather than backing off operator-wide.
//
// Callers should surface HTTP 429 + `Retry-After: 5` with
// `code: "provider_concurrency_exhausted"` / gRPC ResourceExhausted.
type ErrProviderConcurrencyExhausted struct {
	Provider string
	Cap      int
}

func (e *ErrProviderConcurrencyExhausted) Error() string {
	return fmt.Sprintf("provider concurrency exhausted: provider=%s cap=%d", e.Provider, e.Cap)
}

// Code is the typed identifier used in the HTTP error envelope so adapter
// consumers can branch retry strategies on the wire shape.
func (e *ErrProviderConcurrencyExhausted) Code() string { return "provider_concurrency_exhausted" }

// IsProviderConcurrencyExhausted reports whether err is an
// *ErrProviderConcurrencyExhausted. Mirrors IsBackpressure /
// IsPerUserQuotaExhausted.
func IsProviderConcurrencyExhausted(err error) bool {
	var e *ErrProviderConcurrencyExhausted
	return errors.As(err, &e)
}

// ProviderGates maps a provider id → its concurrency gate. Only providers with a
// positive cap have an entry; everything else is uncapped and Acquire short-
// circuits to a noop. The byID map is guarded by mu so Reload (RFC CK config
// reload) can swap it while admissions read it; the underlying Semaphores carry
// their own locking.
type ProviderGates struct {
	mu   sync.RWMutex
	byID map[string]*Semaphore
}

// NewProviderGates builds one gate per provider id whose cap is > 0. queueDepth
// is shared across all gates (the RFC BF P2b LOOMCYCLE_PROVIDER_QUEUE_DEPTH
// knob) and bounds only Acquire. A caps map with no positive entry (the common
// case — no operator set max_concurrent) yields an empty ProviderGates whose
// Acquire is always a noop: zero gates built, zero overhead on the admission
// path.
func NewProviderGates(caps map[string]int, queueDepth int) *ProviderGates {
	return &ProviderGates{byID: buildGates(caps, queueDepth)}
}

// buildGates makes one gate per positive cap, each with NO wait timeout.
func buildGates(caps map[string]int, queueDepth int) map[string]*Semaphore {
	out := make(map[string]*Semaphore, len(caps))
	for id, c := range caps {
		if c > 0 {
			// Per-user cap intentionally left off: this gate counts the GLOBAL
			// in-flight runs to the provider, not per-user — the whole point is
			// bounding total load against a shared resource (e.g. one GPU).
			out[id] = New(c, queueDepth, 0)
		}
	}
	return out
}

// gate returns provider id's gate, or nil when id is uncapped (or g is nil).
func (g *ProviderGates) gate(id string) *Semaphore {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.byID[id]
}

// Acquire reserves a slot for provider id for a NEW top-level request. If no
// gate exists for id (uncapped — the common case), returns the shared noop
// release + nil with zero contention. If gated, it queues with no timeout; a
// full queue maps to *ErrProviderConcurrencyExhausted and a ctx cancel returns
// ctx.Err(). Nil-safe: a nil *ProviderGates is always a noop, so a Server with
// no gates wired keeps today's behaviour. Work that was already admitted uses
// Wait instead.
//
// Call the returned release exactly once when the run's use of the provider
// ends. The Semaphore's release is idempotent (sync.Once), so a double call —
// e.g. defer + a fallback slot-swap — is safe.
func (g *ProviderGates) Acquire(ctx context.Context, id string) (release func(), err error) {
	sem := g.gate(id)
	if sem == nil {
		return noopRelease, nil
	}
	// Empty userID → the per-user path is skipped; this counts the provider's
	// GLOBAL in-flight total, which is exactly the semantics we want.
	rel, aerr := sem.Acquire(ctx)
	if aerr != nil {
		// Queue full → the provider's cap is saturated. Reclassify to the
		// provider-typed error so wire surfaces emit the right 429 code; ctx
		// cancel and any other error pass through unchanged.
		if IsBackpressure(aerr) {
			return nil, &ErrProviderConcurrencyExhausted{Provider: id, Cap: sem.maxConcurrent}
		}
		return nil, aerr
	}
	return rel, nil
}

// Wait reserves a slot for provider id for work that was already admitted (a
// sub-agent, a resumed run, a mid-run fallback). It is never refused: no queue
// depth, no timeout — it returns when a slot frees, FIFO with every other
// waiter, or with ctx.Err() when ctx ends first. Uncapped (or a nil g) is the
// shared noop. Release exactly once (idempotent).
func (g *ProviderGates) Wait(ctx context.Context, id string) (release func(), err error) {
	sem := g.gate(id)
	if sem == nil {
		return noopRelease, nil
	}
	return sem.Wait(ctx)
}

// Has reports whether provider id is gated (has a positive cap). Used by the
// admission fast-path and tests asserting the zero-overhead-when-unconfigured
// guarantee.
func (g *ProviderGates) Has(id string) bool {
	return g.gate(id) != nil
}

// Len returns the number of gated providers. Zero means fully uncapped.
func (g *ProviderGates) Len() int {
	if g == nil {
		return 0
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.byID)
}

// Reload rebuilds the per-provider gates in place from a new caps map (RFC CK
// config reload), swapping byID under the write lock so a changed / added /
// removed provider max_concurrent takes effect on the next admission. In-flight
// runs hold a release closure over their ORIGINAL Semaphore, so they drain on the
// old gate while new admissions use the new one — no count corruption, no leak.
// queueDepth may change. Nil-safe (a Server with no gates never reloads them —
// the caller guards).
func (g *ProviderGates) Reload(caps map[string]int, queueDepth int) {
	if g == nil {
		return
	}
	next := buildGates(caps, queueDepth)
	g.mu.Lock()
	g.byID = next
	g.mu.Unlock()
}

// Stats returns a per-provider snapshot (active + queued) for every gated
// provider, keyed by provider id. Nil when no gates are configured. Feeds the
// loomcycle_provider_slots_in_use / _queue_depth Prometheus gauges + the
// /v1/_concurrency/stats endpoint. The per-gate PerUser field is always nil
// (this gate never does per-user accounting), so callers ignore it.
func (g *ProviderGates) Stats() map[string]Stats {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if len(g.byID) == 0 {
		return nil
	}
	out := make(map[string]Stats, len(g.byID))
	for id, sem := range g.byID {
		out[id] = sem.Stats()
	}
	return out
}
