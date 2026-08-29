package memory

// observed.go — RFC CL phase 1: the observed-time window predicate.
//
// This file owns ALL of the window policy: parsing the caller's mode, widening the
// bounds by slack, and deciding what happens to a row nobody dated. The store is
// handed pre-widened bounds and applies them literally, so the arithmetic exists
// once and cannot drift between the sqlite and postgres backends.

import (
	"fmt"
	"strings"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// ObservedMissing is what a query does with a row that has no observed time.
//
// The default is deliberately the SAFE one rather than the strict one. A row's
// observed time is when it was SAID, and people recount events after they happen:
// measured on the LoCoMo corpus the lag runs from -3 to +10 days with a median of
// +1, so an exact-day window contains the ground-truth evidence in 1 case out of 8.
// A caller who reaches for a time filter and gets a hard one back would be handed a
// silent, confident regression.
type ObservedMissing string

const (
	// MissingOff ignores the predicate entirely — the zero value, so a query that
	// does not mention time behaves exactly as it did before this existed.
	MissingOff ObservedMissing = ""
	// MissingPrefer returns undated rows but ranks them below in-window dated ones.
	// This is the documented default whenever a window IS supplied.
	MissingPrefer ObservedMissing = "prefer"
	// MissingRequire drops undated rows. Honest but sharp: on a corpus nobody dated
	// this returns nothing at all, which is why it is never the default.
	MissingRequire ObservedMissing = "require"
)

// DefaultSlack widens a window on both sides. Three days is a MEASURED number, not
// a guess: on LoCoMo's day-precision questions it captures the evidence in 7 cases
// out of 8, where an unwidened window captures 1 and +-14d captures all 8. Three
// buys most of the recall for the least dilution of a narrow query.
const DefaultSlack = 72 * time.Hour

// ObservedWindow is the caller's time predicate, before widening.
type ObservedWindow struct {
	From    time.Time
	To      time.Time
	Slack   time.Duration
	Missing ObservedMissing
}

// Active reports whether the predicate constrains anything. An explicit
// discriminator rather than "are the bounds zero": a caller may legitimately ask
// for `require` with no window at all, meaning "only rows someone dated".
func (w ObservedWindow) Active() bool {
	return !w.From.IsZero() || !w.To.IsZero() || w.Missing == MissingRequire
}

// Filter renders the window onto a store filter, applying slack. Bounds go to the
// store already widened; the store owns no policy.
func (w ObservedWindow) Filter(f store.MemorySearchFilter) store.MemorySearchFilter {
	if !w.Active() {
		return f
	}
	slack := w.Slack
	if slack < 0 {
		slack = 0
	}
	if !w.From.IsZero() {
		f.ObservedFrom = w.From.Add(-slack)
	}
	if !w.To.IsZero() {
		f.ObservedTo = w.To.Add(slack)
	}
	f.RequireObserved = w.Missing == MissingRequire
	return f
}

// InWindow reports whether a row's observed time falls inside the WIDENED window.
// An undated row is never "in window" — it is separately counted as untimed.
func (w ObservedWindow) InWindow(observedAt time.Time) bool {
	if observedAt.IsZero() {
		return false
	}
	slack := w.Slack
	if slack < 0 {
		slack = 0
	}
	if !w.From.IsZero() && observedAt.Before(w.From.Add(-slack)) {
		return false
	}
	if !w.To.IsZero() && !observedAt.Before(w.To.Add(slack)) {
		return false
	}
	return true
}

// TimeFilterReport is what the predicate did, reported back to the caller.
//
// Without it, "nothing happened in that window" and "nothing in this scope is
// dated" are the same empty list, and an agent cannot tell a real absence from an
// unusable index. The counts describe the CANDIDATE POOL the ranker saw, not the
// whole scope — named as such rather than implying a scope-wide census the search
// never performed.
type TimeFilterReport struct {
	Mode         ObservedMissing `json:"mode"`
	SlackSeconds int64           `json:"slack_seconds"`
	InWindow     int             `json:"in_window"`
	OutOfWindow  int             `json:"out_of_window"`
	Untimed      int             `json:"untimed"`
}

// ParseObservedMissing maps the wire string onto the typed mode. An unknown value
// is an ERROR here, unlike the source selector which drops unknowns: `sources` can
// widen safely, but a mistyped `missing` would silently pick a different policy for
// which rows are dropped, and the caller would never learn of it.
func ParseObservedMissing(v string) (ObservedMissing, error) {
	switch ObservedMissing(strings.ToLower(strings.TrimSpace(v))) {
	case MissingOff:
		return MissingOff, nil
	case MissingPrefer:
		return MissingPrefer, nil
	case MissingRequire:
		return MissingRequire, nil
	}
	return MissingOff, fmt.Errorf("unknown missing mode %q: use \"prefer\" or \"require\"", v)
}

// ParseSlack accepts a Go duration string ("3d" is spelled "72h"), plus a bare
// day form because "3d" is what anyone reaching for this will type first.
func ParseSlack(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return DefaultSlack, nil
	}
	if strings.HasSuffix(v, "d") {
		var days float64
		if _, err := fmt.Sscanf(v, "%fd", &days); err == nil {
			if days < 0 {
				return 0, fmt.Errorf("slack %q is negative", v)
			}
			return time.Duration(days * 24 * float64(time.Hour)), nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("slack %q is not a duration (try \"3d\" or \"72h\"): %w", v, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("slack %q is negative", v)
	}
	return d, nil
}
