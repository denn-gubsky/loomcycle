package calc

import (
	"context"
	"testing"
	"time"
)

// The integrals, derivatives, roots and minima have closed forms (any
// calculus text; the Gaussian integral sqrt(pi) and the Dottie number
// 0.739085133215161, the root of cos(x) = x, as tabulated in Abramowitz
// and Stegun and OEIS A003957). Each is compared at the digits the method
// promises.
func TestCalculus_IntegralsMatchClosedForms(t *testing.T) {
	checkExact(t, false, []textCase{
		{"integrate(x -> x^2, 0, 3)", "9"},
		{"integrate(sin, 0, pi)", "2"},
		{"integrate(x -> 4 / (1 + x^2), 0, 1)", "3.14159265358979"},
		{"integrate(x -> x, 1, 0)", "-0.5"}, // reversed bounds change the sign
		{"integrate(x -> x, 2, 2)", "0"},
		{"integrate(sin, -1, 1)", "0"},                       // cancels: the floor relative to |f| converges
		{"round(integrate(x -> 1 / sqrt(x), 0, 1), 8)", "2"}, // integrable singularity at an end
		{"integrate(x -> x^2 * exp(-x), 0, 50)", "2"},        // gamma(3)
		{"round(integrate(x -> sin(100 * x)^2, 0, pi), 10)", "1.5707963268"},
		{"f(t) = t^3 - t; integrate(f, 0, 2)", "2"},
		{"g = x -> 1; integrate(g, 0, 5)", "5"},
	})
	checkTexts(t, []textCase{
		{"round(integrate(x -> exp(-x^2), -10, 10), 12) == round(sqrt(pi), 12)", "true"},
	})
}

func TestCalculus_DerivativesMatchClosedForms(t *testing.T) {
	checkExact(t, false, []textCase{
		{"deriv(x -> x^3, 2)", "12"},
		{"deriv(sin, 0)", "1"},
		{"round(deriv(log, 10), 12)", "0.1"},
		{"round(deriv(sqrt, 0.001), 9)", "15.811388301"}, // 1 / (2 sqrt(0.001)); the step scales with x
		{"deriv(x -> x^2, 1e6)", "2000000"},
		{"deriv(abs, 0)", "0"}, // the symmetric derivative: at a kink, the mean of the two slopes
	})
	checkTexts(t, []textCase{
		{"round(deriv(exp, 1), 12) == round(e, 12)", "true"},
	})
}

func TestCalculus_RootsAndMinimaMatchClosedForms(t *testing.T) {
	checkExact(t, false, []textCase{
		{"root(x -> x^2 - 2, 0, 2)", "1.41421356237309"},
		{"root(cos, 0, 3)", "1.5707963267949"},
		{"root(x -> cos(x) - x, 0, 1)", "0.739085133215161"},
		{"round(root(x -> exp(x) - 10^6, 0, 100), 12)", "13.815510557964"}, // ln(10^6)
		{"root(x -> x - 1, 1, 3)", "1"},                                    // a root at an end
		{"minimize(x -> (x - 2)^2 + 1, 0, 5)", "2"},
		{"minimize(cos, 0, 2 * pi)", "3.14159265358979"},
		{"round(minimize(x -> 1 / x + x, 0, 5), 6)", "1"}, // the end where f is undefined is never evaluated
		{"round(minimize(x -> x^4 - 3 * x^3 + 2, 0, 4), 6)", "2.25"},
		{"minimize(x -> x, 0, 1)", "0"}, // a minimum at an end is returned exactly
		{"minimize(x -> -x, 0, 1)", "1"},
	})
}

