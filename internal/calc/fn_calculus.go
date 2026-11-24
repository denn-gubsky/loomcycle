package calc

import (
	"container/heap"
	"math"

	"gonum.org/v1/gonum/mat"
)

// SecCalculus holds numeric calculus: integrals, derivatives, roots,
// minima and polynomials.
const SecCalculus = "calculus"

// The methods that take a function (integrate, deriv, root, minimize)
// evaluate it at float points through probe, which charges a step for the
// method's own arithmetic on top of what the call of f charges, and refuses
// past a fixed number of evaluations: a method that has not converged by
// then is refused with no_convergence, never answered with its last guess.
// The step bound still applies across all of them, so a costly f nested in
// a costly method stops at step_limit first.
var calculusFuncs = []builtin{
	{name: "integrate", sig: "integrate(f, a, b)", section: SecCalculus, min: 3, max: 3, fn: fnIntegrate,
		summary: "the integral of f from a to b, by adaptive Gauss-Kronrod quadrature"},
	{name: "deriv", sig: "deriv(f, x)", section: SecCalculus, min: 2, max: 2, fn: fnDeriv,
		summary: "the derivative of f at x, by central differences with extrapolation"},
	{name: "root", sig: "root(f, a, b)", section: SecCalculus, min: 3, max: 3, fn: fnRoot,
		summary: "an x between a and b where f(x) = 0; f(a) and f(b) must differ in sign"},
	{name: "minimize", sig: "minimize(f, a, b)", section: SecCalculus, min: 3, max: 3, fn: fnMinimize,
		summary: "the x between a and b where f is smallest (a local minimum), by Brent's method"},
	{name: "polyfit", sig: "polyfit(x, y, deg)", section: SecCalculus, min: 3, max: 3, fn: fnPolyfit,
		summary: "least-squares polynomial coefficients, highest power first"},
	{name: "polyval", sig: "polyval(p, x)", section: SecCalculus, min: 2, max: 2, fn: fnPolyval,
		summary: "the polynomial with coefficients p (highest power first) at x"},
	{name: "interp", sig: "interp(x, xs, ys)", section: SecCalculus, min: 3, max: 3, fn: fnInterp,
		summary: "linear interpolation at x in the table xs, ys; xs increasing"},
}

// Evaluation caps per method. integrate: 15 per Gauss-Kronrod panel, so
// about 500 bisections; deriv needs at most 2 per row of its 10-row
// table; root and minimize converge in tens of evaluations on a smooth
// function and in about 60 bisection steps at worst for root.
const (
	maxIntegrateEvals = 15_000
	maxDerivEvals     = 20
	maxRootEvals      = 500
	maxMinimizeEvals  = 500
)

// probe evaluates f for a numeric method and counts the evaluations.
type probe struct {
	e     *evaluator
	f     *fnVal
	a     args
	n     int
	limit int
	hint  string
}

func (e *evaluator) probeOf(a args, limit int, hint string) (*probe, *Error) {
	f, err := a.fn(0)
	if err != nil {
		return nil, err
	}
	return &probe{e: e, f: f, a: a, limit: limit, hint: hint}, nil
}

// at returns f(x), refusing past the cap and a result that is not a number.
func (p *probe) at(x float64) (float64, *Error) {
	if p.n >= p.limit {
		return 0, errf(codeNoConvergence, p.a.pos, "%s did not converge within %d evaluations of the function", p.a.name, p.limit).
			withHint(p.hint)
	}
	p.n++
	if err := p.e.charge(1, p.a.pos); err != nil {
		return 0, err
	}
	if math.IsInf(x, 0) || math.IsNaN(x) {
		// deriv's x ± h can pass the float range; a num is never infinite.
		return 0, errf(codeOverflow, p.a.pos, "%s: a point it needs is beyond the 64-bit float range", p.a.name)
	}
	if x == 0 {
		x = 0 // no -0 into f
	}
	v, err := p.e.call(p.f, []val{floatNum(x)}, p.a.pos)
	if err != nil {
		return 0, err
	}
	n, ok := v.(num)
	if !ok {
		return 0, errf(codeType, p.a.pos, "%s: the function must return a number, got a %s", p.a.name, typeName(v))
	}
	y := n.float()
	if math.IsInf(y, 0) {
		return 0, errf(codeOverflow, p.a.pos, "%s: the function's value at %s is too large for a 64-bit float", p.a.name, fmtFloat(x))
	}
	return y, nil
}

