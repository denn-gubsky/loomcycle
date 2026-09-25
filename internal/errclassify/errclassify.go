// Package errclassify turns the runtime's typed errors into the coarse
// category a caller needs to decide what to do next.
//
// WHY IT LIVES HERE AND NOT IN internal/tools: the sentinels it matches on are
// spread across runner, resolve, store, providers and concurrency, and
// internal/runner already reaches internal/tools transitively (runner → loop →
// tools). A classifier inside internal/tools therefore could not see half of
// what it needs to classify. This package sits above all of them instead, and
// nothing imports it back.
//
// WHY A SWITCH AND NOT A REGISTRY: a registry (each package registering its own
// sentinels from init) would invert the imports neatly, but it is a
// package-level mutable global — which this repo does not allow — and it makes
// the set of classified errors invisible to a reader and unenumerable to a
// test. An explicit switch is greppable, and the drift test below it can prove
// the switch and the HTTP status map agree.
package errclassify

import (
	"context"
	"errors"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/concurrency"
	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
	"github.com/denn-gubsky/loomcycle/internal/store"
	"github.com/denn-gubsky/loomcycle/internal/tools"
)

// backpressureBackoff mirrors the `Retry-After: 5` the HTTP surface already
// emits for the same conditions. The point of carrying a hint at all is that
// the two surfaces say ONE thing about how long to wait — if this drifts from
// internal/api/http, one of them is lying to a caller.
const backpressureBackoff = 5 * time.Second

// pausedBackoff is the hint for a quiesced runtime. A pause always ends, so it
// is transient rather than a refusal — but it is deliberate, and an agent that
// retries it every 200ms defeats the quiesce the pause exists to create. Longer
// than backpressureBackoff for that reason: backpressure clears when a slot
// frees, a pause clears when a human is done.
const pausedBackoff = 15 * time.Second

