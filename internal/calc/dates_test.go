package calc

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
	"time"
	// The zone tests must not depend on the host having /usr/share/zoneinfo;
	// the loomcycle binary embeds the same database (cmd/loomcycle/main.go).
	_ "time/tzdata"
)

// Expected calendar facts below were taken from Python's datetime module
// (isocalendar, isoweekday, a day-by-day working-day loop), not from the
// code under test.

func TestDates_ValuesRenderAsISO8601(t *testing.T) {
	checkTexts(t, []textCase{
		{`date("2026-11-20")`, "2026-11-20"},
		{`date(2026, 11, 20)`, "2026-11-20"},
		{`date("0001-01-01")`, "0001-01-01"},
		{`datetime("2026-11-20T14:30:00Z")`, "2026-11-20T14:30:00Z"},
		{`datetime("2026-11-20T14:30")`, "2026-11-20T14:30:00Z"},
		{`datetime("2026-11-20 14:30:05.25")`, "2026-11-20T14:30:05.25Z"},
		{`datetime("2026-11-20T16:30:00+02:00")`, "2026-11-20T14:30:00Z"},
		{`datetime("2026-11-20T09:30:00-0500")`, "2026-11-20T14:30:00Z"},
		{`datetime("2026-11-20")`, "2026-11-20T00:00:00Z"},
		{`datetime(date("2026-11-20"))`, "2026-11-20T00:00:00Z"},
		{`date(datetime("2026-11-20T23:30:00Z"))`, "2026-11-20"},
		{`date(tz(datetime("2026-11-20T23:30:00Z"), "Europe/Kyiv"))`, "2026-11-21"},
		{`[date("2026-11-20"), date("2026-12-01")]`, "[2026-11-20, 2026-12-01]"},
		{`date(["2026-01-01", "2026-02-01"])`, "[2026-01-01, 2026-02-01]"},
	})
}

func TestDates_ResponseReportsTypeExactAndAStringResult(t *testing.T) {
	r := mustEval(t, `date("2026-11-20")`)
	if r.Type != "date" || !r.Exact || r.Result != "2026-11-20" {
		t.Errorf("date: type %q exact %v result %#v", r.Type, r.Exact, r.Result)
	}
	r = mustEval(t, `datetime("2026-11-20T14:30:00Z")`)
	if r.Type != "datetime" || !r.Exact || r.Result != "2026-11-20T14:30:00Z" {
		t.Errorf("datetime: type %q exact %v result %#v", r.Type, r.Exact, r.Result)
	}
	if r := mustEval(t, `[date("2026-11-20"), sqrt(2)]`); r.Exact {
		t.Error("a list holding a float reported exact")
	}
	b, _ := json.Marshal(mustEval(t, `d = date("2026-11-20"); d + 1`))
	if !strings.Contains(string(b), `"result":"2026-11-21"`) || !strings.Contains(string(b), `"d":"2026-11-20"`) {
		t.Errorf("JSON = %s", b)
	}
}

