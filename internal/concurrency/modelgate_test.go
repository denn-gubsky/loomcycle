package concurrency

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

// gateNow enters a gate that must admit at once.
func gateNow(t *testing.T, g *ModelGate, model string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), schedWait)
	defer cancel()
	leave, err := g.Enter(ctx, model)
	if err != nil {
		t.Fatalf("Enter(%q) = %v, want an immediate admission", model, err)
	}
	return leave
}

// gateAsync queues an Enter and waits until the gate counts it as waiting, so
// consecutive calls queue in a known order.
func gateAsync(t *testing.T, g *ModelGate, model string) <-chan func() {
	t.Helper()
	before := g.Stats().Waiting
	ch := acquireAsync(t, schedCtx(t), func(ctx context.Context) (func(), error) { return g.Enter(ctx, model) })
	eventually(t, "enter queued", func() bool { return g.Stats().Waiting > before || len(ch) > 0 })
	return ch
}

func TestModelGate_DisabledGateCountsNothing(t *testing.T) {
	g := NewModelGate(0, time.Second)
	gateNow(t, g, "x")
	gateNow(t, g, "y")
	gateNow(t, g, "z")
	if st := g.Stats(); st.InFlight != nil || st.Waiting != 0 {
		t.Fatalf("stats = %+v, want nothing counted by a disabled gate", st)
	}
}

func TestModelGate_EmptyModelIsNotCapped(t *testing.T) {
	g := NewModelGate(1, 0)
	gateNow(t, g, "x")
	gateNow(t, g, "")
	gateNow(t, g, "")
	if st := g.Stats(); len(st.InFlight) != 1 || st.InFlight["x"] != 1 {
		t.Fatalf("InFlight = %v, want only x counted", st.InFlight)
	}
}

func TestModelGate_UncountedEnterDoesNotAllocate(t *testing.T) {
	ctx := context.Background()
	disabled := NewModelGate(0, 0)
	enabled := NewModelGate(1, 0)
	var nilGate *ModelGate
	allocs := testing.AllocsPerRun(200, func() {
		l1, _ := disabled.Enter(ctx, "x")
		l2, _ := enabled.Enter(ctx, "")
		l3, _ := nilGate.Enter(ctx, "x")
		l1()
		l2()
		l3()
	})
	if allocs != 0 {
		t.Fatalf("allocs per uncounted Enter = %v, want 0", allocs)
	}
}

func TestModelGate_AdmitsMoreCallsToModelInFlight(t *testing.T) {
	g := NewModelGate(1, 0)
	gateNow(t, g, "x")
	gateNow(t, g, "x")
	gateNow(t, g, "x")
	if st := g.Stats(); st.InFlight["x"] != 3 {
		t.Fatalf("InFlight = %v, want three calls to x", st.InFlight)
	}
}

func TestModelGate_CapsDistinctModelsInFlight(t *testing.T) {
	g := NewModelGate(2, 0)
	gateNow(t, g, "x")
	leaveY := gateNow(t, g, "y")
	z := gateAsync(t, g, "z")
	mustStillWait(t, z, "a third model")
	if st := g.Stats(); len(st.InFlight) != 2 || st.Waiting != 1 {
		t.Fatalf("stats = %+v, want two models in flight and one waiter", st)
	}
	leaveY()
	mustGrant(t, z, "the third model once one drained")
	if st := g.Stats(); st.InFlight["z"] != 1 || st.InFlight["y"] != 0 || st.Waiting != 0 {
		t.Fatalf("stats = %+v, want x and z in flight", st)
	}
}

func TestModelGate_ModelStaysInFlightUntilItsLastCallLeaves(t *testing.T) {
	g := NewModelGate(1, 0)
	x1 := gateNow(t, g, "x")
	x2 := gateNow(t, g, "x")
	y := gateAsync(t, g, "y")
	x1()
	mustStillWait(t, y, "y while x still has a call in flight")
	x2()
	mustGrant(t, y, "y once x drained")
}

func TestModelGate_DrainAdmitsOldestWaiterAndAllWaitersForItsModel(t *testing.T) {
	g := NewModelGate(1, 0)
	leaveX := gateNow(t, g, "x")
	y1 := gateAsync(t, g, "y")
	z := gateAsync(t, g, "z")
	y2 := gateAsync(t, g, "y")
	leaveX()
	leaveY1 := mustGrant(t, y1, "the oldest waiter")
	leaveY2 := mustGrant(t, y2, "a later waiter for the oldest waiter's model")
	mustStillWait(t, z, "a waiter for another model")
	if st := g.Stats(); st.InFlight["y"] != 2 || st.Waiting != 1 {
		t.Fatalf("stats = %+v, want y:2 in flight and z waiting", st)
	}
	leaveY1()
	leaveY2()
	mustGrant(t, z, "the next model in line")
}

