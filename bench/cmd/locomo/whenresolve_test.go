package main

import (
	"testing"
	"time"
)

func TestResolveWhen(t *testing.T) {
	const yr = 2023
	iso := func(s string) string { return s }
	for _, tc := range []struct {
		q        string
		from, to string
		ok       bool
	}{
		// A named day becomes that day, widened. Widening is the whole point: the
		// remark answering it is normally made a day or more later.
		{"Which city was Calvin at on October 3, 2023?", "2023-09-30T00:00:00Z", "2023-10-07T00:00:00Z", true},
		{"What movie did Joanna watch on 1 May, 2022?", "2022-04-28T00:00:00Z", "2022-05-05T00:00:00Z", true},
		// A whole month.
		{"Which places in Canada was Evan visiting in July 2023?", "2023-06-28T00:00:00Z", "2023-08-04T00:00:00Z", true},
		// Ordinal parts must beat the month reading — the question asked for the
		// narrower period and resolving it as the whole month would discard that.
		{"Where did Andrew go during the first weekend of August 2023?", "2023-07-29T00:00:00Z", "2023-08-11T00:00:00Z", true},
		{"Where was Calvin located in the last week of October 2023?", "2023-10-22T00:00:00Z", "2023-11-04T00:00:00Z", true},
		{"Which country was Tim visiting in the second week of November?", "2023-11-05T00:00:00Z", "2023-11-18T00:00:00Z", true},
		// No period named: must refuse rather than invent one.
		{"What do Melanie's kids like?", "", "", false},
		{"Who did Maria have dinner with?", "", "", false},
	} {
		from, to, ok := ResolveWhen(tc.q, yr, 3)
		if ok != tc.ok {
			t.Errorf("ResolveWhen(%q) ok=%v, want %v", tc.q, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if got := from.Format(time.RFC3339); got != iso(tc.from) {
			t.Errorf("ResolveWhen(%q) from=%s, want %s", tc.q, got, tc.from)
		}
		if got := to.Format(time.RFC3339); got != iso(tc.to) {
			t.Errorf("ResolveWhen(%q) to=%s, want %s", tc.q, got, tc.to)
		}
	}
}

// The instruction must render in the shape the answerer was observed to copy — a
// window it does not copy is a window that never reaches the store.
func TestWhenInstruction_ShapeTheModelCopies(t *testing.T) {
	from := time.Date(2023, 9, 30, 0, 0, 0, 0, time.UTC)
	to := time.Date(2023, 10, 7, 0, 0, 0, 0, time.UTC)
	got := WhenInstruction(from, to)
	want := "\n\nRestrict your recall with when={\"from\":\"2023-09-30T00:00:00Z\",\"to\":\"2023-10-07T00:00:00Z\"}"
	if got != want {
		t.Errorf("WhenInstruction =\n%q\nwant\n%q", got, want)
	}
}