func TestDates_ArithmeticCountsDaysAndSeconds(t *testing.T) {
	checkTexts(t, []textCase{
		{`date("2026-12-31") - date("2026-11-20")`, "41"},
		{`date("2026-11-20") - date("2026-12-31")`, "-41"},
		{`date("2026-11-20") + 30`, "2026-12-20"},
		{`30 + date("2026-11-20")`, "2026-12-20"},
		{`date("2026-11-20") - 30`, "2026-10-21"},
		{`date("2026-11-20") + 2.0`, "2026-11-22"},
		{`date("2026-11-20") + [0, 7, 14]`, "[2026-11-20, 2026-11-27, 2026-12-04]"},
		{`datetime("2026-11-20T14:30:00Z") + 3600`, "2026-11-20T15:30:00Z"},
		{`datetime("2026-11-20T14:30:00Z") - 0.5`, "2026-11-20T14:29:59.5Z"},
		{`datetime("2026-11-20T00:00:00.5Z") - datetime("2026-11-20T00:00:00Z")`, "0.5"},
		{`datetime("2026-11-21T00:00:00Z") - datetime("2026-11-20T00:00:00Z")`, "86400"},
		{`datetime("2026-11-20T14:30:00+02:00") - datetime("2026-11-20T12:30:00Z")`, "0"},
		{`date("2026-11-20") < date("2026-11-21")`, "true"},
		{`date("2026-11-20") == date(2026, 11, 20)`, "true"},
		{`date("2026-11-20") == 5`, "false"},
		{`tz(datetime("2026-11-20T12:00:00Z"), "Asia/Tokyo") == datetime("2026-11-20T12:00:00Z")`, "true"},
		{`datetime("2026-11-20T12:00:00Z") >= datetime("2026-11-20T12:00:00Z")`, "true"},
		{`sort([date("2026-03-01"), date("2025-12-31"), date("2026-01-15")])`, "[2025-12-31, 2026-01-15, 2026-03-01]"},
		{`len(unique([date("2026-01-01"), date(2026, 1, 1), date("2026-01-02")]))`, "2"},
	})
	if r := mustEval(t, `datetime("2026-11-20T00:00:01Z") - datetime("2026-11-20T00:00:00Z")`); !r.Exact || r.Type != "number" {
		t.Errorf("a datetime difference: type %q exact %v", r.Type, r.Exact)
	}
}

func TestDates_MixingKindsAndOtherOperatorsAreRefused(t *testing.T) {
	for _, expr := range []string{
		`date("2026-11-20") - datetime("2026-11-20T00:00:00Z")`,
		`date("2026-11-20") < datetime("2026-11-20T00:00:00Z")`,
		`date("2026-11-20") == datetime("2026-11-20T00:00:00Z")`,
	} {
		if e := mustFail(t, expr, codeType); !strings.Contains(e.Hint, "date(dt)") {
			t.Errorf("%s: hint %q does not name the conversion", expr, e.Hint)
		}
	}
	for _, expr := range []string{
		`date("2026-11-20") * 2`,
		`date("2026-11-20") + date("2026-11-20")`,
		`-date("2026-11-20")`,
		`1 - date("2026-11-20")`,
		`date("2026-11-20") + 1.5`,
		`date("2026-11-20") < 3`,
		`date("2026-11-20") .* 2`,
		`date("2026-11-20") + "1"`,
		`sort([date("2026-11-20"), 1])`,
		`hour(date("2026-11-20"))`,
		`tz(date("2026-11-20"), "UTC")`,
		`addmonths(datetime("2026-11-20T00:00:00Z"), 1)`,
		`year("2026-11-20")`,
	} {
		mustFail(t, expr, codeType)
	}
}

func TestDates_AmbiguousAndMalformedTextIsRefused(t *testing.T) {
	e := mustFail(t, `date("11/12/2026")`, codeBadDate)
	if !strings.Contains(e.Hint, "ambiguous") {
		t.Errorf("11/12/2026: hint %q", e.Hint)
	}
	mustFail(t, `date("12.11.2026")`, codeBadDate)
	mustFail(t, `date("20261120")`, codeBadDate)
	mustFail(t, `date("Nov 20, 2026")`, codeBadDate)
	mustFail(t, `date(" 2026-11-20")`, codeBadDate)
	if e := mustFail(t, `date("2026-11-20T10:00:00Z")`, codeBadDate); !strings.Contains(e.Hint, "datetime") {
		t.Errorf("a datetime given to date: hint %q", e.Hint)
	}
	mustFail(t, `datetime("2026-11-20T25:00:00Z")`, codeDomain)
	mustFail(t, `datetime("2026-11-20T23:59:60Z")`, codeDomain)
	mustFail(t, `datetime("2026-11-20T10:00:00+25:00")`, codeBadDate)
	mustFail(t, `datetime("2026-11-20T10:00:00 UTC")`, codeBadDate)
	mustFail(t, `date("`+strings.Repeat("9", 1000)+`")`, codeBadDate)
	mustFail(t, `date("2026-02-29")`, codeDomain)
	mustFail(t, `date("1900-02-29")`, codeDomain)
	mustFail(t, `date(2026, 13, 1)`, codeDomain)
	mustFail(t, `date(2026, 11)`, codeArity)
	mustFail(t, `date(20261120)`, codeType)
}

