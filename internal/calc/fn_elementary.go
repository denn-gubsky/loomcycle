package calc

import (
	"math"
	"math/big"
)

var elementaryFuncs = []builtin{
	{name: "abs", sig: "abs(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: fnAbs,
		summary: "absolute value; exact on exact input"},
	{name: "sign", sig: "sign(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: fnSign,
		summary: "-1, 0 or 1"},
	{name: "sqrt", sig: "sqrt(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: fnSqrt,
		summary: "square root; exact when x is the square of a fraction, refused for x < 0"},
	{name: "cbrt", sig: "cbrt(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: float1(math.Cbrt),
		summary: "cube root, real for negative x"},
	{name: "exp", sig: "exp(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: float1(math.Exp),
		summary: "e to the power x"},
	{name: "expm1", sig: "expm1(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: float1(math.Expm1),
		summary: "exp(x) - 1, accurate near 0"},
	{name: "log", sig: "log(x, base?)", section: SecElementary, min: 1, max: 2, vec: true, fn: fnLog,
		summary: "natural logarithm, or the logarithm to base"},
	{name: "log10", sig: "log10(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: logOf(math.Log10, 10),
		summary: "base-10 logarithm"},
	{name: "log2", sig: "log2(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: logOf(math.Log2, 2),
		summary: "base-2 logarithm"},
	{name: "log1p", sig: "log1p(x)", section: SecElementary, min: 1, max: 1, vec: true, fn: fnLog1p,
		summary: "log(1 + x), accurate near 0"},
	{name: "pow", sig: "pow(x, y)", section: SecElementary, min: 2, max: 2, vec: true, fn: fnPow,
		summary: "x^y; exact for an exact base and integer exponent"},
	{name: "hypot", sig: "hypot(x, y)", section: SecElementary, min: 2, max: 2, vec: true, fn: fnHypot,
		summary: "sqrt(x^2 + y^2) without overflow"},
	{name: "floor", sig: "floor(x, n?)", section: SecElementary, min: 1, max: 2, vec: true, fn: rounder(modeFloor),
		summary: "round down, to n decimal places (default 0)"},
	{name: "ceil", sig: "ceil(x, n?)", section: SecElementary, min: 1, max: 2, vec: true, fn: rounder(modeCeil),
		summary: "round up, to n decimal places (default 0)"},
	{name: "round", sig: "round(x, n?)", section: SecElementary, min: 1, max: 2, vec: true, fn: rounder(modeHalfAway),
		summary: "round half away from zero, to n decimal places (default 0)"},
	{name: "roundeven", sig: "roundeven(x, n?)", section: SecElementary, min: 1, max: 2, vec: true, fn: rounder(modeHalfEven),
		summary: "round half to even (banker's rounding), to n decimal places"},
	{name: "trunc", sig: "trunc(x, n?)", section: SecElementary, min: 1, max: 2, vec: true, fn: rounder(modeTrunc),
		summary: "round toward zero, to n decimal places (default 0)"},
	{name: "mod", sig: "mod(a, b)", section: SecElementary, min: 2, max: 2, vec: true, fn: fnMod,
		summary: "floor modulo, the sign of b (same as a % b)"},
	{name: "rem", sig: "rem(a, b)", section: SecElementary, min: 2, max: 2, vec: true, fn: fnRem,
		summary: "truncated remainder, the sign of a"},
	{name: "min", sig: "min(a, b, ...) or min(list)", section: SecElementary, min: 1, max: -1, fn: extremum(-1),
		summary: "smallest of the arguments or of a list"},
	{name: "max", sig: "max(a, b, ...) or max(list)", section: SecElementary, min: 1, max: -1, fn: extremum(1),
		summary: "largest of the arguments or of a list"},
	{name: "clamp", sig: "clamp(x, lo, hi)", section: SecElementary, min: 3, max: 3, vec: true, fn: fnClamp,
		summary: "x limited to the interval [lo, hi]"},
}

func fnAbs(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	if x.exact() {
		return exactNum(new(big.Rat).Abs(x.r)), nil
	}
	return floatNum(math.Abs(x.f)), nil
}

func fnSign(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	if x.exact() {
		return exactInt64(int64(x.r.Sign())), nil
	}
	return floatNum(float64(x.sign())), nil
}

