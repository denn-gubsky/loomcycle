package calc

import (
	"math"
	"math/big"
	"sort"
)

// The statistics functions do their arithmetic with the evaluator's own
// numOp (through chain), so an exact input keeps an exact result wherever
// the formula is rational: mean, median, variance, covariance, quantiles
// and the regression line of exact data are exact fractions. Only a
// square root that does not come out exact (stdev, correlation, zscore)
// or a logarithm (geomean) makes a float. Every pass over the data
// charges one step per element and every sort charges n log n, on top of
// what numOp charges for big exact numbers.
var statisticsFuncs = []builtin{
	{name: "mean", sig: "mean(v) or mean(a, b, ...)", section: SecStatistics, min: 1, max: -1, fn: fnMean,
		summary: "arithmetic mean; exact on exact input"},
	{name: "median", sig: "median(v) or median(a, b, ...)", section: SecStatistics, min: 1, max: -1, fn: fnMedian,
		summary: "middle value, or the mean of the two middle values"},
	{name: "mode", sig: "mode(v) or mode(a, b, ...)", section: SecStatistics, min: 1, max: -1, fn: fnMode,
		summary: "most frequent value; the smallest of tied ones"},
	{name: "spread", sig: "spread(v) or spread(a, b, ...)", section: SecStatistics, min: 1, max: -1, fn: fnSpread,
		summary: "max minus min (the statistical range)"},
	{name: "geomean", sig: "geomean(v) or geomean(a, b, ...)", section: SecStatistics, min: 1, max: -1, fn: fnGeomean,
		summary: "geometric mean of positive values; a float"},
	{name: "harmmean", sig: "harmmean(v) or harmmean(a, b, ...)", section: SecStatistics, min: 1, max: -1, fn: fnHarmmean,
		summary: "harmonic mean of positive values; exact on exact input"},
	{name: "wmean", sig: "wmean(v, w)", section: SecStatistics, min: 2, max: 2, fn: fnWmean,
		summary: "mean of v weighted by w"},
	{name: "variance", sig: "variance(v, kind?)", section: SecStatistics, min: 1, max: 2, fn: fnVariance,
		summary: `sample variance, or "population"; exact on exact input`},
	{name: "stdev", sig: "stdev(v, kind?)", section: SecStatistics, min: 1, max: 2, fn: fnStdev,
		summary: `sample standard deviation, or "population"`},
	{name: "zscore", sig: "zscore(v, kind?)", section: SecStatistics, min: 1, max: 2, fn: fnZscore,
		summary: "each value's distance from the mean in standard deviations"},
	{name: "quantile", sig: "quantile(v, p)", section: SecStatistics, min: 2, max: 2, fn: fnQuantile(false),
		summary: "the p quantile, p from 0 to 1, by linear interpolation"},
	{name: "percentile", sig: "percentile(v, p)", section: SecStatistics, min: 2, max: 2, fn: fnQuantile(true),
		summary: "the p-th percentile, p from 0 to 100, by linear interpolation"},
	{name: "rank", sig: "rank(v)", section: SecStatistics, min: 1, max: 1, fn: fnRank,
		summary: "each value's rank, 1 for the smallest; ties share their average rank"},
	{name: "cumsum", sig: "cumsum(v)", section: SecStatistics, min: 1, max: 1, fn: fnCumulative("+"),
		summary: "running totals"},
	{name: "cumprod", sig: "cumprod(v)", section: SecStatistics, min: 1, max: 1, fn: fnCumulative("*"),
		summary: "running products"},
	{name: "diff", sig: "diff(v)", section: SecStatistics, min: 1, max: 1, fn: fnDiff,
		summary: "differences of neighbours, v[i+1] - v[i]"},
	{name: "covariance", sig: "covariance(x, y, kind?)", section: SecStatistics, min: 2, max: 3, fn: fnCovariance,
		summary: `sample covariance, or "population"; exact on exact input`},
	{name: "correlation", sig: "correlation(x, y)", section: SecStatistics, min: 2, max: 2, fn: fnCorrelation,
		summary: "Pearson correlation coefficient, from -1 to 1"},
	{name: "linreg", sig: "linreg(x, y)", section: SecStatistics, min: 2, max: 2, fn: fnLinreg,
		summary: "least-squares line y = slope*x + intercept, as [slope, intercept]"},
}

