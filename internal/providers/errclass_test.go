package providers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestClassifyError_Table(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		// Status-code paths — the headline matrix.
		{"anthropic 429", fmt.Errorf("anthropic 429: rate limit exceeded"), ErrorClassRetryable},
		{"openai 429", fmt.Errorf("openai 429: throttled"), ErrorClassRetryable},
		{"deepseek 500", fmt.Errorf("deepseek 500: internal server error"), ErrorClassRetryable},
		{"gemini 503", fmt.Errorf("gemini 503: backend unavailable"), ErrorClassRetryable},
		{"ollama 502", fmt.Errorf("ollama 502: bad gateway"), ErrorClassRetryable},
		{"openai 504", fmt.Errorf("openai 504: gateway timeout"), ErrorClassRetryable},
		// Permanent — would fail on next provider too.
		{"anthropic 400", fmt.Errorf("anthropic 400: bad request"), ErrorClassPermanent},
		{"openai 401", fmt.Errorf("openai 401: invalid api key"), ErrorClassPermanent},
		{"deepseek 403", fmt.Errorf("deepseek 403: forbidden"), ErrorClassPermanent},
		{"gemini 422", fmt.Errorf("gemini 422: unprocessable entity"), ErrorClassPermanent},
		{"openai 404", fmt.Errorf("openai 404: model not found"), ErrorClassPermanent},
		// Deprecated — distinct from generic 404. Body contains a
		// retirement marker. The real exemplar from 2026-05-15 when
		// gemini-2.0-flash was retired:
		{"gemini 404 deprecated", fmt.Errorf(`gemini 404: {"error":{"code":404,"message":"This model models/gemini-2.0-flash is no longer available to new users. Please update your code to use a newer model","status":"NOT_FOUND"}}`), ErrorClassDeprecated},
		{"anthropic 404 deprecated", fmt.Errorf("anthropic 404: this model has been deprecated"), ErrorClassDeprecated},
		{"openai 404 retired", fmt.Errorf("openai 404: model retired, please use the latest version"), ErrorClassDeprecated},
		// 404 without a retirement marker stays Permanent (typo / wrong
		// model id). False-negative on deprecation is acceptable UX.
		{"openai 404 plain", fmt.Errorf("openai 404: not found"), ErrorClassPermanent},
		// Ctx-side outcomes.
		{"ctx canceled", context.Canceled, ErrorClassCancelled},
		{"ctx canceled wrapped", fmt.Errorf("call: %w", context.Canceled), ErrorClassCancelled},
		{"ctx deadline exceeded", context.DeadlineExceeded, ErrorClassDeadlineExceeded},
		// v0.8.1 stream-idle: outer ctx is fine, body-wrap canceled
		// MID-STREAM. errors.Is reports DeadlineExceeded but we treat
		// as Retryable — different provider may be healthy.
		{"stream-idle", fmt.Errorf("provider error: stream read: context deadline exceeded"), ErrorClassRetryable},
		// Transport.
		{"http wrapped", fmt.Errorf("http: connection refused"), ErrorClassRetryable},
		{"net.OpError wrapped", fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), ErrorClassRetryable},
		// Garbage in → unknown out (loop treats as non-retryable —
		// safer to surface than silently cascade).
		{"unknown plain", errors.New("something weird happened"), ErrorClassUnknown},
		{"nil", nil, ErrorClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.err)
			if got != tc.want {
				t.Errorf("ClassifyError(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

// TestClassifyError_StreamIdleHasPriorityOverDeadlineExceeded pins
// the order-of-checks invariant: the v0.8.1 stream-idle marker MUST
// be detected before the generic DeadlineExceeded branch fires, even
// though the wrapped error chain satisfies errors.Is(...,
// context.DeadlineExceeded). Without this priority, every stream-idle
// would be misclassified as caller-deadline and lose the retry.
func TestClassifyError_StreamIdleHasPriorityOverDeadlineExceeded(t *testing.T) {
	wrapped := fmt.Errorf("stream read: context deadline exceeded: %w", context.DeadlineExceeded)
	if got := ClassifyError(wrapped); got != ErrorClassRetryable {
		t.Errorf("got %s; want retryable (stream-idle must beat ctx.DeadlineExceeded)", got)
	}
}

func TestErrorClass_StringIsHumanReadable(t *testing.T) {
	cases := map[ErrorClass]string{
		ErrorClassUnknown:          "unknown",
		ErrorClassRetryable:        "retryable",
		ErrorClassPermanent:        "permanent",
		ErrorClassCancelled:        "cancelled",
		ErrorClassDeadlineExceeded: "deadline_exceeded",
	}
	for cls, want := range cases {
		if got := cls.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", cls, got, want)
		}
	}
}
