package ratelimit

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/providers"
)

// fakeResp builds a minimal *http.Response usable in tests.
func fakeResp(status int, headers map[string]string, body string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestNonZeroAttemptReturnsImmediately(t *testing.T) {
	calls := atomic.Int32{}
	resp, err := Do(context.Background(), Config{
		ParseHeader: AnthropicRetryAfter,
		Jitter:      0,
	}, func(ctx context.Context) (*http.Response, error) {
		calls.Add(1)
		return fakeResp(200, nil, "ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Errorf("attempts = %d, want 1", calls.Load())
	}
}

func TestRetriesOn429ThenSucceeds(t *testing.T) {
	calls := atomic.Int32{}
	cfg := Config{
		ParseHeader:  AnthropicRetryAfter,
		MaxAttempts:  3,
		Schedule:     []time.Duration{1 * time.Millisecond, 1 * time.Millisecond},
		Jitter:       0,
		Provider:     "test",
		MaxTotalWait: time.Second,
		OnRetry:      func(string, int, time.Duration, string) {},
	}

	resp, err := Do(context.Background(), cfg, func(ctx context.Context) (*http.Response, error) {
		n := calls.Add(1)
		if n < 3 {
			// 429 with no Retry-After → exp schedule kicks in
			return fakeResp(429, nil, "rate limited"), nil
		}
		return fakeResp(200, nil, "ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("final status = %d", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Errorf("attempts = %d, want 3", calls.Load())
	}
}

func TestRetryAfterHeaderHonored(t *testing.T) {
	var observedWaits []time.Duration
	cfg := Config{
		ParseHeader: AnthropicRetryAfter,
		MaxAttempts: 2,
		Jitter:      0,
		OnRetry: func(_ string, _ int, wait time.Duration, _ string) {
			observedWaits = append(observedWaits, wait)
		},
	}
	calls := atomic.Int32{}
	_, err := Do(context.Background(), cfg, func(ctx context.Context) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			return fakeResp(429, map[string]string{"Retry-After": "0"}, ""), nil
		}
		return fakeResp(200, nil, "ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(observedWaits) != 1 || observedWaits[0] != 0 {
		t.Errorf("observed waits %v, want [0s] (Retry-After: 0 honored)", observedWaits)
	}
}

func TestExpScheduleUsedWhenNoHeader(t *testing.T) {
	var observedReason string
	cfg := Config{
		ParseHeader: AnthropicRetryAfter,
		MaxAttempts: 2,
		Schedule:    []time.Duration{0},
		Jitter:      0,
		OnRetry: func(_ string, _ int, _ time.Duration, reason string) {
			observedReason = reason
		},
	}
	calls := atomic.Int32{}
	_, _ = Do(context.Background(), cfg, func(ctx context.Context) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			return fakeResp(429, nil, ""), nil // no Retry-After
		}
		return fakeResp(200, nil, ""), nil
	})
	if observedReason != ReasonSchedule {
		t.Errorf("reason = %q, want %q", observedReason, ReasonSchedule)
	}
}

func TestCtxCancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := Config{
		ParseHeader: AnthropicRetryAfter,
		MaxAttempts: 3,
		Schedule:    []time.Duration{500 * time.Millisecond},
		Jitter:      0,
		OnRetry: func(string, int, time.Duration, string) {
			// Cancel the moment we enter the sleep.
			go func() {
				time.Sleep(10 * time.Millisecond)
				cancel()
			}()
		},
	}
	_, err := Do(ctx, cfg, func(ctx context.Context) (*http.Response, error) {
		return fakeResp(429, nil, ""), nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestBudgetExhaustionReturnsLast429(t *testing.T) {
	cfg := Config{
		ParseHeader: AnthropicRetryAfter,
		MaxAttempts: 2,
		Schedule:    []time.Duration{1 * time.Millisecond},
		Jitter:      0,
		OnRetry:     func(string, int, time.Duration, string) {},
	}
	calls := atomic.Int32{}
	resp, err := Do(context.Background(), cfg, func(ctx context.Context) (*http.Response, error) {
		calls.Add(1)
		return fakeResp(429, nil, "still busy"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 429 {
		t.Errorf("expected 429 on exhaustion, got %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Errorf("attempts = %d, want 2", calls.Load())
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "still busy" {
		t.Errorf("body = %q (caller should still see error message)", string(body))
	}
}

func TestMaxTotalWaitGuardStopsRetries(t *testing.T) {
	cfg := Config{
		ParseHeader:  AnthropicRetryAfter,
		MaxAttempts:  10,
		MaxTotalWait: 5 * time.Millisecond,
		// Each retry asks for 100ms; second retry would push over budget.
		Schedule: []time.Duration{100 * time.Millisecond},
		Jitter:   0,
		OnRetry:  func(string, int, time.Duration, string) {},
	}
	calls := atomic.Int32{}
	resp, _ := Do(context.Background(), cfg, func(ctx context.Context) (*http.Response, error) {
		calls.Add(1)
		return fakeResp(429, nil, ""), nil
	})
	// Should attempt exactly once: the budget guard fires before the first
	// sleep because 100ms > 5ms remaining.
	if calls.Load() != 1 {
		t.Errorf("attempts = %d, want 1 (budget guard should block first retry)", calls.Load())
	}
	if resp.StatusCode != 429 {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestPropagatesAttemptError(t *testing.T) {
	myErr := errors.New("network down")
	_, err := Do(context.Background(), Config{
		ParseHeader: AnthropicRetryAfter,
		Jitter:      0,
	}, func(ctx context.Context) (*http.Response, error) {
		return nil, myErr
	})
	if !errors.Is(err, myErr) {
		t.Errorf("err = %v, want network down", err)
	}
}

func TestParseHeaderRequired(t *testing.T) {
	_, err := Do(context.Background(), Config{}, func(ctx context.Context) (*http.Response, error) {
		return fakeResp(200, nil, ""), nil
	})
	if err == nil || !strings.Contains(err.Error(), "ParseHeader") {
		t.Errorf("err = %v, want ParseHeader required", err)
	}
}

func TestJitterIsBoundedAndDeterministic(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	d := 100 * time.Millisecond
	got := applyJitter(d, 0.2, rng)
	if got < 80*time.Millisecond || got > 120*time.Millisecond {
		t.Errorf("jittered = %v, want within ±20%% of 100ms", got)
	}
	// Same seed produces same value (determinism for tests).
	rng2 := rand.New(rand.NewSource(42))
	got2 := applyJitter(d, 0.2, rng2)
	if got != got2 {
		t.Errorf("jitter not deterministic with same seed: %v vs %v", got, got2)
	}
}

func TestOnEventFiresWithRetryInfo(t *testing.T) {
	var events []providers.Event
	cfg := Config{
		ParseHeader: AnthropicRetryAfter,
		Provider:    "test-provider",
		MaxAttempts: 3,
		Schedule:    []time.Duration{1 * time.Millisecond, 1 * time.Millisecond},
		Jitter:      0,
		OnRetry:     func(string, int, time.Duration, string) {},
		OnEvent:     func(e providers.Event) { events = append(events, e) },
	}
	calls := atomic.Int32{}
	_, err := Do(context.Background(), cfg, func(ctx context.Context) (*http.Response, error) {
		n := calls.Add(1)
		if n < 3 {
			return fakeResp(429, map[string]string{"Retry-After": "0"}, ""), nil
		}
		return fakeResp(200, nil, "ok"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (one per retry sleep)", len(events))
	}
	for i, ev := range events {
		if ev.Type != providers.EventRetry {
			t.Errorf("event[%d].Type = %q, want %q", i, ev.Type, providers.EventRetry)
		}
		if ev.Retry == nil {
			t.Fatalf("event[%d].Retry is nil", i)
		}
		if ev.Retry.Provider != "test-provider" {
			t.Errorf("event[%d].Retry.Provider = %q, want test-provider", i, ev.Retry.Provider)
		}
		if ev.Retry.Attempt != i+1 {
			t.Errorf("event[%d].Retry.Attempt = %d, want %d", i, ev.Retry.Attempt, i+1)
		}
		if ev.Retry.Reason != ReasonHeader {
			t.Errorf("event[%d].Retry.Reason = %q, want %q", i, ev.Retry.Reason, ReasonHeader)
		}
		// Retry-After: 0 → wait 0ms.
		if ev.Retry.WaitMs != 0 {
			t.Errorf("event[%d].Retry.WaitMs = %d, want 0", i, ev.Retry.WaitMs)
		}
	}
}

func TestOnEventNotCalledWhenNotSet(t *testing.T) {
	// Smoke check: Config.OnEvent==nil must not panic in Do().
	cfg := Config{
		ParseHeader: AnthropicRetryAfter,
		MaxAttempts: 2,
		Schedule:    []time.Duration{1 * time.Millisecond},
		Jitter:      0,
		OnRetry:     func(string, int, time.Duration, string) {},
	}
	calls := atomic.Int32{}
	_, err := Do(context.Background(), cfg, func(ctx context.Context) (*http.Response, error) {
		n := calls.Add(1)
		if n == 1 {
			return fakeResp(429, nil, ""), nil
		}
		return fakeResp(200, nil, ""), nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestJitterZeroIsIdentity(t *testing.T) {
	d := 100 * time.Millisecond
	got := applyJitter(d, 0, nil)
	if got != d {
		t.Errorf("jitter=0 changed duration: %v", got)
	}
}
