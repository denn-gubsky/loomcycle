package loop

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// idleRun runs an interactive append-loop run in the background, reporting
// each park and its terminal done.
type idleRun struct {
	prov   *steerProvider
	parked chan struct{}
	done   chan struct{}
	res    RunResult
	err    error

	mu       sync.Mutex
	doneStop []string // stop reason of each done event
}

func startIdleRun(t *testing.T, ctx context.Context, q chan steer.Message, extra func(*RunOptions)) *idleRun {
	t.Helper()
	r := &idleRun{prov: &steerProvider{}, parked: make(chan struct{}, 8), done: make(chan struct{})}
	opts := RunOptions{
		Provider:    r.prov,
		Model:       "x",
		Tools:       []tools.Tool{noopTool{}},
		Dispatcher:  tools.NewDispatcher([]tools.Tool{noopTool{}}),
		Segments:    steerSegs(),
		SteerQueue:  q,
		Interactive: true,
		OnEvent: func(ev providers.Event) {
			switch ev.Type {
			case providers.EventAwaitingInput:
				r.parked <- struct{}{}
			case providers.EventDone:
				r.mu.Lock()
				r.doneStop = append(r.doneStop, ev.StopReason)
				r.mu.Unlock()
			}
		},
	}
	if extra != nil {
		extra(&opts)
	}
	go func() {
		defer close(r.done)
		r.res, r.err = Run(ctx, opts)
	}()
	return r
}

func (r *idleRun) waitPark(t *testing.T, what string) {
	t.Helper()
	select {
	case <-r.parked:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: the run did not park", what)
	}
}

func (r *idleRun) waitEnd(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(within):
		t.Fatalf("the run did not end within %v", within)
	}
}

// requests returns the provider calls made so far.
func (r *idleRun) requests() [][]providers.Message {
	r.prov.mu.Lock()
	defer r.prov.mu.Unlock()
	return append([][]providers.Message(nil), r.prov.requests...)
}

// A parked interactive run that gets no operator turn within IdleTimeout ends:
// on the answer it had given, with its own stop reason, as a run that finished
// rather than one that failed or was cancelled. Its queue is closed, so a
// message sent after the end is refused rather than dropped.
func TestRun_Interactive_IdleTimeoutEndsAParkedRun(t *testing.T) {
	q := make(chan steer.Message, 4)
	var closed atomic.Bool
	r := startIdleRun(t, context.Background(), q, func(o *RunOptions) {
		o.IdleTimeout = 50 * time.Millisecond
		o.CloseSteerIfEmpty = func() bool {
			if len(q) > 0 {
				return false
			}
			closed.Store(true)
			return true
		}
	})
	r.waitPark(t, "first turn")
	r.waitEnd(t, 3*time.Second)

	if r.err != nil {
		t.Fatalf("Run returned %v: an idle end is not a failure", r.err)
	}
	if r.res.StopReason != StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q", r.res.StopReason, StopReasonIdleTimeout)
	}
	if r.res.FinalText != "ok" {
		t.Errorf("final text = %q, want the answer the run had given", r.res.FinalText)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.doneStop) != 1 || r.doneStop[0] != StopReasonIdleTimeout {
		t.Errorf("done events = %v, want one with stop reason %q", r.doneStop, StopReasonIdleTimeout)
	}
	if !closed.Load() {
		t.Error("the run ended without closing its queue: a message sent after the end would be accepted and dropped")
	}
}

// A message that lands as the idle deadline passes is taken: the close finds
// it waiting, and the run answers it and parks again, with a full window.
func TestRun_Interactive_AMessageThatRacesTheIdleDeadlineIsTaken(t *testing.T) {
	q := make(chan steer.Message, 4)
	var closes atomic.Int32
	r := startIdleRun(t, context.Background(), q, func(o *RunOptions) {
		o.IdleTimeout = 50 * time.Millisecond
		o.CloseSteerIfEmpty = func() bool {
			if closes.Add(1) == 1 {
				// Pushed after the deadline fired, before the queue closed.
				q <- steer.Message{Text: "just in time"}
			}
			return len(q) == 0
		}
	})
	r.waitPark(t, "first turn")
	r.waitPark(t, "the raced message's turn")
	r.waitEnd(t, 3*time.Second)

	reqs := r.requests()
	if len(reqs) != 2 {
		t.Fatalf("provider calls = %d, want 2: the raced message was not answered", len(reqs))
	}
	if userTextIdx(reqs[1], "just in time") < 0 {
		t.Errorf("the second call does not carry the raced message: %+v", reqs[1])
	}
	if r.res.StopReason != StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q after the second park idled", r.res.StopReason, StopReasonIdleTimeout)
	}
}

