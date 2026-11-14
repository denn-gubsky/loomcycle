package http

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/authgate/password"
	"github.com/denn-gubsky/loomcycle/internal/store"
)

type whoKey struct{}

// gatedThrottle answers AuthThrottleGet only when the test says so, per
// caller (named in the context), so a test places each request's store read
// exactly where it wants it between the others' steps.
type gatedThrottle struct {
	store.Store
	failures int
	arrived  chan string
	proceed  map[string]chan struct{}
}

func (g *gatedThrottle) AuthThrottleGet(ctx context.Context, _ string, _ time.Time) (int, time.Time, error) {
	who := ctx.Value(whoKey{}).(string)
	g.arrived <- who
	<-g.proceed[who]
	return g.failures, time.Time{}, nil
}

// never is a wait bound that does not pass.
func never(time.Duration) <-chan time.Time { return make(chan time.Time) }

// Two requests can each be over a key's allowance only because the other was
// counted while it read the store. Once the check they both arrived behind
// has left, the key is free, and they must not both wait on it: stepping back
// from a key with room wakes the waiters to count again.
func TestCheckPlaces_RequestsOverOnlyBecauseOfEachOtherDoNotWaitOnAFreeKey(t *testing.T) {
	g := &gatedThrottle{failures: password.SourceFree, arrived: make(chan string, 8),
		proceed: map[string]chan struct{}{"A": make(chan struct{}), "B": make(chan struct{}), "C": make(chan struct{})}}
	s := &Server{store: g}
	s.checks.after = never
	// One check at a time: the key is at its last free failure.
	blockFor := password.Throttle{MaxFailures: password.DefaultMaxFailures, Lockout: password.DefaultLockout}.SourceBlockFor
	type result struct {
		who   string
		place *checkPlace
		ok    bool
	}
	done := make(chan result, 3)
	start := func(who string) {
		go func() {
			pl, ok := s.reserveCheck(context.WithValue(context.Background(), whoKey{}, who), "key", blockFor, &checkPatience{})
			done <- result{who, pl, ok}
		}()
	}
	hung := time.After(time.Minute)
	reads := func(who string) {
		t.Helper()
		select {
		case got := <-g.arrived:
			if got != who {
				t.Fatalf("%s read the store, want %s", got, who)
			}
		case <-hung:
			t.Fatalf("%s never read the store", who)
		}
	}
	start("A")
	reads("A")
	g.proceed["A"] <- struct{}{}
	a := <-done
	if !a.ok {
		t.Fatal("the first check was refused")
	}
	start("B") // counted second, behind A
	reads("B")
	a.place.leave()
	start("C") // counted second, behind B, which is over and will step back
	reads("C")
	g.proceed["B"] <- struct{}{} // over: steps back, is woken by A's leaving, counts again behind C
	reads("B")
	g.proceed["B"] <- struct{}{} // over again, because of C: waits
	g.proceed["C"] <- struct{}{} // over because of B: steps back from a key nobody holds
	// Both count again; one is admitted, the other waits for it.
	for admitted := 0; admitted < 2; {
		select {
		case who := <-g.arrived:
			g.proceed[who] <- struct{}{}
		case r := <-done:
			if !r.ok {
				t.Fatalf("%s was refused on a key nobody held", r.who)
			}
			admitted++
			r.place.leave()
		case <-hung:
			t.Fatalf("%d of two requests admitted: the others wait on a key nobody holds", admitted)
		}
	}
	if n := len(s.checks.keys); n != 0 {
		t.Fatalf("%d keys left", n)
	}
}

// A turn is never refused while nobody holds it: with as many requests as a
// key lets wait, plus the one checked, every request gets its turn however
// their steps interleave.
func TestCheckPlaces_ATurnIsNeverRefusedWhileTheKeyIsFree(t *testing.T) {
	var p checkPlaces
	p.after = never
	// A request that waited on a free key would stay there: this is what
	// ends it, as a refusal the test counts.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var refused, inTurn, together atomic.Int64
	var wg sync.WaitGroup
	for range 1 + maxCheckWaiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 2000 {
				pl, ok := p.turn(ctx, "login", &checkPatience{})
				if !ok {
					refused.Add(1)
					return
				}
				if inTurn.Add(1) > 1 {
					together.Add(1)
				}
				inTurn.Add(-1)
				pl.leave()
			}
		}()
	}
	wg.Wait()
	if refused.Load() != 0 || together.Load() != 0 {
		t.Fatalf("%d requests were refused a turn nobody held; %d times two had the turn at once", refused.Load(), together.Load())
	}
	if n := len(p.keys); n != 0 {
		t.Fatalf("%d keys left", n)
	}
}

// A waiter whose request ends (the client went away) stops waiting at once,
// is not admitted, and leaves nothing behind.
func TestCheckPlaces_AWaiterWhoseRequestEndsLeavesAtOnce(t *testing.T) {
	var p checkPlaces
	parked := make(chan struct{}, 1)
	p.after = func(time.Duration) <-chan time.Time {
		parked <- struct{}{}
		return make(chan time.Time)
	}
	first, ok := p.turn(context.Background(), "login", &checkPatience{})
	if !ok {
		t.Fatal("the first request was refused its turn")
	}
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan bool, 1)
	go func() {
		_, ok := p.turn(ctx, "login", &checkPatience{})
		got <- ok
	}()
	hung := time.After(time.Minute)
	select {
	case <-parked:
	case <-hung:
		t.Fatal("the second request never waited")
	}
	cancel()
	select {
	case ok := <-got:
		if ok {
			t.Fatal("a request whose context ended was given the turn")
		}
	case <-hung:
		t.Fatal("a waiter whose request ended is still waiting")
	}
	if k := p.keys["login"]; k == nil || k.held != 1 || k.waiting != 0 {
		t.Fatalf("after the waiter left: %+v, want the one holder and no waiter", k)
	}
	first.leave()
	if n := len(p.keys); n != 0 {
		t.Fatalf("%d keys left", n)
	}
}
