package main

// whenresolve.go — resolve a question's date phrase into an explicit window.
//
// This lives in the HARNESS, not the runtime, and deliberately: RFC CL declares
// natural-language date parsing a non-goal for the memory plane ("the predicate takes
// instants, not prose") and puts the job on the caller. The benchmark is a caller, so
// this is that job done here.
//
// It also removes the model's discretion, which is the point. Asked to derive a window
// from a system-prompt rule, the answerer ignored it twice — but it emits `when`
// faithfully when handed one. Measuring the PREDICATE means supplying the window and
// not measuring whether a particular model remembers to build one.

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var months = map[string]time.Month{
	"january": time.January, "february": time.February, "march": time.March,
	"april": time.April, "may": time.May, "june": time.June, "july": time.July,
	"august": time.August, "september": time.September, "october": time.October,
	"november": time.November, "december": time.December,
}

const monthAlt = `(January|February|March|April|May|June|July|August|September|October|November|December)`

var (
	// "on October 3, 2023" / "on 3 October, 2023"
	reDayMonthYear = regexp.MustCompile(`(?i)\b` + monthAlt + `\s+(\d{1,2})(?:st|nd|rd|th)?,?\s*(\d{4})`)
	reDayFirst     = regexp.MustCompile(`(?i)\b(\d{1,2})(?:st|nd|rd|th)?\s+` + monthAlt + `,?\s*(\d{4})`)
	// "the first weekend of August 2023", "the last week of October 2023",
	// "the second week of November"
	reOrdinalPart = regexp.MustCompile(`(?i)\b(first|second|third|fourth|last)\s+(week|weekend)\s+(?:of\s+)?` + monthAlt + `(?:,?\s*(\d{4}))?`)
	// "in July 2023" / "during March 2023" / "in October"
	reMonthYear = regexp.MustCompile(`(?i)\b(?:in|during|of)\s+` + monthAlt + `(?:,?\s*(\d{4}))?`)
)

// ResolveWhen turns a question's date phrase into a half-open [from, to) window,
// already widened. It reports ok=false when the question names no resolvable period —
// a question with no window must be asked WITHOUT one rather than with a guessed one.
//
// slackDays widens both ends. It is generous by default for a measured reason: the
// remark that answers a question about a day is normally made a day or more AFTER it,
// so a window pinned to the exact day misses the row roughly seven times in eight.
func ResolveWhen(question string, defaultYear int, slackDays int) (from, to time.Time, ok bool) {
	yr := func(s string) int {
		if s == "" {
			return defaultYear
		}
		var y int
		fmt.Sscanf(s, "%d", &y)
		return y
	}
	day := func(s string) int {
		var d int
		fmt.Sscanf(s, "%d", &d)
		return d
	}
	mon := func(s string) time.Month { return months[strings.ToLower(s)] }

	widen := func(a, b time.Time) (time.Time, time.Time, bool) {
		d := time.Duration(slackDays) * 24 * time.Hour
		return a.Add(-d), b.Add(d), true
	}

	// Ordinal week/weekend FIRST: "the first weekend of August 2023" also matches the
	// month pattern, and the narrower reading is the one the question asked for.
	if m := reOrdinalPart.FindStringSubmatch(question); m != nil {
		month, year := mon(m[3]), yr(m[4])
		start := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
		var a, b time.Time
		switch strings.ToLower(m[1]) {
		case "first":
			a, b = start, start.AddDate(0, 0, 7)
		case "second":
			a, b = start.AddDate(0, 0, 7), start.AddDate(0, 0, 14)
		case "third":
			a, b = start.AddDate(0, 0, 14), start.AddDate(0, 0, 21)
		case "fourth":
			a, b = start.AddDate(0, 0, 21), start.AddDate(0, 0, 28)
		default: // "last"
			end := start.AddDate(0, 1, 0)
			a, b = end.AddDate(0, 0, -7), end
		}
		return widen(a, b)
	}
	if m := reDayMonthYear.FindStringSubmatch(question); m != nil {
		d := time.Date(yr(m[3]), mon(m[1]), day(m[2]), 0, 0, 0, 0, time.UTC)
		return widen(d, d.AddDate(0, 0, 1))
	}
	if m := reDayFirst.FindStringSubmatch(question); m != nil {
		d := time.Date(yr(m[3]), mon(m[2]), day(m[1]), 0, 0, 0, 0, time.UTC)
		return widen(d, d.AddDate(0, 0, 1))
	}
	if m := reMonthYear.FindStringSubmatch(question); m != nil {
		start := time.Date(yr(m[2]), mon(m[1]), 1, 0, 0, 0, 0, time.UTC)
		return widen(start, start.AddDate(0, 1, 0))
	}
	return time.Time{}, time.Time{}, false
}

// WhenInstruction renders the window as the line appended to the question, in the
// exact shape the answerer was observed to copy faithfully.
func WhenInstruction(from, to time.Time) string {
	return fmt.Sprintf("\n\nRestrict your recall with when={\"from\":%q,\"to\":%q}",
		from.Format(time.RFC3339), to.Format(time.RFC3339))
}
