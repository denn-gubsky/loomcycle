package http

import (
	"testing"

	"github.com/denn-gubsky/loomcycle/internal/store"
)

// TestParseStreamFilter_WalkID: the query param a canvas watches a workflow
// with.
func TestParseStreamFilter_WalkID(t *testing.T) {
	got := parseStreamFilter("u1", map[string][]string{"walk_id": {" r_abc "}})
	if got.WalkID != "r_abc" {
		t.Errorf("WalkID = %q, want r_abc (trimmed)", got.WalkID)
	}
	// Absent means no narrowing — the whole-user stream is unchanged.
	if got := parseStreamFilter("u1", map[string][]string{}); got.WalkID != "" {
		t.Errorf("WalkID = %q with no param, want empty", got.WalkID)
	}
}

// TestStreamFilter_WalkIDMatchesOnParentContext pins the matching rule,
// including the case that is easy to get wrong: a run with NO parent context
// cannot belong to a walk, so it must be filtered OUT rather than passed
// through. A walk view that also showed unrelated runs would not be a walk
// view.
func TestStreamFilter_WalkIDMatchesOnParentContext(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter string
		pc     *store.ParentContext
		want   bool
	}{
		{"no filter passes everything", "", nil, true},
		{"no filter passes a stamped run", "", &store.ParentContext{WalkID: "r_a"}, true},
		{"matching walk passes", "r_a", &store.ParentContext{WalkID: "r_a"}, true},
		{"another walk is excluded", "r_a", &store.ParentContext{WalkID: "r_b"}, false},
		{"an UNSTAMPED run is excluded", "r_a", nil, false},
		{"a stamped-but-empty run is excluded", "r_a", &store.ParentContext{}, false},
	} {
		got := walkIDMatches(tc.filter, "r_member", tc.pc)
		if got != tc.want {
			t.Errorf("%s: match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestStreamFilter_WalkIDMatchesTheWalksOwnRun: a walk's id is its own run's
// id, and that run carries no parent context, so the filter matches it by run
// id — the walk's start, pauses and end belong on the walk's own stream, as the
// walk's own run belongs in its listing. Another walk's run is still excluded.
func TestStreamFilter_WalkIDMatchesTheWalksOwnRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter string
		runID  string
		pc     *store.ParentContext
		want   bool
	}{
		{"the walk's own run passes", "r_a", "r_a", nil, true},
		{"the walk's own run passes with an empty context", "r_a", "r_a", &store.ParentContext{}, true},
		{"another walk's own run is excluded", "r_a", "r_b", nil, false},
		{"a run stamped by another walk is excluded", "r_a", "r_c", &store.ParentContext{WalkID: "r_b"}, false},
	} {
		if got := walkIDMatches(tc.filter, tc.runID, tc.pc); got != tc.want {
			t.Errorf("%s: match = %v, want %v", tc.name, got, tc.want)
		}
	}
}
