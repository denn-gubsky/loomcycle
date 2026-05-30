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

// rateLimiter holds one token bucket per webhook name. Buckets are created
// lazily on first request for a name, using that Def's rate_limit config.
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

// allow checks the named webhook's bucket, creating it from rl config on
// first use. Returns (true, 0) when permitted; (false, retryAfter) when the
// per-Def rate is exceeded (server maps to 429 + Retry-After).
func (r *rateLimiter) allow(name string, rl config.WebhookRateLimit) (bool, time.Duration) {
	now := r.now()
	r.mu.Lock()
	b, ok := r.buckets[name]
	if !ok {
		b = newTokenBucket(rl.RequestsPerMinute, rl.Burst, now)
		r.buckets[name] = b
	}
	r.mu.Unlock()
	return b.allow(now)
}