func TestDates_RangeIsYearsOneTo9999(t *testing.T) {
	mustFail(t, `date("0000-12-31")`, codeDomain)
	mustFail(t, `date("9999-12-31") + 1`, codeOverflow)
	mustFail(t, `date("0001-01-01") - 1`, codeOverflow)
	mustFail(t, `date("2026-01-01") + 10^9`, codeOverflow)
	mustFail(t, `addyears(date("9999-01-01"), 1)`, codeOverflow)
	mustFail(t, `addmonths(date("0001-01-31"), -1)`, codeOverflow)
	mustFail(t, `datetime("9999-12-31T23:59:59Z") + 1`, codeOverflow)
	mustFail(t, `datetime("0001-01-01T00:00:00Z") - 1`, codeOverflow)
	mustFail(t, `datetime("0001-01-01T00:30:00+01:00")`, codeOverflow)
	mustFail(t, `datetime("2026-01-01T00:00:00Z") + 10^30`, codeOverflow)
	mustFail(t, `addworkdays(date("9999-12-01"), 100)`, codeOverflow)
	checkTexts(t, []textCase{
		{`date("9999-12-31") - date("0001-01-01")`, "3652058"},
		{`datetime("9999-12-31T23:59:59.999999999Z") - datetime("0001-01-01T00:00:00Z")`, "315537897599.999999999"},
	})
}

func TestDates_CalendarStepsClampToTheMonthEnd(t *testing.T) {
	checkTexts(t, []textCase{
		{`addmonths(date("2026-01-31"), 1)`, "2026-02-28"},
		{`addmonths(date("2024-01-31"), 1)`, "2024-02-29"},
		{`addmonths(date("2026-03-31"), -1)`, "2026-02-28"},
		{`addmonths(date("2026-11-30"), 3)`, "2027-02-28"},
		{`addmonths(date("2026-11-20"), -11)`, "2025-12-20"},
		{`addmonths(date("2026-11-20"), 0)`, "2026-11-20"},
		{`addyears(date("2024-02-29"), 1)`, "2025-02-28"},
		{`addyears(date("2024-02-29"), 4)`, "2028-02-29"},
		{`addyears(date("2000-02-29"), -100)`, "1900-02-28"},
		{`eomonth(date("2026-01-15"), 0)`, "2026-01-31"},
		{`eomonth(date("2026-01-15"), 1)`, "2026-02-28"},
		{`eomonth(date("2026-01-15"), -1)`, "2025-12-31"},
		{`eomonth(date("2024-01-31"), 1)`, "2024-02-29"},
	})
}

func TestDates_LeapYearsFollowTheGregorianRule(t *testing.T) {
	checkTexts(t, []textCase{
		{`isleap(1900)`, "false"},
		{`isleap(2000)`, "true"},
		{`isleap(2024)`, "true"},
		{`isleap(2026)`, "false"},
		{`isleap(2100)`, "false"},
		{`isleap(date("2000-06-01"))`, "true"},
		{`daysinmonth(date("1900-02-01"))`, "28"},
		{`daysinmonth(date("2000-02-01"))`, "29"},
		{`date("2000-02-29") + 1`, "2000-03-01"},
		{`date("1900-03-01") - date("1900-02-28")`, "1"},
		{`date("2000-03-01") - date("2000-02-28")`, "2"},
		{`dayofyear(date("2020-12-31"))`, "366"},
	})
}

func TestDates_ISOWeeksAtYearBoundaries(t *testing.T) {
	checkTexts(t, []textCase{
		{`isoweek(date("2020-12-31"))`, "53"},
		{`isoweek(date("2021-01-03"))`, "53"},
		{`isoweek(date("2021-01-04"))`, "1"},
		{`isoweek(date("2026-12-28"))`, "53"},
		{`isoweek(date("2027-01-01"))`, "53"},
		{`isoweek(date("2024-12-30"))`, "1"},
		{`isoweek(date("2026-11-20"))`, "47"},
		{`weekday(date("2026-11-20"))`, "5"},
		{`weekday(date("2021-01-03"))`, "7"},
		{`weekday(date("2021-01-04"))`, "1"},
		{`weekday(date("0001-01-01"))`, "1"},
		{`quarter(date("2026-11-20"))`, "4"},
		{`quarter(date("2026-03-31"))`, "1"},
		{`[year(date("2026-11-20")), month(date("2026-11-20")), day(date("2026-11-20"))]`, "[2026, 11, 20]"},
	})
}

