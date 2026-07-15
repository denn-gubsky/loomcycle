package concurrency

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestProviderGates_CapsConcurrentToN — a provider capped at N admits exactly N
// concurrent acquisitions; the N+1th (with no queue room) is refused with the
// typed *ErrProviderConcurrencyExhausted carrying the provider id + cap.
func TestProviderGates_CapsConcurrentToN(t *testing.T) {
	// cap=2, queueDepth=0 so the 3rd acquire is refused immediately rather than
	// queuing — keeps the "peak == cap" assertion deterministic.
	g := NewProviderGates(map[string]int{"p": 2}, 0, 50*time.Millisecond)

	rel1, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	rel2, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("acquire 2: %v", err)
	}
	if st := g.Stats()["p"]; st.Active != 2 {
		t.Fatalf("active = %d, want 2", st.Active)
	}

	// 3rd exceeds the cap and the queue has no room → typed refusal.
	_, err = g.Acquire(context.Background(), "p")
	if !IsProviderConcurrencyExhausted(err) {
		t.Fatalf("3rd acquire err = %v, want ErrProviderConcurrencyExhausted", err)
	}
	var pce *ErrProviderConcurrencyExhausted
	if !asProviderErr(err, &pce) {
		t.Fatalf("err is not *ErrProviderConcurrencyExhausted: %v", err)
	}
	if pce.Provider != "p" || pce.Cap != 2 {
		t.Errorf("typed err provider=%q cap=%d, want p / 2", pce.Provider, pce.Cap)
	}
	if pce.Code() != "provider_concurrency_exhausted" {
		t.Errorf("Code() = %q", pce.Code())
	}

	// Releasing one frees a slot for a fresh acquire.
	rel1()
	rel3, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	rel2()
	rel3()
	if st := g.Stats()["p"]; st.Active != 0 {
		t.Errorf("final active = %d, want 0", st.Active)
	}
}

// TestProviderGates_QueueThenTimeoutReturnsTypedError — with queue room, an
// over-cap acquire waits then times out (no releaser arrives) and returns the
// typed error rather than blocking forever.
func TestProviderGates_QueueThenTimeoutReturnsTypedError(t *testing.T) {
	g := NewProviderGates(map[string]int{"p": 1}, 4, 30*time.Millisecond)
	rel, err := g.Acquire(context.Background(), "p")
	if err != nil {
		t.Fatalf("acquire 1: %v", err)
	}
	defer rel()

	start := time.Now()
	_, err = g.Acquire(context.Background(), "p")
	if !IsProviderConcurrencyExhausted(err) {
		t.Fatalf("queued acquire err = %v, want ErrProviderConcurrencyExhausted", err)
	}
	if elapsed := time.Since(start); elapsed < 20*time.Millisecond {
		t.Errorf("returned after %s, expected to wait ~the queue timeout (30ms)", elapsed)
	}
}

// TestProviderGates_BatchesToCap — N runs (> cap) resolving to a capped provider
// run at most `cap` at a time; the rest queue and drain in batches. The observed
// peak concurrency equals the cap exactly (the operator's VRAM goal).
func TestProviderGates_BatchesToCap(t *testing.T) {
	const cap = 2
	const total = 6
	// Generous queue + timeout so every run eventually admits (none refused).
	g := NewProviderGates(map[string]int{"p": cap}, total, 2*time.Second)

	var mu sync.Mutex
	inFlight, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := g.Acquire(context.Background(), "p")
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond) // simulate the provider call
			mu.Lock()
			inFlight--
			mu.Unlock()
			rel()
		}()
	}
	wg.Wait()

	if peak != cap {
		t.Errorf("peak concurrency = %d, want exactly cap=%d", peak, cap)
	}
	if st := g.Stats()["p"]; st.Active != 0 || st.Queued != 0 {
		t.Errorf("final gate state active=%d queued=%d, want 0/0", st.Active, st.Queued)
	}
}

// TestProviderGates_UncappedProviderIsNoop — a provider with no gate (cap
// unset/0) never queues: Acquire returns a usable noop release immediately, no
// matter how many are outstanding.
func TestProviderGates_UncappedProviderIsNoop(t *testing.T) {
	// Only "capped" is gated; "free" has no entry.
	g := NewProviderGates(map[string]int{"capped": 1, "free": 0}, 0, time.Second)
	if g.Has("free") {
		t.Fatal("free should not be gated (cap 0)")
	}
	for i := 0; i < 100; i++ {
		rel, err := g.Acquire(context.Background(), "free")
		if err != nil {
			t.Fatalf("uncapped acquire %d: %v", i, err)
		}
		if rel == nil {
			t.Fatalf("uncapped acquire %d returned nil release", i)
		}
		// Deliberately do NOT release between iterations — an uncapped provider
		// must never queue or refuse on outstanding count.
	}
	if st := g.Stats()["free"]; st.Active != 0 {
		t.Errorf("uncapped provider recorded active=%d, want 0 (no accounting)", st.Active)
	}
}

// TestProviderGates_NoCapsBuildsNoGates — the zero-overhead-when-unconfigured
// guarantee: a caps map with no positive entry builds ZERO gates, and Acquire is
// a noop for every id. This is the default deployment (no operator set
// max_concurrent).
func TestProviderGates_NoCapsBuildsNoGates(t *testing.T) {
	g := NewProviderGates(map[string]int{"a": 0, "b": 0}, 16, time.Second)
	if g.Len() != 0 {
		t.Fatalf("Len = %d, want 0 gates built", g.Len())
	}
	if g.Stats() != nil {
		t.Errorf("Stats = %v, want nil when no gates", g.Stats())
	}
	rel, err := g.Acquire(context.Background(), "a")
	if err != nil || rel == nil {
		t.Fatalf("Acquire on ungated id: rel==nil=%v err=%v, want noop+nil", rel == nil, err)
	}
}

// TestProviderGates_NilIsNoop — a nil *ProviderGates (a Server with none wired)
// is fully inert: every method is safe and Acquire is a noop.
func TestProviderGates_NilIsNoop(t *testing.T) {
	var g *ProviderGates
	if g.Has("x") || g.Len() != 0 || g.Stats() != nil {
		t.Errorf("nil gates not inert: has=%v len=%d stats=%v", g.Has("x"), g.Len(), g.Stats())
	}
	rel, err := g.Acquire(context.Background(), "x")
	if err != nil || rel == nil {
		t.Fatalf("nil Acquire: rel==nil=%v err=%v, want noop+nil", rel == nil, err)
	}
	rel() // must not panic
}

// asProviderErr is a tiny errors.As wrapper kept local to the test so the assert
// above reads cleanly.
func asProviderErr(err error, target **ErrProviderConcurrencyExhausted) bool {
	e, ok := err.(*ErrProviderConcurrencyExhausted)
	if ok {
		*target = e
	}
	return ok
}