// bounds reads arguments 1 and 2 as the ends of an interval; ordered
// refuses b <= a.
func bounds(a args, ordered bool) (float64, float64, *Error) {
	lo, err := a.realArg(1)
	if err != nil {
		return 0, 0, err
	}
	hi, err := a.realArg(2)
	if err != nil {
		return 0, 0, err
	}
	if ordered && !(lo < hi) {
		return 0, 0, errf(codeDomain, a.pos, "%s: a must be below b, got a = %s and b = %s", a.name, fmtFloat(lo), fmtFloat(hi))
	}
	return lo, hi, nil
}

// Gauss-Kronrod 15-point nodes and weights on [-1, 1] (QUADPACK qk15),
// with the embedded 7-point Gauss weights; the nodes are listed from the
// outside in, the centre last.
var (
	gkNodes = [8]float64{
		0.991455371120812639206854697526329, 0.949107912342758524526189684047851,
		0.864864423359769072789712788640926, 0.741531185599394439863864773280788,
		0.586087235467691130294144845693013, 0.405845151377397166906606412076961,
		0.207784955007898467600689403773245, 0,
	}
	gkWeights = [8]float64{
		0.022935322010529224963732008058970, 0.063092092629978553290700663189204,
		0.104790010322250183839876322541518, 0.140653259715525918745189590510238,
		0.169004726639267902826583426598550, 0.190350578064785409913256402421014,
		0.204432940075298892414161999234649, 0.209482141084727828012999174891714,
	}
	// gaussWeights[i] belongs to gkNodes[2i+1]; the last to the centre.
	gaussWeights = [4]float64{
		0.129484966168869693270611432679082, 0.279705391489276667901467771423780,
		0.381830050505118944950369775488975, 0.417959183673469387755102040816327,
	}
)

// panel is one subinterval with its Kronrod estimate, the estimate of
// |f|'s integral, and the error estimate |Kronrod - Gauss|.
type panel struct{ lo, hi, val, abs, err float64 }

func (p *probe) gk15(lo, hi float64) (panel, *Error) {
	c, h := (lo+hi)/2, (hi-lo)/2
	fc, err := p.at(c)
	if err != nil {
		return panel{}, err
	}
	k, g, abs := fc*gkWeights[7], fc*gaussWeights[3], math.Abs(fc)*gkWeights[7]
	for j := range 7 {
		f1, err := p.at(c - h*gkNodes[j])
		if err != nil {
			return panel{}, err
		}
		f2, err := p.at(c + h*gkNodes[j])
		if err != nil {
			return panel{}, err
		}
		k += gkWeights[j] * (f1 + f2)
		abs += gkWeights[j] * (math.Abs(f1) + math.Abs(f2))
		if j%2 == 1 {
			g += gaussWeights[j/2] * (f1 + f2)
		}
	}
	return panel{lo: lo, hi: hi, val: k * h, abs: abs * math.Abs(h), err: math.Abs((k - g) * h)}, nil
}

// panels is a max-heap on the error estimate: the worst panel is split
// next.
type panels []panel

func (q panels) Len() int           { return len(q) }
func (q panels) Less(i, j int) bool { return q[i].err > q[j].err }
func (q panels) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *panels) Push(x any)        { *q = append(*q, x.(panel)) }
func (q *panels) Pop() any {
	old := *q
	x := old[len(old)-1]
	*q = old[:len(old)-1]
	return x
}

// Integration tolerance: relative to the integral, with a floor relative
// to the integral of |f| so an integral that cancels to zero converges.
const (
	quadRelTol = 1e-10
	quadAbsTol = 1e-14
)

