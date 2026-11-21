package calc

import "math"

// The finance functions follow the spreadsheet conventions (Excel,
// LibreOffice): money paid out is negative and money received positive,
// rate is the rate per period, and when is 0 for payments at the end of
// each period, 1 for the beginning. pmt, fv, pv and npv are rational in
// their inputs, so they go through numOp and stay exact for exact inputs
// and a whole number of periods. nper needs a logarithm and rate and
// irr a root-finder, so those three are floats; a root-finder that does
// not converge refuses, it never returns its last guess.
var financeFuncs = []builtin{
	{name: "pmt", sig: "pmt(rate, n, present, future?, when?)", section: SecFinance, min: 3, max: 5, fn: fnPmt,
		summary: "the payment per period that pays off present (to future) in n periods"},
	{name: "fv", sig: "fv(rate, n, payment, present?, when?)", section: SecFinance, min: 3, max: 5, fn: fnFv,
		summary: "the value after n periods of payment, starting from present"},
	{name: "pv", sig: "pv(rate, n, payment, future?, when?)", section: SecFinance, min: 3, max: 5, fn: fnPv,
		summary: "the present value of n payments (and a final future amount)"},
	{name: "nper", sig: "nper(rate, payment, present, future?, when?)", section: SecFinance, min: 3, max: 5, fn: fnNper,
		summary: "the number of periods payment takes to go from present to future"},
	{name: "rate", sig: "rate(n, payment, present, future?, when?, guess?)", section: SecFinance, min: 3, max: 6, fn: fnRate,
		summary: "the rate per period of an annuity (Excel's RATE); a float"},
	{name: "npv", sig: "npv(rate, flows)", section: SecFinance, min: 2, max: 2, fn: fnNpv,
		summary: "net present value, the first flow discounted one period (as Excel's NPV)"},
	{name: "irr", sig: "irr(flows, guess?)", section: SecFinance, min: 1, max: 2, fn: fnIrr,
		summary: "internal rate of return of flows per period, the first at time 0; a float"},
	{name: "compound", sig: "compound(principal, rate, periods, per_year?)", section: SecFinance, min: 3, max: 4, fn: fnCompound,
		summary: "principal grown at rate per period, or at an annual rate compounded per_year times"},
	{name: "cagr", sig: "cagr(start, end, years)", section: SecFinance, min: 3, max: 3, fn: fnCagr,
		summary: "compound annual growth rate as a fraction; a float"},
	{name: "pct", sig: "pct(part, whole)", section: SecFinance, min: 2, max: 2, fn: fnPct,
		summary: "part as a percentage of whole"},
	{name: "pctchange", sig: "pctchange(old, new)", section: SecFinance, min: 2, max: 2, fn: fnPctChange,
		summary: "the change from old to new as a percentage of old"},
}

const (
	// solverIterations bounds Newton's method in rate and irr, and
	// bisectIterations irr's fallback; each iteration of irr also charges
	// one step per flow.
	solverIterations = 100
	bisectIterations = 200
)

// optNum returns argument i, or def when it was not passed.
func optNum(a args, i int, def num) (num, *Error) {
	if len(a.v) <= i {
		return def, nil
	}
	return a.num(i)
}

// whenArg reads the optional payment timing at i: 0 (end of period, the
// default) or 1 (beginning).
func whenArg(a args, i int) (num, *Error) {
	w, err := optNum(a, i, exactInt64(0))
	if err != nil {
		return num{}, err
	}
	if v, ok := smallInt(w, 1); !ok || v < 0 {
		return num{}, errf(codeDomain, a.pos, "%s: when must be 0 (end of period) or 1 (beginning), got %s", a.name, fmtArg(w))
	}
	return w, nil
}

// annuityArgs reads (rate|n, x, y, z?, when?) — the shape pmt, fv, pv and
// nper share: three required numbers, an optional fourth defaulting to 0
// and the timing.
func annuityArgs(a args) (x0, x1, x2, x3, w num, err *Error) {
	if x0, err = a.num(0); err != nil {
		return
	}
	if x1, err = a.num(1); err != nil {
		return
	}
	if x2, err = a.num(2); err != nil {
		return
	}
	if x3, err = optNum(a, 3, exactInt64(0)); err != nil {
		return
	}
	w, err = whenArg(a, 4)
	return
}

