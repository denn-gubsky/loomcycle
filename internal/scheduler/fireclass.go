package scheduler

import (
	"errors"

	"github.com/denn-gubsky/loomcycle/internal/providers"
	"github.com/denn-gubsky/loomcycle/internal/resolve"
	"github.com/denn-gubsky/loomcycle/internal/runner"
)

// fireClass is what one run's error means for the schedule that fired it.
//
// ONE classifier serves both fire paths — fireOne and the consolidation
// fan-out — because each had its own errors.Is ladder and they drifted: the
// fan-out learned the backpressure family while neither learned the token
// budget or the pause, so an over-budget user or a pause that began mid-sweep
// read as a broken schedule on both.
type fireClass int

const (
	// fireRan: the run ended without error.
	fireRan fireClass = iota
	// fireFailed: a genuine failure. Anything not named below lands here, so
	// an error nobody has reasoned about is never quietly downgraded.
	fireFailed
	// fireUnknownAgent: a config error. No run started and none will on any
	// later fire, so it does not use up max_fires (F38) — counting it would
	// retire the schedule and present a misconfig as N normal runs.
	fireUnknownAgent
	// fireBackpressure: transient load refused admission. Nothing broke.
	fireBackpressure
	// fireDeferred: an admission rule refused the run — the scope's hard token
	// budget is spent, or the run may not use the operator's provider key and
	// the tenant has none of its own. Nothing broke; the run waits for the
	// budget period to roll over or for the tenant's own key. It still counts
	// toward max_fires: the tick happened, and the refusal repeats until
	// someone acts, so not counting it would let it re-present forever.
	fireDeferred
	// firePaused: the runtime is paused (quiescing for a snapshot). Nothing
	// broke and nothing ran. It does NOT count toward max_fires — the operator
	// paused the runtime, the schedule did no work, and the next fire after
	// resume does it. A fan-out stops dispatching on it.
	firePaused
)

// classifyFire buckets a run error. Each case is errors.Is so a wrapped
// sentinel (the runner wraps most of them with context) still classifies.
func classifyFire(err error) fireClass {
	switch {
	case err == nil:
		return fireRan
	case errors.Is(err, runner.ErrUnknownAgent):
		return fireUnknownAgent
	case errors.Is(err, runner.ErrBackpressure),
		errors.Is(err, runner.ErrPerUserQuotaExhausted),
		errors.Is(err, runner.ErrProviderConcurrencyExhausted):
		return fireBackpressure
	case errors.Is(err, runner.ErrTokenLimitExceeded),
		// The operator-key restriction has two layers: routing finds no
		// provider the tenant can key (resolve), or a pinned agent's driver
		// refuses the operator's key mid-run (providers). Same refusal.
		errors.Is(err, resolve.ErrOperatorKeyRestricted),
		errors.Is(err, providers.ErrOperatorKeyForbidden):
		return fireDeferred
	case errors.Is(err, runner.ErrRuntimePaused):
		return firePaused
	default:
		return fireFailed
	}
}

// status is the schedule's last_status for a fire of this class. Every class
// where nothing broke reads "skipped": not "failed", so it pages nobody, and
// not "completed", so a single run's on_complete hooks do not fire for work
// that never happened.
func (c fireClass) status() string {
	switch c {
	case fireRan:
		return "completed"
	case fireBackpressure, fireDeferred, firePaused:
		return "skipped"
	default:
		return "failed"
	}
}

// countsAsFire reports whether a fire of this class uses up one of the
// schedule's max_fires.
func (c fireClass) countsAsFire() bool {
	return c != fireUnknownAgent && c != firePaused
}