func fnSqrt(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	if x.sign() < 0 {
		return nil, errf(codeDomain, a.pos, "sqrt(%s) is not a real number", fmtArg(x)).
			withHint(realArgHint("sqrt", x))
	}
	if x.exact() {
		n, nok := exactSqrt(x.r.Num())
		d, dok := exactSqrt(x.r.Denom())
		if nok && dok {
			return exactNum(new(big.Rat).SetFrac(n, d)), nil
		}
	}
	return checkFloat(math.Sqrt(x.float()), a.pos, "sqrt")
}

func exactSqrt(v *big.Int) (*big.Int, bool) {
	s := new(big.Int).Sqrt(v)
	return s, new(big.Int).Mul(s, s).Cmp(v) == 0
}

// positive refuses x <= 0 for a logarithm; float log would return -Inf or
// NaN, and the evaluator never hands either back.
func positive(a args, x num) *Error {
	if x.sign() <= 0 {
		e := errf(codeDomain, a.pos, "%s(%s) is undefined", a.name, fmtArg(x))
		if x.sign() == 0 {
			e.Hint = "the logarithm of 0 is minus infinity"
		} else {
			e.Hint = "the logarithm of a negative number is not real; " + realArgHint(a.name, x)
		}
		return e
	}
	return nil
}

// logOf is a fixed-base logarithm: f on the float value when it is in
// float64 range (so log10(1000) is exactly 3), else through ln for an exact
// number too large for a float.
func logOf(f func(float64) float64, base float64) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		x, err := a.num(0)
		if err != nil {
			return nil, err
		}
		if err := positive(a, x); err != nil {
			return nil, err
		}
		if v := x.float(); !math.IsInf(v, 0) && v > 0 {
			return checkFloat(f(v), a.pos, a.name)
		}
		lx, err := ln(x, a.pos)
		if err != nil {
			return nil, err
		}
		return checkFloat(lx/math.Log(base), a.pos, a.name)
	}
}

func fnLog(e *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	if err := positive(a, x); err != nil {
		return nil, err
	}
	lx, err := ln(x, a.pos)
	if err != nil {
		return nil, err
	}
	if len(a.v) == 1 {
		return checkFloat(lx, a.pos, "log")
	}
	b, err := a.num(1)
	if err != nil {
		return nil, err
	}
	if b.sign() <= 0 || cmpNum(b, exactInt64(1)) == 0 {
		return nil, errf(codeDomain, a.pos, "log base %s is not allowed", fmtArg(b)).withHint("the base must be positive and not 1")
	}
	lb, err := ln(b, a.pos)
	if err != nil {
		return nil, err
	}
	return checkFloat(lx/lb, a.pos, "log")
}

// ln is the natural log of a positive number, exact numbers past float64
// range included: ln(n/d) = ln n - ln d, each via its bit length.
func ln(x num, pos int) (float64, *Error) {
	if f := x.float(); !math.IsInf(f, 0) && f > 0 {
		return math.Log(f), nil
	}
	if x.exact() {
		return lnInt(x.r.Num()) - lnInt(x.r.Denom()), nil
	}
	return 0, errf(codeDomain, pos, "logarithm of %s is undefined", fmtArg(x))
}

func lnInt(v *big.Int) float64 {
	n := v.BitLen()
	if n <= 1000 {
		f, _ := new(big.Float).SetInt(v).Float64()
		return math.Log(f)
	}
	shift := uint(n - 64)
	top, _ := new(big.Float).SetInt(new(big.Int).Rsh(v, shift)).Float64()
	return math.Log(top) + float64(shift)*math.Ln2
}

func fnLog1p(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	if cmpNum(x, exactInt64(-1)) <= 0 {
		return nil, errf(codeDomain, a.pos, "log1p(%s) is undefined", fmtArg(x)).withHint("log1p needs x > -1")
	}
	return checkFloat(math.Log1p(x.float()), a.pos, "log1p")
}

func fnPow(e *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	y, err := a.num(1)
	if err != nil {
		return nil, err
	}
	return e.pow(x, y, a.pos)
}

func fnHypot(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	y, err := a.num(1)
	if err != nil {
		return nil, err
	}
	return checkFloat(math.Hypot(x.float(), y.float()), a.pos, "hypot")
}

