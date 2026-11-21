package calc

import (
	"math"
	"math/big"
	"regexp"
	"strconv"
	"time"
)

// dateVal is a civil date with no zone or time of day, held as the number
// of days since 1970-01-01 in the proleptic Gregorian calendar, so date
// arithmetic is integer arithmetic.
type dateVal struct{ days int64 }

// timeVal is an instant. loc is where it is rendered and split into parts:
// UTC unless tz() moved it. Two timeVals are equal when their instants are,
// whatever their zones.
type timeVal struct {
	t   time.Time
	loc *time.Location
}

// Both kinds stay within years 1 to 9999: four-digit ISO 8601, and far
// inside what time.Time and an int64 of days can hold.
var (
	minDays    = civilDays(1, time.January, 1)
	maxDays    = civilDays(9999, time.December, 31)
	minInstant = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	maxInstant = time.Date(9999, time.December, 31, 23, 59, 59, 999_999_999, time.UTC)
)

const rangeText = "0001-01-01 to 9999-12-31"

func civilDays(y int, m time.Month, d int) int64 {
	// Midnight UTC is a whole number of days from the epoch, so the
	// division is exact for years before 1970 too.
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400
}

// civil returns the date as midnight UTC, for reading its fields.
func (d dateVal) civil() time.Time { return time.Unix(d.days*86400, 0).UTC() }

func (d dateVal) String() string { return d.civil().Format("2006-01-02") }

func (t timeVal) local() time.Time { return t.t.In(t.loc) }

func (t timeVal) String() string { return t.local().Format(time.RFC3339Nano) }

func isTime(v val) bool {
	switch v.(type) {
	case dateVal, timeVal:
		return true
	}
	return false
}

