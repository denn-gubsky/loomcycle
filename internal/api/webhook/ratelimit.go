package webhook

import (
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/config"
)

const (
	defaultRequestsPerMinute = 60
	defaultBurst             = 10
)

// tokenBucket is a classic token-bucket limiter for one WebhookDef. Tokens
// refill continuously at refillPerSec; allow() removes one token when
// available. Per-replica — the cluster-wide story is intentionally out of
// scope (RFC H Layer-2/L7 concern), this bounds a single replica's intake.
type tokenBucket struct {
	mu           sync.Mutex
	capacity     float64 // burst ceiling
	tokens       float64
	refillPerSec float64
	last         time.Time
	// limits is the def config the bucket was built from, as configured
	// (before defaulting). A def edit changes it, which rebuilds the bucket.
	limits config.WebhookRateLimit
}

func newTokenBucket(requestsPerMinute, burst int, now time.Time) *tokenBucket {
	if requestsPerMinute <= 0 {
		requestsPerMinute = defaultRequestsPerMinute
	}
	if burst <= 0 {
		burst = defaultBurst
	}
	return &tokenBucket{
		capacity:     float64(burst),
		tokens:       float64(burst),
		refillPerSec: float64(requestsPerMinute) / 60.0,
		last:         now,
	}
}

// allow refills based on elapsed time, then consumes one token. Returns
// (true, 0) when a token was available; (false, retryAfter) when the bucket
// is empty, where retryAfter is the wait until one token refills.
func (b *tokenBucket) allow(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * b.refillPerSec
		if b.tokens > b.capacity {
			b.tokens = b.capacity
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	// Time until one full token is available again.
	needed := 1 - b.tokens
	var retry time.Duration
	if b.refillPerSec > 0 {
		retry = time.Duration(needed/b.refillPerSec*float64(time.Second)) + time.Second
	} else {
		retry = time.Minute
	}
	return false, retry
}

// rateLimiter holds one token bucket per webhook def, keyed by webhookKey
// (the def's owner + name). Buckets are created lazily on first request,
// using that Def's rate_limit config, and rebuilt when the config changes.
//
// Keying on the name alone made two tenants' same-named webhooks share one
// bucket (one tenant's traffic throttled the other's), sized by whichever
// def reached it first — and a rate_limit edit took effect only on restart.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	now     func() time.Time
}

func newRateLimiter(now func() time.Time) *rateLimiter {
	if now == nil {
		now = time.Now
	}
	return &rateLimiter{
		buckets: make(map[string]*tokenBucket),
		now:     now,
	}
}

// allow checks the webhook's bucket (key is webhookKey's value), creating it
// from rl on first use and rebuilding it — full, at the new rate — when rl
// differs from the limits it was built with. Returns (true, 0) when
// permitted; (false, retryAfter) when the per-Def rate is exceeded (server
// maps to 429 + Retry-After).
func (r *rateLimiter) allow(key string, rl config.WebhookRateLimit) (bool, time.Duration) {
	now := r.now()
	r.mu.Lock()
	b, ok := r.buckets[key]
	if !ok || b.limits != rl {
		b = newTokenBucket(rl.RequestsPerMinute, rl.Burst, now)
		b.limits = rl
		r.buckets[key] = b
	}
	r.mu.Unlock()
	return b.allow(now)
}