// An operator turn before the deadline keeps the run alive, and the next park
// gets the full window again.
func TestRun_Interactive_AnOperatorTurnBeforeTheIdleDeadlineKeepsTheRun(t *testing.T) {
	q := make(chan steer.Message, 4)
	const idle = 300 * time.Millisecond
	r := startIdleRun(t, context.Background(), q, func(o *RunOptions) {
		o.IdleTimeout = idle
		o.CloseSteerIfEmpty = func() bool { return len(q) == 0 }
	})
	r.waitPark(t, "first turn")
	time.Sleep(idle / 2)
	q <- steer.Message{Text: "still here"}
	r.waitPark(t, "second turn")
	parkedAgain := time.Now()
	r.waitEnd(t, 3*time.Second)

	if waited := time.Since(parkedAgain); waited < idle*3/4 {
		t.Errorf("the second park ended after %v: it did not get a full %v window", waited, idle)
	}
	if n := len(r.requests()); n != 2 {
		t.Errorf("provider calls = %d, want 2", n)
	}
	if r.res.StopReason != StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q", r.res.StopReason, StopReasonIdleTimeout)
	}
}

// IdleTimeout 0 waits for the operator for as long as the run lives.
func TestRun_Interactive_ZeroIdleTimeoutWaitsForever(t *testing.T) {
	q := make(chan steer.Message, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := startIdleRun(t, ctx, q, func(o *RunOptions) {
		o.CloseSteerIfEmpty = func() bool { return len(q) == 0 }
	})
	r.waitPark(t, "first turn")
	select {
	case <-r.done:
		t.Fatalf("a run with no idle timeout ended while parked: stop %q", r.res.StopReason)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	r.waitEnd(t, 3*time.Second)
	if r.res.StopReason == StopReasonIdleTimeout {
		t.Error("a cancelled run reported an idle end")
	}
}

// A deadline that passes while the runtime is paused waits for the lift: a
// paused runtime does not end runs.
func TestRun_Interactive_AnIdleDeadlineWaitsOutAPause(t *testing.T) {
	q := make(chan steer.Message, 4)
	gate := newIdleGate()
	r := startIdleRun(t, context.Background(), q, func(o *RunOptions) {
		o.IdleTimeout = 50 * time.Millisecond
		o.CloseSteerIfEmpty = func() bool { return len(q) == 0 }
		o.PauseGate = gate
	})
	r.waitPark(t, "first turn")
	gate.declare()
	waitCount(t, "paused records", gate.paused.Load, 1)
	select {
	case <-r.done:
		t.Fatal("the run ended while the runtime was paused")
	case <-time.After(200 * time.Millisecond):
	}
	gate.lift()
	r.waitEnd(t, 3*time.Second)
	if r.res.StopReason != StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q once the pause lifted", r.res.StopReason, StopReasonIdleTimeout)
	}
	if gate.paused.Load() != 0 {
		t.Error("the run ended still recorded as paused")
	}
}

// The stateful loop parks through its own twin of the park, and ends the same
// way.
func TestRun_Stateful_IdleTimeoutEndsAParkedRun(t *testing.T) {
	prov := &actionScriptProvider{scripts: []string{
		`{"patch":{"n":1},"done":true,"final":"answer one"}`,
	}}
	q := make(chan steer.Message, 4)
	park := make(chan struct{}, 8)
	done := make(chan struct{})
	var res RunResult
	var err error
	go func() {
		defer close(done)
		res, err = statefulInteractive(t, prov, q, park, func(o *RunOptions) {
			o.IdleTimeout = 50 * time.Millisecond
			o.CloseSteerIfEmpty = func() bool { return len(q) == 0 }
		})
	}()
	select {
	case <-park:
	case <-time.After(3 * time.Second):
		t.Fatal("the stateful run did not park")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the parked stateful run did not end on its idle timeout")
	}
	if err != nil || res.StopReason != StopReasonIdleTimeout {
		t.Errorf("stop = %q err = %v, want %q and no error", res.StopReason, err, StopReasonIdleTimeout)
	}
	if !strings.Contains(res.FinalText, "answer one") {
		t.Errorf("final = %q, want the answer the run had given", res.FinalText)
	}
}

// RFC HI D4: at its idle deadline a parked run asks IdleExtend, and only
// then; a later time holds the park to it, where it asks again, and the run
// ends idle once the answer no longer lies ahead.
func TestRun_Interactive_IdleExtendHoldsAParkedRunUntilItsEnd(t *testing.T) {
	q := make(chan steer.Message, 4)
	const idle = 50 * time.Millisecond
	var asks atomic.Int32
	var leaseEnd atomic.Int64 // unix nanos
	leaseEnd.Store(time.Now().Add(400 * time.Millisecond).UnixNano())
	r := startIdleRun(t, context.Background(), q, func(o *RunOptions) {
		o.IdleTimeout = idle
		o.CloseSteerIfEmpty = func() bool { return len(q) == 0 }
		o.IdleExtend = func(context.Context) time.Time {
			asks.Add(1)
			return time.Unix(0, leaseEnd.Load())
		}
	})
	r.waitPark(t, "first turn")
	parked := time.Now()
	select {
	case <-r.done:
		t.Fatalf("the run ended at %v while its lease ran to 400ms: stop %q", time.Since(parked), r.res.StopReason)
	case <-time.After(250 * time.Millisecond):
	}
	r.waitEnd(t, 3*time.Second)
	if waited := time.Since(parked); waited < 300*time.Millisecond {
		t.Errorf("the run ended %v after parking, before its lease's end", waited)
	}
	if r.res.StopReason != StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q once the lease ended", r.res.StopReason, StopReasonIdleTimeout)
	}
	// Once at the idle deadline and once at the lease's end: never polled.
	if n := asks.Load(); n != 2 {
		t.Errorf("IdleExtend asked %d times, want 2 (the deadline and the lease's end)", n)
	}
}