func fnIntegrate(e *evaluator, a args) (val, *Error) {
	p, err := e.probeOf(a, maxIntegrateEvals,
		"the integrand may be singular or oscillate fast inside the interval; split the interval at the trouble point, or narrow it")
	if err != nil {
		return nil, err
	}
	lo, hi, err := bounds(a, false)
	if err != nil {
		return nil, err
	}
	if lo == hi {
		return floatNum(0), nil
	}
	sign := 1.0
	if lo > hi {
		lo, hi, sign = hi, lo, -1
	}
	first, err := p.gk15(lo, hi)
	if err != nil {
		return nil, err
	}
	q := &panels{first}
	total, abs, errSum := first.val, first.abs, first.err
	for errSum > max(quadRelTol*math.Abs(total), quadAbsTol*abs) {
		worst := heap.Pop(q).(panel)
		mid := (worst.lo + worst.hi) / 2
		left, err := p.gk15(worst.lo, mid)
		if err != nil {
			return nil, err
		}
		right, err := p.gk15(mid, worst.hi)
		if err != nil {
			return nil, err
		}
		heap.Push(q, left)
		heap.Push(q, right)
		total += left.val + right.val - worst.val
		abs += left.abs + right.abs - worst.abs
		errSum += left.err + right.err - worst.err
	}
	// Sum afresh: the running total collects rounding from every split.
	total = 0
	for _, pn := range *q {
		total += pn.val
	}
	return checkFloat(sign*total, a.pos, a.name)
}

// Ridders' extrapolation of central differences (Numerical Recipes,
// dfridr): the step shrinks by derivShrink per row, and the extrapolated
// estimate with the smallest error is kept. An error estimate above
// derivTol, relative to the derivative or absolute below 1, is refused.
const (
	derivShrink = 1.4
	derivRows   = 10
	derivTol    = 1e-7
)

func fnDeriv(e *evaluator, a args) (val, *Error) {
	p, err := e.probeOf(a, maxDerivEvals, "deriv needs f defined and smooth on both sides of x")
	if err != nil {
		return nil, err
	}
	x, err := a.realArg(1)
	if err != nil {
		return nil, err
	}
	// The first step scales with x so a function defined only near x
	// (sqrt at 0.001) is not evaluated far outside its domain.
	h := 0.1 * math.Abs(x)
	if h == 0 {
		h = 0.1
	}
	central := func(h float64) (float64, *Error) {
		up, err := p.at(x + h)
		if err != nil {
			return 0, err
		}
		down, err := p.at(x - h)
		if err != nil {
			return 0, err
		}
		return (up - down) / (2 * h), nil
	}
	var tab [derivRows][derivRows]float64
	if tab[0][0], err = central(h); err != nil {
		return nil, err
	}
	best, bestErr := tab[0][0], math.Inf(1)
	for i := 1; i < derivRows; i++ {
		h /= derivShrink
		if tab[0][i], err = central(h); err != nil {
			return nil, err
		}
		fac := derivShrink * derivShrink
		for j := 1; j <= i; j++ {
			tab[j][i] = (tab[j-1][i]*fac - tab[j-1][i-1]) / (fac - 1)
			fac *= derivShrink * derivShrink
			if d := max(math.Abs(tab[j][i]-tab[j-1][i]), math.Abs(tab[j][i]-tab[j-1][i-1])); d <= bestErr {
				best, bestErr = tab[j][i], d
			}
		}
		// Higher orders have stopped helping: rounding now dominates.
		if math.Abs(tab[i][i]-tab[i-1][i-1]) >= 2*bestErr {
			break
		}
	}
	if !(bestErr <= derivTol*max(1, math.Abs(best))) {
		return nil, errf(codeNoConvergence, a.pos, "deriv: the difference quotients do not settle (error estimate %s)", fmtFloat(bestErr)).
			withHint("f may not be differentiable at x, or may be too noisy there")
	}
	return checkFloat(best, a.pos, a.name)
}

func sameSign(x, y float64) bool { return x > 0 && y > 0 || x < 0 && y < 0 }

