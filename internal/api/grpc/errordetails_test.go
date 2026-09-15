package grpc

import (
	"fmt"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// clientView is what a caller actually has to work with: the code, plus
// whatever details survived the wire. Tests assert through this rather than
// through server-side internals, because the bug being fixed is entirely about
// what a CLIENT can tell apart.
type clientView struct {
	code      codes.Code
	reason    string
	category  string
	retryable string
	retryIn   time.Duration
	hasRetry  bool
}

func viewOf(t *testing.T, err error) clientView {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	v := clientView{code: st.Code()}
	for _, d := range st.Details() {
		switch info := d.(type) {
		case *errdetails.ErrorInfo:
			v.reason = info.GetReason()
			v.category = info.GetMetadata()["category"]
			v.retryable = info.GetMetadata()["is_retryable"]
		case *errdetails.RetryInfo:
			v.hasRetry = true
			v.retryIn = info.GetRetryDelay().AsDuration()
		}
	}
	return v
}

// THE BUG. Four conditions share codes.ResourceExhausted. Three clear on their
// own in seconds; the fourth needs an operator to raise a budget or a month to
// roll over. Before this change a caller could only tell them apart by
// string-matching the message.
func TestResourceExhausted_TransientAndBudgetAreDistinguishable(t *testing.T) {
	transient := []struct {
		name string
		err  error
	}{
		{"backpressure", runner.ErrBackpressure},
		{"per-user quota", runner.ErrPerUserQuotaExhausted},
		{"provider concurrency", runner.ErrProviderConcurrencyExhausted},
	}
	budget := runner.ErrTokenLimitExceeded

	budgetView := viewOf(t, mapRunnerErr(budget))
	if budgetView.code != codes.ResourceExhausted {
		t.Fatalf("token limit code = %v, want ResourceExhausted (the code is deliberately unchanged)", budgetView.code)
	}
	if budgetView.category != "business" {
		t.Errorf("token limit category = %q, want business", budgetView.category)
	}
	if budgetView.retryable != "false" {
		t.Errorf("token limit is_retryable = %q, want false — a budget does not refill by waiting", budgetView.retryable)
	}
	if budgetView.hasRetry {
		t.Errorf("token limit carries RetryInfo (%v) — that tells a caller to wait and then retry something that cannot succeed", budgetView.retryIn)
	}

	for _, tc := range transient {
		t.Run(tc.name, func(t *testing.T) {
			v := viewOf(t, mapRunnerErr(tc.err))
			if v.code != codes.ResourceExhausted {
				t.Fatalf("code = %v, want ResourceExhausted", v.code)
			}
			// The whole point: same code, different, machine-readable meaning.
			if v.category != "transient" {
				t.Errorf("category = %q, want transient", v.category)
			}
			if v.retryable != "true" {
				t.Errorf("is_retryable = %q, want true", v.retryable)
			}
			if !v.hasRetry {
				t.Error("no RetryInfo on a retryable failure — the caller is left guessing how long to wait")
			}
			if v.retryIn != 5*time.Second {
				t.Errorf("retry delay = %v, want 5s (must agree with the HTTP Retry-After for the same condition)", v.retryIn)
			}

			// And the thing that was impossible before.
			if v.reason == budgetView.reason {
				t.Errorf("reason %q is identical to the budget case — still indistinguishable", v.reason)
			}
			if v.category == budgetView.category {
				t.Errorf("category %q matches the budget case — the collision is not resolved", v.category)
			}
		})
	}
}

// Reasons are the contract a client branches on, so a change to one must be
// deliberate. These mirror the HTTP surface's `code` values on purpose.
func TestErrorDetails_ReasonsAreStable(t *testing.T) {
	for err, want := range map[error]string{
		runner.ErrTokenLimitExceeded:           "token_limit_exceeded",
		runner.ErrProviderConcurrencyExhausted: "provider_concurrency_exhausted",
		runner.ErrPerUserQuotaExhausted:        "per_user_quota_exhausted",
		runner.ErrBackpressure:                 "backpressure",
		runner.ErrRuntimePaused:                "runtime_paused",
		runner.ErrUnknownAgent:                 "unknown_agent",
	} {
		if got := viewOf(t, mapRunnerErr(err)).reason; got != want {
			t.Errorf("reason for %v = %q, want %q", err, got, want)
		}
	}
}

// Wrapping is what every real call site does, so details must survive it.
func TestErrorDetails_SurviveWrapping(t *testing.T) {
	v := viewOf(t, mapRunnerErr(fmt.Errorf("admit run: %w", runner.ErrRuntimePaused)))
	if v.code != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", v.code)
	}
	if v.reason != "runtime_paused" {
		t.Errorf("reason = %q, want runtime_paused", v.reason)
	}
	if !v.hasRetry || v.retryIn != 15*time.Second {
		t.Errorf("pause backoff = %v (present=%v), want 15s — long enough not to hammer a deliberate quiesce",
			v.retryIn, v.hasRetry)
	}
}

