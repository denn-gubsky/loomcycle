package concurrency

import (
	"context"
	"testing"
	"time"
)

// TestSemaphore_SetCaps: RFC CK live concurrency reload — raising the cap admits
// runs that were backpressured at the old cap; both are read under the mutex.
func TestSemaphore_SetCaps(t *testing.T) {
	s := New(1, 0, 50*time.Millisecond) // cap 1, no queue
	rel1, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, err := s.Acquire(context.Background()); !IsBackpressure(err) {
		t.Fatalf("at cap 1 + no queue, want backpressure, got %v", err)
	}
	s.SetCaps(3, 0) // raise to 3
	rel2, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("after SetCaps(3): %v", err)
	}
	rel3, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatalf("third slot after SetCaps(3): %v", err)
	}
	rel1()
	rel2()
	rel3()
}

// TestProviderGates_Reload: RFC CK — Reload adds/removes/re-caps gates in place.
func TestProviderGates_Reload(t *testing.T) {
	g := NewProviderGates(map[string]int{"a": 1}, 0, 50*time.Millisecond)
	if !g.Has("a") || g.Has("b") {
		t.Fatalf("initial: Has(a)=%v Has(b)=%v", g.Has("a"), g.Has("b"))
	}
	// Reload: raise a to 2, add b.
	g.Reload(map[string]int{"a": 2, "b": 1}, 0)
	if !g.Has("a") || !g.Has("b") || g.Len() != 2 {
		t.Fatalf("after reload: Has(a)=%v Has(b)=%v Len=%d", g.Has("a"), g.Has("b"), g.Len())
	}
	r1, _ := g.Acquire(context.Background(), "a")
	r2, err := g.Acquire(context.Background(), "a") // a is now cap 2
	if err != nil {
		t.Fatalf("a should admit 2 after reload: %v", err)
	}
	r1()
	r2()
	// Reload: drop a entirely.
	g.Reload(map[string]int{"b": 1}, 0)
	if g.Has("a") {
		t.Error("a should be uncapped (noop) after being removed on reload")
	}
}

// TestProviderGates_ReloadRace: concurrent Acquire/Has/Len vs Reload — under -race
// this fails if the byID swap is not synchronized.
func TestProviderGates_ReloadRace(t *testing.T) {
	g := NewProviderGates(map[string]int{"a": 5}, 4, 50*time.Millisecond)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			if rel, err := g.Acquire(context.Background(), "a"); err == nil {
				rel()
			}
			g.Has("a")
			g.Len()
			g.Stats()
		}
		close(done)
	}()
	for i := 0; i < 2000; i++ {
		g.Reload(map[string]int{"a": 5, "b": 1}, 4)
	}
	<-done
}