// fnRoot is Brent's method (Numerical Recipes, zbrent) on a bracket. It
// stops when the bracket is within a few ulps of the root, or within
// 1e-15 of the bracket's size for a root at or near zero.
func fnRoot(e *evaluator, a args) (val, *Error) {
	p, err := e.probeOf(a, maxRootEvals, "f may be discontinuous inside the bracket; narrow it")
	if err != nil {
		return nil, err
	}
	lo, hi, err := bounds(a, true)
	if err != nil {
		return nil, err
	}
	fa, err := p.at(lo)
	if err != nil {
		return nil, err
	}
	if fa == 0 {
		return checkFloat(lo, a.pos, a.name)
	}
	fb, err := p.at(hi)
	if err != nil {
		return nil, err
	}
	if fb == 0 {
		return checkFloat(hi, a.pos, a.name)
	}
	if sameSign(fa, fb) {
		return nil, errf(codeDomain, a.pos, "root: f(a) = %s and f(b) = %s have the same sign, so the interval brackets no root", fmtFloat(fa), fmtFloat(fb)).
			withHint("pick a and b with f(a) and f(b) of opposite signs; minimize finds where a curve that only touches zero is lowest")
	}
	xtol := 1e-15 * max(math.Abs(lo), math.Abs(hi))
	a0, b := lo, hi
	c, fc := b, fb
	var d, step float64
	for {
		if sameSign(fb, fc) {
			c, fc = a0, fa
			d = b - a0
			step = d
		}
		if math.Abs(fc) < math.Abs(fb) {
			a0, b, c = b, c, b
			fa, fb, fc = fb, fc, fb
		}
		tol := 2*machEps*math.Abs(b) + 0.5*xtol
		xm := 0.5 * (c - b)
		if math.Abs(xm) <= tol || fb == 0 {
			return checkFloat(b, a.pos, a.name)
		}
		if math.Abs(step) >= tol && math.Abs(fa) > math.Abs(fb) {
			// Inverse quadratic interpolation, or secant when a0 == c.
			s := fb / fa
			var pp, q float64
			if a0 == c {
				pp, q = 2*xm*s, 1-s
			} else {
				q0, r := fa/fc, fb/fc
				pp = s * (2*xm*q0*(q0-r) - (b-a0)*(r-1))
				q = (q0 - 1) * (r - 1) * (s - 1)
			}
			if pp > 0 {
				q = -q
			}
			pp = math.Abs(pp)
			if 2*pp < min(3*xm*q-math.Abs(tol*q), math.Abs(step*q)) {
				step, d = d, pp/q
			} else {
				d, step = xm, xm
			}
		} else {
			d, step = xm, xm
		}
		a0, fa = b, fb
		if math.Abs(d) > tol {
			b += d
		} else {
			b += math.Copysign(tol, xm)
		}
		if fb, err = p.at(b); err != nil {
			return nil, err
		}
	}
}

// goldenSection is the fraction of the larger part a golden-section step
// moves into.
const goldenSection = 0.3819660112501051

// minimizeTol is the relative tolerance on x: about sqrt of the machine
// epsilon, the best a minimum can be located from function values alone.
const minimizeTol = 1.5e-8

