package http

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/auth"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// runRateLimiter bounds how often each caller may start runs: one token
// bucket per (tenant, subject).
//
// Per subject and not per token, because a consumer that mints a fresh token
// for a user every day would otherwise hand that user a full bucket with each
// one. Per replica, like the webhook limiter: with N replicas a caller's
// effective rate is up to N times the setting. That is documented, and it is
// the right trade for a check that has to cost nothing — it runs before
// anything is read from a store.
type runRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*runRateBucket
	now     func() time.Time
}

type runRateBucket struct {
	tokens float64
	last   time.Time
	// limit is the rate the bucket was last used at.
	limit auth.RateLimit
}

// runRateMaxBuckets bounds the map. A bucket that has refilled says nothing a
// new bucket would not, so full ones are dropped when the map is this large.
const runRateMaxBuckets = 65536

func newRunRateLimiter(now func() time.Time) *runRateLimiter {
	if now == nil {
		now = time.Now
	}
	return &runRateLimiter{buckets: make(map[string]*runRateBucket), now: now}
}

// allow takes one token from the caller's bucket. It returns (true, 0) when
// there was one, and (false, wait) when there was not, where wait is how long
// until one refills.
func (l *runRateLimiter) allow(tenant, subject string, limit auth.RateLimit) (bool, time.Duration) {
	now := l.now()
	burst := float64(limit.EffectiveBurst())
	perSec := float64(limit.PerMinute) / 60.0

	l.mu.Lock()
	defer l.mu.Unlock()
	key := tenant + "\x00" + subject
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= runRateMaxBuckets {
			l.dropFullLocked(now)
		}
		b = &runRateBucket{tokens: burst, last: now, limit: limit}
		l.buckets[key] = b
	}
	// Refill at the rate the bucket was LAST used at, then adopt the rate of
	// this call. A change of rate never refills the bucket: two tokens of one
	// user with different rates must not be a way to top it up by
	// alternating between them. It can only lower what is there to the new
	// burst.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(float64(b.limit.EffectiveBurst()), b.tokens+elapsed*float64(b.limit.PerMinute)/60.0)
		b.last = now
	}
	b.limit = limit
	if b.tokens > burst {
		b.tokens = burst
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / perSec * float64(time.Second))
}

// dropFullLocked removes buckets that have refilled completely.
func (l *runRateLimiter) dropFullLocked(now time.Time) {
	for k, b := range l.buckets {
		full := float64(b.limit.EffectiveBurst())
		if b.tokens+now.Sub(b.last).Seconds()*float64(b.limit.PerMinute)/60.0 >= full {
			delete(l.buckets, k)
		}
	}
}

// rateLimitedError is the refusal: it carries how long to wait, and is
// runner.ErrRateLimited to errors.Is.
type rateLimitedError struct{ RetryAfter time.Duration }

func (e *rateLimitedError) Error() string {
	return fmt.Sprintf("%s: starting runs too often; retry in %ds", runner.ErrRateLimited, retryAfterSeconds(e.RetryAfter))
}

func (e *rateLimitedError) Is(target error) bool { return target == runner.ErrRateLimited }

// RetryDelay is the wait as Retry-After states it (whole seconds, at least
// one), so gRPC's RetryInfo names the same wait the HTTP header does.
func (e *rateLimitedError) RetryDelay() time.Duration {
	return time.Duration(retryAfterSeconds(e.RetryAfter)) * time.Second
}

// retryAfterSeconds rounds a wait up to whole seconds, at least one: the
// header carries seconds, and "0" would tell a client to retry at once.
func retryAfterSeconds(d time.Duration) int {
	s := int(math.Ceil(d.Seconds()))
	if s < 1 {
		s = 1
	}
	return s
}

// SetDefaultRunRate sets the rate applied to a caller whose token carries
// none (LOOMCYCLE_RUN_RATE_PER_MINUTE / _BURST). A zero PerMinute means no
// default: such a caller is not rate limited.
func (s *Server) SetDefaultRunRate(limit auth.RateLimit) { s.defaultRunRate = limit }

// runRateFor returns the rate that applies to a principal, and whether one
// does.
//
// A rate on the token wins. Otherwise the server default applies — but not to
// an operator: an admin, a tenant operator and the legacy token run the
// deployment, and a default meant to bound end users must not be what stops
// them. A rate set on such a token by name still applies.
func (s *Server) runRateFor(p auth.Principal) (auth.RateLimit, bool) {
	if p.Restrictions.Rate != nil {
		return *p.Restrictions.Rate, true
	}
	if s.defaultRunRate.PerMinute <= 0 || p.Legacy || auth.HasScope(p.Scopes, auth.ScopeTenant) {
		return auth.RateLimit{}, false
	}
	return s.defaultRunRate, true
}

// admitPrincipalRate takes one run start from the caller's rate, or refuses
// with a *rateLimitedError.
//
// It is called ONCE per run a caller asks for, as the first thing each intake
// does: before the body is validated, before the agent is looked up, before a
// row is written or a slot is taken. A refused start is meant to cost the
// server nothing, because the case it exists for is a caller in a loop.
//
// Like the other principal checks it reads the live principal. A run started
// under a definition's own identity is not counted, and neither is a call
// made from inside a run — an agent's own tool use is its definition's
// business, not its starter's rate.
func (s *Server) admitPrincipalRate(ctx context.Context) error {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || s.runRate == nil || tools.RunID(ctx) != "" {
		return nil
	}
	limit, limited := s.runRateFor(p)
	if !limited {
		return nil
	}
	if allowed, wait := s.runRate.allow(p.TenantID, p.Subject, limit); !allowed {
		return &rateLimitedError{RetryAfter: wait}
	}
	return nil
}

// writeRateLimited is the HTTP form of the refusal: 429 with Retry-After.
func writeRateLimited(w http.ResponseWriter, err error) {
	var rl *rateLimitedError
	wait := time.Second
	if errors.As(err, &rl) {
		wait = rl.RetryAfter
	}
	w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
	writeJSONError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
}