// chain chains numOp calls and keeps the first error, so a formula reads as
// one expression and is checked once at the end.
type chain struct {
	e   *evaluator
	pos int
	err *Error
}

func (o *chain) do(op string, x, y num) num {
	if o.err != nil {
		return num{}
	}
	v, err := o.e.numOp(op, x, y, o.pos)
	if err != nil {
		o.err = err
		return num{}
	}
	return v.(num)
}

func (o *chain) add(x, y num) num { return o.do("+", x, y) }
func (o *chain) sub(x, y num) num { return o.do("-", x, y) }
func (o *chain) mul(x, y num) num { return o.do("*", x, y) }
func (o *chain) div(x, y num) num { return o.do("/", x, y) }
func (o *chain) pow(x, y num) num { return o.do("^", x, y) }

// sum adds xs, charging one step per element.
func (o *chain) sum(xs []num) num {
	if o.err == nil {
		o.err = o.e.charge(len(xs), o.pos)
	}
	s := exactInt64(0)
	for _, x := range xs {
		s = o.add(s, x)
	}
	return s
}

func (o *chain) mean(xs []num) num { return o.div(o.sum(xs), exactInt64(int64(len(xs)))) }

// result returns n, or the first error the chain met.
func (o *chain) result(n num) (val, *Error) {
	if o.err != nil {
		return nil, o.err
	}
	return n, nil
}

// numbersOf returns the elements of l, refusing anything but numbers.
func numbersOf(l []val, a args) ([]num, *Error) {
	out := make([]num, len(l))
	for i, x := range l {
		n, ok := x.(num)
		if !ok {
			return nil, errf(codeType, a.pos, "%s needs a list of numbers, got a %s element", a.name, typeName(x))
		}
		out[i] = n
	}
	return out, nil
}

// numList returns argument i as a list of numbers.
func numList(a args, i int) ([]num, *Error) {
	l, err := a.list(i)
	if err != nil {
		return nil, err
	}
	return numbersOf(l, a)
}

// sample returns the data of a one-list aggregate: the single list
// argument, or the arguments themselves, like sum. An empty sample is
// refused, as min([]) is.
func sample(a args) ([]num, *Error) {
	items := a.v
	if len(a.v) == 1 {
		if l, ok := a.v[0].([]val); ok {
			items = l
		}
	}
	if len(items) == 0 {
		return nil, errf(codeDomain, a.pos, "%s of an empty list is undefined", a.name)
	}
	return numbersOf(items, a)
}

// nonEmpty is numList refusing an empty list.
func nonEmpty(a args, i int) ([]num, *Error) {
	xs, err := numList(a, i)
	if err == nil && len(xs) == 0 {
		err = errf(codeDomain, a.pos, "%s of an empty list is undefined", a.name)
	}
	return xs, err
}

// sortedNums returns a sorted copy of xs, charging n log n steps.
func (e *evaluator) sortedNums(xs []num, pos int) ([]num, *Error) {
	if err := e.charge(sortCost(len(xs)), pos); err != nil {
		return nil, err
	}
	s := append([]num(nil), xs...)
	sort.SliceStable(s, func(i, j int) bool { return cmpNum(s[i], s[j]) < 0 })
	return s, nil
}

// ddof reads the optional kind argument at i: "sample" (the default,
// dividing by n-1) or "population" (dividing by n).
func ddof(a args, i int) (int64, *Error) {
	if len(a.v) <= i {
		return 1, nil
	}
	s, ok := a.v[i].(string)
	if !ok {
		return 0, errf(codeType, a.pos, "%s: argument %d must be \"sample\" or \"population\", got a %s", a.name, i+1, typeName(a.v[i]))
	}
	switch s {
	case "sample":
		return 1, nil
	case "population":
		return 0, nil
	}
	return 0, errf(codeDomain, a.pos, "%s: kind %q is not \"sample\" or \"population\"", a.name, s).
		withHint("the default is the sample (n-1) form; pass \"population\" to divide by n")
}

func fnMean(e *evaluator, a args) (val, *Error) {
	xs, err := sample(a)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	return o.result(o.mean(xs))
}

