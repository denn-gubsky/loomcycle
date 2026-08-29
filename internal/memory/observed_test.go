package memory

import (
	"testing"
	"time"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

func mustTime(t *testing.T, v string) time.Time {
	t.Helper()
	p, err := time.Parse(time.RFC3339, v)
	if err != nil {
		t.Fatalf("parse %q: %v", v, err)
	}
	return p
}

// A window with no stated policy must default to prefer, NOT require.
//
// This is the whole safety property. Measured on LoCoMo, an exact-day hard window
// contains the ground-truth evidence in 1 case out of 8 — so a caller who asks for
// a window and silently gets a hard filter is handed a confident regression.
func TestParseWhen_WindowDefaultsToPreferNotRequire(t *testing.T) {
	w, err := ParseWhen(&WhenInput{From: "2023-10-01T00:00:00Z", To: "2023-10-04T00:00:00Z"})
	if err != nil {
		t.Fatalf("ParseWhen: %v", err)
	}
	if w.Missing != MissingPrefer {
		t.Errorf("missing = %q, want %q — a window with no stated policy must never "+
			"become a hard filter", w.Missing, MissingPrefer)
	}
	if w.Slack != DefaultSlack {
		t.Errorf("slack = %v, want the measured default %v", w.Slack, DefaultSlack)
	}
	f := w.Filter(mustFilter())
	if f.RequireObserved {
		t.Error("prefer mode set RequireObserved — undated rows must survive to be demoted, not dropped")
	}
}

// THE FOOTGUN, made explicit and measurable. An exact-day window is how a caller
// misses the row that answers the question, because a remark about an event is
// typically made a day or more AFTER it. The default slack is what rescues it.
func TestObservedWindow_ExactDayMissesTheRecountedRow_SlackRescuesIt(t *testing.T) {
	// "Which city was Calvin at on October 3, 2023?" — the turn that answers it was
	// spoken on October 4. This is a real case from the corpus, not a constructed one.
	asked := mustTime(t, "2023-10-03T00:00:00Z")
	evidence := mustTime(t, "2023-10-04T14:00:00Z")

	exact := ObservedWindow{From: asked, To: asked.Add(24 * time.Hour), Missing: MissingRequire}
	if exact.InWindow(evidence) {
		t.Fatal("fixture is wrong: the evidence must fall OUTSIDE an exact-day window, " +
			"otherwise this test proves nothing")
	}

	withSlack := exact
	withSlack.Slack = DefaultSlack
	if !withSlack.InWindow(evidence) {
		t.Errorf("the default slack (%v) must bring a next-day recounting into the window; "+
			"without that the window is worse than no window at all", DefaultSlack)
	}
}

// An unknown missing mode is an error, not a silent fallback: it selects which rows
// get DROPPED, and a typo quietly choosing a different policy is unacceptable.
func TestParseObservedMissing_RejectsUnknown(t *testing.T) {
	if _, err := ParseObservedMissing("prefered"); err == nil {
		t.Error("a misspelled mode must be refused — silently picking a drop policy is worse than failing")
	}
	for _, ok := range []string{"", "prefer", "require", "REQUIRE", " prefer "} {
		if _, err := ParseObservedMissing(ok); err != nil {
			t.Errorf("ParseObservedMissing(%q) = %v, want accepted", ok, err)
		}
	}
}

func TestParseSlack_AcceptsDaysAndDurations(t *testing.T) {
	cases := map[string]time.Duration{"": DefaultSlack, "3d": 72 * time.Hour, "72h": 72 * time.Hour, "0d": 0}
	for in, want := range cases {
		got, err := ParseSlack(in)
		if err != nil {
			t.Errorf("ParseSlack(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSlack(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := ParseSlack("-1d"); err == nil {
		t.Error("negative slack must be refused — it would NARROW the window, the opposite of the point")
	}
	if _, err := ParseSlack("soon"); err == nil {
		t.Error("unparseable slack must be refused, not silently defaulted")
	}
}

// A malformed timestamp is refused rather than ignored: a caller who mistypes a date
// and gets an unfiltered search back would read the results as time-filtered.
func TestParseWhen_RefusesMalformedInput(t *testing.T) {
	for _, in := range []*WhenInput{
		{From: "October 3rd"},
		{To: "2023-13-45T00:00:00Z"},
		{From: "2023-10-04T00:00:00Z", To: "2023-10-01T00:00:00Z"}, // reversed
	} {
		if _, err := ParseWhen(in); err == nil {
			t.Errorf("ParseWhen(%+v) was accepted; a bad window must fail loudly", in)
		}
	}
}

// Active is an explicit discriminator, not "are the bounds zero": `require` with no
// window is a legitimate query meaning "only rows someone dated".
func TestObservedWindow_RequireWithNoBoundsIsStillActive(t *testing.T) {
	w := ObservedWindow{Missing: MissingRequire}
	if !w.Active() {
		t.Error("require with no bounds must be active — it means \"only dated rows\"")
	}
	if !w.Filter(mustFilter()).RequireObserved {
		t.Error("require must reach the store filter even with no bounds")
	}
}

func mustFilter() store.MemorySearchFilter { return store.MemorySearchFilter{} }
