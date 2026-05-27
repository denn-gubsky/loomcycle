package postgres

import (
	"context"
	"errors"
	"testing"
	"time"
)

// transientErr fakes the postgres driver's SQLSTATE 53300 shape so
// isTransientConnErr classifies it the same way it would in
// production. Plain `errors.New` is fine — the classifier matches on
// substring, not error-type.
var transientErr = errors.New(`failed to connect: server error: FATAL: sorry, too many clients already (SQLSTATE 53300)`)

// permanentErr is what any non-connection failure looks like — the
// classifier must NOT retry these.
var permanentErr = errors.New(`syntax error at or near "FROM"`)

func TestIsTransientConnErr_ClassifiesKnownTransients(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantHit bool
	}{
		{"sqlstate_53300", errors.New("SQLSTATE 53300"), true},
		{"too_many_clients_phrase", errors.New("FATAL: sorry, too many clients already"), true},
		{"connection_refused", errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"), true},
		{"production_shape", transientErr, true},

		{"nil", nil, false},
		{"syntax_error", permanentErr, false},
		{"fk_violation", errors.New("violates foreign key constraint"), false},
		{"unique_violation", errors.New("SQLSTATE 23505"), false},
		// Mid-query errors are NOT retryable — INSERT may have
		// committed before the wire dropped. Classifier MUST NOT
		// match these.
		{"eof_mid_query", errors.New("unexpected EOF"), false},
		{"broken_pipe", errors.New("write: broken pipe"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientConnErr(tc.err); got != tc.wantHit {
				t.Errorf("isTransientConnErr(%v) = %v, want %v", tc.err, got, tc.wantHit)
			}
		})
	}
}

// TestRetryOnTransientConn_SucceedsAfterRetries — the canonical
// "launch storm self-heals" case. Attempt 1 + 2 fail transient,
// attempt 3 succeeds. The helper must return nil.
func TestRetryOnTransientConn_SucceedsAfterRetries(t *testing.T) {
	calls := 0
	err := retryOnTransientConn(context.Background(), func() error {
		calls++
		if calls < 3 {
			return transientErr
		}
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil after recovery", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// TestRetryOnTransientConn_GivesUpAfterMaxAttempts — sustained
// transient errors propagate the last one after three attempts.
// 50ms + 150ms = 200ms total backoff worst case; the test pad is
// generous enough to absorb scheduler jitter on slow runners.
func TestRetryOnTransientConn_GivesUpAfterMaxAttempts(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retryOnTransientConn(context.Background(), func() error {
		calls++
		return transientErr
	})
	elapsed := time.Since(start)

	if err != transientErr {
		t.Errorf("err = %v, want the underlying transientErr", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (full retry budget exhausted)", calls)
	}
	// 50ms + 150ms = 200ms total backoff. The third attempt fires
	// after both sleeps. Cap at 500ms to leave headroom for slow
	// CI runners without making the test slow on dev.
	if elapsed < 180*time.Millisecond {
		t.Errorf("elapsed = %v, want at least ~200ms across backoffs", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed = %v, want under 500ms", elapsed)
	}
}

// TestRetryOnTransientConn_PermanentErrorReturnsImmediately — the
// classifier separates transient from permanent so we don't waste
// retry budget on FK violations, syntax errors, etc.
func TestRetryOnTransientConn_PermanentErrorReturnsImmediately(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retryOnTransientConn(context.Background(), func() error {
		calls++
		return permanentErr
	})
	elapsed := time.Since(start)

	if err != permanentErr {
		t.Errorf("err = %v, want the underlying permanentErr", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry on permanent)", calls)
	}
	if elapsed > 30*time.Millisecond {
		t.Errorf("elapsed = %v, want immediate return", elapsed)
	}
}

// TestRetryOnTransientConn_CtxCancelShortCircuitsBackoff — a
// cancelled ctx must surface ctx.Err() rather than the underlying
// transient error, AND must not sleep through the remaining backoff.
func TestRetryOnTransientConn_CtxCancelShortCircuitsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	done := make(chan struct{})
	var err error
	go func() {
		err = retryOnTransientConn(ctx, func() error {
			calls++
			if calls == 1 {
				// Cancel immediately so the first backoff hits the
				// cancellation path.
				cancel()
			}
			return transientErr
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("retryOnTransientConn did not return after ctx cancel")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancelled before second attempt)", calls)
	}
}

// TestRetryOnTransientConn_FirstAttemptSucceedsNoOverhead — the
// happy path: when the call succeeds first try, no backoff fires.
// Sanity that the helper doesn't pessimise the common case.
func TestRetryOnTransientConn_FirstAttemptSucceedsNoOverhead(t *testing.T) {
	calls := 0
	start := time.Now()
	err := retryOnTransientConn(context.Background(), func() error {
		calls++
		return nil
	})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
	if elapsed > 5*time.Millisecond {
		t.Errorf("elapsed = %v, want near-zero on happy path", elapsed)
	}
}
