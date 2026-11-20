package calc

import "math"

var specialFuncs = []builtin{
	{name: "gamma", sig: "gamma(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: fnGamma, summary: "the gamma function; gamma(n) = (n-1)!"},
	{name: "lgamma", sig: "lgamma(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: fnLgamma, summary: "log |gamma(x)|, for arguments where gamma overflows"},
	{name: "beta", sig: "beta(a, b)", section: SecSpecial, min: 2, max: 2, vec: true, fn: fnBeta, summary: "the beta function gamma(a) gamma(b) / gamma(a + b)"},
	{name: "erf", sig: "erf(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: float1(math.Erf), summary: "the error function"},
	{name: "erfc", sig: "erfc(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: float1(math.Erfc), summary: "1 - erf(x), accurate for large x"},
	{name: "erfinv", sig: "erfinv(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: inRange(math.Erfinv, -1, 1, false), summary: "inverse of erf, -1 < x < 1"},
	{name: "erfcinv", sig: "erfcinv(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: inRange(math.Erfcinv, 0, 2, false), summary: "inverse of erfc, 0 < x < 2"},
	{name: "j0", sig: "j0(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: float1(math.J0), summary: "Bessel function of the first kind, order 0"},
	{name: "j1", sig: "j1(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: float1(math.J1), summary: "Bessel function of the first kind, order 1"},
	{name: "jn", sig: "jn(n, x)", section: SecSpecial, min: 2, max: 2, fn: besselN(math.Jn, false), summary: "Bessel function of the first kind, integer order n"},
	{name: "y0", sig: "y0(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: inRange(math.Y0, 0, math.Inf(1), false), summary: "Bessel function of the second kind, order 0, x > 0"},
	{name: "y1", sig: "y1(x)", section: SecSpecial, min: 1, max: 1, vec: true, fn: inRange(math.Y1, 0, math.Inf(1), false), summary: "Bessel function of the second kind, order 1, x > 0"},
	{name: "yn", sig: "yn(n, x)", section: SecSpecial, min: 2, max: 2, fn: besselN(math.Yn, true), summary: "Bessel function of the second kind, integer order n, x > 0"},
}

// gammaPole reports whether x is 0 or a negative integer, where gamma has
// a pole.
func gammaPole(x float64) bool { return x <= 0 && x == math.Trunc(x) }

func fnGamma(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	v := x.float()
	if gammaPole(v) {
		return nil, errf(codeDomain, a.pos, "gamma(%s) is a pole (undefined)", fmtArg(x)).withHint("gamma is undefined at 0 and the negative integers")
	}
	return checkFloat(math.Gamma(v), a.pos, "gamma")
}

func fnLgamma(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	v := x.float()
	if gammaPole(v) {
		return nil, errf(codeDomain, a.pos, "lgamma(%s) is a pole (undefined)", fmtArg(x))
	}
	lg, _ := math.Lgamma(v)
	return checkFloat(lg, a.pos, "lgamma")
}

func fnBeta(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	y, err := a.num(1)
	if err != nil {
		return nil, err
	}
	p, q := x.float(), y.float()
	if gammaPole(p) || gammaPole(q) {
		return nil, errf(codeDomain, a.pos, "beta(%s, %s) is undefined at a pole of gamma", fmtArg(x), fmtArg(y))
	}
	if gammaPole(p + q) {
		// gamma(a+b) is infinite while gamma(a), gamma(b) are finite.
		return floatNum(0), nil
	}
	la, sa := math.Lgamma(p)
	lb, sb := math.Lgamma(q)
	lab, sab := math.Lgamma(p + q)
	return checkFloat(float64(sa*sb*sab)*math.Exp(la+lb-lab), a.pos, "beta")
}

// besselN takes an integer order n, then x; yn also needs x > 0.
func besselN(f func(int, float64) float64, positiveX bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		n, err := a.small(0, 10_000)
		if err != nil {
			return nil, err
		}
		if l, ok := a.v[1].([]val); ok {
			out := make([]val, 0, len(l))
			for _, xv := range l {
				if err := e.charge(1, a.pos); err != nil {
					return nil, err
				}
				v, err := besselOne(f, positiveX, int(n), args{v: []val{a.v[0], xv}, pos: a.pos, name: a.name})
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, nil
		}
		return besselOne(f, positiveX, int(n), a)
	}
}

func besselOne(f func(int, float64) float64, positiveX bool, n int, a args) (val, *Error) {
	x, err := a.num(1)
	if err != nil {
		return nil, err
	}
	if positiveX && x.sign() <= 0 {
		return nil, errf(codeDomain, a.pos, "%s(%d, %s) is undefined: x must be > 0", a.name, n, fmtArg(x))
	}
	return checkFloat(f(n, x.float()), a.pos, a.name)
}
