package replay

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCache_SecondUseInsideTheWindowIsRefused(t *testing.T) {
	c := New(10)
	now := time.Unix(1_800_000_000, 0)
	c.SetClock(func() time.Time { return now })
	if err := c.Admit("n1", now.Add(time.Minute)); err != nil {
		t.Fatalf("first admit: %v", err)
	}
	if err := c.Admit("n1", now.Add(time.Minute)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second admit = %v, want ErrReplayed", err)
	}
	now = now.Add(2 * time.Minute)
	if err := c.Admit("n1", now.Add(time.Minute)); err != nil {
		t.Fatalf("admit after expiry: %v", err)
	}
}

func TestCache_FullOfLiveValuesRefusesRatherThanEvicts(t *testing.T) {
	c := New(3)
	now := time.Unix(1_800_000_000, 0)
	c.SetClock(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if err := c.Admit(fmt.Sprint(i), now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Admit("x", now.Add(time.Minute)); !errors.Is(err, ErrFull) {
		t.Fatalf("admit into a full cache = %v, want ErrFull", err)
	}
	// A live entry was not evicted to make room: it is still a replay.
	if err := c.Admit("0", now.Add(time.Minute)); !errors.Is(err, ErrReplayed) {
		t.Fatalf("live entry after a full refusal = %v, want ErrReplayed", err)
	}
	now = now.Add(2 * time.Minute)
	if err := c.Admit("x", now.Add(time.Minute)); err != nil {
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
			if c.Admit("same", time.Now().Add(time.Minute)) == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("%d presentations admitted, want 1", ok.Load())
	}
}
