package speech

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
)

func operatorCaps() Caps {
	return Caps{MaxCharsPerCall: 4000, MaxCharsPerRun: 100000, MaxSecondsPerRun: 3600}
}

// TestEffective_ZeroAgentFieldsTakeTheOperatorCaps — an agent that sets
// nothing runs under the operator's caps.
func TestEffective_ZeroAgentFieldsTakeTheOperatorCaps(t *testing.T) {
	if got := Effective(operatorCaps(), Caps{}); got != operatorCaps() {
		t.Errorf("caps = %+v, want the operator's %+v", got, operatorCaps())
	}
}

// TestEffective_AgentCapsOnlyLower — min semantics: an agent value below the
// operator's lowers it, one above does not raise it, and nonsense values
// (negative, NaN) are ignored.
func TestEffective_AgentCapsOnlyLower(t *testing.T) {
	low := Effective(operatorCaps(), Caps{MaxCharsPerCall: 1000, MaxCharsPerRun: 5000, MaxSecondsPerRun: 60})
	if low != (Caps{MaxCharsPerCall: 1000, MaxCharsPerRun: 5000, MaxSecondsPerRun: 60}) {
		t.Errorf("lowered caps = %+v", low)
	}
	high := Effective(operatorCaps(), Caps{MaxCharsPerCall: 9000, MaxCharsPerRun: 1e9, MaxSecondsPerRun: 1e9})
	if high != operatorCaps() {
		t.Errorf("raised caps = %+v, want the operator's", high)
	}
	odd := Effective(operatorCaps(), Caps{MaxCharsPerCall: -1, MaxCharsPerRun: -5, MaxSecondsPerRun: math.NaN()})
	if odd != operatorCaps() {
		t.Errorf("caps with negative/NaN agent fields = %+v, want the operator's", odd)
	}
}

// TestEffective_PerCallNeverAbovePerRun — an agent that lowers only the
// per-run cap below the operator's per-call cap gets the per-call cap held
// down with it.
func TestEffective_PerCallNeverAbovePerRun(t *testing.T) {
	c := Effective(operatorCaps(), Caps{MaxCharsPerRun: 500})
	if c.MaxCharsPerCall != 500 || c.MaxCharsPerRun != 500 {
		t.Errorf("caps = %+v, want per-call held to the per-run 500", c)
	}
}

// TestEffective_ZeroOperatorAllowsNothing — an operator with no engine has
// zero caps, and no agent value raises them.
func TestEffective_ZeroOperatorAllowsNothing(t *testing.T) {
	c := Effective(Caps{}, Caps{MaxCharsPerCall: 10, MaxCharsPerRun: 10, MaxSecondsPerRun: 10})
	if c != (Caps{}) {
		t.Errorf("caps = %+v, want all zero", c)
	}
	if err := NewBudget(c).ReserveChars(1); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("reserve under zero caps = %v, want ErrCapExceeded", err)
	}
}

// TestEstimateSeconds_UsesTheMeasuredDefaultRate — 17 chars/s unless a
// positive finite rate is given.
func TestEstimateSeconds_UsesTheMeasuredDefaultRate(t *testing.T) {
	cases := []struct {
		chars int
		rate  float64
		want  float64
	}{
		{170, 0, 10},
		{170, -3, 10},
		{170, math.NaN(), 10},
		{170, math.Inf(1), 10},
		{150, 15, 10},
		{0, 17, 0},
		{-4, 17, 0},
	}
	for _, c := range cases {
		if got := EstimateSeconds(c.chars, c.rate); got != c.want {
			t.Errorf("EstimateSeconds(%d, %v) = %v, want %v", c.chars, c.rate, got, c.want)
		}
	}
}

// TestBudget_RefusalNamesTheCapAndReservesNothing — a reservation over a
// per-run cap fails with a *CapError naming it, and holds nothing.
func TestBudget_RefusalNamesTheCapAndReservesNothing(t *testing.T) {
	b := NewBudget(Caps{MaxCharsPerRun: 100, MaxSecondsPerRun: 10})
	if err := b.ReserveChars(60); err != nil {
		t.Fatal(err)
	}
	err := b.ReserveChars(41)
	var ce *CapError
	if !errors.As(err, &ce) || !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("err = %v, want a *CapError matching ErrCapExceeded", err)
	}
	if ce.Cap != "max_chars_per_run" || ce.Limit != 100 || ce.Used != 60 || ce.Requested != 41 {
		t.Errorf("cap error = %+v", ce)
	}
	if !strings.Contains(err.Error(), "max_chars_per_run") {
		t.Errorf("message %q does not name the cap", err)
	}
	if b.Chars() != 60 {
		t.Errorf("chars = %d after refusal, want 60", b.Chars())
	}
	if err := b.ReserveChars(40); err != nil {
		t.Errorf("exact fit refused: %v", err)
	}

	if err := b.ReserveSeconds(9.5); err != nil {
		t.Fatal(err)
	}
	err = b.ReserveSeconds(1)
	if !errors.As(err, &ce) || ce.Cap != "max_seconds_per_run" {
		t.Fatalf("err = %v, want max_seconds_per_run", err)
	}
	if b.Seconds() != 9.5 {
		t.Errorf("seconds = %v after refusal, want 9.5", b.Seconds())
	}
}