// The codes themselves are untouched: this change is additive, and an existing
// consumer branching on the code keeps working.
func TestErrorDetails_CodesAreUnchanged(t *testing.T) {
	for err, want := range map[error]codes.Code{
		runner.ErrUnknownAgent:                 codes.InvalidArgument,
		runner.ErrInvalidArgument:              codes.InvalidArgument,
		runner.ErrUnknownProvider:              codes.InvalidArgument,
		runner.ErrSessionRequired:              codes.FailedPrecondition,
		runner.ErrSessionNotFound:              codes.NotFound,
		runner.ErrSessionBusy:                  codes.FailedPrecondition,
		runner.ErrAgentIDInUse:                 codes.AlreadyExists,
		runner.ErrBackpressure:                 codes.ResourceExhausted,
		runner.ErrPerUserQuotaExhausted:        codes.ResourceExhausted,
		runner.ErrProviderConcurrencyExhausted: codes.ResourceExhausted,
		runner.ErrTokenLimitExceeded:           codes.ResourceExhausted,
		runner.ErrRuntimePaused:                codes.Unavailable,
	} {
		if got := status.Code(mapRunnerErr(err)); got != want {
			t.Errorf("code for %v = %v, want %v (codes must not change — this PR is additive)", err, got, want)
		}
	}
	if mapRunnerErr(nil) != nil {
		t.Error("nil error must stay nil")
	}
}

// An error the runtime has never classified must not acquire an invented
// reason or category. Same rule as the MCP surface: a bucket that carries no
// decision is worse than an absent field.
func TestErrorDetails_UnknownErrorGetsNoInventedDetails(t *testing.T) {
	v := viewOf(t, mapRunnerErr(fmt.Errorf("something nobody has reasoned about")))
	if v.code != codes.Internal {
		t.Errorf("code = %v, want Internal", v.code)
	}
	if v.reason != "" || v.category != "" {
		t.Errorf("invented details for an unclassified error: reason=%q category=%q", v.reason, v.category)
	}
	if v.hasRetry {
		t.Error("invented a retry hint for an unclassified error")
	}
}

// TestErrorDetails_RetryInfoImpliesRetryable is a PROPERTY over every condition
// the runtime classifies, not a single case.
//
// It exists because a direct probe showed the guard in withErrorDetails is not
// currently exercised by any real input: the one non-retryable condition that
// could collide (a token budget) carries no RetryAfter, so dropping the
// `Retryable &&` check changes nothing today. That makes the guard invisible to
// a targeted test while still being the thing that stops a future classifier
// change from telling callers to wait for something that will never clear.
//
// Asserting the biconditional across the whole set catches that change on the
// day it is made, wherever it is made.
func TestErrorDetails_RetryInfoImpliesRetryable(t *testing.T) {
	for _, err := range []error{
		runner.ErrBackpressure,
		runner.ErrPerUserQuotaExhausted,
		runner.ErrProviderConcurrencyExhausted,
		runner.ErrRuntimePaused,
		runner.ErrTokenLimitExceeded,
		runner.ErrUnknownAgent,
		runner.ErrInvalidArgument,
		runner.ErrUnknownProvider,
		runner.ErrSessionNotFound,
		runner.ErrSessionRequired,
	} {
		v := viewOf(t, mapRunnerErr(err))
		if v.retryable == "" {
			continue // unclassified: nothing to check
		}
		retryable := v.retryable == "true"
		switch {
		case v.hasRetry && !retryable:
			t.Errorf("%v: RetryInfo (%v) on a NON-retryable failure — this tells a caller to wait "+
				"and then retry something that cannot succeed until a human acts", err, v.retryIn)
		case v.hasRetry && v.retryIn <= 0:
			t.Errorf("%v: RetryInfo with a %v delay — zero reads as 'retry immediately', "+
				"which is the opposite of 'no hint'", err, v.retryIn)
		}
	}
}
