package grpc

import (
	"errors"
	"strconv"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// errorDomain namespaces our reasons in google.rpc.ErrorInfo, per that
// message's contract: a reason is only unique within a domain.
const errorDomain = "loomcycle"

// reasonFor returns the stable machine-readable identifier for a condition.
//
// These strings deliberately MIRROR the `code` values the HTTP surface already
// puts in its JSON error bodies. A consumer that speaks both transports can
// then keep ONE table instead of two, which is the whole point of classifying
// in one place and rendering it three ways.
//
// They are lower_snake_case rather than the UPPER_SNAKE_CASE that Google's own
// services use for ErrorInfo.Reason. Matching our HTTP codes exactly is worth
// more here than matching an external house style, since the alternative is two
// spellings of the same condition inside one product.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, runner.ErrTokenLimitExceeded):
		return "token_limit_exceeded"
	case errors.Is(err, runner.ErrProviderConcurrencyExhausted):
		return "provider_concurrency_exhausted"
	case errors.Is(err, runner.ErrPerUserQuotaExhausted):
		return "per_user_quota_exhausted"
	case errors.Is(err, runner.ErrBackpressure):
		return "backpressure"
	case errors.Is(err, runner.ErrRuntimePaused):
		return "runtime_paused"
	case errors.Is(err, resolve.ErrOperatorKeyRestricted),
		errors.Is(err, providers.ErrOperatorKeyForbidden):
		return "operator_key_restricted"
	case errors.Is(err, resolve.ErrTierAgentNotAvailable):
		return "tier_agent_not_available"
	case errors.Is(err, resolve.ErrTierUnavailable):
		return "tier_unavailable"
	case errors.Is(err, resolve.ErrPinUnavailable):
		return "pin_unavailable"
	case errors.Is(err, runner.ErrUnknownAgent), errors.Is(err, resolve.ErrUnknownAgent):
		return "unknown_agent"
	case errors.Is(err, runner.ErrUnknownProvider):
		return "unknown_provider"
	case errors.Is(err, runner.ErrSessionNotFound):
		return "session_not_found"
	case errors.Is(err, runner.ErrSessionRequired):
		return "session_required"
	case errors.Is(err, runner.ErrInvalidArgument), errors.Is(err, resolve.ErrInvalidArgument):
		return "invalid_argument"
	}
	return ""
}

// withErrorDetails attaches google.rpc.ErrorInfo — and RetryInfo where there is
// a backoff to give — to a status, so a caller can tell apart conditions that
// share a gRPC code.
//
// WHY THIS EXISTS: codes.ResourceExhausted covers FOUR conditions here.
// Backpressure, the per-user quota and the per-provider cap are all transient
// and clear on their own in seconds. A token budget does NOT: it needs an
// operator to raise it or the month to roll over. A caller that retries the
// budget case on the same schedule as the other three burns its attempts
// against a wall, and until now the only way to tell them apart was to
// string-match the message — which the old code comment admitted.
//
// The code itself is UNCHANGED. ResourceExhausted is a defensible reading of a
// quota, existing consumers keep working, and the distinguishing information
// arrives in the details where gRPC says it belongs. This is additive.
//
// Classification comes from the shared classifier, so gRPC cannot drift from
// what the MCP surface reports for the same condition.
func withErrorDetails(st *status.Status, err error) error {
	if st == nil {
		return nil
	}
	info, classified := errclassify.CategoryOf(err)
	reason := reasonFor(err)
	if !classified && reason == "" {
		return st.Err()
	}

	ei := &errdetails.ErrorInfo{
		Reason: reason,
		Domain: errorDomain,
	}
	if classified {
		ei.Metadata = map[string]string{
			// The same two facts the MCP surface puts in structuredContent.
			"category":     string(info.Category),
			"is_retryable": strconv.FormatBool(info.Retryable),
		}
	}

	details := []protoadapt.MessageV1{ei}
	// RetryInfo rides only on a genuinely retryable failure. Attaching a delay
	// to a budget exhaustion would tell a caller to wait and then retry
	// something that cannot succeed until a human acts — the exact confusion
	// this function exists to remove.
	if classified && info.Retryable && info.RetryAfter != nil {
		details = append(details, &errdetails.RetryInfo{
			RetryDelay: durationpb.New(*info.RetryAfter),
		})
	}

	withDetails, derr := st.WithDetails(details...)
	if derr != nil {
		// Never fail an RPC because we could not decorate its error. The
		// caller still gets the code and message they got before.
		return st.Err()
	}
	return withDetails.Err()
}
