package calc

import (
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
)

// dateFuncs are the calendar functions. A date is a civil date; a datetime
// an instant (see datetime.go). today() and now() read Request.Now: with
// the random functions they are the only calls whose answer is not fixed
// by the expression.
var dateFuncs = []builtin{
	{name: "date", sig: "date(s) or date(y, m, d)", section: SecDates, min: 1, max: 3, vec: true, fn: fnDate,
		summary: "the date of an ISO 8601 string, of year/month/day, or of a datetime in its zone"},
	{name: "datetime", sig: "datetime(s)", section: SecDates, min: 1, max: 1, vec: true, fn: fnDatetime,
		summary: "the instant of an ISO 8601 / RFC 3339 string (no zone means UTC), or midnight UTC of a date"},
	{name: "today", sig: "today()", section: SecDates, min: 0, max: 0, fn: fnToday,
		summary: "the current date in UTC"},
	{name: "now", sig: "now()", section: SecDates, min: 0, max: 0, fn: fnNow,
		summary: "the current instant, to the second, in UTC"},
	{name: "tz", sig: "tz(dt, zone)", section: SecDates, min: 2, max: 2, vec: true, fn: fnTz,
		summary: "the same instant, rendered and split into parts in an IANA zone such as \"Europe/Kyiv\""},
	{name: "addmonths", sig: "addmonths(d, n)", section: SecDates, min: 2, max: 2, vec: true, fn: fnAddMonths,
		summary: "n calendar months after d, clamped to the end of a shorter month"},
	{name: "addyears", sig: "addyears(d, n)", section: SecDates, min: 2, max: 2, vec: true, fn: fnAddYears,
		summary: "n years after d; 29 February becomes 28 February in a common year"},
	{name: "eomonth", sig: "eomonth(d, n)", section: SecDates, min: 2, max: 2, vec: true, fn: fnEOMonth,
		summary: "the last day of the month n months after d's month"},
	{name: "datediff", sig: "datediff(a, b, unit)", section: SecDates, min: 3, max: 3, vec: true, fn: fnDateDiff,
		summary: "whole days, weeks, months or years from a to b; negative when a is after b"},
	{name: "age", sig: "age(birth, on?)", section: SecDates, min: 1, max: 2, vec: true, fn: fnAge,
		summary: "completed years from birth to on (default today)"},
	{name: "yearfrac", sig: "yearfrac(a, b, basis)", section: SecDates, min: 3, max: 3, vec: true, fn: fnYearFrac,
		summary: "the years from a to b under a day-count basis: act/365, act/360, 30/360, 30e/360, act/act"},
	{name: "workdays", sig: "workdays(a, b, holidays?)", section: SecDates, min: 2, max: 3, vec: true, fn: fnWorkdays,
		summary: "Monday-to-Friday days from a to b, both included, less the holidays"},
	{name: "addworkdays", sig: "addworkdays(d, n, holidays?)", section: SecDates, min: 2, max: 3, vec: true, fn: fnAddWorkdays,
		summary: "the date n working days after d (before, for a negative n), skipping weekends and holidays"},
	{name: "xnpv", sig: "xnpv(rate, flows, dates)", section: SecDates, min: 3, max: 3, fn: fnXNPV,
		summary: "the net present value of dated cash flows, discounted act/365 from the first date"},
	{name: "xirr", sig: "xirr(flows, dates, guess?)", section: SecDates, min: 2, max: 3, fn: fnXIRR,
		summary: "the annual rate at which xnpv of the flows is zero"},
	{name: "year", sig: "year(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { return int64(t.Year()) }),
		summary: "the year of a date or datetime"},
	{name: "month", sig: "month(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { return int64(t.Month()) }),
		summary: "the month, 1 to 12"},
	{name: "day", sig: "day(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { return int64(t.Day()) }),
		summary: "the day of the month, 1 to 31"},
	{name: "weekday", sig: "weekday(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { return int64(isoWeekday(t.Weekday())) }),
		summary: "the ISO day of the week: 1 is Monday, 7 is Sunday"},
	{name: "isoweek", sig: "isoweek(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { _, w := t.ISOWeek(); return int64(w) }),
		summary: "the ISO 8601 week number, 1 to 53"},
	{name: "dayofyear", sig: "dayofyear(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { return int64(t.YearDay()) }),
		summary: "the day of the year, 1 to 366"},
	{name: "quarter", sig: "quarter(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { return int64(t.Month()-1)/3 + 1 }),
		summary: "the calendar quarter, 1 to 4"},
	{name: "daysinmonth", sig: "daysinmonth(d)", section: SecDates, min: 1, max: 1, vec: true, fn: datePart(func(t time.Time) int64 { return int64(daysIn(t.Year(), t.Month())) }),
		summary: "the number of days in d's month"},
	{name: "isleap", sig: "isleap(d)", section: SecDates, min: 1, max: 1, vec: true, fn: fnIsLeap,
		summary: "whether the year of a date, or a year number, is a leap year"},
	{name: "hour", sig: "hour(dt)", section: SecDates, min: 1, max: 1, vec: true, fn: clockPart(func(t time.Time) num { return exactInt64(int64(t.Hour())) }),
		summary: "the hour of a datetime in its zone, 0 to 23"},
	{name: "minute", sig: "minute(dt)", section: SecDates, min: 1, max: 1, vec: true, fn: clockPart(func(t time.Time) num { return exactInt64(int64(t.Minute())) }),
		summary: "the minute, 0 to 59"},
	{name: "second", sig: "second(dt)", section: SecDates, min: 1, max: 1, vec: true, fn: clockPart(func(t time.Time) num {
		return exactNum(big.NewRat(int64(t.Second())*1_000_000_000+int64(t.Nanosecond()), 1_000_000_000))
	}),
		summary: "the second, 0 to 59, with its fraction"},
	{name: "datefmt", sig: "datefmt(d, pattern)", section: SecDates, min: 2, max: 2, vec: true, fn: fnDateFmt,
		summary: "d written with the tokens YYYY YY MMMM MMM MM M DD D dddd ddd HH mm ss; [text] is literal"},
}