func fnMedian(e *evaluator, a args) (val, *Error) {
	xs, err := sample(a)
	if err != nil {
		return nil, err
	}
	s, err := e.sortedNums(xs, a.pos)
	if err != nil {
		return nil, err
	}
	n := len(s)
	if n%2 == 1 {
		return s[n/2], nil
	}
	o := &chain{e: e, pos: a.pos}
	return o.result(o.div(o.add(s[n/2-1], s[n/2]), exactInt64(2)))
}

// fnMode returns the most frequent value; among values tied for most
// frequent, the smallest. Values that compare equal (2 and 2.0) count as
// one value.
func fnMode(e *evaluator, a args) (val, *Error) {
	xs, err := sample(a)
	if err != nil {
		return nil, err
	}
	s, err := e.sortedNums(xs, a.pos)
	if err != nil {
		return nil, err
	}
	best, bestN := s[0], 0
	for i := 0; i < len(s); {
		j := i + 1
		for j < len(s) && cmpNum(s[j], s[i]) == 0 {
			j++
		}
		if j-i > bestN {
			best, bestN = s[i], j-i
		}
		i = j
	}
	return best, nil
}

func fnSpread(e *evaluator, a args) (val, *Error) {
	xs, err := sample(a)
	if err != nil {
		return nil, err
	}
	if err := e.charge(len(xs), a.pos); err != nil {
		return nil, err
	}
	lo, hi := xs[0], xs[0]
	for _, x := range xs[1:] {
		if cmpNum(x, lo) < 0 {
			lo = x
		}
		if cmpNum(x, hi) > 0 {
			hi = x
		}
	}
	o := &chain{e: e, pos: a.pos}
	return o.result(o.sub(hi, lo))
}

// positives refuses a value that is not above zero, for the geometric and
// harmonic means.
func positives(xs []num, a args) *Error {
	for _, x := range xs {
		if x.sign() <= 0 {
			return errf(codeDomain, a.pos, "%s needs values above zero, got %s", a.name, fmtArg(x))
		}
	}
	return nil
}

func fnGeomean(e *evaluator, a args) (val, *Error) {
	xs, err := sample(a)
	if err != nil {
		return nil, err
	}
	if err := positives(xs, a); err != nil {
		return nil, err
	}
	if err := e.charge(len(xs), a.pos); err != nil {
		return nil, err
	}
	// The mean of the logarithms: a product of many values would overflow
	// a float long before the mean does.
	s := 0.0
	for _, x := range xs {
		l, err := ln(x, a.pos)
		if err != nil {
			return nil, err
		}
		s += l
	}
	return checkFloat(math.Exp(s/float64(len(xs))), a.pos, "geomean")
}

func fnHarmmean(e *evaluator, a args) (val, *Error) {
	xs, err := sample(a)
	if err != nil {
		return nil, err
	}
	if err := positives(xs, a); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	inv := make([]num, len(xs))
	for i, x := range xs {
		inv[i] = o.div(exactInt64(1), x)
	}
	return o.result(o.div(exactInt64(int64(len(xs))), o.sum(inv)))
}

// pair returns two number lists of the same length, at least min long.
func pair(a args, min int) ([]num, []num, *Error) {
	xs, err := numList(a, 0)
	if err != nil {
		return nil, nil, err
	}
	ys, err := numList(a, 1)
	if err != nil {
		return nil, nil, err
	}
	if len(xs) != len(ys) {
		return nil, nil, errf(codeShape, a.pos, "%s needs two lists of the same length, got %d and %d", a.name, len(xs), len(ys))
	}
	if len(xs) < min {
		return nil, nil, errf(codeDomain, a.pos, "%s needs at least %d values, got %d", a.name, min, len(xs))
	}
	return xs, ys, nil
}

func fnWmean(e *evaluator, a args) (val, *Error) {
	xs, ws, err := pair(a, 1)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	prod := make([]num, len(xs))
	for i := range xs {
		prod[i] = o.mul(xs[i], ws[i])
	}
	sw := o.sum(ws)
	if o.err == nil && sw.isZero() {
		return nil, errf(codeDomain, a.pos, "wmean: the weights sum to zero")
	}
	return o.result(o.div(o.sum(prod), sw))
}