// fnMinimize is Brent's method (Numerical Recipes, brent; MATLAB's
// fminbnd) inside [a, b]. It finds a local minimum; an end it has closed
// in on is compared at the close, so a minimum at an end is returned
// exactly.
func fnMinimize(e *evaluator, a args) (val, *Error) {
	p, err := e.probeOf(a, maxMinimizeEvals, "f may be flat or noisy near its minimum; narrow the interval")
	if err != nil {
		return nil, err
	}
	lo, hi, err := bounds(a, true)
	if err != nil {
		return nil, err
	}
	zeps := 1e-10 * (hi - lo)
	x := lo + goldenSection*(hi-lo)
	fx, err := p.at(x)
	if err != nil {
		return nil, err
	}
	w, v, fw, fv := x, x, fx, fx
	l, r := lo, hi
	var d, step float64
	for {
		xm := 0.5 * (l + r)
		tol1 := minimizeTol*math.Abs(x) + zeps
		tol2 := 2 * tol1
		if math.Abs(x-xm) <= tol2-0.5*(r-l) {
			break
		}
		parabolic := false
		if math.Abs(step) > tol1 {
			rr := (x - w) * (fx - fv)
			q := (x - v) * (fx - fw)
			pp := (x-v)*q - (x-w)*rr
			q = 2 * (q - rr)
			if q > 0 {
				pp = -pp
			}
			q = math.Abs(q)
			prev := step
			step = d
			if math.Abs(pp) < math.Abs(0.5*q*prev) && pp > q*(l-x) && pp < q*(r-x) {
				d = pp / q
				if u := x + d; u-l < tol2 || r-u < tol2 {
					d = math.Copysign(tol1, xm-x)
				}
				parabolic = true
			}
		}
		if !parabolic {
			if x >= xm {
				step = l - x
			} else {
				step = r - x
			}
			d = goldenSection * step
		}
		u := x + math.Copysign(tol1, d)
		if math.Abs(d) >= tol1 {
			u = x + d
		}
		fu, err := p.at(u)
		if err != nil {
			return nil, err
		}
		if fu <= fx {
			if u >= x {
				l = x
			} else {
				r = x
			}
			v, w, x = w, x, u
			fv, fw, fx = fw, fx, fu
			continue
		}
		if u < x {
			l = u
		} else {
			r = u
		}
		switch {
		case fu <= fw || w == x:
			v, w, fv, fw = w, u, fw, fu
		case fu <= fv || v == x || v == w:
			v, fv = u, fu
		}
	}
	// Brent never evaluates the ends; when it has closed in on one, the
	// end itself may be the minimum. An end far from x is not evaluated:
	// f need not be defined there (1/x + x on [0, 5]).
	near := 4 * (minimizeTol*math.Abs(x) + zeps)
	for _, end := range []float64{lo, hi} {
		if math.Abs(x-end) > near {
			continue
		}
		fe, err := p.at(end)
		if err != nil {
			return nil, err
		}
		if fe <= fx {
			x, fx = end, fe
		}
	}
	return checkFloat(x, a.pos, a.name)
}