func TestDates_DifferencesCountCompleteUnits(t *testing.T) {
	checkTexts(t, []textCase{
		{`datediff(date("2026-01-31"), date("2026-02-28"), "months")`, "0"},
		{`datediff(date("2026-01-31"), date("2026-03-01"), "months")`, "1"},
		{`datediff(date("2026-01-31"), date("2026-03-31"), "months")`, "2"},
		{`datediff(date("2026-03-01"), date("2026-01-31"), "months")`, "-1"},
		{`datediff(date("2024-02-29"), date("2025-02-28"), "years")`, "0"},
		{`datediff(date("2024-02-29"), date("2025-03-01"), "years")`, "1"},
		{`datediff(date("2025-03-01"), date("2024-02-29"), "years")`, "-1"},
		{`datediff(date("2026-11-20"), date("2026-12-03"), "weeks")`, "1"},
		{`datediff(date("2026-12-03"), date("2026-11-20"), "weeks")`, "-1"},
		{`datediff(date("2026-11-20"), date("2026-12-31"), "days")`, "41"},
		{`datediff(date("2026-11-20"), date("2026-12-31"), "day")`, "41"},
		{`age(date("1990-11-21"), date("2026-11-20"))`, "35"},
		{`age(date("1990-11-20"), date("2026-11-20"))`, "36"},
		{`age(date("2000-02-29"), date("2026-02-28"))`, "25"},
	})
	mustFail(t, `datediff(date("2026-01-01"), date("2026-02-01"), "fortnights")`, codeDomain)
	mustFail(t, `age(date("2026-11-21"), date("2026-11-20"))`, codeDomain)
}

// The published values: Microsoft's YEARFRAC page (1 Jan 2012 to 30 Jul
// 2012: basis 0 0.58055556, basis 1 0.57650273, basis 3 0.57808219) and the
// ISDA 1999 memo on Actual/Actual (1 Nov 2003 to 1 May 2004: ISDA
// 0.497724380567).
func TestDates_YearFracMatchesPublishedValues(t *testing.T) {
	checkTexts(t, []textCase{
		{`round(yearfrac(date("2012-01-01"), date("2012-07-30"), "30/360"), 8)`, "0.58055556"},
		{`round(yearfrac(date("2012-01-01"), date("2012-07-30"), "act/act"), 8)`, "0.57650273"},
		{`round(yearfrac(date("2012-01-01"), date("2012-07-30"), "act/365"), 8)`, "0.57808219"},
		{`round(yearfrac(date("2003-11-01"), date("2004-05-01"), "act/act"), 12)`, "0.497724380567"},
		{`yearfrac(date("2003-11-01"), date("2004-05-01"), "act/360")`, "0.505555555555556"},
		{`yearfrac(date("2004-05-01"), date("2003-11-01"), "act/365")`, "-0.498630136986301"},
		// Excel's basis 0 treats the last day of February as the 30th when
		// both ends are one.
		{`yearfrac(date("2024-02-29"), date("2025-02-28"), "30/360")`, "1"},
		{`yearfrac(date("2026-01-31"), date("2026-03-31"), "30/360")`, "0.166666666666667"},
		{`yearfrac(date("2026-01-30"), date("2026-03-31"), "30e/360")`, "0.166666666666667"},
		{`yearfrac(date("2026-01-01"), date("2028-01-01"), "act/act")`, "2"},
	})
	if r := mustEval(t, `yearfrac(date("2012-01-01"), date("2012-07-30"), "30/360")`); !r.Exact || r.Fraction != "209/360" {
		t.Errorf("30/360: exact %v fraction %q, want 209/360", r.Exact, r.Fraction)
	}
	mustFail(t, `yearfrac(date("2026-01-01"), date("2026-02-01"), "act/364")`, codeDomain)
}