// comoment is the sum of (x - mean x)(y - mean y), and the two means.
func (o *chain) comoment(xs, ys []num) (s, mx, my num) {
	mx, my = o.mean(xs), o.mean(ys)
	if o.err == nil {
		o.err = o.e.charge(len(xs), o.pos)
	}
	s = exactInt64(0)
	for i := range xs {
		s = o.add(s, o.mul(o.sub(xs[i], mx), o.sub(ys[i], my)))
	}
	return s, mx, my
}

// spreadOf divides a comoment by n - ddof, refusing a sample of one.
func (o *chain) spreadOf(s num, n, dd int64, a args) num {
	if o.err == nil && n-dd < 1 {
		o.err = errf(codeDomain, a.pos, "%s of a sample needs at least 2 values, got %d", a.name, n).
			withHint(`pass "population" to divide by n instead`)
	}
	return o.div(s, exactInt64(n-dd))
}

// varianceArgs computes the variance of argument 0 with the kind at 1.
func varianceArgs(e *evaluator, a args) ([]num, num, num, *Error) {
	xs, err := nonEmpty(a, 0)
	if err != nil {
		return nil, num{}, num{}, err
	}
	dd, err := ddof(a, 1)
	if err != nil {
		return nil, num{}, num{}, err
	}
	o := &chain{e: e, pos: a.pos}
	s, m, _ := o.comoment(xs, xs)
	v := o.spreadOf(s, int64(len(xs)), dd, a)
	return xs, v, m, o.err
}

func fnVariance(e *evaluator, a args) (val, *Error) {
	_, v, _, err := varianceArgs(e, a)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// sqrtNum is sqrt through the evaluator's own sqrt, so an exact square
// (variance 9/4) gives an exact root.
func (e *evaluator) sqrtNum(x num, pos int) (num, *Error) {
	v, err := fnSqrt(e, args{v: []val{x}, pos: pos, name: "sqrt"})
	if err != nil {
		return num{}, err
	}
	return v.(num), nil
}

func fnStdev(e *evaluator, a args) (val, *Error) {
	_, v, _, err := varianceArgs(e, a)
	if err != nil {
		return nil, err
	}
	return e.sqrtNum(v, a.pos)
}

func fnZscore(e *evaluator, a args) (val, *Error) {
	xs, v, m, err := varianceArgs(e, a)
	if err != nil {
		return nil, err
	}
	sd, err := e.sqrtNum(v, a.pos)
	if err != nil {
		return nil, err
	}
	if sd.isZero() {
		return nil, errf(codeDomain, a.pos, "zscore: every value is the same, so the standard deviation is zero")
	}
	if err := e.charge(len(xs), a.pos); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	out := make([]val, len(xs))
	for i, x := range xs {
		out[i] = o.div(o.sub(x, m), sd)
	}
	if o.err != nil {
		return nil, o.err
	}
	return out, nil
}

// fnQuantile is the linear-interpolation quantile (numpy's default, Excel's
// PERCENTILE.INC, R type 7): the value at position (n-1)p of the sorted
// data, counting from 0, interpolated between its neighbours.
func fnQuantile(percent bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		xs, err := nonEmpty(a, 0)
		if err != nil {
			return nil, err
		}
		p, err := a.num(1)
		if err != nil {
			return nil, err
		}
		top := exactInt64(1)
		if percent {
			top = exactInt64(100)
		}
		if p.sign() < 0 || cmpNum(p, top) > 0 {
			return nil, errf(codeDomain, a.pos, "%s: p must be from 0 to %s, got %s", a.name, fmtArg(top), fmtArg(p))
		}
		s, err := e.sortedNums(xs, a.pos)
		if err != nil {
			return nil, err
		}
		o := &chain{e: e, pos: a.pos}
		if percent {
			p = o.div(p, top)
		}
		h := o.mul(exactInt64(int64(len(s)-1)), p)
		if o.err != nil {
			return nil, o.err
		}
		var lo int64
		if h.exact() {
			lo = ratFloor(h.r).Int64()
		} else {
			lo = int64(math.Floor(h.f))
		}
		if lo >= int64(len(s)-1) {
			return s[len(s)-1], nil
		}
		frac := o.sub(h, exactInt64(lo))
		if o.err == nil && frac.isZero() {
			return s[lo], nil
		}
		return o.result(o.add(s[lo], o.mul(frac, o.sub(s[lo+1], s[lo]))))
	}
}

