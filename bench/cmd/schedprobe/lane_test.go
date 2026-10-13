package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// acquireAsync starts an acquire and returns a channel that yields the slot.
func acquireAsync(t *testing.T, l *lane, agent string) <-chan int {
	t.Helper()
	ch := make(chan int, 1)
	go func() {
		i, err := l.acquire(context.Background(), agent)
		if err != nil {
			t.Errorf("acquire %s: %v", agent, err)
		}
		ch <- i
	}()
	return ch
}

func mustWait(t *testing.T, ch <-chan int, within time.Duration) int {
	t.Helper()
	select {
	case i := <-ch:
		return i
	case <-time.After(within):
		t.Fatal("acquire did not return")
		return -1
	}
}

func mustBlock(t *testing.T, ch <-chan int, for_ time.Duration) {
	t.Helper()
	select {
	case i := <-ch:
		t.Fatalf("acquire returned slot %d, want it still waiting", i)
	case <-time.After(for_):
	}
}

func TestLane_CallPolicyHandsSlotToWaiterAtRelease(t *testing.T) {
	l := policyLane("call", 1, 0, 0)
	a, _ := l.acquire(context.Background(), "a")
	b := acquireAsync(t, l, "b")
	mustBlock(t, b, 30*time.Millisecond)
	l.release("a", a)
	mustWait(t, b, time.Second)
}

func TestLane_LeaseKeepsSlotForOwnerWithinHold(t *testing.T) {
	l := policyLane("lease", 1, 4, time.Second)
	i, _ := l.acquire(context.Background(), "a")
	l.release("a", i)
	b := acquireAsync(t, l, "b")
	mustBlock(t, b, 30*time.Millisecond)
	// The owner comes back inside its hold: it gets its slot ahead of b.
	if _, err := l.acquire(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	mustBlock(t, b, 30*time.Millisecond)
}

func TestLane_LeaseYieldsAfterQuantumWhenOthersWait(t *testing.T) {
	l := policyLane("lease", 1, 2, time.Minute)
	ctx := context.Background()
	i, _ := l.acquire(ctx, "a")
	l.release("a", i)
	b := acquireAsync(t, l, "b")
	mustBlock(t, b, 20*time.Millisecond)
	i, _ = l.acquire(ctx, "a") // second call: still within the quantum
	l.release("a", i)          // quantum spent and b waits: the slot goes to b now
	mustWait(t, b, time.Second)
}

func TestLane_LeaseHandsOffWhenHoldExpires(t *testing.T) {
	l := policyLane("lease", 1, 8, 50*time.Millisecond)
	i, _ := l.acquire(context.Background(), "a")
	l.release("a", i)
	// b queues DURING the hold; it must get the slot when the hold runs out, not
	// wait for a's next call.
	b := acquireAsync(t, l, "b")
	mustBlock(t, b, 20*time.Millisecond)
	mustWait(t, b, time.Second)
}

func TestLane_RunPolicyHoldsSlotUntilDone(t *testing.T) {
	l := policyLane("run", 1, 0, 0)
	ctx := context.Background()
	i, _ := l.acquire(ctx, "a")
	l.release("a", i)
	b := acquireAsync(t, l, "b")
	mustBlock(t, b, 50*time.Millisecond) // between a's calls: still a's
	i, _ = l.acquire(ctx, "a")
	l.release("a", i)
	l.done("a")
	mustWait(t, b, time.Second)
}

func TestLane_CancelledWaiterLeavesQueue(t *testing.T) {
	l := policyLane("call", 1, 0, 0)
	a, _ := l.acquire(context.Background(), "a")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := l.acquire(ctx, "b"); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled acquire returned no error")
	}
	l.release("a", a)
	// The slot must be free for a newcomer, not handed to the cancelled waiter.
	c := acquireAsync(t, l, "c")
	mustWait(t, c, time.Second)
}

func TestLane_NeverRunsMoreCallsThanSlots(t *testing.T) {
	for _, pol := range []struct {
		name    string
		quantum int
		hold    time.Duration
	}{{"call", 0, 0}, {"run", 0, 0}, {"lease", 2, 2 * time.Millisecond}, {"lease", 0, time.Millisecond}} {
		t.Run(fmt.Sprintf("%s-q%d-h%s", pol.name, pol.quantum, pol.hold), func(t *testing.T) {
			const slots = 3
			l := policyLane(pol.name, slots, pol.quantum, pol.hold)
			var inFlight, peak atomic.Int32
			var wg sync.WaitGroup
			for a := 0; a < 10; a++ {
				wg.Add(1)
				go func(agent string) {
					defer wg.Done()
					defer l.done(agent)
					for c := 0; c < 5; c++ {
						i, err := l.acquire(context.Background(), agent)
						if err != nil {
							t.Error(err)
							return
						}
						n := inFlight.Add(1)
						for {
							p := peak.Load()
							if n <= p || peak.CompareAndSwap(p, n) {
								break
							}
						}
						time.Sleep(time.Millisecond)
						inFlight.Add(-1)
						l.release(agent, i)
					}
				}(fmt.Sprintf("a%d", a))
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("agents did not finish: a slot was lost")
			}
			if p := peak.Load(); p > slots {
				t.Fatalf("peak calls in flight %d, want <= %d", p, slots)
			}
		})
	}
}