// An IdleExtend answering the zero time, or a time already past, lets the
// run end at its idle deadline as without it.
func TestRun_Interactive_IdleExtendWithNoLiveLeaseEndsTheRun(t *testing.T) {
	for name, at := range map[string]func() time.Time{
		"zero": func() time.Time { return time.Time{} },
		"past": func() time.Time { return time.Now().Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			q := make(chan steer.Message, 4)
			r := startIdleRun(t, context.Background(), q, func(o *RunOptions) {
				o.IdleTimeout = 50 * time.Millisecond
				o.CloseSteerIfEmpty = func() bool { return len(q) == 0 }
				o.IdleExtend = func(context.Context) time.Time { return at() }
			})
			r.waitPark(t, "first turn")
			r.waitEnd(t, time.Second)
			if r.res.StopReason != StopReasonIdleTimeout {
				t.Errorf("stop reason = %q, want %q", r.res.StopReason, StopReasonIdleTimeout)
			}
		})
	}
}

// A deadline that passed while paused asks IdleExtend at the lift: a lease
// still live holds the run.
func TestRun_Interactive_IdleExtendIsAskedWhenAPauseLifts(t *testing.T) {
	q := make(chan steer.Message, 4)
	gate := newIdleGate()
	var leaseEnd atomic.Int64
	r := startIdleRun(t, context.Background(), q, func(o *RunOptions) {
		o.IdleTimeout = 300 * time.Millisecond
		o.CloseSteerIfEmpty = func() bool { return len(q) == 0 }
		o.PauseGate = gate
		o.IdleExtend = func(context.Context) time.Time { return time.Unix(0, leaseEnd.Load()) }
	})
	r.waitPark(t, "first turn")
	gate.declare()
	waitCount(t, "paused records", gate.paused.Load, 1)
	time.Sleep(400 * time.Millisecond) // the idle deadline passes while paused
	leaseEnd.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
	gate.lift()
	select {
	case <-r.done:
		t.Fatal("the run ended at the lift while a lease was live")
	case <-time.After(150 * time.Millisecond):
	}
	r.waitEnd(t, 3*time.Second)
	if r.res.StopReason != StopReasonIdleTimeout {
		t.Errorf("stop reason = %q, want %q", r.res.StopReason, StopReasonIdleTimeout)
	}
}