// NumPy docs examples: numpy.polyfit's cubic fit, numpy.polyval and
// numpy.interp; a degree-1 fit is linreg's line ([1.99, 0.05], exact there).
func TestCalculus_PolynomialsAndInterpolationMatchNumPy(t *testing.T) {
	checkExact(t, false, []textCase{
		{"round(polyfit([0, 1, 2, 3, 4, 5], [0, 0.8, 0.9, 0.1, -0.8, -1], 3), 8)", "[0.08703704, -0.81349206, 1.69312169, -0.03968254]"},
		{"round(polyfit([0, 1, 2, 3], [1, 3, 7, 13], 2), 10)", "[1, 1, 1]"},
		{"round(polyfit(1:5, [2.1, 3.9, 6.2, 7.8, 10.1], 1), 12)", "[1.99, 0.05]"},
	})
	checkExact(t, true, []textCase{
		{"polyval([3, 0, 1], 5)", "76"},
		{"polyval([1, 2, 3], [0, 1, 2])", "[3, 6, 11]"},
		{"interp([1, 1.5, 2.72], [1, 2, 3], [3, 2, 0])", "[3, 2.5, 0.56]"},
		{"interp(2.5, [1, 2, 3], [10, 20, 40])", "30"},
		{"interp(3, [1, 2, 3], [10, 20, 40])", "40"},
	})
	checkTexts(t, []textCase{
		{"polyval([1/2, 1/3], 1/5) == 13/30", "true"},
		{"interp(1/3, [0, 1], [0, 1/7]) == 1/21", "true"},
	})
}

func TestCalculus_RefusesWhatHasNoAnswer(t *testing.T) {
	for _, expr := range []string{
		"integrate(x -> 1 / x, 0, 1)",    // not integrable
		"deriv(x -> if(x > 0, 1, 0), 0)", // a jump
		"deriv(x -> sin(1 / x), 0.001)",  // too rough at this scale
	} {
		mustFail(t, expr, codeNoConvergence)
	}
	for _, expr := range []string{
		"root(x -> x^2 + 1, -1, 1)",                    // no sign change
		"root(x -> x, 2, 1)", "minimize(x -> x, 1, 1)", // a not below b
		"polyfit([1, 1, 1], [1, 2, 3], 1)", // one distinct x for a line
		"polyfit([1, 2], [1, 2], 2)", "polyfit([1, 2], [1, 2], -1)", "polyfit([1, 2], [1, 2], 0.5)",
		"polyval([], 2)",
		"interp(0, [1, 2], [1, 2])", "interp(3, [1, 2], [1, 2])", // outside the table
		"interp(1, [2, 1], [1, 2])", "interp(1, [1, 1], [1, 2])", "interp(1, [1], [1])",
	} {
		mustFail(t, expr, codeDomain)
	}
	for _, expr := range []string{
		"integrate(5, 0, 1)", "integrate(x -> [x], 0, 1)", `root(x -> "a", 0, 1)`,
		`interp("a", [1, 2], [1, 2])`, `polyval([1], ["a"])`, "deriv(sin, [1])",
	} {
		mustFail(t, expr, codeType)
	}
	mustFail(t, "polyfit([1, 2, 3], [1, 2], 1)", codeShape)
	mustFail(t, "interp(1, [1, 2], [1])", codeShape)
	mustFail(t, "integrate((x, y) -> x, 0, 1)", codeArity)
	// x ± h past the float range never reaches f as an infinity.
	mustFail(t, "deriv(x -> 1, 1.7e308)", codeOverflow)
	// An error inside f is f's own, not a convergence failure.
	mustFail(t, "minimize(x -> -1 / x, 0, 1)", codeDivZero)
}

// counter is a built-in f that counts its evaluations and charges no steps
// of its own, so a test sees exactly what the method charges.
func counter(n *int, f func(float64) float64) *fnVal {
	return &fnVal{name: "f", builtin: &builtin{name: "f", min: 1, max: 1,
		fn: func(_ *evaluator, a args) (val, *Error) {
			*n++
			return floatNum(f(a.v[0].(num).float())), nil
		}}}
}

func newTestEvaluator() *evaluator {
	return &evaluator{ctx: context.Background(), funcs: builtinIndex, sizes: map[listKey]int{}}
}

