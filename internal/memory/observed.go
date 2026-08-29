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

	// AsOf asks a DIFFERENT question from From/To: not "when was this said" but
	// "what was true at this instant" (RFC CL phase 2). Composable with the window —
	// "what did we know in November about October" needs both.
	//
	// Always a hard filter, with no soft reading, because there isn't one: a row
	// valid over a different interval is not a weaker answer to "what was true on the
	// 3rd", it is a wrong one. Missing governs only whether rows with NO interval
	// survive.
	AsOf time.Time
}

// Active reports whether the predicate constrains anything. An explicit
// discriminator rather than "are the bounds zero": a caller may legitimately ask
// for `require` with no window at all, meaning "only rows someone dated".
func (w ObservedWindow) Active() bool {
	return !w.From.IsZero() || !w.To.IsZero() || !w.AsOf.IsZero() || w.Missing == MissingRequire
}

// Filter renders the window onto a store filter, applying slack. Bounds go to the
// store already widened; the store owns no policy.
func (w ObservedWindow) Filter(f store.MemorySearchFilter) store.MemorySearchFilter {
	if !w.Active() {
		return f
	}
	// PREFER APPLIES NO STORE BOUNDS AT ALL. The window is purely a ranking signal
	// there, so nothing is dropped and the report can count all three categories.
	//
	// Bounding the store in prefer mode looked harmless and was not: it dropped
	// out-of-window DATED rows while keeping undated ones, so a row known to be from
	// November was treated more harshly than a row with no date at all. That is the
	// regression this predicate exists to avoid — the evidence for "the last week of
	// October" was spoken on November 2, and prefer would have discarded it while
	// keeping every undated row in the corpus.
	// AsOf is applied in BOTH modes — see the field comment: there is no soft
	// reading of "what was true then". `missing` still decides whether a row with no
	// interval survives, which is the part that has a defensible soft reading.
	if !w.AsOf.IsZero() {
		f.AsOf = w.AsOf
		f.RequireValid = w.Missing == MissingRequire
	}
	if w.Missing != MissingRequire {
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
	f.RequireObserved = true
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

// WhenInput is the wire shape of the `when` predicate, shared VERBATIM by the
// in-band Memory tool and the off-run HTTP search.
//
// One struct and one parser on purpose. This subsystem has shipped two production
// bugs from exactly the opposite arrangement — parseSources and parseMemorySources
// drifting apart so `sources:["notes"]` meant different things on the two surfaces,
// and the recall projection drifting from search's. A predicate that decides which
// rows get DROPPED is the last place to hand-maintain two copies.
type WhenInput struct {
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Slack   string `json:"slack,omitempty"`
	Missing string `json:"missing,omitempty"`
	AsOf    string `json:"as_of,omitempty"`
}

// ParseWhen turns the wire shape into the typed window.
//
// Timestamps are RFC3339. Unparseable input is an ERROR rather than a silent
// no-op: a caller who mistypes a date and gets an unfiltered search back would
// read the results as time-filtered when they are not, which is a worse outcome
// than being told the date is wrong.
func ParseWhen(in *WhenInput) (ObservedWindow, error) {
	var w ObservedWindow
	if in == nil {
		return w, nil
	}
	parseTS := func(field, v string) (time.Time, error) {
		if strings.TrimSpace(v) == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
		if err != nil {
			return time.Time{}, fmt.Errorf("when.%s %q is not an RFC3339 timestamp (e.g. 2023-10-01T00:00:00Z)", field, v)
		}
		return t, nil
	}
	var err error
	if w.From, err = parseTS("from", in.From); err != nil {
		return ObservedWindow{}, err
	}
	if w.To, err = parseTS("to", in.To); err != nil {
		return ObservedWindow{}, err
	}
	if !w.From.IsZero() && !w.To.IsZero() && w.To.Before(w.From) {
		return ObservedWindow{}, fmt.Errorf("when.to (%s) is before when.from (%s)", in.To, in.From)
	}
	if w.AsOf, err = parseTS("as_of", in.AsOf); err != nil {
		return ObservedWindow{}, err
	}
	if w.Slack, err = ParseSlack(in.Slack); err != nil {
		return ObservedWindow{}, err
	}
	if w.Missing, err = ParseObservedMissing(in.Missing); err != nil {
		return ObservedWindow{}, err
	}
	// A window with no stated policy gets the SAFE one. Defaulting to `require`
	// here would turn every "search around October" into a hard filter, which on an
	// undated corpus returns nothing at all.
	if w.Missing == MissingOff && (!w.From.IsZero() || !w.To.IsZero()) {
		w.Missing = MissingPrefer
	}
	return w, nil
}

// SetTimes is the parsed form of the three write-side times, shared by the in-band
// tool and the off-run PUT so both accept exactly the same vocabulary.
type SetTimes struct {
	ObservedAt time.Time
	ValidAt    time.Time
	InvalidAt  time.Time
}