// --- argument helpers ---

// date returns argument i as a date. A datetime or a string is refused
// with the conversion that would make it one.
func (a args) date(i int) (dateVal, *Error) {
	switch v := a.v[i].(type) {
	case dateVal:
		return v, nil
	case timeVal:
		return dateVal{}, errf(codeType, a.pos, "%s: argument %d must be a date, got a datetime", a.name, i+1).
			withHint("date(dt) is its calendar date")
	case string:
		return dateVal{}, errf(codeType, a.pos, "%s: argument %d must be a date, got a string", a.name, i+1).
			withHint("wrap the text: date(\"2026-11-20\")")
	}
	return dateVal{}, errf(codeType, a.pos, "%s: argument %d must be a date, got a %s", a.name, i+1, typeName(a.v[i]))
}

// fields returns the calendar fields of a date, or of a datetime in its zone.
func (a args) fields(i int) (time.Time, *Error) {
	switch v := a.v[i].(type) {
	case dateVal:
		return v.civil(), nil
	case timeVal:
		return v.local(), nil
	}
	err := errf(codeType, a.pos, "%s: argument %d must be a date or a datetime, got a %s", a.name, i+1, typeName(a.v[i]))
	if _, isStr := a.v[i].(string); isStr {
		err.Hint = "wrap the text: date(\"2026-11-20\")"
	}
	return time.Time{}, err
}

func (a args) str(i int) (string, *Error) {
	s, ok := a.v[i].(string)
	if !ok {
		return "", errf(codeType, a.pos, "%s: argument %d must be a string, got a %s", a.name, i+1, typeName(a.v[i]))
	}
	return s, nil
}

func isoWeekday(w time.Weekday) int { return (int(w)+6)%7 + 1 }

// weekdayOf is the ISO weekday of a day number: 1970-01-01 was a Thursday.
func weekdayOf(days int64) int64 { return floorModInt(days+3, 7) + 1 }

