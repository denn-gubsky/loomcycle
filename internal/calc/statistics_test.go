package calc

import (
	"context"
	"testing"
	"time"
)

// checkExact evaluates each expression and asserts its text and that the
// result is exact (want true) or a float (want false).
func checkExact(t *testing.T, want bool, cases []textCase) {
	t.Helper()
	for _, c := range cases {
		r, err := Eval(context.Background(), Request{Expr: c.expr})
		if err != nil {
			t.Errorf("Eval(%q): %v", c.expr, err)
			continue
		}
		if r.Text != c.want {
			t.Errorf("Eval(%q) = %q, want %q", c.expr, r.Text, c.want)
		}
		if r.Exact != want {
			t.Errorf("Eval(%q) exact = %v, want %v", c.expr, r.Exact, want)
		}
	}
}

// Data from the Wikipedia "Standard deviation" worked example: population
// sd of [2,4,4,4,5,5,7,9] is exactly 2, the sample variance 32/7.
const sdData = "[2, 4, 4, 4, 5, 5, 7, 9]"

// Data from the Excel docs for STDEV.S / STDEV.P / VAR.S / VAR.P (breaking
// strength of ten tools).
const excelSD = "[1345, 1301, 1368, 1322, 1310, 1370, 1318, 1350, 1303, 1299]"

func TestStatistics_ExactResultsOnExactInput(t *testing.T) {
	checkExact(t, true, []textCase{
		{"mean([1, 2, 3, 4])", "2.5"},
		{"mean(1, 2)", "1.5"},
		{"mean([1/3, 1/6])", "0.25"},
		{"median([3, 1, 2])", "2"},
		{"median([4, 1, 3, 2])", "2.5"},
		{"median(1/3, 1/2)", "0.416666666666667"},
		{"mode([3, 3, 1, 2, 2])", "2"},
		{"mode([5])", "5"},
		{"spread([4, 9, 1])", "8"},
		{"variance(" + sdData + ")", "4.57142857142857"},
		{"variance(" + sdData + `, "population")`, "4"},
		{"stdev(" + sdData + `, "population")`, "2"},
		{"variance(" + excelSD + `)`, "754.266666666667"},
		{"variance(" + excelSD + `, "population")`, "678.84"},
		{"zscore(" + sdData + `, "population")`, "[-1.5, -0.5, -0.5, -0.5, 0, 0, 1, 2]"},
		{"harmmean([1, 2, 4])", "1.71428571428571"},
		// Excel docs COVARIANCE.S and COVARIANCE.P examples.
		{"covariance([2, 4, 8], [5, 11, 12])", "9.66666666666667"},
		{`covariance([3, 2, 4, 5, 6], [9, 7, 12, 15, 17], "population")`, "5.2"},
		// Excel docs SLOPE example: slope 0.305556 (11/36); intercept 19/6.
		{"linreg([1, 2, 3], [5, 7, 9])", "[2, 3]"},
		{"correlation([1, 2, 3], [2, 4, 6])", "1"},
		{"cumsum([1, 2, 3, 4])", "[1, 3, 6, 10]"},
		{"cumprod([1, 2, 3, 4])", "[1, 2, 6, 24]"},
		{"diff([1, 4, 9, 16])", "[3, 5, 7]"},
		{"rank([10, 20, 10, 30])", "[1.5, 3, 1.5, 4]"},
		{"rank([3, 3, 3])", "[2, 2, 2]"},
	})
	// Booleans and empty lists carry no exactness; == against the exact
	// fraction proves the value is exact, since a float never equals it.
	checkTexts(t, []textCase{
		{"variance(" + sdData + ") == 32/7", "true"},
		{"harmmean([1, 2, 4]) == 12/7", "true"},
		{"wmean([1, 2, 3], [3, 2, 1]) == 5/3", "true"},
		{"linreg([6, 5, 11, 7, 5, 4, 4], [2, 3, 9, 1, 8, 7, 5]) == [11/36, 19/6]", "true"},
		{"cumsum([])", "[]"},
		{"diff([7])", "[]"},
		{"quantile([4, 1, 3, 2], 0.5) == median([4, 1, 3, 2])", "true"},
	})
}

func TestStatistics_FloatResults(t *testing.T) {
	checkExact(t, false, []textCase{
		{"stdev(" + sdData + ")", "2.1380899352994"},
		// Excel docs: STDEV.S = 27.46391572, STDEV.P = 26.05455814.
		{"round(stdev(" + excelSD + "), 8)", "27.46391572"},
		{"round(stdev(" + excelSD + `, "population"), 8)`, "26.05455814"},
		// Excel docs GEOMEAN example: 5.476986969 (shown cut, not rounded:
		// the value is 5.4769869696...).
		{"round(geomean([4, 5, 8, 7, 11, 4, 3]), 8)", "5.47698697"},
		{"geomean(2, 8)", "4"},
		// Excel docs CORREL example: 0.997054486.
		{"round(correlation([3, 2, 4, 5, 6], [9, 7, 12, 15, 17]), 9)", "0.997054486"},
		{"mean([1, 2.5 + sin(0)])", "1.75"},
	})
	checkExact(t, true, []textCase{
		// Excel docs HARMEAN example, exact here.
		{"round(harmmean([4, 5, 8, 7, 11, 4, 3]), 9)", "5.028375962"},
	})
}

