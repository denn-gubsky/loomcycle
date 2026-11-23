package calc

import (
	"math"
	"math/big"

	"gonum.org/v1/gonum/stat/distuv"
)

// SecDistributions holds the probability distributions. They are a section
// of their own rather than part of statistics so each article stays inside
// its help word budget.
const SecDistributions = "distributions"

// The distribution functions compute in float64 through gonum/stat/distuv:
// every result is a float, even on exact input. Each takes its point (x, k
// or p) first, so a list of points maps element by element (vec), and its
// parameters after. A parameter out of range is refused with domain, never
// clamped. Each call is constant work (gonum's inverses iterate a bounded
// number of times), so the step charged per call is the evaluator's own.
var distributionFuncs = []builtin{
	{name: "normpdf", sig: "normpdf(x, mu?, sigma?)", section: SecDistributions, min: 1, max: 3, vec: true, fn: normal(pdf),
		summary: "normal density at x; mu 0 and sigma 1 by default"},
	{name: "normcdf", sig: "normcdf(x, mu?, sigma?)", section: SecDistributions, min: 1, max: 3, vec: true, fn: normal(cdf),
		summary: "normal probability of a value at most x"},
	{name: "norminv", sig: "norminv(p, mu?, sigma?)", section: SecDistributions, min: 1, max: 3, vec: true, fn: normal(inv),
		summary: "the x with normcdf(x) = p, 0 < p < 1"},
	{name: "tpdf", sig: "tpdf(x, df)", section: SecDistributions, min: 2, max: 2, vec: true, fn: studentT(pdf),
		summary: "Student's t density with df degrees of freedom"},
	{name: "tcdf", sig: "tcdf(x, df)", section: SecDistributions, min: 2, max: 2, vec: true, fn: studentT(cdf),
		summary: "Student's t probability of a value at most x"},
	{name: "tinv", sig: "tinv(p, df)", section: SecDistributions, min: 2, max: 2, vec: true, fn: studentT(inv),
		summary: "the x with tcdf(x, df) = p, 0 < p < 1"},
	{name: "chi2pdf", sig: "chi2pdf(x, df)", section: SecDistributions, min: 2, max: 2, vec: true, fn: chiSquared(pdf),
		summary: "chi-squared density with df degrees of freedom"},
	{name: "chi2cdf", sig: "chi2cdf(x, df)", section: SecDistributions, min: 2, max: 2, vec: true, fn: chiSquared(cdf),
		summary: "chi-squared probability of a value at most x"},
	{name: "chi2inv", sig: "chi2inv(p, df)", section: SecDistributions, min: 2, max: 2, vec: true, fn: chiSquared(inv),
		summary: "the x with chi2cdf(x, df) = p, 0 <= p < 1"},
	{name: "fpdf", sig: "fpdf(x, d1, d2)", section: SecDistributions, min: 3, max: 3, vec: true, fn: fisherF(pdf),
		summary: "F density with d1 and d2 degrees of freedom"},
	{name: "fcdf", sig: "fcdf(x, d1, d2)", section: SecDistributions, min: 3, max: 3, vec: true, fn: fisherF(cdf),
		summary: "F probability of a value at most x"},
	{name: "finv", sig: "finv(p, d1, d2)", section: SecDistributions, min: 3, max: 3, vec: true, fn: fisherF(inv),
		summary: "the x with fcdf(x, d1, d2) = p, 0 <= p < 1"},
	{name: "exppdf", sig: "exppdf(x, lambda)", section: SecDistributions, min: 2, max: 2, vec: true, fn: exponential(pdf),
		summary: "exponential density with rate lambda (mean 1/lambda)"},
	{name: "expcdf", sig: "expcdf(x, lambda)", section: SecDistributions, min: 2, max: 2, vec: true, fn: exponential(cdf),
		summary: "exponential probability of a value at most x"},
	{name: "expinv", sig: "expinv(p, lambda)", section: SecDistributions, min: 2, max: 2, vec: true, fn: exponential(inv),
		summary: "the x with expcdf(x, lambda) = p, 0 <= p < 1"},
	{name: "unifpdf", sig: "unifpdf(x, a, b)", section: SecDistributions, min: 3, max: 3, vec: true, fn: uniform(pdf),
		summary: "uniform density on [a, b]"},
	{name: "unifcdf", sig: "unifcdf(x, a, b)", section: SecDistributions, min: 3, max: 3, vec: true, fn: uniform(cdf),
		summary: "uniform probability of a value at most x"},
	{name: "unifinv", sig: "unifinv(p, a, b)", section: SecDistributions, min: 3, max: 3, vec: true, fn: uniform(inv),
		summary: "the x with unifcdf(x, a, b) = p, 0 <= p <= 1"},
	{name: "binompdf", sig: "binompdf(k, n, p)", section: SecDistributions, min: 3, max: 3, vec: true, fn: binomial(false),
		summary: "probability of exactly k successes in n trials of probability p"},
	{name: "binomcdf", sig: "binomcdf(k, n, p)", section: SecDistributions, min: 3, max: 3, vec: true, fn: binomial(true),
		summary: "probability of at most k successes in n trials"},
	{name: "poisspdf", sig: "poisspdf(k, lambda)", section: SecDistributions, min: 2, max: 2, vec: true, fn: poisson(false),
		summary: "probability of exactly k events at mean rate lambda"},
	{name: "poisscdf", sig: "poisscdf(k, lambda)", section: SecDistributions, min: 2, max: 2, vec: true, fn: poisson(true),
		summary: "probability of at most k events at mean rate lambda"},
}