// polyfit solves the least-squares system of the Vandermonde matrix by QR
// (gonum), admitted through heavy like the matrix functions. A system that
// is singular or too badly conditioned for float64 (too few distinct x for
// the degree, or a high degree over a wide x range) is refused, never
// answered with noise.
func fnPolyfit(e *evaluator, a args) (val, *Error) {
	xs, err := e.vectorOf(a.v[0], a.name, a.pos)
	if err != nil {
		return nil, err
	}
	ys, err := e.vectorOf(a.v[1], a.name, a.pos)
	if err != nil {
		return nil, err
	}
	if len(xs) != len(ys) {
		return nil, errf(codeShape, a.pos, "polyfit: x has %d values and y has %d", len(xs), len(ys))
	}
	deg, err := a.small(2, int64(len(xs)))
	if err != nil || deg < 0 {
		return nil, errf(codeDomain, a.pos, "polyfit: the degree must be a whole number from 0 to %d (one less than the number of points)", len(xs)-1)
	}
	n, c := len(xs), int(deg)+1
	if c > n {
		return nil, errf(codeDomain, a.pos, "polyfit: a degree-%d polynomial needs at least %d points, got %d", deg, c, n)
	}
	if n*c > MaxMatrixElems {
		return nil, errf(codeTooLarge, a.pos, "polyfit: the fit reads %d × %d values; the limit is %d", n, c, MaxMatrixElems)
	}
	vm := mat.NewDense(n, c, nil)
	for i, x := range xs {
		pw := 1.0
		for j := c - 1; j >= 0; j-- {
			vm.Set(i, j, pw)
			pw *= x
		}
	}
	var coef mat.Dense
	err = e.run(vm, cubicWork(n, c), a, func() *Error {
		if err := coef.Solve(vm, mat.NewDense(n, 1, ys)); err != nil {
			return errf(codeDomain, a.pos, "polyfit: the points do not determine a degree-%d polynomial in 64-bit floats (too few distinct x, or too badly conditioned)", deg).
				withHint("lower the degree, or shift and scale x first, e.g. x - mean(x)")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e.valOfFloats(mat.Col(nil, 0, &coef), a.name, a.pos)
}

// eachPoint applies f to argument i, or to each element when it is a
// list, the way vec maps a first argument.
func eachPoint(a args, i int, f func(num) (val, *Error)) (val, *Error) {
	l, ok := a.v[i].([]val)
	if !ok {
		x, err := a.num(i)
		if err != nil {
			return nil, err
		}
		return f(x)
	}
	out := make([]val, len(l))
	for k, v := range l {
		x, ok := v.(num)
		if !ok {
			return nil, errf(codeType, a.pos, "%s: the points must be numbers, got a %s", a.name, typeName(v))
		}
		y, err := f(x)
		if err != nil {
			return nil, err
		}
		out[k] = y
	}
	return out, nil
}

// fnPolyval is Horner's rule in the evaluator's own arithmetic, so exact
// coefficients at an exact x give an exact value.
func fnPolyval(e *evaluator, a args) (val, *Error) {
	if polyHasComplex(a) {
		return polyvalComplex(e, a)
	}
	coef, err := numList(a, 0)
	if err != nil {
		return nil, err
	}
	if len(coef) == 0 {
		return nil, errf(codeDomain, a.pos, "polyval needs at least one coefficient").
			withHint("[2, 0, -1] is 2x^2 - 1: highest power first")
	}
	return eachPoint(a, 1, func(x num) (val, *Error) {
		o := &chain{e: e, pos: a.pos}
		o.err = e.charge(len(coef), a.pos)
		y := coef[0]
		for _, c := range coef[1:] {
			y = o.add(o.mul(y, x), c)
		}
		return o.result(y)
	})
}

// fnInterp interpolates linearly between the two table points around x,
// found by bisection, in the evaluator's own arithmetic (exact on exact
// input). A point outside the table is refused: extrapolating is a
// different question.
func fnInterp(e *evaluator, a args) (val, *Error) {
	xs, err := numList(a, 1)
	if err != nil {
		return nil, err
	}
	ys, err := numList(a, 2)
	if err != nil {
		return nil, err
	}
	if len(xs) != len(ys) {
		return nil, errf(codeShape, a.pos, "interp: xs has %d values and ys has %d", len(xs), len(ys))
	}
	if len(xs) < 2 {
		return nil, errf(codeDomain, a.pos, "interp needs at least two points, got %d", len(xs))
	}
	if err := e.charge(len(xs), a.pos); err != nil {
		return nil, err
	}
	for i := 1; i < len(xs); i++ {
		if cmpNum(xs[i-1], xs[i]) >= 0 {
			return nil, errf(codeDomain, a.pos, "interp: xs must be strictly increasing; xs[%d] = %s is not above xs[%d] = %s", i+1, fmtArg(xs[i]), i, fmtArg(xs[i-1])).
				withHint("sort the points by x first, and merge repeated x values")
		}
	}
	last := len(xs) - 1
	return eachPoint(a, 0, func(x num) (val, *Error) {
		if cmpNum(x, xs[0]) < 0 || cmpNum(x, xs[last]) > 0 {
			return nil, errf(codeDomain, a.pos, "interp: %s is outside the table, %s to %s", fmtArg(x), fmtArg(xs[0]), fmtArg(xs[last])).
				withHint("interp does not extrapolate; fit a line with linreg or polyfit to go beyond the table")
		}
		if err := e.charge(bisectSteps(len(xs)), a.pos); err != nil {
			return nil, err
		}
		// The first i with xs[i] >= x, at least 1.
		i, j := 1, last
		for i < j {
			m := (i + j) / 2
			if cmpNum(xs[m], x) < 0 {
				i = m + 1
			} else {
				j = m
			}
		}
		o := &chain{e: e, pos: a.pos}
		t := o.div(o.sub(x, xs[i-1]), o.sub(xs[i], xs[i-1]))
		return o.result(o.add(ys[i-1], o.mul(t, o.sub(ys[i], ys[i-1]))))
	})
}

// bisectSteps is the number of bisection steps over n values.
func bisectSteps(n int) int {
	b := 1
	for n > 1 {
		n /= 2
		b++
	}
	return b
}
