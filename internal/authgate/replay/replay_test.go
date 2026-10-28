package replay

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func TestCache_SecondUseInsideTheWindowIsRefused(t *testing.T) {
	c := New(10)
	if err := c.Admit("n1", t0.Add(time.Minute), t0); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if err := c.Admit("n1", t0.Add(time.Minute), t0.Add(59*time.Second)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second admit = %v, want ErrReplayed", err)
	}
	later := t0.Add(2 * time.Minute)
	if err := c.Admit("n1", later.Add(time.Minute), later); err != nil {
		t.Fatalf("admit after expiry: %v", err)
	}
}

func TestCache_FullOfLiveValuesRefusesRatherThanEvicts(t *testing.T) {
	c := New(3)
	for i := 0; i < 3; i++ {
		if err := c.Admit(fmt.Sprint(i), t0.Add(time.Minute), t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Admit("x", t0.Add(time.Minute), t0); !errors.Is(err, ErrFull) {
		t.Fatalf("admit into a full cache = %v, want ErrFull", err)
	}
	// A live entry was not evicted to make room: it is still a replay.
	if err := c.Admit("0", t0.Add(time.Minute), t0); !errors.Is(err, ErrReplayed) {
		t.Fatalf("live entry after a full refusal = %v, want ErrReplayed", err)
	}
	later := t0.Add(2 * time.Minute)
	if err := c.Admit("x", later.Add(time.Minute), later); err != nil {
		t.Fatalf("admit once the old entries expired: %v", err)
	}
	if c.Len() != 1 {
		t.Fatalf("expired entries not swept: len %d", c.Len())
	}
}

func TestCache_ConcurrentPresentationsAdmitExactlyOne(t *testing.T) {
	c := New(0)
	var ok atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.Admit("same", t0.Add(time.Minute), t0) == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d presentations admitted, want 1", ok.Load())
	}
}

func TestCache_ReservedValueRefusesASecondPresentationUntilReleased(t *testing.T) {
	c := New(10)
	if err := c.Reserve("sig", t0); err != nil {
		t.Fatal(err)
	}
	// In flight: a concurrent identical presentation is refused.
	if err := c.Reserve("sig", t0); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second reserve while in flight = %v, want ErrReplayed", err)
	}
	// The check failed (STS refused it): the value never counted.
	c.Release("sig")
	if c.Len() != 0 {
		t.Fatalf("a released reservation is still held: len %d", c.Len())
	}
	if err := c.Reserve("sig", t0); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}
	c.Commit("sig", t0.Add(time.Minute))
	if err := c.Reserve("sig", t0.Add(30*time.Second)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("reserve of a committed value = %v, want ErrReplayed", err)
	}
	if err := c.Admit("sig", t0.Add(2*time.Minute), t0.Add(30*time.Second)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("admit of a committed value = %v, want ErrReplayed", err)
	}
}

func TestCache_ReleaseNeverDropsACommittedValue(t *testing.T) {
	c := New(10)
	_ = c.Admit("v", t0.Add(time.Minute), t0)
	c.Release("v")
	if err := c.Admit("v", t0.Add(time.Minute), t0); !errors.Is(err, ErrReplayed) {
		t.Fatalf("a Release removed a committed value: %v", err)
	}
}

func TestCache_ReservationsCountTowardTheBound(t *testing.T) {
	c := New(2)
	_ = c.Reserve("a", t0)
	_ = c.Reserve("b", t0)
	if err := c.Reserve("c", t0); !errors.Is(err, ErrFull) {
		t.Fatalf("third reservation in a cache of 2 = %v, want ErrFull", err)
	}
}

func TestEntryExpiry_OutlivesTheLastAcceptableMoment(t *testing.T) {
	window := 5 * time.Minute
	signed := t0
	if !EntryExpiry(signed, window).After(signed.Add(window)) {
		t.Fatal("the entry expires before the value stops being acceptable")
	}
}