// refuseRate refuses a rate of -100% or less, where (1+rate)^n is zero or
// changes sign.
func refuseRate(r num, a args) *Error {
	if cmpNum(r, exactInt64(-1)) <= 0 {
		return errf(codeDomain, a.pos, "%s: rate %s is -100%% or less", a.name, fmtArg(r))
	}
	return nil
}

// growth returns g = (1+r)^n and k = (1+r*when)(g-1)/r, the factor that
// turns a payment per period into its value at period n.
func (o *chain) growth(r, n, w num) (g, k num) {
	g = o.pow(o.add(exactInt64(1), r), n)
	k = o.div(o.mul(o.add(exactInt64(1), o.mul(r, w)), o.sub(g, exactInt64(1))), r)
	return g, k
}

func neg(o *chain, x num) num { return o.sub(exactInt64(0), x) }

// fnPmt: pv*g + pmt*k + fv = 0, so pmt = -(pv*g + fv) / k.
func fnPmt(e *evaluator, a args) (val, *Error) {
	r, n, present, future, w, err := annuityArgs(a)
	if err != nil {
		return nil, err
	}
	if err := refuseRate(r, a); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	if r.isZero() {
		return o.result(neg(o, o.div(o.add(present, future), n)))
	}
	g, k := o.growth(r, n, w)
	return o.result(neg(o, o.div(o.add(o.mul(present, g), future), k)))
}

func fnFv(e *evaluator, a args) (val, *Error) {
	r, n, payment, present, w, err := annuityArgs(a)
	if err != nil {
		return nil, err
	}
	if err := refuseRate(r, a); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	if r.isZero() {
		return o.result(neg(o, o.add(present, o.mul(payment, n))))
	}
	g, k := o.growth(r, n, w)
	return o.result(neg(o, o.add(o.mul(present, g), o.mul(payment, k))))
}

func fnPv(e *evaluator, a args) (val, *Error) {
	r, n, payment, future, w, err := annuityArgs(a)
	if err != nil {
		return nil, err
	}
	if err := refuseRate(r, a); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	if r.isZero() {
		return o.result(neg(o, o.add(future, o.mul(payment, n))))
	}
	g, k := o.growth(r, n, w)
	return o.result(neg(o, o.div(o.add(future, o.mul(payment, k)), g)))
}

// fnNper solves pv*g + pmt*k + fv = 0 for n: with z = pmt(1+r*when)/r,
// (1+r)^n = (z - fv) / (z + pv).
func fnNper(e *evaluator, a args) (val, *Error) {
	r, payment, present, future, w, err := annuityArgs(a)
	if err != nil {
		return nil, err
	}
	if err := refuseRate(r, a); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	if r.isZero() {
		if payment.isZero() {
			return nil, errf(codeDomain, a.pos, "nper: with a zero rate and a zero payment the balance never changes")
		}
		return o.result(neg(o, o.div(o.add(present, future), payment)))
	}
	z := o.div(o.mul(payment, o.add(exactInt64(1), o.mul(r, w))), r)
	num1, den := o.sub(z, future), o.add(z, present)
	if o.err != nil {
		return nil, o.err
	}
	if den.isZero() || num1.isZero() || num1.sign() != den.sign() {
		return nil, errf(codeDomain, a.pos, "nper: payment %s never takes the balance from %s to %s at this rate", fmtArg(payment), fmtArg(present), fmtArg(future)).
			withHint("check the signs: money paid out is negative, money received positive")
	}
	q := o.div(num1, den)
	if o.err != nil {
		return nil, o.err
	}
	lq, err := ln(q, a.pos)
	if err != nil {
		return nil, err
	}
	return checkFloat(lq/math.Log1p(r.float()), a.pos, "nper")
}

// annuity is f(r) = pv*(1+r)^n + pmt*(1+r*w)*((1+r)^n - 1)/r + fv in
// floats, its derivative in r, and the sum of the terms' sizes, which
// scales the convergence test.
func annuity(r, n, pmt, pv, fv, w float64) (f, df, size float64) {
	lg := n * math.Log1p(r)
	g := math.Exp(lg)
	gm1 := math.Expm1(lg)
	dg := n * g / (1 + r)
	var k, dk float64
	if r == 0 {
		k = n
		dk = n*(n-1)/2 + w*n
	} else {
		k = (1 + r*w) * gm1 / r
		dk = w*gm1/r + (1+r*w)*(dg*r-gm1)/(r*r)
	}
	f = pv*g + pmt*k + fv
	df = pv*dg + pmt*dk
	size = math.Abs(pv*g) + math.Abs(pmt*k) + math.Abs(fv)
	return f, df, size
}