func TestModelGate_WaitEndsOnlyWhenContextEnds(t *testing.T) {
	// maxWait bounds overtaking, never the wait itself.
	g := NewModelGate(1, time.Millisecond)
	gateNow(t, g, "x")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := g.Enter(ctx, "y"); errc <- err }()
	eventually(t, "y waiting", func() bool { return g.Stats().Waiting == 1 })
	select {
	case err := <-errc:
		t.Fatalf("waiting Enter returned %v on its own, want it to wait", err)
	case <-time.After(schedSettle):
	}
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Enter = %v, want context.Canceled", err)
		}
	case <-time.After(schedWait):
		t.Fatal("cancelled Enter did not return")
	}
	if st := g.Stats(); st.Waiting != 0 || st.InFlight["x"] != 1 || len(st.InFlight) != 1 {
		t.Fatalf("stats = %+v, want only x in flight and nobody waiting", st)
	}
}

func TestModelGate_ModelInFlightOvertakesWaiterBeforeMaxWait(t *testing.T) {
	g := NewModelGate(1, time.Minute)
	gateNow(t, g, "x")
	y := gateAsync(t, g, "y")
	gateNow(t, g, "x") // x is in flight and y has not waited long: no reason to hold x back
	mustStillWait(t, y, "y behind x")
}

func TestModelGate_StarvedWaiterMakesModelInFlightQueue(t *testing.T) {
	g := NewModelGate(1, 5*time.Millisecond)
	leaveX := gateNow(t, g, "x")
	y := gateAsync(t, g, "y")
	time.Sleep(20 * time.Millisecond) // y has now waited past maxWait
	// A new call to x would keep x in flight for ever; it queues behind y.
	x2 := gateAsync(t, g, "x")
	mustStillWait(t, x2, "a new call to the model in flight, behind a starved waiter")
	leaveX()
	leaveY := mustGrant(t, y, "the starved waiter once x drained")
	mustStillWait(t, x2, "x while y is in flight")
	leaveY()
	mustGrant(t, x2, "x once y drained")
}

func TestModelGate_ZeroMaxWaitNeverHoldsBackModelInFlight(t *testing.T) {
	g := NewModelGate(1, 0)
	gateNow(t, g, "x")
	y := gateAsync(t, g, "y")
	time.Sleep(20 * time.Millisecond)
	gateNow(t, g, "x")
	mustStillWait(t, y, "y behind x")
}

func TestModelGate_CancelledStarvedWaiterAdmitsCallsQueuedBehindIt(t *testing.T) {
	g := NewModelGate(1, 5*time.Millisecond)
	gateNow(t, g, "x")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := g.Enter(ctx, "y"); errc <- err }()
	eventually(t, "y waiting", func() bool { return g.Stats().Waiting == 1 })
	time.Sleep(20 * time.Millisecond)
	x2 := gateAsync(t, g, "x")
	mustStillWait(t, x2, "x behind the starved y")
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Enter = %v, want context.Canceled", err)
	}
	// y is gone and x is still in flight: nothing holds x2 back any more.
	mustGrant(t, x2, "x once the waiter it queued behind left")
}

func TestModelGate_LeaveIsIdempotent(t *testing.T) {
	g := NewModelGate(1, 0)
	x1 := gateNow(t, g, "x")
	gateNow(t, g, "x")
	x1()
	x1()
	if st := g.Stats(); st.InFlight["x"] != 1 {
		t.Fatalf("InFlight = %v after a double leave, want x:1", st.InFlight)
	}
	y := gateAsync(t, g, "y")
	mustStillWait(t, y, "y while x is still in flight")
}

func TestModelGate_SetLimitRaiseAdmitsWaiters(t *testing.T) {
	g := NewModelGate(1, 0)
	gateNow(t, g, "x")
	y := gateAsync(t, g, "y")
	z := gateAsync(t, g, "z")
	g.SetLimit(2, 0)
	mustGrant(t, y, "the oldest waiter after the cap rose")
	mustStillWait(t, z, "a waiter beyond the new cap")
}