// Every evaluation of f costs the call's step plus one for the method's own
// arithmetic, so the step bound limits the methods' work as well as f's.
func TestCalculus_EveryEvaluationIsCharged(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*evaluator, args) (val, *Error)
		f    func(float64) float64
		rest []val
	}{
		{"integrate", fnIntegrate, func(x float64) float64 { return 1 / (x*x + 1e-4) }, []val{floatNum(-1), floatNum(1)}},
		{"deriv", fnDeriv, func(x float64) float64 { return x * x * x }, []val{floatNum(2)}},
		{"root", fnRoot, func(x float64) float64 { return x*x - 2 }, []val{floatNum(0), floatNum(2)}},
		{"minimize", fnMinimize, func(x float64) float64 { return (x - 1) * (x - 1) }, []val{floatNum(0), floatNum(3)}},
	}
	for _, c := range cases {
		n := 0
		e := newTestEvaluator()
		argv := append([]val{counter(&n, c.f)}, c.rest...)
		if _, err := c.fn(e, args{v: argv, name: c.name}); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if n < 5 || e.steps < 2*n {
			t.Errorf("%s evaluated f %d times and charged %d steps, want at least %d", c.name, n, e.steps, 2*n)
		}
	}
}

// A method that does not converge stops at its cap and is refused, never
// answered with its last estimate, and never runs past the cap.
func TestCalculus_EvaluationCapsStopMethodsThatDoNotConverge(t *testing.T) {
	cases := []struct {
		name  string
		fn    func(*evaluator, args) (val, *Error)
		f     func(float64) float64
		rest  []val
		limit int
	}{
		{"integrate", fnIntegrate, func(x float64) float64 { return 1 / x }, []val{floatNum(0), floatNum(1)}, maxIntegrateEvals},
		{"deriv", fnDeriv, func(x float64) float64 {
			if x > 0 {
				return 1
			}
			return 0
		}, []val{floatNum(0)}, maxDerivEvals},
	}
	for _, c := range cases {
		n := 0
		e := newTestEvaluator()
		argv := append([]val{counter(&n, c.f)}, c.rest...)
		_, err := c.fn(e, args{v: argv, name: c.name})
		if err == nil || err.Code != string(codeNoConvergence) {
			t.Errorf("%s: err = %v, want no_convergence", c.name, err)
		}
		if n > c.limit {
			t.Errorf("%s evaluated f %d times, over its cap of %d", c.name, n, c.limit)
		}
	}
	// The cap itself: the evaluation past it is refused before f runs.
	n := 0
	e := newTestEvaluator()
	p := &probe{e: e, f: counter(&n, func(x float64) float64 { return x }), a: args{name: "t"}, limit: 3}
	for i := range 4 {
		_, err := p.at(1)
		if i < 3 && err != nil || i == 3 && (err == nil || err.Code != string(codeNoConvergence)) {
			t.Errorf("evaluation %d: err = %v", i+1, err)
		}
	}
	if n != 3 {
		t.Errorf("f ran %d times under a cap of 3", n)
	}
}

// A costly f inside a method reaches the step bound: each evaluation pays
// for f's own work. Under a minute-long wall, not 2 s: under -race on a
// loaded machine the wall would be reached first, and it is the steps that
// are tested.
func TestCalculus_CostlyFunctionsReachTheStepLimit(t *testing.T) {
	for _, expr := range []string{
		"integrate(x -> sum(1:500) * 0 + 1 / x, 0, 1)",
		"root(x -> sum(1:500000) * 0 + x - 0.3, 0, 1)",
	} {
		_, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err == nil || err.Code != string(codeStepLimit) {
			t.Errorf("%s: err = %v, want step_limit", expr, err)
		}
	}
}

// The table functions charge a step per coefficient, point and table row.
func TestCalculus_TableFunctionsChargeTheirPasses(t *testing.T) {
	const n = 1000
	xs := make([]val, n)
	for i := range xs {
		xs[i] = exactInt64(int64(i))
	}
	cases := []struct {
		fn       func(*evaluator, args) (val, *Error)
		name     string
		argv     []val
		minSteps int
	}{
		{fnPolyval, "polyval", []val{xs, exactInt64(1)}, n},
		{fnInterp, "interp", []val{exactInt64(5), xs, xs}, n},
		{fnPolyfit, "polyfit", []val{xs, xs, exactInt64(3)}, 2 * n},
	}
	for _, c := range cases {
		e := newTestEvaluator()
		if _, err := c.fn(e, args{v: c.argv, name: c.name}); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if e.steps < c.minSteps {
			t.Errorf("%s over %d values charged %d steps, want at least %d", c.name, n, e.steps, c.minSteps)
		}
	}
}