// Microsoft's XNPV and XIRR pages use one example: -10,000 on 1 Jan 2008,
// then 2,750 / 4,250 / 3,250 / 2,750 on 1 Mar 2008, 30 Oct 2008, 15 Feb 2009
// and 1 Apr 2009. XNPV at 9% is 2,086.65; XIRR is 0.373362535, which is
// Excel's iterate stopped at its 0.000001% tolerance: the root, by
// bisection in Python, is 0.3733625335, so the check is to 8 places.
func TestDates_XNPVAndXIRRMatchExcelsExamples(t *testing.T) {
	vars := map[string]any{
		"flows": []any{-10000, 2750, 4250, 3250, 2750},
		"ds":    []any{"2008-01-01", "2008-03-01", "2008-10-30", "2009-02-15", "2009-04-01"},
	}
	for expr, want := range map[string]string{
		`round(xnpv(0.09, flows, map(ds, s -> date(s))), 2)`:                               "2086.65",
		`round(xirr(flows, map(ds, s -> date(s))), 8)`:                                     "0.37336253",
		`round(xirr(flows, map(ds, s -> date(s)), 0.5), 8)`:                                "0.37336253",
		`round(xirr(flows, map(ds, s -> date(s)), -0.9), 8)`:                               "0.37336253",
		`round(xnpv(xirr(flows, map(ds, s -> date(s))), flows, map(ds, s -> date(s))), 6)`: "0",
	} {
		r, err := Eval(context.Background(), Request{Expr: expr, Vars: rawVars(t, vars)})
		if err != nil || r.Text != want {
			t.Errorf("%s = %q, %v; want %s", expr, r.Text, err, want)
		}
	}
	for expr, code := range map[string]errCode{
		`xirr([100, 200], [date("2026-01-01"), date("2027-01-01")])`:      codeDomain,
		`xirr([-100, 110], [date("2026-01-01")])`:                         codeShape,
		`xirr([-100, 110], [date("2026-01-01"), date("2025-01-01")])`:     codeDomain,
		`xnpv(-1, [-100, 110], [date("2026-01-01"), date("2027-01-01")])`: codeDomain,
		`xnpv(0.1, [-100, 110], ["2026-01-01", "2027-01-01"])`:            codeType,
	} {
		mustFail(t, expr, code)
	}
	checkTexts(t, []textCase{
		{`round(xirr([-100, 110], [date("2026-01-01"), date("2027-01-01")]), 12)`, "0.1"},
	})
}

func TestDates_WorkingDaysCountInclusiveMondayToFriday(t *testing.T) {
	checkTexts(t, []textCase{
		{`workdays(date("2026-11-01"), date("2026-11-30"))`, "21"},
		{`workdays(date("2026-11-01"), date("2026-11-30"), [date("2026-11-26"), date("2026-11-27")])`, "19"},
		{`workdays(date("2026-11-01"), date("2026-11-30"), [date("2026-11-26"), date("2026-11-26"), date("2026-11-28")])`, "20"},
		{`workdays(date("2026-11-30"), date("2026-11-01"))`, "-21"},
		{`workdays(date("2026-11-21"), date("2026-11-22"))`, "0"},
		{`workdays(date("2026-11-20"), date("2026-11-20"))`, "1"},
		{`workdays(date("0001-01-01"), date("9999-12-31"))`, "2608615"},
		{`addworkdays(date("2026-11-20"), 5)`, "2026-11-27"},
		{`addworkdays(date("2026-11-21"), 1)`, "2026-11-23"},
		{`addworkdays(date("2026-11-21"), -1)`, "2026-11-20"},
		{`addworkdays(date("2026-11-20"), 0)`, "2026-11-20"},
		{`addworkdays(date("2026-11-20"), 10, [date("2026-11-26"), date("2026-11-27")])`, "2026-12-08"},
		{`addworkdays(date("2026-12-01"), -3, date("2026-11-27"))`, "2026-11-25"},
		{`addworkdays(date("2000-01-03"), 2000000)`, "9666-02-22"},
	})
	mustFail(t, `workdays(date("2026-11-01"), date("2026-11-30"), ["2026-11-26"])`, codeType)
}