func floorModInt(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

func floorDivInt(a, b int64) int64 { return (a - floorModInt(a, b)) / b }

// --- constructors and the clock ---

func fnDate(_ *evaluator, a args) (val, *Error) {
	switch len(a.v) {
	case 1:
		switch v := a.v[0].(type) {
		case string:
			return parseDate(v, a.pos, "date")
		case dateVal:
			return v, nil
		case timeVal:
			l := v.local()
			return dateVal{days: civilDays(l.Year(), l.Month(), l.Day())}, nil
		}
		return nil, errf(codeType, a.pos, "date: needs a string, a datetime or three numbers, got a %s", typeName(a.v[0])).
			withHint("date(\"2026-11-20\") or date(2026, 11, 20)")
	case 3:
		var f [3]int64
		for i := range f {
			n, err := a.small(i, 1_000_000)
			if err != nil {
				return nil, err
			}
			f[i] = n
		}
		days, err := civilFields(f[0], f[1], f[2], a.pos, "date")
		if err != nil {
			return nil, err
		}
		return dateVal{days: days}, nil
	}
	return nil, errf(codeArity, a.pos, "date takes 1 or 3 arguments, got %d", len(a.v)).withHint("date(s) or date(y, m, d)")
}

func fnDatetime(_ *evaluator, a args) (val, *Error) {
	switch v := a.v[0].(type) {
	case string:
		return parseDatetime(v, a.pos, "datetime")
	case dateVal:
		return timeVal{t: v.civil(), loc: time.UTC}, nil
	case timeVal:
		return v, nil
	}
	return nil, errf(codeType, a.pos, "datetime: needs a string or a date, got a %s", typeName(a.v[0])).
		withHint("datetime(\"2026-11-20T14:30:00Z\")")
}

// clock reads Request.Now once per evaluation, so every today() and now()
// in one expression sees the same instant: now() - now() is 0 even when a
// second boundary passes mid-evaluation.
func (e *evaluator) clock() time.Time {
	if e.nowAt.IsZero() {
		e.nowAt = e.now().UTC()
	}
	return e.nowAt
}

func fnToday(e *evaluator, _ args) (val, *Error) {
	t := e.clock()
	return dateVal{days: civilDays(t.Year(), t.Month(), t.Day())}, nil
}

func fnNow(e *evaluator, a args) (val, *Error) {
	return checkInstant(e.clock().Truncate(time.Second), time.UTC, a.pos, "now()")
}

// zoneName admits IANA names ("Europe/Kyiv", "Etc/GMT+5", "UTC"); the
// pattern keeps a path such as "../x" from ever reaching LoadLocation.
var zoneName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+\-]*(/[A-Za-z0-9_+\-]+){0,3}$`)

func fnTz(e *evaluator, a args) (val, *Error) {
	t, ok := a.v[0].(timeVal)
	if !ok {
		err := errf(codeType, a.pos, "tz: argument 1 must be a datetime, got a %s", typeName(a.v[0]))
		if _, isDate := a.v[0].(dateVal); isDate {
			err.Hint = "a date has no time of day; write tz(datetime(d), zone) for its midnight UTC"
		}
		return nil, err
	}
	name, err := a.str(1)
	if err != nil {
		return nil, err
	}
	loc, err := e.zone(name, a.pos)
	if err != nil {
		return nil, err
	}
	return timeVal{t: t.t, loc: loc}, nil
}

func (e *evaluator) zone(name string, pos int) (*time.Location, *Error) {
	if loc, ok := e.zones[name]; ok {
		return loc, nil
	}
	shown := name
	if len(shown) > maxDateText {
		shown = shown[:maxDateText] + "…"
	}
	// "Local" is the host's zone: the answer would depend on the server.
	if len(name) > maxDateText || name == "Local" || !zoneName.MatchString(name) {
		return nil, errf(codeBadDate, pos, "tz: %q is not an IANA time zone name", shown).
			withHint("write a name such as \"Europe/Kyiv\", \"America/New_York\" or \"UTC\"")
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, errf(codeBadDate, pos, "tz: unknown time zone %q", shown).
			withHint("IANA names are Area/City, e.g. \"Europe/Kyiv\", \"Asia/Tokyo\"; abbreviations such as CEST or PST are not zone names")
	}
	if e.zones == nil {
		e.zones = map[string]*time.Location{}
	}
	e.zones[name] = loc
	return loc, nil
}

// --- calendar steps ---

// maxMonthStep is more months than the whole 1-9999 range.
const maxMonthStep = 120_000

// shiftMonths moves d by n calendar months; the day is clamped to the
// target month's length, or set to it when toEnd.
func shiftMonths(d dateVal, n int64, toEnd bool, pos int, fn string) (val, *Error) {
	t := d.civil()
	total := int64(t.Year())*12 + int64(t.Month()-1) + n
	y, m := floorDivInt(total, 12), time.Month(floorModInt(total, 12)+1)
	if y < 1 || y > 9999 {
		return nil, errf(codeOverflow, pos, "%s: the result falls outside %s", fn, rangeText)
	}
	last := daysIn(int(y), m)
	day := min(t.Day(), last)
	if toEnd {
		day = last
	}
	return dateVal{days: civilDays(int(y), m, day)}, nil
}

func monthStep(a args, scale int64) (dateVal, int64, *Error) {
	d, err := a.date(0)
	if err != nil {
		return dateVal{}, 0, err
	}
	n, err := a.small(1, maxMonthStep/scale)
	if err != nil {
		return dateVal{}, 0, err
	}
	return d, n * scale, nil
}

func fnAddMonths(_ *evaluator, a args) (val, *Error) {
	d, n, err := monthStep(a, 1)
	if err != nil {
		return nil, err
	}
	return shiftMonths(d, n, false, a.pos, a.name)
}

func fnAddYears(_ *evaluator, a args) (val, *Error) {
	d, n, err := monthStep(a, 12)
	if err != nil {
		return nil, err
	}
	return shiftMonths(d, n, false, a.pos, a.name)
}

func fnEOMonth(_ *evaluator, a args) (val, *Error) {
	d, n, err := monthStep(a, 1)
	if err != nil {
		return nil, err
	}
	return shiftMonths(d, n, true, a.pos, a.name)
}

// --- differences ---

// wholeMonths counts complete calendar months from lo to hi (lo <= hi), as
// Excel's DATEDIF "M": a month completes when hi's day of the month
// reaches lo's, so 31 January to 28 February is 0 and to 1 March is 1.
func wholeMonths(lo, hi dateVal) int64 {
	a, b := lo.civil(), hi.civil()
	m := int64(b.Year()-a.Year())*12 + int64(b.Month()-a.Month())
	if b.Day() < a.Day() {
		m--
	}
	return m
}

// signedMonths is wholeMonths from a to b, negated when a is after b.
func signedMonths(a, b dateVal) int64 {
	if a.days > b.days {
		return -wholeMonths(b, a)
	}
	return wholeMonths(a, b)
}

func twoDates(a args) (dateVal, dateVal, *Error) {
	d1, err := a.date(0)
	if err != nil {
		return dateVal{}, dateVal{}, err
	}
	d2, err := a.date(1)
	return d1, d2, err
}

func fnDateDiff(_ *evaluator, a args) (val, *Error) {
	d1, d2, err := twoDates(a)
	if err != nil {
		return nil, err
	}
	unit, err := a.str(2)
	if err != nil {
		return nil, err
	}
	switch strings.TrimSuffix(unit, "s") {
	case "day":
		return exactInt64(d2.days - d1.days), nil
	case "week":
		return exactInt64((d2.days - d1.days) / 7), nil
	case "month":
		return exactInt64(signedMonths(d1, d2)), nil
	case "year":
		return exactInt64(signedMonths(d1, d2) / 12), nil
	}
	return nil, errf(codeDomain, a.pos, "datediff: unknown unit %q", unit).
		withHint("the unit is \"days\", \"weeks\", \"months\" or \"years\"")
}

func fnAge(e *evaluator, a args) (val, *Error) {
	birth, err := a.date(0)
	if err != nil {
		return nil, err
	}
	var on dateVal
	if len(a.v) == 2 {
		if on, err = a.date(1); err != nil {
			return nil, err
		}
	} else {
		t, _ := fnToday(e, a)
		on = t.(dateVal)
	}
	if on.days < birth.days {
		return nil, errf(codeDomain, a.pos, "age: %s is before the birth date %s", on, birth)
	}
	return exactInt64(wholeMonths(birth, on) / 12), nil
}

func fnYearFrac(_ *evaluator, a args) (val, *Error) {
	d1, d2, err := twoDates(a)
	if err != nil {
		return nil, err
	}
	basis, err := a.str(2)
	if err != nil {
		return nil, err
	}
	neg := d1.days > d2.days
	if neg {
		d1, d2 = d2, d1
	}
	var r *big.Rat
	switch strings.ToLower(basis) {
	case "act/365":
		r = big.NewRat(d2.days-d1.days, 365)
	case "act/360":
		r = big.NewRat(d2.days-d1.days, 360)
	case "30/360":
		r = big.NewRat(days360US(d1, d2), 360)
	case "30e/360":
		r = big.NewRat(days360EU(d1, d2), 360)
	case "act/act":
		r = actActISDA(d1, d2)
	default:
		return nil, errf(codeDomain, a.pos, "yearfrac: unknown basis %q", basis).
			withHint("the basis is \"act/365\", \"act/360\", \"30/360\", \"30e/360\" or \"act/act\"")
	}
	if neg {
		r.Neg(r)
	}
	return exactNum(r), nil
}

// days360US is the US (NASD) 30/360 day count as Excel's YEARFRAC basis 0
// computes it, February rules included (d1 <= d2).
func days360US(d1, d2 dateVal) int64 {
	a, b := d1.civil(), d2.civil()
	da, db := a.Day(), b.Day()
	lastFebA := a.Month() == time.February && da == daysIn(a.Year(), time.February)
	lastFebB := b.Month() == time.February && db == daysIn(b.Year(), time.February)
	switch {
	case da == 31 && db == 31:
		da, db = 30, 30
	case da == 31:
		da = 30
	case da == 30 && db == 31:
		db = 30
	case lastFebA && lastFebB:
		da, db = 30, 30
	case lastFebA:
		da = 30
	}
	return days360(a, b, da, db)
}

// days360EU is the European 30E/360 day count (Excel basis 4): a 31st
// counts as the 30th on both ends.
func days360EU(d1, d2 dateVal) int64 {
	a, b := d1.civil(), d2.civil()
	return days360(a, b, min(a.Day(), 30), min(b.Day(), 30))
}

func days360(a, b time.Time, da, db int) int64 {
	return int64(b.Year()-a.Year())*360 + int64(b.Month()-a.Month())*30 + int64(db-da)
}

// actActISDA splits the period at each 1 January: the days falling in a
// leap year count 1/366, the others 1/365 (d1 <= d2).
func actActISDA(d1, d2 dateVal) *big.Rat {
	y1, y2 := d1.civil().Year(), d2.civil().Year()
	yearLen := func(y int) int64 { return civilDays(y+1, time.January, 1) - civilDays(y, time.January, 1) }
	if y1 == y2 {
		return big.NewRat(d2.days-d1.days, yearLen(y1))
	}
	r := big.NewRat(civilDays(y1+1, time.January, 1)-d1.days, yearLen(y1))
	r.Add(r, big.NewRat(int64(y2-y1-1), 1))
	return r.Add(r, big.NewRat(d2.days-civilDays(y2, time.January, 1), yearLen(y2)))
}

// --- working days ---

// holidays reads the optional holiday argument i: a date or a list of
// dates. It returns the distinct ones that fall on a weekday, sorted.
func (e *evaluator) holidays(a args, i int) ([]int64, *Error) {
	if len(a.v) <= i {
		return nil, nil
	}
	items, isList := a.v[i].([]val)
	if !isList {
		items = []val{a.v[i]}
	}
	if err := e.charge(len(items)+sortCost(len(items)), a.pos); err != nil {
		return nil, err
	}
	var out []int64
	for _, x := range items {
		d, ok := x.(dateVal)
		if !ok {
			return nil, errf(codeType, a.pos, "%s: holidays must be dates, got a %s", a.name, typeName(x)).
				withHint("map the strings first: map(hs, s -> date(s))")
		}
		if weekdayOf(d.days) <= 5 {
			out = append(out, d.days)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	n := 0
	for i, d := range out {
		if i == 0 || d != out[n-1] {
			out[n] = d
			n++
		}
	}
	return out[:n], nil
}

// countIn is the number of sorted days in [lo, hi].
func countIn(days []int64, lo, hi int64) int64 {
	i := sort.Search(len(days), func(k int) bool { return days[k] >= lo })
	j := sort.Search(len(days), func(k int) bool { return days[k] > hi })
	return int64(j - i)
}

// weekdaysIn counts Monday-to-Friday days in [lo, hi] in O(1).
func weekdaysIn(lo, hi int64) int64 {
	n := hi - lo + 1
	c := n / 7 * 5
	w := weekdayOf(lo) - 1 // 0 is Monday
	for i := int64(0); i < n%7; i++ {
		if (w+i)%7 < 5 {
			c++
		}
	}
	return c
}

func fnWorkdays(e *evaluator, a args) (val, *Error) {
	d1, d2, err := twoDates(a)
	if err != nil {
		return nil, err
	}
	hs, err := e.holidays(a, 2)
	if err != nil {
		return nil, err
	}
	lo, hi, sign := d1.days, d2.days, int64(1)
	if lo > hi {
		lo, hi, sign = hi, lo, -1
	}
	return exactInt64(sign * (weekdaysIn(lo, hi) - countIn(hs, lo, hi))), nil
}

// stepWeekdays is the date n Monday-to-Friday days after d (n > 0) or
// before it (n < 0), d itself not counted, in O(1). From a weekend day the
// count starts as if from the Friday before (forward) or the Monday after
// (backward), so Saturday + 1 is Monday, as in Excel's WORKDAY.
func stepWeekdays(d, n int64) int64 {
	w := weekdayOf(d)
	if n > 0 {
		if w > 5 {
			d -= w - 5
			w = 5
		}
		total := w - 1 + n
		return d - (w - 1) + total/5*7 + total%5
	}
	if w > 5 {
		d += 8 - w
		w = 1
	}
	back := 5 - w // days from d forward to its Friday
	total := back - n
	return d + back - total/5*7 - total%5
}

// maxWorkdayStep is more working days than the 1-9999 range holds.
const maxWorkdayStep = 3_000_000

func fnAddWorkdays(e *evaluator, a args) (val, *Error) {
	d, err := a.date(0)
	if err != nil {
		return nil, err
	}
	n, err := a.small(1, maxWorkdayStep)
	if err != nil {
		return nil, err
	}
	hs, err := e.holidays(a, 2)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return d, nil
	}
	// Step over the weekdays, then once more for each holiday the span
	// passed over, until a step passes no new holiday. Each round either
	// ends or meets at least one more of the len(hs) holidays.
	cur, used := stepWeekdays(d.days, n), int64(0)
	for range len(hs) + 1 {
		if err := e.charge(1, a.pos); err != nil {
			return nil, err
		}
		var met int64
		if n > 0 {
			met = countIn(hs, d.days+1, cur)
		} else {
			met = countIn(hs, cur, d.days-1)
		}
		if met == used {
			break
		}
		step := met - used
		if n < 0 {
			step = -step
		}
		cur, used = stepWeekdays(cur, step), met
		if cur < minDays-7 || cur > maxDays+7 {
			break
		}
	}
	return checkDate(cur, a.pos, "addworkdays: the result")
}

// --- dated cash flows ---

// datedFlows reads flows (numbers) and dates (dates) of one length, and
// returns each flow with its time in years after the first date, act/365.
func (e *evaluator) datedFlows(a args, fi, di int) (flows, years []float64, err *Error) {
	fl, err := a.list(fi)
	if err != nil {
		return nil, nil, err
	}
	dl, err := a.list(di)
	if err != nil {
		return nil, nil, err
	}
	if len(fl) != len(dl) {
		return nil, nil, errf(codeShape, a.pos, "%s: %d flows but %d dates", a.name, len(fl), len(dl))
	}
	if len(fl) == 0 {
		return nil, nil, errf(codeDomain, a.pos, "%s: there are no cash flows", a.name)
	}
	if err := e.charge(len(fl), a.pos); err != nil {
		return nil, nil, err
	}
	flows, years = make([]float64, len(fl)), make([]float64, len(fl))
	var first dateVal
	for i := range fl {
		f, ok := fl[i].(num)
		if !ok {
			return nil, nil, errf(codeType, a.pos, "%s: flow %d is a %s, not a number", a.name, i+1, typeName(fl[i]))
		}
		d, ok := dl[i].(dateVal)
		if !ok {
			return nil, nil, errf(codeType, a.pos, "%s: date %d is a %s, not a date", a.name, i+1, typeName(dl[i])).
				withHint("map the strings first: map(ds, s -> date(s))")
		}
		if i == 0 {
			first = d
		} else if d.days < first.days {
			return nil, nil, errf(codeDomain, a.pos, "%s: date %d (%s) is before the first date (%s)", a.name, i+1, d, first).
				withHint("the first flow is the start; put the earliest date first")
		}
		flows[i], years[i] = f.float(), float64(d.days-first.days)/365
	}
	return flows, years, nil
}

// npv is Σ p/(1+r)^t and its derivative in r.
func npv(r float64, flows, years []float64) (f, df float64) {
	for i, p := range flows {
		v := p * math.Pow(1+r, -years[i])
		f += v
		df -= years[i] * v / (1 + r)
	}
	return f, df
}

func fnXNPV(e *evaluator, a args) (val, *Error) {
	rate, err := a.num(0)
	if err != nil {
		return nil, err
	}
	flows, years, err := e.datedFlows(a, 1, 2)
	if err != nil {
		return nil, err
	}
	if rate.float() <= -1 {
		return nil, errf(codeDomain, a.pos, "xnpv: the rate must be above -1 (-100%%)")
	}
	f, _ := npv(rate.float(), flows, years)
	return checkFloat(f, a.pos, "xnpv")
}

// xirrProbe are the rates bisection tries for a sign change when Newton's
// method does not converge from the guess.
var xirrProbe = []float64{-0.999999, -0.99, -0.9, -0.75, -0.5, -0.25, 0, 0.1, 0.25, 0.5, 1, 2, 5, 10, 100, 1e3, 1e4, 1e6}

// fnXIRR solves xnpv(r) = 0: Newton's method from the guess, then
// bisection over a bracketing pair of xirrProbe. It refuses rather than
// return a rate that does not zero the value.
func fnXIRR(e *evaluator, a args) (val, *Error) {
	flows, years, err := e.datedFlows(a, 0, 1)
	if err != nil {
		return nil, err
	}
	guess := 0.1
	if len(a.v) == 3 {
		g, err := a.num(2)
		if err != nil {
			return nil, err
		}
		if guess = g.float(); guess <= -1 {
			return nil, errf(codeDomain, a.pos, "xirr: the guess must be above -1")
		}
	}
	pos, neg, scale := false, false, 0.0
	for _, p := range flows {
		pos, neg, scale = pos || p > 0, neg || p < 0, scale+math.Abs(p)
	}
	if !pos || !neg {
		return nil, errf(codeDomain, a.pos, "xirr: the flows need at least one payment and one receipt").
			withHint("an investment is negative, its returns positive")
	}
	tol := 1e-10 * scale
	r := guess
	for range 100 {
		if err := e.charge(len(flows), a.pos); err != nil {
			return nil, err
		}
		f, df := npv(r, flows, years)
		if math.IsNaN(f) || math.IsInf(f, 0) || df == 0 || math.IsNaN(df) || math.IsInf(df, 0) {
			break
		}
		next := r - f/df
		if next <= -1 {
			next = (r - 1) / 2 // halfway to -1, never past it
		}
		if math.Abs(next-r) <= 1e-14*math.Max(1, math.Abs(r)) {
			if g, _ := npv(next, flows, years); math.Abs(g) <= tol {
				return checkFloat(next, a.pos, "xirr")
			}
			break
		}
		r = next
	}
	return e.xirrBisect(a, flows, years, tol)
}

func (e *evaluator) xirrBisect(a args, flows, years []float64, tol float64) (val, *Error) {
	for i := 0; i+1 < len(xirrProbe); i++ {
		lo, hi := xirrProbe[i], xirrProbe[i+1]
		flo, _ := npv(lo, flows, years)
		fhi, _ := npv(hi, flows, years)
		if math.IsNaN(flo) || math.IsNaN(fhi) || flo*fhi > 0 {
			continue
		}
		for range 200 {
			if err := e.charge(len(flows), a.pos); err != nil {
				return nil, err
			}
			mid := (lo + hi) / 2
			fm, _ := npv(mid, flows, years)
			if fm*flo > 0 {
				lo, flo = mid, fm
			} else {
				hi = mid
			}
			if hi-lo <= 1e-15*math.Max(1, math.Abs(lo)) {
				break
			}
		}
		r := (lo + hi) / 2
		if f, _ := npv(r, flows, years); math.Abs(f) <= tol {
			return checkFloat(r, a.pos, "xirr")
		}
	}
	return nil, errf(codeDomain, a.pos, "xirr: no rate makes the net present value of these flows zero").
		withHint("check the signs and dates of the flows, or pass a guess near the rate you expect")
}

// --- parts ---

func datePart(f func(time.Time) int64) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		t, err := a.fields(0)
		if err != nil {
			return nil, err
		}
		return exactInt64(f(t)), nil
	}
}

func clockPart(f func(time.Time) num) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		t, ok := a.v[0].(timeVal)
		if !ok {
			err := errf(codeType, a.pos, "%s: argument 1 must be a datetime, got a %s", a.name, typeName(a.v[0]))
			if _, isDate := a.v[0].(dateVal); isDate {
				err.Hint = "a date has no time of day"
			}
			return nil, err
		}
		return f(t.local()), nil
	}
}

func fnIsLeap(_ *evaluator, a args) (val, *Error) {
	if _, ok := a.v[0].(num); ok {
		y, err := a.small(0, 1<<53)
		if err != nil {
			return nil, err
		}
		return isLeap(y), nil
	}
	t, err := a.fields(0)
	if err != nil {
		return nil, err
	}
	return isLeap(int64(t.Year())), nil
}

// --- formatting ---

// dateTokens are the pattern tokens. A pattern is read in runs of one
// ASCII letter, and each run must be one of these whole: "MMMMM" or "DDD"
// is refused, not read as MMMM+M or DD+D.
var dateTokens = map[string]bool{"YYYY": true, "YY": true, "MMMM": true, "MMM": true, "MM": true, "M": true,
	"DD": true, "D": true, "dddd": true, "ddd": true, "HH": true, "mm": true, "ss": true}

// tokenList is how a hint names the tokens when no single one fits.
const tokenList = "the tokens are YYYY YY MMMM MMM MM M DD D dddd ddd HH mm ss"

// maxPattern bounds a datefmt pattern; each output byte past it is charged.
const maxPattern = 100

func fnDateFmt(e *evaluator, a args) (val, *Error) {
	t, err := a.fields(0)
	if err != nil {
		return nil, err
	}
	pat, err := a.str(1)
	if err != nil {
		return nil, err
	}
	if len(pat) > maxPattern {
		return nil, errf(codeDomain, a.pos, "datefmt: the pattern is %d bytes; the limit is %d", len(pat), maxPattern)
	}
	var b strings.Builder
	for i := 0; i < len(pat); {
		if pat[i] == '[' {
			end := strings.IndexByte(pat[i:], ']')
			if end < 0 {
				return nil, errf(codeBadDate, a.pos, "datefmt: a [ in the pattern is not closed").withHint("text in [brackets] is copied as is")
			}
			b.WriteString(pat[i+1 : i+end])
			i += end + 1
			continue
		}
		// A stray letter or a strftime % used to be copied through, so
		// "%V" or "EEEE" came back as that text: a wrong answer that read
		// as right. Letters and % are tokens or refused; other bytes
		// (digits, punctuation, spaces, non-ASCII text) are copied.
		if pat[i] == '%' {
			found := "%"
			if i+1 < len(pat) && isASCIILetter(pat[i+1]) {
				found = pat[i : i+2]
			}
			return nil, foreignToken(a.pos, found)
		}
		if !isASCIILetter(pat[i]) {
			b.WriteByte(pat[i])
			i++
			continue
		}
		j := i + 1
		for j < len(pat) && pat[j] == pat[i] {
			j++
		}
		tok := pat[i:j]
		if !dateTokens[tok] {
			return nil, foreignToken(a.pos, tok)
		}
		b.WriteString(formatToken(t, tok))
		i = j
	}
	if err := e.charge(ceilDiv(b.Len(), 64), a.pos); err != nil {
		return nil, err
	}
	return b.String(), nil
}

func isASCIILetter(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }

// foreignToken refuses a pattern token datefmt does not have, naming the
// one to write. found is a run of one letter, or % with the letter after it.
func foreignToken(pos int, found string) *Error {
	return errf(codeBadDate, pos, "datefmt: %q in the pattern is not a token", found).
		withHint(tokenAdvice(found) + "; text in [brackets] is copied as is")
}

// tokenAdvice maps the strftime (%A), Java/ICU (EEEE, yyyy, dd) and Moment
// (DDD, Q, w) forms models write to this pattern's token or function.
func tokenAdvice(found string) string {
	switch found {
	case "%A":
		return "write dddd for the weekday name"
	case "%a":
		return "write ddd for the short weekday name"
	case "%Y", "%G":
		return "write YYYY for the year"
	case "%y":
		return "write YY for the two-digit year"
	case "%B":
		return "write MMMM for the month name"
	case "%b", "%h":
		return "write MMM for the short month name"
	case "%m":
		return "write MM for the month number"
	case "%d":
		return "write DD for the day of the month"
	case "%e":
		return "write D for the day of the month"
	case "%H", "%I":
		return "write HH for the hour, 00 to 23"
	case "%M":
		return "write mm for the minute"
	case "%S":
		return "write ss for the second"
	case "%V", "%W", "%U":
		return "use isoweek(d) for the week number"
	case "%j":
		return "use dayofyear(d) for the day of the year"
	case "%u", "%w":
		return "use weekday(d) for the weekday number, 1 (Monday) to 7"
	case "%p":
		return "there is no AM/PM: HH is 00 to 23"
	case "%":
		return "a literal % goes in brackets: [%]"
	}
	if found[0] == '%' {
		return tokenList
	}
	switch found[0] {
	case 'E':
		if len(found) >= 4 {
			return "write dddd for the weekday name"
		}
		return "write ddd for the short weekday name"
	case 'y', 'Y':
		if len(found) == 2 {
			return "write YY for the two-digit year"
		}
		return "write YYYY for the year"
	case 'M':
		return "write MMMM for the month name"
	case 'd':
		return "write DD for the day of the month"
	case 'D':
		return "use dayofyear(d) for the day of the year"
	case 'w', 'W':
		return "use isoweek(d) for the week number"
	case 'Q':
		return "use quarter(d) for the quarter"
	case 'H', 'h', 'k':
		return "write HH for the hour, 00 to 23"
	case 'a', 'A':
		return "there is no AM/PM: HH is 00 to 23"
	case 'm':
		return "write mm for the minute"
	case 's':
		return "write ss for the second"
	}
	return tokenList
}

func formatToken(t time.Time, tok string) string {
	switch tok {
	case "YYYY":
		return t.Format("2006")
	case "YY":
		return t.Format("06")
	case "MMMM":
		return t.Month().String()
	case "MMM":
		return t.Month().String()[:3]
	case "MM":
		return t.Format("01")
	case "M":
		return t.Format("1")
	case "DD":
		return t.Format("02")
	case "D":
		return t.Format("2")
	case "dddd":
		return t.Weekday().String()
	case "ddd":
		return t.Weekday().String()[:3]
	case "HH":
		return t.Format("15")
	case "mm":
		return t.Format("04")
	}
	return t.Format("05") // ss
}