// CategoryOf classifies err. The second return is false when err is not one of
// the conditions this runtime knows how to categorise — callers must treat that
// as "leave it unclassified", NOT as a fallback bucket. Inventing a category for
// an error nobody has reasoned about is how an agent ends up retrying something
// that can never succeed.
//
// Order matters: context cancellation is checked before anything else, because
// a cancelled ctx can be wrapped underneath a domain error and the caller's
// tear-down should never be reported as a retryable outage.
func CategoryOf(err error) (tools.ErrorInfo, bool) {
	if err == nil {
		return tools.ErrorInfo{}, false
	}

	// Caller went away. Not retryable, not the tool's fault, and deliberately
	// NOT classified: there is no caller left to hand a category to, and
	// labelling it would put a misleading error in the transcript.
	if errors.Is(err, context.Canceled) {
		return tools.ErrorInfo{}, false
	}

	switch {

	// --- transient: valid request, try again ---

	case errors.Is(err, resolve.ErrTierUnavailable),
		errors.Is(err, resolve.ErrPinUnavailable):
		return transient(
			"No provider in the requested tier is available right now. The request is valid and should succeed once a provider recovers.",
			nil,
		), true

	case errors.Is(err, runner.ErrBackpressure),
		errors.Is(err, runner.ErrPerUserQuotaExhausted),
		errors.Is(err, runner.ErrProviderConcurrencyExhausted),
		isProviderConcurrency(err):
		return transient(
			"The runtime is at a concurrency limit. The request is valid and should succeed on retry.",
			ptr(backpressureBackoff),
		), true

	case errors.Is(err, runner.ErrRuntimePaused):
		return transient(
			"The runtime is paused and is not admitting new runs. This is temporary — an operator resume will clear it. Wait before retrying rather than polling.",
			ptr(pausedBackoff),
		), true

	case errors.Is(err, context.DeadlineExceeded):
		return transient(
			"The operation exceeded its time budget before completing. Retrying may succeed, particularly with a smaller request.",
			nil,
		), true

	// --- business: well-formed, refused by a rule ---

	// 429 on HTTP, alongside genuine backpressure — but a budget does not
	// refill by waiting, so telling an agent to retry it is telling it to
	// burn iterations against a wall. This is the clearest case where the
	// category cannot be read off the HTTP status.
	case errors.Is(err, runner.ErrTokenLimitExceeded):
		return business(
			"The token budget for this scope is exhausted for the current period. Retrying will not help; the budget must be raised or the period must roll over.",
		), true

	case errors.Is(err, resolve.ErrTierAgentNotAvailable):
		return business(
			"This agent is not available for the caller's user_tier. Retrying will not help; use an agent the tier permits, or ask an operator to widen it.",
		), true

	case errors.Is(err, store.ErrVectorUnsupported),
		errors.Is(err, store.ErrEmbedderNotConfigured),
		errors.Is(err, store.ErrCapabilityUnsupported):
		return business(
			"This deployment is not configured for that capability. Retrying will not help — use a different approach, or ask an operator to enable it.",
		), true

	case errors.Is(err, store.ErrDimensionMismatch):
		return business(
			"Stored embeddings were written with a different embedding model than the one configured now. Retrying will not help; the stored rows must be re-embedded.",
		), true

	case errors.Is(err, store.ErrMemoryQuotaExceeded),
		errors.Is(err, store.ErrMemoryValueTooLarge):
		return business(
			"The write exceeds a configured storage limit. Retrying the same write will fail; store less, or ask an operator to raise the limit.",
		), true

	// --- permission: caller lacks authority it could be granted ---

	// NOTE: nothing here is about another tenant's rows. A cross-tenant miss
	// folds to an opaque not-found by design, and must stay that way — a
	// permission category on such a read would confirm the row exists.
	case errors.Is(err, resolve.ErrOperatorKeyRestricted),
		errors.Is(err, providers.ErrOperatorKeyForbidden):
		return tools.ErrorInfo{
			Category:    tools.CategoryPermission,
			Retryable:   false,
			Description: "This principal may not use the operator's provider API keys. Retrying will not help — supply the tenant's own provider credential, or ask an operator for the scope that permits it.",
		}, true

	// --- validation: caller can fix this alone ---

	case errors.Is(err, runner.ErrUnknownAgent),
		errors.Is(err, resolve.ErrUnknownAgent):
		return validation(
			"No agent exists with that name. List the available agents and call again with one of them.",
		), true

	case errors.Is(err, runner.ErrUnknownProvider):
		return validation(
			"No provider is configured under that name. Call again naming a configured provider, or omit the pin and let the tier resolve one.",
		), true

	case errors.Is(err, runner.ErrInvalidArgument),
		errors.Is(err, resolve.ErrInvalidArgument):
		return validation(
			"The request is malformed. Correct the named field and call again.",
		), true

	case errors.Is(err, runner.ErrSessionNotFound):
		return validation(
			"No session exists with that id. Start a fresh run instead of continuing this one.",
		), true

	case errors.Is(err, runner.ErrRunNotConfigured):
		return tools.ErrorInfo{
			Category:    tools.CategoryBusiness,
			Retryable:   false,
			Description: "The configured run has already started or been discarded, so it cannot be started again. Read it with get_run.",
		}, true

	case errors.Is(err, runner.ErrDraftChanged):
		return tools.ErrorInfo{
			Category:    tools.CategoryBusiness,
			Retryable:   true,
			Description: "The configured run was edited while this start waited, so nothing was started. Start it again to run the edited version.",
		}, true

	case errors.Is(err, runner.ErrSessionRequired):
		return validation(
			"That action is session-bound and this deployment has no store wired for it. Call it without a session, or use a deployment with persistence.",
		), true
	}

	return tools.ErrorInfo{}, false
}

// isProviderConcurrency reports whether err is the per-provider gate's typed
// refusal. It is a struct pointer rather than a sentinel, so errors.Is on the
// runner alias does not catch it on its own.
func isProviderConcurrency(err error) bool {
	var pce *concurrency.ErrProviderConcurrencyExhausted
	return errors.As(err, &pce)
}

func transient(desc string, after *time.Duration) tools.ErrorInfo {
	return tools.ErrorInfo{
		Category:    tools.CategoryTransient,
		Retryable:   true,
		Description: desc,
		RetryAfter:  after,
	}
}

func business(desc string) tools.ErrorInfo {
	return tools.ErrorInfo{
		Category:    tools.CategoryBusiness,
		Retryable:   false,
		Description: desc,
	}
}

func validation(desc string) tools.ErrorInfo {
	return tools.ErrorInfo{
		Category:    tools.CategoryValidation,
		Retryable:   false,
		Description: desc,
	}
}

func ptr(d time.Duration) *time.Duration { return &d }
