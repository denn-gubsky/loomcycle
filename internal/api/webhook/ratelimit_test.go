package webhook

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

func TestRateLimit_BurstThenExceeded_Returns429Signal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rl := newRateLimiter(fixedClock(now))
	cfg := config.WebhookRateLimit{RequestsPerMinute: 60, Burst: 3}

	// Burst of 3 should pass.
	for i := 0; i < 3; i++ {
		if ok, _ := rl.allow("hook", cfg); !ok {
			t.Fatalf("burst request %d unexpectedly limited", i+1)
		}
	}
	// 4th within the same instant (no refill) must be limited.
	ok, retry := rl.allow("hook", cfg)
	if ok {
		t.Fatal("4th request past burst was allowed")
	}
	if retry <= 0 {
		t.Fatalf("expected positive Retry-After, got %v", retry)
	}
}

func TestRateLimit_RefillAfterTime_AllowsAgain(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := base
	rl := newRateLimiter(func() time.Time { return clock })
	cfg := config.WebhookRateLimit{RequestsPerMinute: 60, Burst: 1} // 1 token/sec

	if ok, _ := rl.allow("hook", cfg); !ok {
		t.Fatal("first request limited")
	}
	if ok, _ := rl.allow("hook", cfg); ok {
		t.Fatal("second immediate request should be limited")
	}
	// Advance 2s → ~2 tokens refilled (capped at burst=1).
	clock = base.Add(2 * time.Second)
	if ok, _ := rl.allow("hook", cfg); !ok {
		t.Fatal("request after refill was limited")
	}
}

func TestRateLimit_PerWebhookIsolation(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	rl := newRateLimiter(fixedClock(now))
	cfg := config.WebhookRateLimit{RequestsPerMinute: 60, Burst: 1}

	rl.allow("hook-a", cfg) // exhaust hook-a
	if ok, _ := rl.allow("hook-a", cfg); ok {
		t.Fatal("hook-a should be exhausted")
	}
	// hook-b has its own bucket.
	if ok, _ := rl.allow("hook-b", cfg); !ok {
		t.Fatal("hook-b limited by hook-a's bucket")
	}
}