func TestModelGate_SetLimitDisableAdmitsEveryWaiter(t *testing.T) {
	g := NewModelGate(1, 0)
	gateNow(t, g, "x")
	y := gateAsync(t, g, "y")
	z := gateAsync(t, g, "z")
	g.SetLimit(0, 0)
	leaveY := mustGrant(t, y, "y once the gate is off")
	leaveZ := mustGrant(t, z, "z once the gate is off")
	leaveY()
	leaveZ()
	if st := g.Stats(); st.Waiting != 0 || len(st.InFlight) != 1 {
		t.Fatalf("stats = %+v, want only x left", st)
	}
}

func TestModelGate_SetLimitLowerLetsModelsInFlightFinish(t *testing.T) {
	g := NewModelGate(2, 0)
	leaveX := gateNow(t, g, "x")
	leaveY := gateNow(t, g, "y")
	g.SetLimit(1, 0)
	if st := g.Stats(); len(st.InFlight) != 2 {
		t.Fatalf("InFlight = %v, want both models still in flight", st.InFlight)
	}
	leaveX2 := gateNow(t, g, "x") // a model in flight is still admitted
	z := gateAsync(t, g, "z")
	leaveY()
	mustStillWait(t, z, "z while the lowered cap is still full")
	leaveX()
	mustStillWait(t, z, "z while x has a call left")
	leaveX2()
	mustGrant(t, z, "z once the gate is under its new cap")
}

func TestModelGate_SetLimitAppliesMaxWait(t *testing.T) {
	g := NewModelGate(1, 0)
	gateNow(t, g, "x")
	y := gateAsync(t, g, "y")
	time.Sleep(10 * time.Millisecond)
	g.SetLimit(1, 5*time.Millisecond)
	x2 := gateAsync(t, g, "x")
	mustStillWait(t, x2, "x behind a waiter that is starved under the new bound")
	mustStillWait(t, y, "y while x is in flight")
}

func TestModelGate_CancelRacingAdmissionLeaksNoPlace(t *testing.T) {
	g := NewModelGate(1, 0)
	for i := 0; i < 500; i++ {
		leaveX := gateNow(t, g, "x")
		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			leave func()
			err   error
		}
		resc := make(chan result, 1)
		go func() {
			leave, err := g.Enter(ctx, "y")
			resc <- result{leave, err}
		}()
		eventually(t, "y waiting", func() bool { return g.Stats().Waiting == 1 })
		go leaveX()
		cancel()
		res := <-resc
		if res.err == nil {
			res.leave()
		}
		eventually(t, "gate empty", func() bool {
			st := g.Stats()
			return len(st.InFlight) == 0 && st.Waiting == 0
		})
	}
}

func TestModelGate_StormNeverExceedsMaxModels(t *testing.T) {
	const maxModels = 2
	g := NewModelGate(maxModels, 500*time.Microsecond)
	// The test's own view of what computes, kept apart from the gate's.
	var mu sync.Mutex
	computing := map[string]int{}
	peak := 0
	var wg sync.WaitGroup
	for w := 0; w < 24; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			for i := 0; i < 60; i++ {
				model := fmt.Sprintf("m%d", rng.Intn(5))
				ctx, cancel := context.WithCancel(context.Background())
				if rng.Intn(3) == 0 {
					go func(d time.Duration) { time.Sleep(d); cancel() }(time.Duration(rng.Intn(1500)) * time.Microsecond)
				}
				leave, err := g.Enter(ctx, model)
				if err != nil {
					cancel()
					continue
				}
				mu.Lock()
				computing[model]++
				if len(computing) > peak {
					peak = len(computing)
				}
				mu.Unlock()
				time.Sleep(time.Duration(rng.Intn(300)) * time.Microsecond)
				mu.Lock()
				if computing[model]--; computing[model] == 0 {
					delete(computing, model)
				}
				mu.Unlock()
				leave()
				if rng.Intn(8) == 0 {
					leave()
				}
				cancel()
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("storm did not finish: a waiter was never admitted (stats %+v)", g.Stats())
	}
	if peak > maxModels {
		t.Fatalf("peak distinct models computing = %d, want <= %d", peak, maxModels)
	}
	if st := g.Stats(); len(st.InFlight) != 0 || st.Waiting != 0 {
		t.Fatalf("stats after the storm = %+v, want an empty gate", st)
	}
}