type roundMode int

const (
	modeFloor roundMode = iota
	modeCeil
	modeTrunc
	modeHalfAway
	modeHalfEven
)

// rounder rounds to n decimal places (n may be negative: round(1234, -2) is
// 1200). Exact input stays exact. A float is rounded through its exact
// binary value, so round(2.675, 2) gives 2.67 — the float 2.675 is really
// 2.67499999... — rather than an artefact of scaling by 100 in floating
// point.
func rounder(mode roundMode) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		x, err := a.num(0)
		if err != nil {
			return nil, err
		}
		places := int64(0)
		if len(a.v) == 2 {
			if places, err = a.small(1, maxRoundPlaces); err != nil {
				return nil, err
			}
		}
		scale := pow10Rat(int(places))
		scaled := new(big.Rat).Mul(x.rat(), scale)
		r := new(big.Rat).SetFrac(roundInt(scaled, mode), big.NewInt(1))
		r.Quo(r, scale)
		if x.exact() {
			return e.exactResult(r, a.pos)
		}
		f, _ := r.Float64()
		return checkFloat(f, a.pos, a.name)
	}
}

func roundInt(r *big.Rat, mode roundMode) *big.Int {
	switch mode {
	case modeFloor:
		return ratFloor(r)
	case modeCeil:
		neg := new(big.Rat).Neg(r)
		return new(big.Int).Neg(ratFloor(neg))
	case modeTrunc:
		return new(big.Int).Quo(r.Num(), r.Denom())
	case modeHalfAway:
		return roundHalfAway(r)
	}
	fl := ratFloor(r)
	diff := new(big.Rat).Sub(r, new(big.Rat).SetInt(fl))
	switch diff.Cmp(big.NewRat(1, 2)) {
	case -1:
		return fl
	case 1:
		return fl.Add(fl, big.NewInt(1))
	}
	if fl.Bit(0) == 0 {
		return fl
	}
	return fl.Add(fl, big.NewInt(1))
}

func fnMod(e *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	y, err := a.num(1)
	if err != nil {
		return nil, err
	}
	return e.numOp("%", x, y, a.pos)
}

func fnRem(e *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	y, err := a.num(1)
	if err != nil {
		return nil, err
	}
	if y.isZero() {
		return nil, errf(codeDivZero, a.pos, "division by zero in rem")
	}
	if x.exact() && y.exact() {
		q := new(big.Rat).Quo(x.r, y.r)
		t := new(big.Rat).SetInt(new(big.Int).Quo(q.Num(), q.Denom()))
		return e.exactResult(new(big.Rat).Sub(x.r, t.Mul(t, y.r)), a.pos)
	}
	return checkFloat(math.Mod(x.float(), y.float()), a.pos, "rem")
}

// extremum is min (dir -1) or max (dir 1) over the arguments, or over the
// single list argument.
func extremum(dir int) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		items := a.v
		if len(a.v) == 1 {
			if l, ok := a.v[0].([]val); ok {
				items = l
			}
		}
		if len(items) == 0 {
			return nil, errf(codeDomain, a.pos, "%s of an empty list is undefined", a.name)
		}
		if err := e.charge(len(items), a.pos); err != nil {
			return nil, err
		}
		var best num
		for i, it := range items {
			n, ok := it.(num)
			if !ok {
				return nil, errf(codeType, a.pos, "%s needs numbers, got a %s", a.name, typeName(it))
			}
			if i == 0 || cmpNum(n, best)*dir > 0 {
				best = n
			}
		}
		return best, nil
	}
}

func fnClamp(_ *evaluator, a args) (val, *Error) {
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	lo, err := a.num(1)
	if err != nil {
		return nil, err
	}
	hi, err := a.num(2)
	if err != nil {
		return nil, err
	}
	if cmpNum(lo, hi) > 0 {
		return nil, errf(codeDomain, a.pos, "clamp: lo %s is above hi %s", fmtArg(lo), fmtArg(hi))
	}
	switch {
	case cmpNum(x, lo) < 0:
		return lo, nil
	case cmpNum(x, hi) > 0:
		return hi, nil
	}
	return x, nil
}
