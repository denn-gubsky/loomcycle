package loop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// codeAgentWaitingProvider is waitingProvider with a code-js provider's shape:
// unbounded iterations and a lifetime limit of its own.
type codeAgentWaitingProvider struct {
	waitingProvider
	wall time.Duration
}

func (p *codeAgentWaitingProvider) Capabilities() providers.Capabilities {
	return providers.Capabilities{Streaming: true, UnboundedIterations: true, RunWallLimit: p.wall}
}

// A run the loop's own wall-limit watcher cut mid-turn reports stop reason
// wall_limit, as a run the registry's wall-limit cancel ended does. It used
// to report "cancelled" — the mid-turn cancel exit's — on a run that is
// recorded failed.
func TestRun_WallLimitCutMidTurnReportsWallLimit(t *testing.T) {
	cases := map[string]RunOptions{
		"max_wall_seconds, no canceller": wallOpts(&waitingProvider{}, 1),
		"code agent lifetime limit":      wallOpts(&codeAgentWaitingProvider{wall: 300 * time.Millisecond}, 0),
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			res, err := Run(context.Background(), opts)
			if !errors.Is(err, errWallLimit) {
				t.Fatalf("err = %v, want the wall-limit failure", err)
			}
			if res.StopReason != StopReasonWallLimit {
				t.Errorf("stop_reason = %q, want %q", res.StopReason, StopReasonWallLimit)
			}
		})
	}
}
