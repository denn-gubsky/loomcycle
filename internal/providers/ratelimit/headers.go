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
// telling you when each bucket refills. We try Retry-After first, then take
// the larger of the two reset windows so we wait for the more constrained
// bucket.
func OpenAIRetryAfter(h http.Header) (time.Duration, bool) {
	if d, ok := parseRetryAfterSeconds(h.Get("Retry-After")); ok {
		return d, true
	}
	var max time.Duration
	for _, k := range []string{"X-Ratelimit-Reset-Requests", "X-Ratelimit-Reset-Tokens"} {
		if v := h.Get(k); v != "" {
			if d, err := time.ParseDuration(v); err == nil && d > max {
				max = d
			}
		}
	}
	if max > 0 {
		return max, true
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
