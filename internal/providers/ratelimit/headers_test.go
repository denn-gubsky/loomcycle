package ratelimit

import (
	"net/http"
	"testing"
	"time"
)

func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestAnthropicRetryAfter(t *testing.T) {
	cases := []struct {
		name string
		h    http.Header
		want time.Duration
		ok   bool
	}{
		{"missing header", headers(), 0, false},
		{"valid seconds", headers("Retry-After", "30"), 30 * time.Second, true},
		{"zero seconds", headers("Retry-After", "0"), 0, true},
		{"non-numeric", headers("Retry-After", "soon"), 0, false},
		{"negative rejected", headers("Retry-After", "-5"), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := AnthropicRetryAfter(tc.h)
			if d != tc.want || ok != tc.ok {
				t.Errorf("got (%v, %v), want (%v, %v)", d, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestOpenAIRetryAfterPrefersRetryAfter(t *testing.T) {
	// When both Retry-After and x-ratelimit-reset-* are present, Retry-After
	// wins (it's the canonical signal; reset-* are for proactive throttling).
	h := headers(
		"Retry-After", "5",
		"X-Ratelimit-Reset-Requests", "10s",
		"X-Ratelimit-Reset-Tokens", "20s",
	)
	d, ok := OpenAIRetryAfter(h)
	if !ok || d != 5*time.Second {
		t.Errorf("got (%v, %v), want (5s, true)", d, ok)
	}
}

func TestOpenAIRetryAfterFallsBackToResetTokens(t *testing.T) {
	// No Retry-After; pick the bigger reset.
	h := headers(
		"X-Ratelimit-Reset-Requests", "100ms",
		"X-Ratelimit-Reset-Tokens", "12.5s",
	)
	d, ok := OpenAIRetryAfter(h)
	if !ok || d != 12500*time.Millisecond {
		t.Errorf("got (%v, %v), want (12.5s, true)", d, ok)
	}
}

func TestOpenAIRetryAfterMissingAllReturnsFalse(t *testing.T) {
	d, ok := OpenAIRetryAfter(headers())
	if ok || d != 0 {
		t.Errorf("got (%v, %v), want (0, false)", d, ok)
	}
}

func TestOpenAIRetryAfterIgnoresMalformedDuration(t *testing.T) {
	// "later" is not a Go duration literal; should be ignored, not panic.
	h := headers("X-Ratelimit-Reset-Requests", "later")
	d, ok := OpenAIRetryAfter(h)
	if ok || d != 0 {
		t.Errorf("got (%v, %v), want (0, false)", d, ok)
	}
}

func TestOllamaRetryAfter(t *testing.T) {
	h := headers("Retry-After", "15")
	d, ok := OllamaRetryAfter(h)
	if !ok || d != 15*time.Second {
		t.Errorf("got (%v, %v), want (15s, true)", d, ok)
	}
}
