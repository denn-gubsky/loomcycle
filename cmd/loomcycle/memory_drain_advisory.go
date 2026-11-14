package main

import (
	"context"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/config"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// pendingTargets is the one store call the undrained-memory check makes.
type pendingTargets interface {
	MemoryPendingTargetsAllTenants(ctx context.Context, scope store.MemoryScope, limit int) ([]store.MemoryPendingTarget, error)
}

// undrainedMemoryWarnings returns the advisory of each memory scope that no
// enabled schedule drains AND that has rows waiting in its queue now.
//
// Config can only say that an agent could enqueue; every agent holding the
// Memory tool can. Whether that is a problem is a fact about the store: a
// queue with rows in it and nothing to drain them is memory being lost, an
// empty one is a deployment that has not used the feature. So the advisory
// is shown from here, at boot, for the scopes where it is true.
//
// A store that cannot answer is not an empty queue: the advisory is shown,
// with the fault.
func undrainedMemoryWarnings(ctx context.Context, st pendingTargets, scopes []config.UndrainedMemoryScope) []string {
	var out []string
	for _, s := range scopes {
		targets, err := st.MemoryPendingTargetsAllTenants(ctx, store.MemoryScope(s.Scope), 1)
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("%s (whether anything is queued could not be read: %v)", s.Advisory, err))
		case len(targets) > 0:
			out = append(out, s.Advisory+" Items are waiting in this queue now.")
		}
	}
	return out
}
