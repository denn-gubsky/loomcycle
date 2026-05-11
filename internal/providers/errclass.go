package providers

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
)

// ErrorClass categorises a provider call's terminal error so the loop's
// v0.8.2 runtime-fallback path can decide whether to switch to the
// next provider in the user_tier's candidate list.
//
// The classification is intentionally coarse — three buckets — because
// the policy decision the loop makes is binary (fallback or propagate)
// plus a "cancellation" carve-out for caller-initiated tear-downs that
// shouldn't trigger any retry shape.
type ErrorClass int

const (
	// ErrorClassUnknown — couldn't classify; loop treats as non-
	// retryable (safer to surface than to silently cascade).
	ErrorClassUnknown ErrorClass = iota

	// ErrorClassRetryable — transient provider-side issue that may
	// resolve on a fresh attempt against a different provider. Covers:
	//
	//   - HTTP 429 (rate limit — the headline "free-tier exhausted"
	//     case; or paid-tier burst)
	//   - HTTP 500/502/503/504 (provider-side outage)
	//   - Network errors (DNS, connection refused, TCP reset)
	//   - Stream-idle deadline (v0.8.1's per-byte idle timeout firing
	//     because the provider stalled mid-stream)
	//
	// Loop policy: if FallbackPolicy.Enabled and attempts <
	// MaxAttempts, ReResolve to the next provider and continue.
	ErrorClassRetryable

	// ErrorClassPermanent — the request is bad in a way that would
	// fail identically against any other provider. Covers:
	//
	//   - HTTP 400 (bad request — payload-shape issue)
	//   - HTTP 401/403 (auth — operator config; cascading would burn
	//     through every provider's quota for no benefit)
	//   - HTTP 422 (semantic validation — same as 400)
	//
	// Loop policy: surface to caller regardless of FallbackPolicy.
	ErrorClassPermanent

	// ErrorClassCancelled — context.Canceled or a context.Cause that
	// wraps it. Caller-initiated tear-down (HTTP client disconnect,
	// cancel API hit). Loop policy: NEVER retry; the caller signalled
	// abandon. Distinct from Permanent so the loop can emit the
	// matching stop reason ("cancelled" vs "error") without parsing
	// the error message.
	ErrorClassCancelled

	// ErrorClassDeadlineExceeded — context.DeadlineExceeded on the
	// ROOT ctx (not the v0.8.1 idle-body wrap, which becomes a
	// retryable stream-read error). Loop policy: surface; the
	// caller's deadline cap has been hit and switching providers
	// won't extend it. Distinct from Permanent so callers see a
	// "deadline_exceeded" stop reason.
	ErrorClassDeadlineExceeded
)

// String returns a human-readable label for log + event payloads.
func (c ErrorClass) String() string {
	switch c {
	case ErrorClassRetryable:
		return "retryable"
	case ErrorClassPermanent:
		return "permanent"
	case ErrorClassCancelled:
		return "cancelled"
	case ErrorClassDeadlineExceeded:
		return "deadline_exceeded"
	default:
		return "unknown"
	}
}

// ClassifyError inspects err and returns its bucket. Drivers in v0.8.2
// still format their errors with `fmt.Errorf("anthropic %d: %s", ...)`
// or `fmt.Errorf("http: %w", err)`; the classifier matches on those
// shapes plus the standard errors.Is checks. A future v0.9.x can
// replace the string-matching with typed errors (a *ProviderHTTPError
// type that drivers return directly) — out of scope for PR 2.
//
// Order of checks matters: ctx-cancelled / ctx-deadline BEFORE error-
// string matching, because the wrapped ctx errors satisfy errors.Is
// even when buried under "http: ...".
func ClassifyError(err error) ErrorClass {
	if err == nil {
		return ErrorClassUnknown
	}
	// Stream-idle deadline (v0.8.1 per-byte idle wrap) surfaces as
	// "stream read: context deadline exceeded ...". errors.Is on the
	// outer err DOES report DeadlineExceeded (the wrap chain), but
	// we want to treat this as RETRYABLE (the provider stalled mid-
	// stream — a different provider might be healthy), NOT as a
	// caller-side deadline.
	//
	// Distinguish by the substring marker the body-wrap leaves in
	// the error text. Pre-empt the DeadlineExceeded branch below.
	if strings.Contains(err.Error(), "stream read: context deadline exceeded") {
		return ErrorClassRetryable
	}
	if errors.Is(err, context.Canceled) {
		return ErrorClassCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorClassDeadlineExceeded
	}
	// Status-coded errors: "anthropic 429: ...", "openai 500: ...",
	// "gemini 503: ...", "deepseek 502: ...". The drivers all use the
	// same shape — see the fmt.Errorf calls in each driver's Call().
	if code := statusFromError(err); code != 0 {
		switch {
		case code == 429:
			return ErrorClassRetryable
		case code >= 500 && code <= 599:
			return ErrorClassRetryable
		case code == 400 || code == 401 || code == 403 || code == 422:
			return ErrorClassPermanent
		}
		// Other 4xx (404/409/etc.) → Permanent. The agent / model id
		// is wrong in a way another provider won't fix.
		if code >= 400 && code <= 499 {
			return ErrorClassPermanent
		}
	}
	// Pure transport errors (`http: <wrapped>` from the drivers'
	// non-2xx return path, or a wrapped net.Error). Retryable —
	// another provider's transport may be healthy.
	if strings.HasPrefix(err.Error(), "http: ") {
		return ErrorClassRetryable
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return ErrorClassRetryable
	}
	return ErrorClassUnknown
}

// statusRe matches the leading "<name> <code>:" pattern the drivers
// emit. Anchored to start-of-string so we don't false-positive on a
// body that happens to contain the substring.
var statusRe = regexp.MustCompile(`^[a-z][a-z0-9_-]* (\d{3}):`)

// statusFromError extracts the HTTP status code from a driver-formatted
// error, or 0 if the error doesn't match the "<name> <code>:" prefix.
func statusFromError(err error) int {
	match := statusRe.FindStringSubmatch(err.Error())
	if match == nil {
		return 0
	}
	code, _ := strconv.Atoi(match[1])
	return code
}
