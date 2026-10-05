package scheduler

import (
	"errors"
	"fmt"

	"github.com/denn-gubsky/loomcycle/internal/errclassify"
	"github.com/denn-gubsky/loomcycle/internal/errkind"
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
	// fireTeamNotStartable: the same config error for a team tick — the team
	// is missing or retired, or the schedule sets a variable it does not
	// declare or a value it refuses. No walk started and none will until the
	// schedule or the team is changed, so it does not use up max_fires either.
	fireTeamNotStartable
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
	case errors.Is(err, runner.ErrTeamNotStartable):
		return fireTeamNotStartable
	case errors.Is(err, runner.ErrBackpressure),
		errors.Is(err, runner.ErrPerUserQuotaExhausted),
		errors.Is(err, runner.ErrProviderConcurrencyExhausted):
		return fireBackpressure
	case errors.Is(err, runner.ErrTokenLimitExceeded), isOperatorKeyErr(err):
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
	return c != fireUnknownAgent && c != fireTeamNotStartable && c != firePaused
}

// isOperatorKeyErr reports whether err is the operator-key restriction. It has
// two layers: routing finds no provider the tenant can key (resolve), or a
// pinned agent's driver refuses the operator's key mid-run (providers). Same
// refusal.
func isOperatorKeyErr(err error) bool {
	return errors.Is(err, resolve.ErrOperatorKeyRestricted) ||
		errors.Is(err, providers.ErrOperatorKeyForbidden)
}

// errSubRunOperatorKeyRefused is what a pass that RAN reports when a call
// inside it was refused the operator's key — most often its extractor
// sub-agent. The pass itself completes (a code agent catches the refusal and
// holds its watermark), so RunOnce returns nil; without this the schedule
// read "completed" for a tenant that is never consolidated. It wraps the
// routing sentinel so classifyFire defers it like an admission refusal.
var errSubRunOperatorKeyRefused = fmt.Errorf("%w: a call inside the pass (its sub-agent) was refused the operator's provider key, so the pass consolidated nothing that needed it", resolve.ErrOperatorKeyRestricted)

// isOperatorKeyRefusal reports whether a failed tool call's classification is
// the operator-key refusal — the structured signal a pass's event stream
// carries when a sub-agent spawn inside it was refused.
//
// Compared against the one classifier's own output for the sentinel, never
// against error text: the category alone is not enough (permission also means
// "a scope was not granted", which no key fixes), and the classifier is what
// produced this Info in the first place, so the two cannot drift.
func isOperatorKeyRefusal(info *errkind.Info) bool {
	if info == nil {
		return false
	}
	want, ok := errclassify.CategoryOf(resolve.ErrOperatorKeyRestricted)
	return ok && info.Category == want.Category && info.Description == want.Description
}
