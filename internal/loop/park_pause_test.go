package loop

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/steer"
)

// idleGate is a PauseGate a test drives by hand: declare() pauses, lift()
// resumes. It records what the waiting run did with it.
type idleGate struct {
	mu       sync.Mutex
	pauseCh  chan struct{}
	resumeCh chan struct{}
	paused   atomic.Int32 // PauseIdle records currently held
	records  atomic.Int32 // PauseIdle calls that recorded
}

func newIdleGate() *idleGate {
	return &idleGate{pauseCh: make(chan struct{}), resumeCh: make(chan struct{})}
}

func (g *idleGate) PauseRequested() bool           { return false }
func (g *idleGate) Park(ctx context.Context) error { return nil }
func (g *idleGate) PauseCh() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pauseCh
}
func (g *idleGate) PauseIdle() (<-chan struct{}, func(), bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.paused.Add(1)
	g.records.Add(1)
	var once sync.Once
	return g.resumeCh, func() { once.Do(func() { g.paused.Add(-1) }) }, true
}
func (g *idleGate) declare() { g.mu.Lock(); close(g.pauseCh); g.mu.Unlock() }
func (g *idleGate) lift() {
	g.mu.Lock()
	close(g.resumeCh)
	g.pauseCh, g.resumeCh = make(chan struct{}), make(chan struct{})
	g.mu.Unlock()
}

func waitCount(t *testing.T, what string, get func() int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if get() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s = %d, want %d", what, get(), want)
}

// Held for review or parked for input, a run waiting on a person records
// itself paused when a pause is declared, un-records when it is lifted, and
// takes the next pause again — all without leaving its wait.
func TestPark_AWaitingRunTakesPartInAPause(t *testing.T) {
	for name, mutate := range map[string]func(*RunOptions){
		"held for review":  nil,
		"parked for input": func(o *RunOptions) { o.Review = false; o.Interactive = true },
	} {
		t.Run(name, func(t *testing.T) {
			gate := newIdleGate()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := startReviewRun(t, ctx, func(o *RunOptions) {
				o.PauseGate = gate
				if mutate != nil {
					mutate(o)
				}
			})
			if name == "held for review" {
				r.waitFor(t, providers.EventAwaitingReview)
			} else {
				r.waitFor(t, providers.EventAwaitingInput)
			}
			gate.declare()
			waitCount(t, "paused records", gate.paused.Load, 1)
			gate.lift()
			waitCount(t, "paused records after the lift", gate.paused.Load, 0)
			gate.declare()
			waitCount(t, "records across two pauses", gate.records.Load, 2)
			if r.prov.calls() != 1 {
				t.Errorf("calls = %d: the pause moved the waiting run", r.prov.calls())
			}
			cancel()
			r.finish(t)
			if gate.paused.Load() != 0 {
				t.Error("the run left its wait still recorded as paused")
			}
		})
	}
}

// A verdict that arrives while the runtime is paused is acted on when the
// pause lifts, and the run that leaves its wait then holds no record.
func TestPark_LeavingTheWaitReleasesThePauseRecord(t *testing.T) {
	gate := newIdleGate()
	r := startReviewRun(t, context.Background(), func(o *RunOptions) { o.PauseGate = gate })
	r.waitFor(t, providers.EventAwaitingReview)
	gate.declare()
	waitCount(t, "paused records", gate.paused.Load, 1)
	r.q <- steer.Message{Kind: steer.KindApprove, EnqueuedAt: time.Now()}
	gate.lift()
	r.result(t)
	if gate.paused.Load() != 0 {
		t.Error("an approved run is still recorded as paused")
	}
}