// distKind is which of a continuous distribution's three functions a
// builtin computes.
type distKind int

const (
	pdf distKind = iota
	cdf
	inv
)

// continuous is the part of a gonum distribution the functions use.
type continuous interface {
	Prob(float64) float64
	CDF(float64) float64
	Quantile(float64) float64
}

// realArg returns argument i as a float, refusing a number too large for
// one (an exact 10^400 would reach gonum as +Inf).
func (a args) realArg(i int) (float64, *Error) {
	n, err := a.num(i)
	if err != nil {
		return 0, err
	}
	f := n.float()
	if math.IsInf(f, 0) {
		return 0, errf(codeOverflow, a.pos, "%s: argument %d is too large for a 64-bit float", a.name, i+1)
	}
	return f, nil
}

// positive returns argument i, refusing a value that is not above zero.
func (a args) positive(i int, what string) (float64, *Error) {
	v, err := a.realArg(i)
	if err == nil && !(v > 0) {
		err = errf(codeDomain, a.pos, "%s: %s must be above 0, got %s", a.name, what, fmtFloat(v))
	}
	return v, err
}

func fmtFloat(f float64) string { return fmtArg(floatNum(f)) }

// distribute computes kind at the point (argument 0) of the distribution
// d. An inverse needs a probability: lowOpen and highOpen say whether p = 0
// and p = 1 are refused because the quantile there is infinite.
func distribute(d continuous, kind distKind, a args, lowOpen, highOpen bool) (val, *Error) {
	x, err := a.realArg(0)
	if err != nil {
		return nil, err
	}
	var y float64
	switch kind {
	case pdf:
		y = d.Prob(x)
	case cdf:
		y = d.CDF(x)
	default:
		if x < 0 || x > 1 || lowOpen && x == 0 || highOpen && x == 1 {
			lo, hi := "<=", "<="
			if lowOpen {
				lo = "<"
			}
			if highOpen {
				hi = "<"
			}
			return nil, errf(codeDomain, a.pos, "%s: the probability must satisfy 0 %s p %s 1, got %s", a.name, lo, hi, fmtFloat(x)).
				withHint("the quantile there is infinite or the probability does not exist")
		}
		y = d.Quantile(x)
	}
	if math.IsInf(y, 0) {
		return nil, errf(codeDomain, a.pos, "%s(%s) is infinite", a.name, fmtFloat(x))
	}
	return checkFloat(y, a.pos, a.name)
}

func normal(kind distKind) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		mu, sigma := 0.0, 1.0
		var err *Error
		if len(a.v) > 1 {
			if mu, err = a.realArg(1); err != nil {
				return nil, err
			}
		}
		if len(a.v) > 2 {
			if sigma, err = a.positive(2, "sigma"); err != nil {
				return nil, err
			}
		}
		return distribute(distuv.Normal{Mu: mu, Sigma: sigma}, kind, a, true, true)
	}
}

func studentT(kind distKind) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		df, err := a.positive(1, "df")
		if err != nil {
			return nil, err
		}
		return distribute(distuv.StudentsT{Mu: 0, Sigma: 1, Nu: df}, kind, a, true, true)
	}
}

func chiSquared(kind distKind) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		df, err := a.positive(1, "df")
		if err != nil {
			return nil, err
		}
		return distribute(distuv.ChiSquared{K: df}, kind, a, false, true)
	}
}

func fisherF(kind distKind) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		d1, err := a.positive(1, "d1")
		if err != nil {
			return nil, err
		}
		d2, err := a.positive(2, "d2")
		if err != nil {
			return nil, err
		}
		return distribute(distuv.F{D1: d1, D2: d2}, kind, a, false, true)
	}
}

func exponential(kind distKind) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		rate, err := a.positive(1, "lambda")
		if err != nil {
			return nil, err
		}
		return distribute(distuv.Exponential{Rate: rate}, kind, a, false, true)
	}
}

func uniform(kind distKind) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		lo, err := a.realArg(1)
		if err != nil {
			return nil, err
		}
		hi, err := a.realArg(2)
		if err != nil {
			return nil, err
		}
		if !(lo < hi) {
			return nil, errf(codeDomain, a.pos, "%s: a must be below b, got a = %s and b = %s", a.name, fmtFloat(lo), fmtFloat(hi))
		}
		return distribute(distuv.Uniform{Min: lo, Max: hi}, kind, a, false, false)
	}
}

// maxTrials bounds n and k of the discrete functions: past 2^53 a float no
// longer holds every integer, so k and n would not be the counts written.
const maxTrials = 1 << 53