// fnRank ranks ascending, 1 for the smallest; values that compare equal
// share the mean of the ranks they span (scipy's rankdata "average", Excel's
// RANK.AVG with order 1).
func fnRank(e *evaluator, a args) (val, *Error) {
	xs, err := numList(a, 0)
	if err != nil {
		return nil, err
	}
	if err := e.charge(sortCost(len(xs))+len(xs), a.pos); err != nil {
		return nil, err
	}
	idx := make([]int, len(xs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return cmpNum(xs[idx[i]], xs[idx[j]]) < 0 })
	out := make([]val, len(xs))
	for i := 0; i < len(idx); {
		j := i + 1
		for j < len(idx) && cmpNum(xs[idx[j]], xs[idx[i]]) == 0 {
			j++
		}
		// Positions i..j-1 hold ranks i+1..j; their mean is (i+1+j)/2.
		r := exactNum(big.NewRat(int64(i+1+j), 2))
		for k := i; k < j; k++ {
			out[idx[k]] = r
		}
		i = j
	}
	return out, nil
}

func fnCumulative(op string) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		xs, err := numList(a, 0)
		if err != nil {
			return nil, err
		}
		if err := e.charge(len(xs), a.pos); err != nil {
			return nil, err
		}
		o := &chain{e: e, pos: a.pos}
		out := make([]val, len(xs))
		var acc num
		for i, x := range xs {
			if i == 0 {
				acc = x
			} else {
				acc = o.do(op, acc, x)
			}
			out[i] = acc
		}
		if o.err != nil {
			return nil, o.err
		}
		return out, nil
	}
}

func fnDiff(e *evaluator, a args) (val, *Error) {
	xs, err := numList(a, 0)
	if err != nil {
		return nil, err
	}
	if err := e.charge(len(xs), a.pos); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	out := make([]val, 0, max(len(xs)-1, 0))
	for i := 1; i < len(xs); i++ {
		out = append(out, o.sub(xs[i], xs[i-1]))
	}
	if o.err != nil {
		return nil, o.err
	}
	return out, nil
}

func fnCovariance(e *evaluator, a args) (val, *Error) {
	xs, ys, err := pair(a, 1)
	if err != nil {
		return nil, err
	}
	dd, err := ddof(a, 2)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	s, _, _ := o.comoment(xs, ys)
	return o.result(o.spreadOf(s, int64(len(xs)), dd, a))
}

// moments returns sxy, sxx, syy and the means, refusing data where x (or,
// for correlation, y) does not vary.
func moments(e *evaluator, a args, needY bool) (sxy, sxx, syy, mx, my num, err *Error) {
	xs, ys, err := pair(a, 2)
	if err != nil {
		return
	}
	o := &chain{e: e, pos: a.pos}
	sxy, mx, my = o.comoment(xs, ys)
	sxx, _, _ = o.comoment(xs, xs)
	if needY {
		syy, _, _ = o.comoment(ys, ys)
	}
	if o.err != nil {
		return sxy, sxx, syy, mx, my, o.err
	}
	if sxx.isZero() || needY && syy.isZero() {
		err = errf(codeDomain, a.pos, "%s: every x (or every y) is the same, so the result is undefined", a.name)
	}
	return
}

func fnCorrelation(e *evaluator, a args) (val, *Error) {
	sxy, sxx, syy, _, _, err := moments(e, a, true)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	prod := o.mul(sxx, syy)
	if o.err != nil {
		return nil, o.err
	}
	root, err := e.sqrtNum(prod, a.pos)
	if err != nil {
		return nil, err
	}
	return o.result(o.div(sxy, root))
}

// fnLinreg fits y = slope*x + intercept by least squares and returns
// [slope, intercept]; exact on exact data.
func fnLinreg(e *evaluator, a args) (val, *Error) {
	sxy, sxx, _, mx, my, err := moments(e, a, false)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	slope := o.div(sxy, sxx)
	intercept := o.sub(my, o.mul(slope, mx))
	if o.err != nil {
		return nil, o.err
	}
	return []val{slope, intercept}, nil
}