// TestBudget_ReserveIsAllOrNothingAcrossBothCaps — when the seconds do not
// fit, the characters of the same call are not held either.
func TestBudget_ReserveIsAllOrNothingAcrossBothCaps(t *testing.T) {
	b := NewBudget(Caps{MaxCharsPerRun: 1000, MaxSecondsPerRun: 10})
	err := b.Reserve(100, 11)
	if !errors.Is(err, ErrCapExceeded) {
		t.Fatalf("err = %v, want ErrCapExceeded", err)
	}
	if b.Chars() != 0 || b.Seconds() != 0 {
		t.Errorf("after refusal chars=%d seconds=%v, want both 0", b.Chars(), b.Seconds())
	}
	if err := b.Reserve(100, 5); err != nil {
		t.Fatal(err)
	}
	if b.Chars() != 100 || b.Seconds() != 5 {
		t.Errorf("chars=%d seconds=%v, want 100 / 5", b.Chars(), b.Seconds())
	}
}

// TestBudget_RefusesNonsenseAmounts — zero, negative, NaN and infinite
// requests are errors that are not cap errors, and reserve nothing.
func TestBudget_RefusesNonsenseAmounts(t *testing.T) {
	b := NewBudget(operatorCaps())
	for _, n := range []int{0, -1} {
		if err := b.ReserveChars(n); err == nil || errors.Is(err, ErrCapExceeded) {
			t.Errorf("ReserveChars(%d) = %v, want a non-cap error", n, err)
		}
	}
	for _, s := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if err := b.ReserveSeconds(s); err == nil || errors.Is(err, ErrCapExceeded) {
			t.Errorf("ReserveSeconds(%v) = %v, want a non-cap error", s, err)
		}
	}
	if err := b.Reserve(10, math.NaN()); err == nil || b.Chars() != 0 {
		t.Errorf("Reserve(10, NaN) = %v with chars %d, want an error and nothing held", err, b.Chars())
	}
	if err := b.ReserveChars(math.MaxInt); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("ReserveChars(MaxInt) = %v, want a cap error (no overflow)", err)
	}
}

// TestBudget_ReleaseAndAdd — releases give back at most what is held; AddSeconds
// passes the cap so the next reservation is refused.
func TestBudget_ReleaseAndAdd(t *testing.T) {
	b := NewBudget(Caps{MaxCharsPerRun: 100, MaxSecondsPerRun: 10})
	if err := b.Reserve(50, 5); err != nil {
		t.Fatal(err)
	}
	b.ReleaseChars(80)
	b.ReleaseSeconds(2)
	if b.Chars() != 0 || b.Seconds() != 3 {
		t.Errorf("chars=%d seconds=%v, want 0 / 3", b.Chars(), b.Seconds())
	}
	b.AddSeconds(8)
	if b.Seconds() != 11 {
		t.Errorf("seconds = %v, want 11 (past the cap)", b.Seconds())
	}
	if err := b.ReserveSeconds(0.1); !errors.Is(err, ErrCapExceeded) {
		t.Errorf("reservation after passing the cap = %v, want refused", err)
	}
}

// TestBudget_ConcurrentReservationsNeverPassTheCap — many goroutines racing
// for the same budget reserve exactly up to the cap and no more (run with
// -race).
func TestBudget_ConcurrentReservationsNeverPassTheCap(t *testing.T) {
	const workers, perWorker = 16, 50
	b := NewBudget(Caps{MaxCharsPerRun: 1000, MaxSecondsPerRun: 500})
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWorker {
				if b.Reserve(3, 1) == nil {
					mu.Lock()
					granted++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	// 1000/3 = 333 calls fit the chars cap; 500 fit the seconds cap.
	if granted != 333 || b.Chars() != 999 || b.Seconds() != 333 {
		t.Errorf("granted=%d chars=%d seconds=%v, want 333 / 999 / 333", granted, b.Chars(), b.Seconds())
	}
}
