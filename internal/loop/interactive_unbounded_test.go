package loop

import (
	"context"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// An interactive run with the DEFAULT (unset) MaxIterations must not stop at the
// 16-turn soft cap. It's operator-driven and Cancel-bounded, and each end_turn
// park + operator turn consumes a loop iteration — so capping at 16 silently
// ends a live terminal session after 16 turns (the reported bug). This drives
// more than 16 operator turns and asserts the run is still parked (not
// terminated). Fails on the pre-fix loop, where iterCap defaulted to 16 for
// interactive runs too and the run terminated around turn 16.
func TestRun_Interactive_DefaultIterationsAreUnbounded(t *testing.T) {
	const turns = 20 // > the 16 default soft cap

	q := make(chan steer.Message, turns+2)
	parked := make(chan struct{}, turns+4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prov := &endTurnProvider{}
	done := make(chan struct{})
	go func() {
		_, _ = Run(ctx, RunOptions{
			Provider:    prov,
			Model:       "x",
			Tools:       []tools.Tool{noopTool{}},
			Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
			Segments:    steerSegs(),
			SteerQueue:  q,
			Interactive: true,
			// MaxIterations unset (0): a non-interactive run would default to 16;
			// an interactive run must instead be unbounded.
			OnEvent: func(ev providers.Event) {
				if ev.Type == providers.EventAwaitingInput {
					parked <- struct{}{}
				}
			},
		})
		close(done)
	}()

	waitPark := func() {
		t.Helper()
		select {
		case <-parked:
		case <-done:
			t.Fatal("interactive run terminated instead of parking — default iterations still capped at 16?")
		case <-time.After(3 * time.Second):
			t.Fatal("interactive run did not park within 3s")
		}
	}

	waitPark() // initial end_turn → park
	for i := 0; i < turns; i++ {
		q <- steer.Message{Text: "keep going"}
		waitPark()
	}

	// Past 16 turns and still parked → the soft cap was lifted for the
	// interactive run.
	select {
	case <-done:
		t.Fatal("interactive run terminated before Cancel — default iterations capped at 16")
	default:
	}
	if got := prov.calls(); got != turns+1 {
		t.Errorf("provider calls = %d, want %d (1 initial + %d operator turns)", got, turns+1, turns)
	}

	// Cancel still ends the persistent run cleanly (the hard ceiling never bites
	// at this scale; Cancel is the real terminator).
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("interactive run did not terminate after Cancel")
	}
}
