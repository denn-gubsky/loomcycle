package mcp

import (
	"context"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RunDynamicAgentSweeper starts a periodic goroutine that deletes
// expired rows from the dynamic_agents table. Designed to be wired
// from cmd/loomcycle/main.go alongside other sweepers (memory,
// channel, metrics).
//
// interval=0 disables the sweeper (the operator may prefer to manage
// retention out-of-band, or operate at low registration volume where
// expired rows have negligible cost). DynamicAgentGet always filters
// expired rows at read time, so functional correctness is preserved
// either way — disabling only forgoes the storage-reclamation.
//
// Returns immediately after starting the goroutine. The goroutine
// exits when ctx is done.
func RunDynamicAgentSweeper(ctx context.Context, st store.Store, interval time.Duration, logf func(string, ...any)) {
	if interval <= 0 || st == nil {
		return
	}
	if logf == nil {
		logf = defaultLogf
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n, err := st.DynamicAgentSweep(ctx)
				if err != nil {
					logf("dynamic_agents sweep: %v", err)
					continue
				}
				if n > 0 {
					logf("dynamic_agents: swept %d expired", n)
				}
			}
		}
	}()
}
