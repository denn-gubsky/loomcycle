package calc

import (
	"context"
	"testing"
)

// Known answers from the Excel function reference (support.microsoft.com,
// the example of each function), unless noted.
func TestFinance_MatchesTheSpreadsheetWorkedExamples(t *testing.T) {
	checkExact(t, true, []textCase{
		// PMT: 8% a year, 10 monthly payments, 10,000 loan → -1,037.03.
		{"round(pmt(0.08/12, 10, 10000), 2)", "-1037.03"},
		// PMT: saving 50,000 in 18 years at 6% → -129.08 a month.
		{"round(pmt(0.06/12, 18*12, 0, 50000), 2)", "-129.08"},
		// FV: 6%, 10 payments of -200 at the start, -500 already in → 2,581.40.
		{"round(fv(0.06/12, 10, -200, -500, 1), 2)", "2581.4"},
		// PV: an annuity paying 500 a month for 20 years at 8% → -59,777.15.
		{"round(pv(0.08/12, 12*20, 500), 2)", "-59777.15"},
		// NPV: 10%, a 10,000 outlay a year from now and three returns → 1,188.44.
		{"round(npv(0.1, [-10000, 3000, 4200, 6800]), 2)", "1188.44"},
		{"compound(1000, 0.05, 2)", "1102.5"},
		// A 30-year mortgage stays exact: 5% a year, 200,000 → 1,073.64 a month.
		{"round(pmt(0.05/12, 360, 200000), 2)", "-1073.64"},
		// The numbers-help example: 5% a year compounded monthly for 10 years.
		{"round(compound(1000, 0.05, 10, 12), 2)", "1647.01"},
		{"pct(45, 180)", "25"},
		{"pctchange(1840, 2125)", "15.4891304347826"},
		{"pctchange(80, 60)", "-25"},
		{"pmt(0, 12, 1200)", "-100"},
		{"fv(0, 12, -100)", "1200"},
		{"pv(0, 12, -100, 0)", "1200"},
		{"nper(0, -100, 1200)", "12"},
	})
	checkExact(t, false, []textCase{
		// NPER: 12%, -100 at the start of each month, -1,000 in, 10,000 target → 59.6738657.
		{"round(nper(0.12/12, -100, -1000, 10000, 1), 7)", "59.6738657"},
		// RATE: a 4-year 8,000 loan at 200 a month → 0.0077014725 (0.77%) a month.
		{"round(periodrate(48, -200, 8000), 10)", "0.0077014725"},
		{"round(periodrate(48, -200, 8000) * 12, 4)", "0.0924"},
		// IRR: -70,000 then five years of income → 8.66%; four years → -2.12%;
		// two years with guess -10% → -44.35%.
		{"round(irr([-70000, 12000, 15000, 18000, 21000, 26000]), 9)", "0.086630948"},
		{"round(irr([-70000, 12000, 15000, 18000, 21000]), 9)", "-0.021244848"},
		{"round(irr([-70000, 12000, 15000], -0.1), 9)", "-0.443506941"},
		{"cagr(100, 121, 2)", "0.1"},
		// cagr with a fractional power: 100 → 200 in 5 years.
		{"round(cagr(100, 200, 5), 9)", "0.148698355"},
	})
}

// pmt, fv and pv round-trip each other: the payment pmt computes, fed to
// pv, gives back the loan. Exact inputs keep the identity exact.
func TestFinance_AnnuityFunctionsAreConsistentAndExact(t *testing.T) {
	checkTexts(t, []textCase{
		{"p = pmt(0.05/12, 60, 20000); pv(0.05/12, 60, p) == 20000", "true"},
		{"p = pmt(0.05/12, 60, 20000, 0, 1); pv(0.05/12, 60, p, 0, 1) == 20000", "true"},
		{"p = pmt(0.04, 10, -1000, 5000); fv(0.04, 10, p, -1000) == 5000", "true"},
		{"round(nper(0.05/12, pmt(0.05/12, 360, 200000), 200000), 9)", "360"},
		{"round(periodrate(360, pmt(0.05/12, 360, 200000), 200000) * 12, 12)", "0.05"},
		{"round(irr([-1000, 1100]), 12)", "0.1"},
		// Far guesses: a wild first step is halved back, not the end of
		// the search.
		{"round(irr([-1000, 1100], 50), 12)", "0.1"},
		{"f = map(1:1000, i -> if(i == 1, -1000, 1)); r = irr(f); [r < 0, abs(npv(r, f)) < 1e-6]", "[true, true]"},
	})
}

func TestFinance_RefusesWhatHasNoAnswer(t *testing.T) {
	for _, expr := range []string{
		// irr: no sign change.
		"irr([100, 200, 300])", "irr([-100, -200])", "irr([0, 0])",
		// irr: changes sign but has no real root (-1 + 3v - 3v² < 0 for all v).
		"irr([-1, 3, -3])",
		// periodrate: receiving 1,000 now and 100 a period, never paying: no rate.
		"periodrate(10, 100, 1000)",
		"periodrate(0, -100, 1000)",
		// nper: payments that never reach the target.
		"nper(0.01, 5, -1000)",
		"nper(0, 0, 1000)",
		// rates of -100% or less, timing other than 0 or 1.
		"pmt(-1, 10, 1000)", "npv(-1.5, [1, 2])", "fv(0.01, 10, -100, 0, 2)",
		"compound(100, 0.05, 2, 0)", "cagr(0, 100, 2)", "cagr(100, 200, 0)",
	} {
		mustFail(t, expr, codeDomain)
	}
	mustFail(t, "pct(5, 0)", codeDivZero)
	mustFail(t, "pctchange(0, 5)", codeDivZero)
	mustFail(t, "npv(0.1, 5)", codeType)
	mustFail(t, `irr(["a", 1])`, codeType)
}

// rate is not a function, so `rate` stays usable as a name: the
// numbers-help example and the signatures' own argument name rely on it.
func TestFinance_RateStaysAnAssignableName(t *testing.T) {
	checkTexts(t, []textCase{
		{"rate = 0.06/12; round(pmt(rate, 24, 5000), 2)", "-221.6"},
	})
	// Called as Excel's RATE, the refusal names the function to use.
	if e := mustFail(t, "rate(48, -200, 8000)", codeUnknownFunc); e.Hint != "use periodrate" {
		t.Errorf("hint = %q, want %q", e.Hint, "use periodrate")
	}
}

// irr's solver charges one step per flow per iteration, so a long series
// passed in vars (where building it costs nothing) still stops at the step
// bound instead of running iterations x flows unbounded.
func TestFinance_IrrOnALongSeriesStaysInsideTheStepLimit(t *testing.T) {
	flows := make([]int, 150_000)
	flows[0] = -1_000_000
	for i := 1; i < len(flows); i++ {
		flows[i] = 1
	}
	_, err := Eval(context.Background(), Request{Expr: "irr(f)", Vars: rawVars(t, map[string]any{"f": flows})})
	if err == nil || err.Code != string(codeStepLimit) {
		t.Fatalf("irr over %d flows: err = %v, want step_limit", len(flows), err)
	}
}
