package http

import (
	"context"

	"github.com/denn-gubsky/loomcycle/internal/awaited"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

// fillAwaitedStateForRunning enriches a slice of agentResponse with
// the awaited_state / awaited_on fields (see awaited.ForRun) for each
// running entry. Non-running rows are skipped.
func fillAwaitedStateForRunning(ctx context.Context, st store.Store, items []agentResponse) {
	for i, item := range items {
		if item.Status != store.RunRunning {
			continue
		}
		if state, on := awaited.ForRun(ctx, st, item.RunID); state != "" {
			items[i].AwaitedState = state
			items[i].AwaitedOn = on
		}
	}
}
