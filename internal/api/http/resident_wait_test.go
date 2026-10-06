package http

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// A parent waiting on its resident child's turn — open/send blocking until the
// child parks, the same bounded by timeout_ms, or poll waiting with a timeout
// — is parked: a code agent's budget does not run while it waits.
func TestResidentAwaitTurn_ParksTheCallersClockWhileTheChildWorks(t *testing.T) {
	const childTakes = 400 * time.Millisecond
	for _, tc := range []struct {
		name          string
		timeout       time.Duration
		blockWhenZero bool
	}{
		{"send blocks until the child parks", 0, true},
		{"send bounded by timeout_ms", 5 * time.Second, true},
		{"poll waits with timeout_ms", 5 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &residentChild{}
			turnDone := rc.beginTurn(time.Now())
			go func() {
				time.Sleep(childTakes)
				rc.endTurn("awaiting_input")
			}()
			clock := providers.NewRunClock(time.Now(), providers.RunClockState{})
			ctx := providers.WithRunClock(context.Background(), clock)
			if _, state, err := rc.awaitTurn(ctx, turnDone, tc.timeout, tc.blockWhenZero); err != nil || state != "awaiting_input" {
				t.Fatalf("awaitTurn = %q, %v; want the child parked", state, err)
			}
			if got := clock.State().Waited; got < childTakes*3/4 {
				t.Fatalf("the parent's clock counted %s as waited, want about %s", got, childTakes)
			}
		})
	}
}
