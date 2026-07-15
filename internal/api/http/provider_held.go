// provider_held.go — RFC BF P2c ancestor-held-provider set.
//
// P2b gates a TOP-LEVEL run on its resolved provider but deliberately leaves
// sub-agents ungated, to avoid a self-deadlock: a fan-out parent holding
// provider P's slot could otherwise queue behind its own children that also want
// P. P2c gates EVERY run — top-level and sub-agent — on its resolved provider,
// EXCEPT it skips the gate for a provider an ANCESTOR in the same run tree
// already holds (the deadlock carve-out).
//
// The mechanism is a ctx-carried set of provider ids whose gate slots are held
// by ancestors. A run stamps its own provider onto the set (via
// Server.heldSlotCtx) exactly when it takes a REAL capped slot that persists for
// the loop it guards; its descendants read the set (via holdsProviderSlot) and
// take the carve-out when their provider is already held above them.
package http

import "context"

// heldProviderSlotsKey is the unexported ctx key for the ancestor-held provider
// set. A dedicated unexported type keeps the key private to this package per the
// Go context convention (no cross-package collision or read).
type heldProviderSlotsKey struct{}

// withHeldProviderSlot returns a ctx whose ancestor-held set is the parent's set
// PLUS providerID.
//
// COPY-ON-ADD is load-bearing: parallel_spawn runs its children concurrently off
// the SAME parent ctx, and each child augments that ctx with its OWN resolved
// provider. Mutating a shared map here would race the sibling goroutines and
// cross-contaminate their carve-out decisions, so every add allocates a fresh
// superset the caller owns exclusively.
func withHeldProviderSlot(ctx context.Context, providerID string) context.Context {
	prev, _ := ctx.Value(heldProviderSlotsKey{}).(map[string]struct{})
	next := make(map[string]struct{}, len(prev)+1)
	for id := range prev {
		next[id] = struct{}{}
	}
	next[providerID] = struct{}{}
	return context.WithValue(ctx, heldProviderSlotsKey{}, next)
}

// holdsProviderSlot reports whether an ancestor in this run tree already holds
// providerID's gate slot. When true the current run must NOT acquire that gate
// again — the ancestor would await its own descendant queued behind the same cap
// (a self-deadlock), so the descendant runs UNGATED (the P2c carve-out).
func holdsProviderSlot(ctx context.Context, providerID string) bool {
	held, _ := ctx.Value(heldProviderSlotsKey{}).(map[string]struct{})
	_, ok := held[providerID]
	return ok
}