func daysIn(y int, m time.Month) int {
	return time.Date(y, m+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

func isLeap(y int64) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

func checkDate(days int64, pos int, what string) (dateVal, *Error) {
	if days < minDays || days > maxDays {
		return dateVal{}, errf(codeOverflow, pos, "%s falls outside %s", what, rangeText)
	}
	return dateVal{days: days}, nil
}

func checkInstant(t time.Time, loc *time.Location, pos int, what string) (timeVal, *Error) {
	if t.Before(minInstant) || t.After(maxInstant) {
		return timeVal{}, errf(codeOverflow, pos, "%s falls outside %s (UTC)", what, rangeText)
	}
	return timeVal{t: t, loc: loc}, nil
}

// maxDateText bounds a date or datetime string before any parsing: the
// longest valid form is 35 bytes.
const maxDateText = 64

var (
	isoDate     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	isoDatetime = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})(?:[T ](\d{2}):(\d{2})(?::(\d{2})(?:[.,](\d{1,9}))?)?(Z|[+-]\d{2}(?::?\d{2})?)?)?$`)
	// dayMonthForms are the forms whose order of day and month depends on
	// the writer's country: 11/12/2026, 12.11.2026, 11-12-2026.
	dayMonthForms = regexp.MustCompile(`^\d{1,2}[/.\-]\d{1,2}[/.\-]\d{2,4}`)
)

func badDate(pos int, fn, kind, s string) *Error {
	if len(s) > maxDateText {
		s = s[:maxDateText] + "…"
	}
	err := errf(codeBadDate, pos, "%s: %q is not an ISO 8601 %s", fn, s, kind)
	switch {
	case dayMonthForms.MatchString(s):
		return err.withHint("the order of day and month is ambiguous; write year-month-day, e.g. \"2026-11-12\" for 12 November 2026")
	case kind == "date":
		return err.withHint("write \"YYYY-MM-DD\", e.g. \"2026-11-20\"")
	}
	return err.withHint("write \"YYYY-MM-DDTHH:MM:SS\" with Z or an offset, e.g. \"2026-11-20T14:30:00Z\"; no zone means UTC")
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s) // the regexps admit only digits
	return n
}

// civilFields checks y-m-d and returns the date's days. Year 0 and a day
// past the month's end are refused, never rolled over.
func civilFields(y, m, d int64, pos int, fn string) (int64, *Error) {
	switch {
	case y < 1 || y > 9999:
		return 0, errf(codeDomain, pos, "%s: year %d is outside 1 to 9999", fn, y)
	case m < 1 || m > 12:
		return 0, errf(codeDomain, pos, "%s: month %d is outside 1 to 12", fn, m)
	case d < 1 || d > int64(daysIn(int(y), time.Month(m))):
		return 0, errf(codeDomain, pos, "%s: %s %d has %d days, not %d", fn, time.Month(m), y, daysIn(int(y), time.Month(m)), d).
			withHint("for the last day of a month use eomonth(d, 0)")
	}
	return civilDays(int(y), time.Month(m), int(d)), nil
}

func parseDate(s string, pos int, fn string) (dateVal, *Error) {
	if len(s) > maxDateText {
		return dateVal{}, badDate(pos, fn, "date", s)
	}
	m := isoDate.FindStringSubmatch(s)
	if m == nil {
		err := badDate(pos, fn, "date", s)
		if isoDatetime.MatchString(s) {
			err.Hint = "that is a datetime; write datetime(\"…\"), or date(datetime(\"…\")) for its UTC date"
		}
		return dateVal{}, err
	}
	days, err := civilFields(int64(atoi(m[1])), int64(atoi(m[2])), int64(atoi(m[3])), pos, fn)
	return dateVal{days: days}, err
}

// parseDatetime reads ISO 8601 / RFC 3339: a date, optionally a time to
// the nanosecond, optionally Z or an offset; no zone means UTC. The result
// renders in UTC whatever offset it was written with.
func parseDatetime(s string, pos int, fn string) (timeVal, *Error) {
	if len(s) > maxDateText {
		return timeVal{}, badDate(pos, fn, "datetime", s)
	}
	m := isoDatetime.FindStringSubmatch(s)
	if m == nil {
		return timeVal{}, badDate(pos, fn, "datetime", s)
	}
	days, err := civilFields(int64(atoi(m[1])), int64(atoi(m[2])), int64(atoi(m[3])), pos, fn)
	if err != nil {
		return timeVal{}, err
	}
	h, mi, sec, ns := atoi(m[4]), atoi(m[5]), atoi(m[6]), 0
	if m[7] != "" {
		frac := m[7] + "000000000"[len(m[7]):]
		ns = atoi(frac)
	}
	if h > 23 || mi > 59 || sec > 59 {
		return timeVal{}, errf(codeDomain, pos, "%s: %02d:%02d:%02d is not a time of day", fn, h, mi, sec).
			withHint("hours 0-23, minutes and seconds 0-59; there are no leap seconds")
	}
	off, oerr := parseOffset(m[8], pos, fn)
	if oerr != nil {
		return timeVal{}, oerr
	}
	unix := days*86400 + int64(h*3600+mi*60+sec) - int64(off)
	return checkInstant(time.Unix(unix, int64(ns)).UTC(), time.UTC, pos, fn+"(\""+s+"\")")
}

// parseOffset reads "", "Z", "+02", "+0200" or "+02:00" as seconds east of UTC.
func parseOffset(s string, pos int, fn string) (int, *Error) {
	if s == "" || s == "Z" {
		return 0, nil
	}
	digits := s[1:]
	if len(digits) > 2 && digits[2] == ':' {
		digits = digits[:2] + digits[3:]
	}
	h, mi := atoi(digits[:2]), 0
	if len(digits) == 4 {
		mi = atoi(digits[2:])
	}
	if h > 23 || mi > 59 || len(digits) != 2 && len(digits) != 4 {
		return 0, errf(codeBadDate, pos, "%s: %q is not a UTC offset", fn, s).withHint("write Z, +02:00 or -05:00")
	}
	off := h*3600 + mi*60
	if s[0] == '-' {
		off = -off
	}
	return off, nil
}

// mixHint is the hint for any operation that meets a date and a datetime.
const mixHint = "convert one first: date(dt) is the calendar date of a datetime, datetime(d) is midnight UTC of a date"

// timeArith implements + and - where a date or datetime is involved:
// date ± whole days, datetime ± seconds, date - date in days, datetime -
// datetime in seconds. Every other combination is refused.
func (e *evaluator) timeArith(op string, l, r val, pos int) (val, *Error) {
	_, lDate := l.(dateVal)
	_, rDate := r.(dateVal)
	_, lTime := l.(timeVal)
	_, rTime := r.(timeVal)
	switch {
	case lDate && rTime || lTime && rDate:
		return nil, errf(codeType, pos, "cannot apply %s to a date and a datetime", op).withHint(mixHint)
	case op != "+" && op != "-":
		return nil, errf(codeType, pos, "cannot apply %s to a %s and a %s", op, typeName(l), typeName(r)).
			withHint("dates take + and - only: d + days, d - days, d2 - d1; datetimes count seconds the same way")
	}
	switch lv := l.(type) {
	case dateVal:
		switch rv := r.(type) {
		case dateVal:
			if op == "-" {
				return exactInt64(lv.days - rv.days), nil
			}
		case num:
			return e.addDays(lv, rv, op == "-", pos)
		}
	case timeVal:
		switch rv := r.(type) {
		case timeVal:
			if op == "-" {
				return secondsBetween(lv.t, rv.t), nil
			}
		case num:
			return addSeconds(lv, rv, op == "-", pos)
		}
	case num:
		if op == "+" {
			switch rv := r.(type) {
			case dateVal:
				return e.addDays(rv, lv, false, pos)
			case timeVal:
				return addSeconds(rv, lv, false, pos)
			}
		}
	}
	return nil, errf(codeType, pos, "cannot apply %s to a %s and a %s", op, typeName(l), typeName(r)).
		withHint("add or subtract a number from a date (d + 30, d - 30), or subtract two dates (d2 - d1)")
}

// maxDayStep is more days than the whole 1-9999 range, so a step under it
// fits an int64 sum and the range check decides.
const maxDayStep = 4_000_000

func (e *evaluator) addDays(d dateVal, n num, neg bool, pos int) (val, *Error) {
	k, ok := smallInt(n, maxDayStep)
	if !ok {
		if isIntegral(n) {
			return nil, errf(codeOverflow, pos, "adding %s days passes %s", fmtArg(n), rangeText)
		}
		return nil, errf(codeType, pos, "a date moves by whole days, not %s", fmtArg(n)).
			withHint("for a time of day use datetime(d) + seconds")
	}
	if neg {
		k = -k
	}
	return checkDate(d.days+k, pos, "the date")
}

func isIntegral(n num) bool {
	if n.exact() {
		return n.r.IsInt()
	}
	return n.f == math.Trunc(n.f)
}

var nsPerSecond = big.NewRat(1_000_000_000, 1)

// maxSecondStep is more seconds than the whole 1-9999 range.
const maxSecondStep = 400_000_000_000

// addSeconds moves an instant by n seconds, rounded to the nanosecond.
func addSeconds(t timeVal, n num, neg bool, pos int) (val, *Error) {
	r := n.rat()
	if new(big.Rat).Abs(r).Cmp(big.NewRat(maxSecondStep, 1)) > 0 {
		return nil, errf(codeOverflow, pos, "adding %s seconds passes %s", fmtArg(n), rangeText)
	}
	ns := roundHalfAway(new(big.Rat).Mul(r, nsPerSecond))
	if neg {
		ns.Neg(ns)
	}
	sec, rem := new(big.Int).QuoRem(ns, big.NewInt(1_000_000_000), new(big.Int))
	moved := time.Unix(t.t.Unix()+sec.Int64(), int64(t.t.Nanosecond())+rem.Int64()).UTC()
	return checkInstant(moved, t.loc, pos, "the datetime")
}

// secondsBetween is a - b in seconds, exact to the nanosecond.
func secondsBetween(a, b time.Time) num {
	r := big.NewRat(int64(a.Nanosecond()-b.Nanosecond()), 1_000_000_000)
	return exactNum(r.Add(r, big.NewRat(a.Unix()-b.Unix(), 1)))
}

// cmpTime orders two dates or two datetimes and refuses any other pair.
func cmpTime(l, r val, pos int) (int, *Error) {
	switch lv := l.(type) {
	case dateVal:
		switch rv := r.(type) {
		case dateVal:
			return cmpInt64(lv.days, rv.days), nil
		case timeVal:
			return 0, errf(codeType, pos, "cannot compare a date and a datetime").withHint(mixHint)
		}
	case timeVal:
		switch rv := r.(type) {
		case timeVal:
			return lv.t.Compare(rv.t), nil
		case dateVal:
			return 0, errf(codeType, pos, "cannot compare a datetime and a date").withHint(mixHint)
		}
	}
	return 0, errf(codeType, pos, "cannot order a %s and a %s", typeName(l), typeName(r))
}

func cmpInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
