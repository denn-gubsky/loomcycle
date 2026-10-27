package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// RFC FA P3: store.AuthReplayStore, on both backends.

func testAuthReplayAdmitsOncePerWindow(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := pwgT0
	until := now.Add(5 * time.Minute)
	if ok, err := s.AuthReplayAdmit(ctx, "awsiam:ci", "k1", until, now); !ok || err != nil {
		t.Fatalf("first admit = %v, %v", ok, err)
	}
	if ok, err := s.AuthReplayAdmit(ctx, "awsiam:ci", "k1", until, now.Add(time.Minute)); ok || err != nil {
		t.Fatalf("second admit inside the window = %v, %v; want false", ok, err)
	}
	// Another scope is another gate: the same key is new there.
	if ok, err := s.AuthReplayAdmit(ctx, "awsiam:other", "k1", until, now); !ok || err != nil {
		t.Fatalf("same key in another scope = %v, %v", ok, err)
	}
	// Once expired, the pair may be admitted again (the credential that
	// carried it can no longer be valid anyway).
	later := until.Add(time.Second)
	if ok, err := s.AuthReplayAdmit(ctx, "awsiam:ci", "k1", later.Add(5*time.Minute), later); !ok || err != nil {
		t.Fatalf("admit after expiry = %v, %v", ok, err)
	}
	if ok, _ := s.AuthReplayAdmit(ctx, "awsiam:ci", "k1", later.Add(5*time.Minute), later); ok {
		t.Fatal("the re-admitted pair was admitted twice")
	}
	n, err := s.AuthReplayDeleteExpired(ctx, until.Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("delete expired = %d, %v; want 1 (the other scope's row)", n, err)
	}
	if ok, _ := s.AuthReplayAdmit(ctx, "awsiam:ci", "k1", later.Add(5*time.Minute), later); ok {
		t.Fatal("the sweep removed a live row")
	}
}

func testAuthReplayConcurrentAdmitsAdmitOne(t *testing.T, s store.Store) {
	ctx := context.Background()
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.AuthReplayAdmit(ctx, "awsiam:ci", "race", pwgT0.Add(time.Hour), pwgT0)
			if err != nil {
				t.Errorf("admit: %v", err)
			}
			if ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("%d concurrent admits succeeded, want 1", admitted.Load())
	}
}