// TestDates_WorkingDaysAgreeWithADayByDayCount checks the O(1) counts
// against the obvious loop on random spans, starts and holiday sets.
func TestDates_WorkingDaysAgreeWithADayByDayCount(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	base := civilDays(2026, time.January, 1)
	isWork := func(d int64, hs map[int64]bool) bool { return weekdayOf(d) <= 5 && !hs[d] }
	for range 2000 {
		a := base + rng.Int64N(60)
		b := a + rng.Int64N(60) - 20
		hs := map[int64]bool{}
		var hl []val
		for range rng.IntN(6) {
			h := base + rng.Int64N(120) - 20
			hs[h] = true
			hl = append(hl, dateVal{days: h})
		}
		lo, hi, sign := a, b, int64(1)
		if lo > hi {
			lo, hi, sign = hi, lo, -1
		}
		want := int64(0)
		for d := lo; d <= hi; d++ {
			if isWork(d, hs) {
				want++
			}
		}
		e := &evaluator{ctx: context.Background(), funcs: builtinIndex}
		got, err := fnWorkdays(e, args{v: []val{dateVal{a}, dateVal{b}, hl}, name: "workdays"})
		if err != nil || cmpNum(got.(num), exactInt64(sign*want)) != 0 {
			t.Fatalf("workdays(%s, %s, %v) = %v, %v; want %d", dateVal{a}, dateVal{b}, hl, got, err, sign*want)
		}
		n := rng.Int64N(40) - 20
		d, left := a, n
		for left != 0 {
			if n > 0 {
				d++
				if isWork(d, hs) {
					left--
				}
			} else {
				d--
				if isWork(d, hs) {
					left++
				}
			}
		}
		got, err = fnAddWorkdays(e, args{v: []val{dateVal{a}, exactInt64(n), hl}, name: "addworkdays"})
		if err != nil || got.(dateVal).days != d {
			t.Fatalf("addworkdays(%s, %d, %v) = %v, %v; want %s", dateVal{a}, n, hl, got, err, dateVal{d})
		}
	}
}

// Kyiv moved to summer time at 01:00 UTC on 29 March 2026 (03:00 → 04:00)
// and back at 01:00 UTC on 25 October; New York at 07:00 UTC on 8 March
// (02:00 → 03:00) and back at 06:00 UTC on 1 November.
func TestDates_NamedZonesFollowDaylightSavingChanges(t *testing.T) {
	checkTexts(t, []textCase{
		{`tz(datetime("2026-03-29T00:30:00Z"), "Europe/Kyiv")`, "2026-03-29T02:30:00+02:00"},
		{`tz(datetime("2026-03-29T00:30:00Z"), "Europe/Kyiv") + 3600`, "2026-03-29T04:30:00+03:00"},
		{`hour(tz(datetime("2026-03-29T01:30:00Z"), "Europe/Kyiv"))`, "4"},
		{`tz(datetime("2026-10-25T00:30:00Z"), "Europe/Kyiv") + 3600`, "2026-10-25T03:30:00+02:00"},
		{`tz(datetime("2026-03-08T06:30:00Z"), "America/New_York")`, "2026-03-08T01:30:00-05:00"},
		{`tz(datetime("2026-03-08T06:30:00Z"), "America/New_York") + 3600`, "2026-03-08T03:30:00-04:00"},
		{`tz(datetime("2026-11-01T05:30:00Z"), "America/New_York")`, "2026-11-01T01:30:00-04:00"},
		{`tz(datetime("2026-11-01T05:30:00Z"), "America/New_York") + 3600`, "2026-11-01T01:30:00-05:00"},
		{`tz(datetime("2026-11-20T14:30:00Z"), "UTC")`, "2026-11-20T14:30:00Z"},
		{`datefmt(tz(datetime("2026-11-20T14:30:00Z"), "Asia/Kolkata"), "HH:mm")`, "20:00"},
		{`[minute(datetime("2026-11-20T14:05:09.5Z")), second(datetime("2026-11-20T14:05:09.5Z"))]`, "[5, 9.5]"},
	})
	for _, zone := range []string{"Nowhere/Atlantis", "EST5", "CEST", "PST", "Local", "../../etc/passwd", "/etc/localtime", "", strings.Repeat("A", 100)} {
		in, _ := json.Marshal(zone)
		mustFail(t, `tz(datetime("2026-11-20T00:00:00Z"), `+string(in)+`)`, codeBadDate)
	}
}

