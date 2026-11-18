package builtin

// document_measure_period.go — the calendar of the taxonomy's series (RFC GM 6A.1,
// D32): which period an instant falls in, the period before and after, and its
// label. Every period is in UTC, so a series does not move with the server's zone.

import (
	"fmt"
	"time"

	memrank "github.com/denn-gubsky/loomcycle/internal/memory"
)

// schemePeriod is the period a scheme's series are kept in: its @period, else month.
func schemePeriod(s memrank.OntologyScheme) string {
	if s.Period == "" {
		return memrank.SchemePeriodMonth
	}
	return s.Period
}

// periodStart is the start (unix nanos) of the period of the given length that holds
// at: the day's midnight, the ISO week's Monday, the month's first day.
func periodStart(at int64, period string) int64 {
	t := time.Unix(0, at).UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case memrank.SchemePeriodDay:
		return day.UnixNano()
	case memrank.SchemePeriodWeek:
		// ISO weeks start on Monday; Go's Weekday counts from Sunday.
		back := (int(day.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -back).UnixNano()
	}
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).UnixNano()
}

// shiftPeriod is the start of the period n periods after start (n < 0: before).
func shiftPeriod(start int64, period string, n int) int64 {
	t := time.Unix(0, start).UTC()
	switch period {
	case memrank.SchemePeriodDay:
		return t.AddDate(0, 0, n).UnixNano()
	case memrank.SchemePeriodWeek:
		return t.AddDate(0, 0, 7*n).UnixNano()
	}
	return t.AddDate(0, n, 0).UnixNano()
}

// periodLabel names a period: 2026-07 (month), 2026-W29 (ISO week), 2026-07-21 (day).
func periodLabel(start int64, period string) string {
	t := time.Unix(0, start).UTC()
	switch period {
	case memrank.SchemePeriodDay:
		return t.Format("2006-01-02")
	case memrank.SchemePeriodWeek:
		y, w := t.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	}
	return t.Format("2006-01")
}
