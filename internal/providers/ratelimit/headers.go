package ratelimit

import (
	"net/http"
	"strconv"
	"time"
)

// AnthropicRetryAfter parses Anthropic Messages API rate-limit headers.
// Anthropic always emits `retry-after` (integer seconds) on 429 per the
// public docs. The richer `anthropic-ratelimit-*-reset` headers are RFC 3339
// timestamps useful for proactive throttling but redundant on 429.
func AnthropicRetryAfter(h http.Header) (time.Duration, bool) {
	return parseRetryAfterSeconds(h.Get("Retry-After"))
}

// OpenAIRetryAfter parses OpenAI Chat Completions rate-limit headers.
//
// OpenAI's Retry-After is sometimes present (seconds) but not guaranteed.
// On every response (200 or 429) they emit relative durations like "120ms"
// or "12.5s" in `x-ratelimit-reset-requests` and `x-ratelimit-reset-tokens`
// telling you when each bucket refills.
//
// When Retry-After is missing and we have both reset values, we take the
// MIN of the non-zero ones rather than the max. Reasoning: a 429 means at
// least one bucket emptied, but we don't know which. Picking the max
// always succeeds on the first retry but over-waits when the empty bucket
// was the smaller one. Picking the min is responsive: if we guessed
// right, we retry at the perfect moment; if wrong, we 429 again and the
// next retry sleeps the *real* needed wait. Total wall-clock wait is
// the same as max in the wrong-guess case but we use one extra attempt
// budget — cheap (we have 5 attempts).
func OpenAIRetryAfter(h http.Header) (time.Duration, bool) {
	if d, ok := parseRetryAfterSeconds(h.Get("Retry-After")); ok {
		return d, true
	}
	var min time.Duration
	for _, k := range []string{"X-Ratelimit-Reset-Requests", "X-Ratelimit-Reset-Tokens"} {
		if v := h.Get(k); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				if min == 0 || d < min {
					min = d
				}
			}
		}
	}
	if min > 0 {
		return min, true
	}
	return 0, false
}

// OllamaRetryAfter parses Ollama rate-limit headers. The OSS server
// doesn't 429 in practice, but Ollama Cloud may emit a standard Retry-After.
// Defensive: same shape as Anthropic.
func OllamaRetryAfter(h http.Header) (time.Duration, bool) {
	return parseRetryAfterSeconds(h.Get("Retry-After"))
}

// parseRetryAfterSeconds parses an HTTP Retry-After header in seconds form.
// HTTP also allows HTTP-date form (RFC 7231); we don't support it because
// none of the three providers use it. Returns (0, false) on missing /
// malformed.
func parseRetryAfterSeconds(v string) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(v)
	if err != nil || secs < 0 {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}