func fnRate(e *evaluator, a args) (val, *Error) {
	n, payment, present, future, w, err := annuityArgs(a)
	if err != nil {
		return nil, err
	}
	guess, err := optNum(a, 5, floatNum(0.1))
	if err != nil {
		return nil, err
	}
	if n.sign() <= 0 {
		return nil, errf(codeDomain, a.pos, "rate: n must be above zero, got %s", fmtArg(n))
	}
	nf, pf, vf, ff, wf := n.float(), payment.float(), present.float(), future.float(), w.float()
	f := func(r float64) (float64, float64, float64) { return annuity(r, nf, pf, vf, ff, wf) }
	if r, ok, err := e.newton(f, guess.float(), 1, a.pos); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return checkFloat(r, a.pos, "rate")
	}
	return nil, errf(codeDomain, a.pos, "rate: no rate solves these cash flows (the search from %s did not converge)", fmtArg(guess)).
		withHint("check the signs (money paid out negative, received positive); if a rate should exist, pass a guess near it")
}

// fnNpv discounts flows[i] by (1+rate)^i for i from 1, as Excel's NPV
// does: the first flow is one period away, not at time 0. Horner's rule
// from the last flow keeps it exact for exact inputs.
func fnNpv(e *evaluator, a args) (val, *Error) {
	r, err := a.num(0)
	if err != nil {
		return nil, err
	}
	if err := refuseRate(r, a); err != nil {
		return nil, err
	}
	flows, err := numList(a, 1)
	if err != nil {
		return nil, err
	}
	if err := e.charge(len(flows), a.pos); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	base := o.add(exactInt64(1), r)
	acc := exactInt64(0)
	for i := len(flows) - 1; i >= 0; i-- {
		acc = o.div(o.add(acc, flows[i]), base)
	}
	return o.result(acc)
}

// npv0 is the net present value with the first flow at time 0, its
// derivative in r, and the sum of the discounted flows' sizes.
func npv0(flows []float64, r float64) (f, df, size float64) {
	v := 1 / (1 + r)
	var p, dp, s float64
	for t := len(flows) - 1; t >= 0; t-- {
		dp = dp*v + p
		p = p*v + flows[t]
		s = s*v + math.Abs(flows[t])
	}
	return p, -dp * v * v, s
}

func finite(xs ...float64) bool {
	for _, x := range xs {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}

// fnIrr finds r with npv0(r) = 0: Newton's method from guess, then, if
// that fails, bisection over -99% (or nearer 0, see irrBisect) to 1000% when the value changes sign
// there. Flows with no sign change, or where neither converges, are
// refused.
func fnIrr(e *evaluator, a args) (val, *Error) {
	xs, err := numList(a, 0)
	if err != nil {
		return nil, err
	}
	guess, err := optNum(a, 1, floatNum(0.1))
	if err != nil {
		return nil, err
	}
	flows := make([]float64, len(xs))
	pos, negs := false, false
	for i, x := range xs {
		flows[i] = x.float()
		pos = pos || x.sign() > 0
		negs = negs || x.sign() < 0
	}
	if !pos || !negs {
		return nil, errf(codeDomain, a.pos, "irr: the flows need at least one negative and one positive value").
			withHint("an investment is negative (paid out), its returns positive")
	}
	f := func(r float64) (float64, float64, float64) { return npv0(flows, r) }
	if r, ok, err := e.newton(f, guess.float(), len(flows), a.pos); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return checkFloat(r, a.pos, "irr")
	}
	if r, ok, err := irrBisect(e, a, flows); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return checkFloat(r, a.pos, "irr")
	}
	return nil, errf(codeDomain, a.pos, "irr: no rate makes the net present value zero (the search from %s did not converge)", fmtArg(guess)).
		withHint("flows that change sign more than once can have no IRR; if one should exist, pass a guess near it")
}