func TestDates_DatefmtWritesTokensAndCopiesPunctuation(t *testing.T) {
	checkTexts(t, []textCase{
		{`datefmt(date("2026-11-05"), "YYYY-MM-DD")`, "2026-11-05"},
		{`datefmt(date("2026-11-05"), "D MMMM YYYY")`, "5 November 2026"},
		{`datefmt(date("2026-11-05"), "ddd, D MMM YY")`, "Thu, 5 Nov 26"},
		{`datefmt(date("2026-11-05"), "dddd M/D")`, "Thursday 11/5"},
		{`datefmt(date("2026-11-05"), "[Due:] DD.MM.YYYY")`, "Due: 05.11.2026"},
		{`datefmt(date("2026-11-05"), "HH:mm:ss")`, "00:00:00"},
		{`datefmt(datetime("2026-11-05T07:08:09Z"), "YYYY-MM-DD HH:mm:ss")`, "2026-11-05 07:08:09"},
		{`datefmt(date("2026-11-05"), "YYYYMMDD")`, "20261105"},
		{`datefmt(date("2026-11-05"), "(D) 1 - 2 ! # _ , ; @")`, "(5) 1 - 2 ! # _ , ; @"},
		{`datefmt(date("2026-11-05"), "D.MM.YYYY г.")`, "5.11.2026 г."},
	})
	mustFail(t, `datefmt(date("2026-11-05"), "[open")`, codeBadDate)
	mustFail(t, `datefmt(date("2026-11-05"), "`+strings.Repeat("Y", 101)+`")`, codeDomain)
}

func TestDates_DatefmtBracketsKeepLettersAsText(t *testing.T) {
	checkTexts(t, []textCase{
		{`datefmt(date("2026-11-05"), "[Week of] D MMM")`, "Week of 5 Nov"},
		{`datefmt(date("2026-11-05"), "[Q x] !")`, "Q x !"},
		{`datefmt(date("2026-11-05"), "D[%]")`, "5%"},
		{`datefmt(date("2026-11-05"), "[YYYY] YYYY")`, "YYYY 2026"},
		{`datefmt(date("2026-11-05"), "[]D")`, "5"},
	})
}

// A letter that is not a token used to be copied through, so "%V" or
// "EEEE" came back as the literal text: a wrong answer that looked
// right. Each is refused now, and the hint names what to write.
func TestDates_DatefmtRefusesForeignTokensWithTheRightOne(t *testing.T) {
	for _, c := range []struct{ pat, found, hint string }{
		{"%A", "%A", "dddd"},
		{"EEEE", "EEEE", "dddd"},
		{"%a", "%a", "ddd"},
		{"EEE", "EEE", "ddd"},
		{"%Y-%m-%d", "%Y", "YYYY"},
		{"yyyy-MM-dd", "yyyy", "YYYY"},
		{"%y", "%y", "YY"},
		{"%m", "%m", "MM"},
		{"%d", "%d", "DD"},
		{"MM/dd", "dd", "DD"},
		{"%H", "%H", "HH"},
		{"%M", "%M", "mm"},
		{"%S", "%S", "ss"},
		{"%B", "%B", "MMMM"},
		{"%b", "%b", "MMM"},
		{"%V", "%V", "isoweek(d)"},
		{"%W", "%W", "isoweek(d)"},
		{"[Week] ww", "ww", "isoweek(d)"},
		{"%j", "%j", "dayofyear(d)"},
		{"DDD", "DDD", "dayofyear(d)"},
		{"Q", "Q", "quarter(d)"},
		{"h:mm a", "h", "HH"},
		{"HH:mm A", "A", "HH"},
		{"Do MMM", "o", "YYYY YY MMMM MMM MM M DD D dddd ddd HH mm ss"},
		{"YYYYY", "YYYYY", "YYYY"},
		{"MMMMM", "MMMMM", "MMMM"},
		{"Week YYYY", "W", "isoweek(d)"},
		{"50%", "%", "[%]"},
	} {
		in, _ := json.Marshal(c.pat)
		err := mustFail(t, `datefmt(date("2026-11-05"), `+string(in)+`)`, codeBadDate)
		if want := strconv.Quote(c.found); !strings.Contains(err.Detail, want) {
			t.Errorf("datefmt %q: detail %q does not name %s", c.pat, err.Detail, want)
		}
		if !strings.Contains(err.Hint, c.hint) || !strings.Contains(err.Hint, "[brackets]") {
			t.Errorf("datefmt %q: hint %q, want one naming %q and [brackets]", c.pat, err.Hint, c.hint)
		}
	}
}

