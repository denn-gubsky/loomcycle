package channels

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fires Bus.Notify(channel) at visibleAt for an in-flight subscriber.
func TestScheduler_FiresAtVisibleAt(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)

	woke := make(chan bool, 1)
	go func() {
		woke <- bus.Wait(context.Background(), "ch1", 500*time.Millisecond)
	}()

	if !sched.Schedule("ch1", "msg_abc", time.Now().Add(80*time.Millisecond)) {
		t.Fatal("Schedule returned false; expected timer armed")
	}
	if sched.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1", sched.PendingCount())
	}

	select {
	case ok := <-woke:
		if !ok {
			t.Error("Wait returned false (timed out) — Notify did not fire at visibleAt")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Wait never returned within 300ms — Notify lost")
	}

	// Timer cleaned up after firing.
	if sched.PendingCount() != 0 {
		t.Errorf("PendingCount post-fire = %d, want 0", sched.PendingCount())
	}
}

// Past visibleAt fires Notify synchronously; no timer is registered.
func TestScheduler_PastVisibleAtFiresImmediate(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)

	// Pre-register a waiter so we can observe the synchronous Notify.
	gotNotify := make(chan struct{}, 1)
	go func() {
		if bus.Wait(context.Background(), "ch1", 100*time.Millisecond) {
			gotNotify <- struct{}{}
		}
	}()
	// Small delay so the goroutine has registered the waiter.
	time.Sleep(20 * time.Millisecond)

	armed := sched.Schedule("ch1", "msg_abc", time.Now().Add(-10*time.Second))
	if armed {
		t.Error("Schedule returned true for past-time; expected false (synchronous Notify)")
	}
	if sched.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0 (no timer for past visibleAt)", sched.PendingCount())
	}

	select {
	case <-gotNotify:
		// good
	case <-time.After(150 * time.Millisecond):
		t.Fatal("Notify never fired for past-time Schedule")
	}
}

// MaxPending cap silently rejects new schedules.
func TestScheduler_MaxPendingCap(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 2)

	if !sched.Schedule("ch1", "msg_1", time.Now().Add(1*time.Second)) {
		t.Fatal("first Schedule should succeed")
	}
	if !sched.Schedule("ch1", "msg_2", time.Now().Add(1*time.Second)) {
		t.Fatal("second Schedule should succeed")
	}
	if sched.Schedule("ch1", "msg_3", time.Now().Add(1*time.Second)) {
		t.Error("third Schedule should refuse (over cap)")
	}
	if sched.PendingCount() != 2 {
		t.Errorf("PendingCount = %d, want 2 (capped)", sched.PendingCount())
	}
}

// Idempotent re-Schedule on same msgID is a no-op.
func TestScheduler_IdempotentSchedule(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)

	if !sched.Schedule("ch1", "msg_abc", time.Now().Add(500*time.Millisecond)) {
		t.Fatal("first Schedule should succeed")
	}
	if sched.Schedule("ch1", "msg_abc", time.Now().Add(500*time.Millisecond)) {
		t.Error("re-Schedule of same msgID should refuse")
	}
	if sched.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1", sched.PendingCount())
	}
}

// Cancel removes a pending timer before it fires.
func TestScheduler_CancelBeforeFire(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)

	var notified atomic.Int32
	go func() {
		// Should never fire — we cancel before visibleAt.
		if bus.Wait(context.Background(), "ch1", 200*time.Millisecond) {
			notified.Add(1)
		}
	}()
	time.Sleep(10 * time.Millisecond)

	sched.Schedule("ch1", "msg_abc", time.Now().Add(150*time.Millisecond))
	sched.Cancel("msg_abc")

	if sched.PendingCount() != 0 {
		t.Errorf("PendingCount post-Cancel = %d, want 0", sched.PendingCount())
	}

	// Wait for the long-poll timeout. Notify should NOT have fired.
	time.Sleep(220 * time.Millisecond)
	if notified.Load() != 0 {
		t.Error("Notify fired despite Cancel")
	}
}

// Cancel on unknown msgID is a no-op.
func TestScheduler_CancelUnknown(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)
	sched.Cancel("msg_does_not_exist")
	if sched.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0", sched.PendingCount())
	}
}

// timerMapLen counts live registry entries — exposes orphaned entries that
// PendingCount (an atomic counter that nets to the right value) cannot reveal.
func timerMapLen(s *Scheduler) int {
	n := 0
	s.timers.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestScheduler_ConcurrentScheduleCancelDrainsClean is the exp7 C1 invariant
// lock for the placeholder-claim + CompareAndSwap rewrite: under concurrent
// Schedule/Cancel the registry must drain to zero entries with no pendCnt
// drift and no panic (Cancel can observe the nil placeholder mid-Schedule).
// Run under -race.
//
// Not a strict fail-before: the original arm-then-store orphan race is
// practically unreproducible because AfterFunc's microsecond firing latency
// dwarfs the few-nanosecond window between arming the timer and storing it —
// so this locks the post-fix invariants (clean drain, no panic, no data race)
// rather than failing on the old code.
func TestScheduler_ConcurrentScheduleCancelDrainsClean(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)

	const n = 400
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "msg_" + strconv.Itoa(i)
			sched.Schedule("ch", id, time.Now().Add(20*time.Millisecond))
			if i%2 == 0 {
				sched.Cancel(id) // race the just-armed timer / its placeholder
			}
		}(i)
	}
	wg.Wait()

	// Let any uncancelled timers fire + run their cleanup.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if timerMapLen(sched) == 0 && sched.PendingCount() == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := timerMapLen(sched); got != 0 {
		t.Errorf("registry has %d entries after drain, want 0 (orphaned entry?)", got)
	}
	if got := sched.PendingCount(); got != 0 {
		t.Errorf("PendingCount = %d after drain, want 0", got)
	}
}

// Bootstrap calls the supplied scan with a yield callback that
// reschedules each pending row.
func TestScheduler_Bootstrap(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)

	scan := func(yield func(channel, msgID string, visibleAt time.Time)) error {
		yield("ch1", "msg_a", time.Now().Add(1*time.Second))
		yield("ch2", "msg_b", time.Now().Add(2*time.Second))
		return nil
	}
	if err := sched.Bootstrap(context.Background(), scan); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if sched.PendingCount() != 2 {
		t.Errorf("PendingCount after Bootstrap = %d, want 2", sched.PendingCount())
	}
}

// Bootstrap errors propagate.
func TestScheduler_BootstrapError(t *testing.T) {
	bus := NewBus()
	sched := NewScheduler(bus, 0)

	wantErr := errors.New("boom")
	scan := func(_ func(string, string, time.Time)) error { return wantErr }
	if err := sched.Bootstrap(context.Background(), scan); !errors.Is(err, wantErr) {
		t.Errorf("Bootstrap err = %v, want wrap of %v", err, wantErr)
	}
}