// newton finds a root of f (value, derivative, scale) from r by Newton's
// method, charging cost steps per evaluation of f. A step that leaves the
// domain (a rate of -100% or less) or makes f overflow is halved back
// toward r, so one wild step from a far guess does not end the search. It
// reports false, never a last guess, when the iteration does not settle on
// a point where f is zero to 1e-9 of the terms' size.
func (e *evaluator) newton(f func(float64) (float64, float64, float64), r float64, cost, pos int) (float64, bool, *Error) {
	for i := 0; i < solverIterations; i++ {
		if err := e.charge(cost, pos); err != nil {
			return 0, false, err
		}
		v, dv, size := f(r)
		if !finite(v, dv) || dv == 0 {
			return 0, false, nil
		}
		next := r - v/dv
		for k := 0; k < 60; k++ {
			if next > -1 {
				if err := e.charge(cost, pos); err != nil {
					return 0, false, err
				}
				if nv, ndv, _ := f(next); finite(nv, ndv) {
					break
				}
			}
			next = (r + next) / 2
		}
		if math.Abs(next-r) <= 1e-12*math.Max(1, math.Abs(r)) {
			v2, _, size2 := f(next)
			return next, finite(v2) && math.Abs(v2) <= 1e-9*math.Max(size, size2), nil
		}
		r = next
	}
	return 0, false, nil
}

func irrBisect(e *evaluator, a args, flows []float64) (float64, bool, *Error) {
	// A long series overflows near -99% (1/(1+r) to the power of its
	// length), so the low end moves toward 0 until the value is finite.
	lo, hi := -0.99, 10.0
	flo, _, _ := npv0(flows, lo)
	for k := 0; k < 60 && !finite(flo); k++ {
		if err := e.charge(len(flows), a.pos); err != nil {
			return 0, false, err
		}
		lo /= 2
		flo, _, _ = npv0(flows, lo)
	}
	fhi, _, _ := npv0(flows, hi)
	if !finite(flo, fhi) || (flo < 0) == (fhi < 0) {
		return 0, false, nil
	}
	for i := 0; i < bisectIterations && hi-lo > 1e-15; i++ {
		if err := e.charge(len(flows), a.pos); err != nil {
			return 0, false, err
		}
		mid := (lo + hi) / 2
		fm, _, _ := npv0(flows, mid)
		if (fm < 0) == (flo < 0) {
			lo, flo = mid, fm
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, true, nil
}

// fnCompound is principal*(1+rate)^periods, or with per_year = m,
// principal*(1+rate/m)^(periods*m) for an annual rate and periods in years.
func fnCompound(e *evaluator, a args) (val, *Error) {
	p, err := a.num(0)
	if err != nil {
		return nil, err
	}
	r, err := a.num(1)
	if err != nil {
		return nil, err
	}
	n, err := a.num(2)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	if len(a.v) == 4 {
		m, err := a.num(3)
		if err != nil {
			return nil, err
		}
		if v, ok := smallInt(m, 1_000_000); !ok || v < 1 {
			return nil, errf(codeDomain, a.pos, "compound: per_year must be a whole number from 1 to 1000000, got %s", fmtArg(m))
		}
		r, n = o.div(r, m), o.mul(n, m)
	}
	if o.err == nil {
		if err := refuseRate(r, a); err != nil {
			return nil, err
		}
	}
	return o.result(o.mul(p, o.pow(o.add(exactInt64(1), r), n)))
}

func fnCagr(e *evaluator, a args) (val, *Error) {
	start, err := a.num(0)
	if err != nil {
		return nil, err
	}
	end, err := a.num(1)
	if err != nil {
		return nil, err
	}
	years, err := a.num(2)
	if err != nil {
		return nil, err
	}
	if start.sign() <= 0 || end.sign() < 0 || years.sign() <= 0 {
		return nil, errf(codeDomain, a.pos, "cagr needs start above zero, end zero or above and years above zero")
	}
	o := &chain{e: e, pos: a.pos}
	return o.result(o.sub(o.pow(o.div(end, start), o.div(exactInt64(1), years)), exactInt64(1)))
}

func fnPct(e *evaluator, a args) (val, *Error) {
	part, err := a.num(0)
	if err != nil {
		return nil, err
	}
	whole, err := a.num(1)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	return o.result(o.mul(o.div(part, whole), exactInt64(100)))
}

func fnPctChange(e *evaluator, a args) (val, *Error) {
	old, err := a.num(0)
	if err != nil {
		return nil, err
	}
	nw, err := a.num(1)
	if err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	return o.result(o.mul(o.div(o.sub(nw, old), old), exactInt64(100)))
}