// countArg returns argument i as a whole number from 0 to maxTrials; a
// fractional count is refused, not floored.
func (a args) countArg(i int, what string) (float64, *Error) {
	n, err := a.num(i)
	if err != nil {
		return 0, err
	}
	v, ok := smallInt(n, maxTrials)
	if !ok || v < 0 {
		return 0, errf(codeDomain, a.pos, "%s: %s must be a whole number from 0 to 2^53, got %s", a.name, what, fmtArg(n))
	}
	return float64(v), nil
}

// probability returns argument i, refusing a value outside [0, 1].
func (a args) probability(i int) (float64, *Error) {
	p, err := a.realArg(i)
	if err == nil && !(p >= 0 && p <= 1) {
		err = errf(codeDomain, a.pos, "%s: the probability p must be from 0 to 1, got %s", a.name, fmtFloat(p))
	}
	return p, err
}

func binomial(cumulative bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		k, err := a.countArg(0, "k")
		if err != nil {
			return nil, err
		}
		n, err := a.countArg(1, "n")
		if err != nil {
			return nil, err
		}
		p, err := a.probability(2)
		if err != nil {
			return nil, err
		}
		if k > n {
			return nil, errf(codeDomain, a.pos, "%s: k = %s is more than the n = %s trials", a.name, fmtFloat(k), fmtFloat(n))
		}
		if v, ok, err := exactBinomial(e, a, int64(k), int64(n), cumulative); ok {
			return v, err
		}
		// gonum computes 0 · log(0) at p = 0 or 1, which is NaN; the
		// distribution there is a single point, so answer directly.
		if p == 0 || p == 1 {
			at := 0.0 // where all the probability sits
			if p == 1 {
				at = n
			}
			switch {
			case !cumulative && k == at, cumulative && k >= at:
				return floatNum(1), nil
			}
			return floatNum(0), nil
		}
		d := distuv.Binomial{N: n, P: p}
		if cumulative {
			return checkFloat(d.CDF(k), a.pos, a.name)
		}
		return checkFloat(d.Prob(k), a.pos, a.name)
	}
}

// exactBinomial computes the binomial probability in exact arithmetic when
// p is an exact fraction a/d and d^n fits the exact-number cap: the sum over
// i = k (pdf) or i = 0..k (cdf) of C(n, i) a^i (d-a)^(n-i), over d^n, then
// rounded once to a float. The answer is then the same to the last digit
// on every platform, which gonum's lgamma and exp path is not (an ulp apart
// between amd64 and arm64). ok is false when the exact path does not apply.
func exactBinomial(e *evaluator, a args, k, n int64, cumulative bool) (v val, ok bool, err *Error) {
	pn, isNum := a.v[2].(num)
	if !isNum || !pn.exact() {
		return nil, false, nil
	}
	d := pn.r.Denom()
	if n*int64(d.BitLen()) > MaxExactBits {
		return nil, false, nil
	}
	an := pn.r.Num()
	bn := new(big.Int).Sub(d, an)
	lo := k
	if cumulative {
		lo = 0
	}
	// Each term is a few products of numbers up to n·bits(d) bits.
	if err := e.charge(int(k-lo+1)*(1+int(n)*d.BitLen()/bitStepCost), a.pos); err != nil {
		return nil, true, err
	}
	// From i = k down to lo, so (d-a)^(n-i) grows by one factor per term
	// and a^i shrinks by an exact division.
	c := new(big.Int).Binomial(n, k)
	ap := new(big.Int).Exp(an, big.NewInt(k), nil)
	bp := new(big.Int).Exp(bn, big.NewInt(n-k), nil)
	sum, t := new(big.Int), new(big.Int)
	for i := k; ; i-- {
		t.Mul(c, ap)
		sum.Add(sum, t.Mul(t, bp))
		if i == lo {
			break
		}
		c.Mul(c, big.NewInt(i))
		c.Quo(c, big.NewInt(n-i+1))
		bp.Mul(bp, bn)
		if an.Sign() != 0 {
			ap.Quo(ap, an)
		} else if i == 1 {
			ap.SetInt64(1) // 0^0
		}
	}
	// The float nearest the exact value: big.Rat rounds correctly, so this
	// is the same on every platform. Not the exact value itself: a decimal p
	// makes it a terminating decimal of hundreds of digits (binomcdf(2, 50,
	// 0.02) has 100), which renders in full.
	f, _ := new(big.Rat).SetFrac(sum, new(big.Int).Exp(d, big.NewInt(n), nil)).Float64()
	v, err = checkFloat(f, a.pos, a.name)
	return v, true, err
}

func poisson(cumulative bool) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		k, err := a.countArg(0, "k")
		if err != nil {
			return nil, err
		}
		lambda, err := a.positive(1, "lambda")
		if err != nil {
			return nil, err
		}
		d := distuv.Poisson{Lambda: lambda}
		if cumulative {
			return checkFloat(d.CDF(k), a.pos, a.name)
		}
		return checkFloat(d.Prob(k), a.pos, a.name)
	}
}