// today() and now() read Request.Now, so a test pins the clock and two
// evaluations agree.
func TestDates_TodayAndNowReadTheRequestClock(t *testing.T) {
	clock := func() time.Time { return time.Date(2026, 11, 20, 23, 30, 15, 500, time.FixedZone("", -5*3600)) }
	for expr, want := range map[string]string{
		`today()`:                             "2026-11-21",
		`now()`:                               "2026-11-21T04:30:15Z",
		`age(date("2000-11-21"))`:             "26",
		`date("2026-12-25") - today()`:        "34",
		`hour(tz(now(), "America/New_York"))`: "23",
	} {
		r, err := Eval(context.Background(), Request{Expr: expr, Now: clock})
		if err != nil || r.Text != want {
			t.Errorf("%s = %q, %v; want %s", expr, r.Text, err, want)
		}
	}
	// One evaluation reads the clock once, even if it ticks in between.
	ticks := time.Date(2026, 11, 20, 23, 59, 59, 0, time.UTC)
	ticking := func() time.Time { ticks = ticks.Add(time.Second); return ticks }
	r, err := Eval(context.Background(), Request{Expr: `[now() - now(), today() - today()]`, Now: ticking})
	if err != nil || r.Text != "[0, 0]" {
		t.Errorf("a ticking clock: %q, %v; want [0, 0]", r.Text, err)
	}
	// Without a pinned clock, the server's: today() is within a day of it.
	r = mustEval(t, `today()`)
	if got, _ := time.Parse("2006-01-02", r.Text); time.Since(got) < 0 || time.Since(got) > 48*time.Hour {
		t.Errorf("today() = %s", r.Text)
	}
}

func TestDates_StepsAreChargedNotLooped(t *testing.T) {
	// A span of 2.6 million working days with a thousand holidays stays
	// inside the step bound only if it is not walked once per day.
	start := time.Now()
	r, err := Eval(context.Background(), Request{Expr: `addworkdays(date("0001-01-01"), 2600000, date("2026-11-20") + (0:1000))`})
	if err != nil {
		t.Fatalf("addworkdays with 1001 holidays: %v", err)
	}
	// 9969-09-22 is from a day-by-day Python loop.
	if r.Text != "9969-09-22" {
		t.Errorf("result %q, want 9969-09-22", r.Text)
	}
	if d := time.Since(start); d > MaxWall {
		t.Errorf("took %v", d)
	}
	// Formatting is charged by output size, a step per 64 bytes, so a
	// million formatted strings cannot hold more than 64 MB.
	for pat, want := range map[string]int{"YYYY": 1, "[" + strings.Repeat("x", 98) + "]": 2} {
		e := &evaluator{ctx: context.Background(), nextCheck: ctxCheckEvery}
		if _, err := fnDateFmt(e, args{v: []val{dateVal{}, pat}, name: "datefmt"}); err != nil || e.steps != want {
			t.Errorf("datefmt with a %d-byte pattern charged %d steps (%v), want %d", len(pat), e.steps, err, want)
		}
	}
}