// The linear-interpolation method: numpy's default, Excel's
// PERCENTILE.INC, R type 7. Excel docs: PERCENTILE.INC({1,2,3,4}, 0.3) =
// 1.9.
func TestStatistics_QuantileInterpolatesLinearly(t *testing.T) {
	checkExact(t, true, []textCase{
		{"quantile([1, 2, 3, 4], 0.3)", "1.9"},
		{"percentile([1, 2, 3, 4], 30)", "1.9"},
		{"quantile([1, 2, 3, 4], 0.25)", "1.75"},
		{"quantile([4, 1, 3, 2], 0)", "1"},
		{"quantile([4, 1, 3, 2], 1)", "4"},
		{"quantile([7], 0.9)", "7"},
		{"percentile([10, 20, 30, 40, 50], 90)", "46"},
		{"quantile([1, 2, 3], 1/3)", "1.66666666666667"},
	})
	mustFail(t, "quantile([1, 2], 1.5)", codeDomain)
	mustFail(t, "quantile([1, 2], -0.1)", codeDomain)
	mustFail(t, "percentile([1, 2], 101)", codeDomain)
}

func TestStatistics_RefusesEmptyMismatchedAndDegenerateInput(t *testing.T) {
	for _, expr := range []string{
		"mean([])", "median([])", "mode([])", "spread([])", "geomean([])", "harmmean([])",
		"variance([])", "stdev([])", "quantile([], 0.5)", "wmean([], [])",
		"variance([5])",                        // a sample of one
		"geomean([2, 0])", "harmmean([-1, 2])", // non-positive
		"zscore([3, 3, 3])",           // zero deviation
		"correlation([1, 1], [2, 3])", // x does not vary
		"linreg([2, 2], [1, 5])",      // vertical line
		"linreg([1], [1])",            // one point
		"wmean([1, 2], [1, -1])",      // weights sum to zero
		`variance([1, 2], "pop")`,     // unknown kind
	} {
		mustFail(t, expr, codeDomain)
	}
	for _, expr := range []string{
		"covariance([1, 2, 3], [1, 2])", "correlation([1, 2], [1])", "linreg([1, 2, 3], [4, 5])", "wmean([1, 2], [1])",
	} {
		mustFail(t, expr, codeShape)
	}
	for _, expr := range []string{
		`mean(["a"])`, "mean([[1, 2]])", "cumsum(5)", `variance([1, 2], 0)`, `median(1, "x")`,
	} {
		mustFail(t, expr, codeType)
	}
	// A population variance of one value is defined.
	checkTexts(t, []textCase{{`variance([5], "population")`, "0"}})
	// range keeps its meaning as the sequence builder.
	checkTexts(t, []textCase{{"range(1, 3)", "[1, 2, 3]"}})
}

// Sorting functions charge n log n, so a list built within the step bound
// cannot buy unbounded sort work: 100,000 values cost 100,000 steps to
// build but 1.7 million to sort, and are refused at step_limit before the
// sort runs.
//
// It runs under a minute-long wall, not the 2 s one: under -race on a loaded
// machine building the range alone can pass 2 s, and the call then ends in
// time_limit before the charge is reached. The step limit is what is tested.
func TestStatistics_SortChargesKeepLargeInputsInsideTheStepLimit(t *testing.T) {
	for _, expr := range []string{
		"median(1:100000)", "rank(1:100000)", "mode(1:100000)", "percentile(1:100000, 50)",
	} {
		_, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err == nil || err.Code != string(codeStepLimit) {
			t.Errorf("%s: err = %v, want step_limit", expr, err)
		}
	}
}

// Every pass over the data is charged: called directly on n values, each
// function counts at least its passes times n (n log n for a sort), so the
// step bound limits the work, not only the size of the list.
func TestStatistics_EveryPassIsCharged(t *testing.T) {
	const n = 1000
	xs := make([]val, n)
	for i := range xs {
		xs[i] = exactInt64(int64(i%7 + 1))
	}
	cases := []struct {
		fn       func(*evaluator, args) (val, *Error)
		name     string
		argv     []val
		minSteps int
	}{
		{fnMean, "mean", []val{xs}, n},
		{fnMedian, "median", []val{xs}, sortCost(n)},
		{fnMode, "mode", []val{xs}, sortCost(n)},
		{fnRank, "rank", []val{xs}, sortCost(n) + n},
		{fnQuantile(false), "quantile", []val{xs, exactInt64(0)}, sortCost(n)},
		{fnSpread, "spread", []val{xs}, n},
		{fnGeomean, "geomean", []val{xs}, n},
		{fnHarmmean, "harmmean", []val{xs}, n},
		{fnWmean, "wmean", []val{xs, xs}, 2 * n},
		{fnVariance, "variance", []val{xs}, 3 * n},
		{fnZscore, "zscore", []val{xs}, 4 * n},
		{fnCumulative("+"), "cumsum", []val{xs}, n},
		{fnDiff, "diff", []val{xs}, n},
		{fnCovariance, "covariance", []val{xs, xs}, 3 * n},
		{fnCorrelation, "correlation", []val{xs, xs}, 9 * n},
		{fnLinreg, "linreg", []val{xs, xs}, 6 * n},
		{fnNpv, "npv", []val{floatNum(0.1), xs}, n},
		{fnIrr, "irr", []val{append([]val{exactInt64(-4000)}, xs[1:]...)}, n},
	}
	for _, c := range cases {
		e := &evaluator{ctx: context.Background(), funcs: builtinIndex, sizes: map[listKey]int{}}
		if _, err := c.fn(e, args{v: c.argv, name: c.name}); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if e.steps < c.minSteps {
			t.Errorf("%s over %d values charged %d steps, want at least %d", c.name, n, e.steps, c.minSteps)
		}
	}
}
