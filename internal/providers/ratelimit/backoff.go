// Package ratelimit provides shared 429-retry handling for provider drivers.
//
// Why a shared helper: each provider (Anthropic, OpenAI, Ollama) returns 429
// with different headers, but the response is the same shape for our
// purposes — drain body, sleep some duration, retry the exact same request.
// The helper owns the loop + sleep + ctx logic; per-provider parsers in
// headers.go know how to read each provider's retry-after signal.
//
// Why retrying inside the driver matters: without it, a 429 surfaces as an
// EventError, the loop's run terminates with status=failed, and the agent's
// conversation context is lost. Retrying preserves the entire request
// (messages, tools, system blocks, cache breakpoints) by virtue of holding
// the marshalled body bytes outside the retry loop and re-sending them.
package ratelimit

import (
	"context"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http"
	"time"
)

// ParseFn extracts a retry-after duration from response headers. Returns
// (delay, ok) where ok=false means no retry-after-equivalent header was
// found and the caller should fall back to the exponential schedule.
type ParseFn func(http.Header) (time.Duration, bool)

// Config configures the retry behaviour. All fields except ParseHeader
// have sensible defaults applied by Do.
type Config struct {
	// ParseHeader extracts a retry-after wait from response headers.
	// Required.
	ParseHeader ParseFn

	// Provider is a label included in retry logs. Optional.
	Provider string

	// MaxAttempts is the maximum number of attempts (including the first).
	// Default: 5.
	MaxAttempts int

	// MaxTotalWait caps the total time spent waiting across all retries.
	// Once exceeded, the last 429 response is returned without further
	// retry. Default: 5 minutes.
	MaxTotalWait time.Duration

	// Schedule is the fallback delay schedule used when ParseHeader
	// returns ok=false. Indexed by attempt number (0 = first retry).
	// Default: 10s, 20s, 40s, 60s, 120s.
	Schedule []time.Duration

	// Jitter is the fraction (0..1) of the computed delay to randomise
	// per retry. Default: 0.2 (±20%). Set to 0 in tests for determinism.
	Jitter float64

	// OnRetry is called before each sleep with diagnostic info. The
	// default writes a structured log line via the standard logger.
	OnRetry func(provider string, attempt int, wait time.Duration, reason string)

	// rng is for tests to inject a deterministic source. nil = global rand.
	rng *rand.Rand
}

// Default schedule applied when no Retry-After header is parsed.
var defaultSchedule = []time.Duration{
	10 * time.Second,
	20 * time.Second,
	40 * time.Second,
	60 * time.Second,
	120 * time.Second,
}

func (c *Config) applyDefaults() {
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 5
	}
	if c.MaxTotalWait <= 0 {
		c.MaxTotalWait = 5 * time.Minute
	}
	if len(c.Schedule) == 0 {
		c.Schedule = defaultSchedule
	}
	if c.Jitter < 0 {
		c.Jitter = 0
	}
	if c.OnRetry == nil {
		c.OnRetry = defaultOnRetry
	}
}

func defaultOnRetry(provider string, attempt int, wait time.Duration, reason string) {
	log.Printf(`{"event":"rate_limit_retry","provider":%q,"attempt":%d,"wait":%q,"reason":%q}`,
		provider, attempt, wait.String(), reason)
}

// Reason strings passed to OnRetry.
const (
	ReasonHeader   = "retry-after header"
	ReasonSchedule = "exponential backoff"
)

// Do calls attempt repeatedly until it returns a non-429 response or the
// retry budget is exhausted. The returned response is the first non-429 (on
// success) or the last 429 (on budget exhaustion); in both cases its body
// has NOT been read — caller owns it.
//
// Bodies of intermediate 429 responses are drained + closed by Do so the
// underlying TCP connection can be reused for the retry.
//
// ctx propagates into each attempt and into the inter-attempt sleep; a
// cancelled ctx breaks out of the sleep immediately and returns ctx.Err().
//
// Network errors from attempt are returned immediately without retry; this
// helper is specifically for HTTP 429, not for arbitrary transient failures.
func Do(ctx context.Context, cfg Config, attempt func(ctx context.Context) (*http.Response, error)) (*http.Response, error) {
	if cfg.ParseHeader == nil {
		return nil, errors.New("ratelimit: ParseHeader is required")
	}
	cfg.applyDefaults()

	var totalWait time.Duration
	var lastResp *http.Response

	for i := 0; i < cfg.MaxAttempts; i++ {
		resp, err := attempt(ctx)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}
		lastResp = resp

		// Last attempt — return the 429 with its body intact so the caller
		// can read the error message.
		if i+1 >= cfg.MaxAttempts {
			break
		}

		// Compute wait
		wait, fromHeader := cfg.ParseHeader(resp.Header)
		reason := ReasonHeader
		if !fromHeader {
			reason = ReasonSchedule
			idx := i
			if idx >= len(cfg.Schedule) {
				idx = len(cfg.Schedule) - 1
			}
			wait = cfg.Schedule[idx]
		}
		wait = applyJitter(wait, cfg.Jitter, cfg.rng)

		// Total-wait budget guard.
		if totalWait+wait > cfg.MaxTotalWait {
			break
		}

		// Drain + close so the connection is reusable for the retry.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()

		cfg.OnRetry(cfg.Provider, i+1, wait, reason)

		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		totalWait += wait
	}
	// Budget exhausted; return the last 429 untouched (body intact for
	// caller's normal error path).
	return lastResp, nil
}

// applyJitter returns d ± frac×d. With frac=0 returns d unchanged. With
// rng==nil uses the global rand.
func applyJitter(d time.Duration, frac float64, rng *rand.Rand) time.Duration {
	if frac <= 0 {
		return d
	}
	delta := float64(d) * frac
	var off float64
	if rng != nil {
		off = rng.Float64()*2*delta - delta
	} else {
		off = rand.Float64()*2*delta - delta
	}
	out := time.Duration(float64(d) + off)
	if out < 0 {
		return 0
	}
	return out
}
