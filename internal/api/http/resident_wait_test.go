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

// A parent whose wait on its child is cancelled closes the wait: one left open
// would stop its budget for good.
func TestResidentAwaitTurn_CancelledWaitIsClosed(t *testing.T) {
	rc := &residentChild{}
	turnDone := rc.beginTurn(time.Now())
	clock := providers.NewRunClock(time.Now(), providers.RunClockState{})
	ctx, cancel := context.WithTimeout(providers.WithRunClock(context.Background(), clock), 100*time.Millisecond)
	defer cancel()
	if _, state, _ := rc.awaitTurn(ctx, turnDone, 0, true); state != "interrupted" {
		t.Fatalf("state = %q, want interrupted", state)
	}
	before := clock.State().Waited
	time.Sleep(50 * time.Millisecond)
	if after := clock.State().Waited; after != before {
		t.Fatalf("the wait was left open after awaitTurn returned (%s → %s)", before, after)
	}
}
