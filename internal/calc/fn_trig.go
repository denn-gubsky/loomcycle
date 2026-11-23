package calc

import "math"

// Angles are radians; deg and rad convert.
var trigFuncs = []builtin{
	{name: "sin", sig: "sin(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Sin), summary: "sine of x radians"},
	{name: "cos", sig: "cos(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Cos), summary: "cosine of x radians"},
	{name: "tan", sig: "tan(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Tan), summary: "tangent of x radians"},
	{name: "asin", sig: "asin(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: inRange(math.Asin, -1, 1, true), summary: "arcsine in radians, x in [-1, 1]"},
	{name: "acos", sig: "acos(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: inRange(math.Acos, -1, 1, true), summary: "arccosine in radians, x in [-1, 1]"},
	{name: "atan", sig: "atan(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Atan), summary: "arctangent in radians"},
	{name: "atan2", sig: "atan2(y, x)", section: SecTrig, min: 2, max: 2, vec: true, fn: fnAtan2, summary: "angle of the point (x, y) in radians, in (-pi, pi]"},
	{name: "sec", sig: "sec(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: reciprocal(math.Cos), summary: "secant, 1/cos(x)"},
	{name: "csc", sig: "csc(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: reciprocal(math.Sin), summary: "cosecant, 1/sin(x)"},
	{name: "cot", sig: "cot(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: reciprocal(math.Tan), summary: "cotangent, 1/tan(x)"},
	{name: "sinh", sig: "sinh(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Sinh), summary: "hyperbolic sine"},
	{name: "cosh", sig: "cosh(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Cosh), summary: "hyperbolic cosine"},
	{name: "tanh", sig: "tanh(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Tanh), summary: "hyperbolic tangent"},
	{name: "asinh", sig: "asinh(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(math.Asinh), summary: "inverse hyperbolic sine"},
	{name: "acosh", sig: "acosh(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: inRange(math.Acosh, 1, math.Inf(1), true), summary: "inverse hyperbolic cosine, x >= 1"},
	{name: "atanh", sig: "atanh(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: inRange(math.Atanh, -1, 1, false), summary: "inverse hyperbolic tangent, -1 < x < 1"},
	{name: "deg", sig: "deg(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(func(x float64) float64 { return x * 180 / math.Pi }), summary: "radians to degrees"},
	{name: "rad", sig: "rad(x)", section: SecTrig, min: 1, max: 1, vec: true, fn: float1(func(x float64) float64 { return x * math.Pi / 180 }), summary: "degrees to radians"},
}

// inRange refuses x outside [lo, hi] (or (lo, hi) when closed is false)
// with the interval in the message, instead of a bare NaN.
func inRange(f func(float64) float64, lo, hi float64, closed bool) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		x, err := a.num(0)
		if err != nil {
			return nil, err
		}
		v := x.float()
		out := v < lo || v > hi
		if !closed {
			out = v <= lo || v >= hi
		}
		if out {
			open, shut := "[", "]"
			if !closed {
				open, shut = "(", ")"
			}
			return nil, errf(codeDomain, a.pos, "%s(%s) is undefined: x must be in %s%g, %g%s", a.name, fmtArg(x), open, lo, hi, shut).
				withHint(realArgHint(a.name, x))
		}
		return checkFloat(f(v), a.pos, a.name)
	}
}

func reciprocal(f func(float64) float64) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		x, err := a.num(0)
		if err != nil {
			return nil, err
		}
		d := f(x.float())
		if d == 0 {
			return nil, errf(codeDivZero, a.pos, "%s(%s) divides by zero", a.name, fmtArg(x))
		}
		return checkFloat(1/d, a.pos, a.name)
	}
}

func fnAtan2(_ *evaluator, a args) (val, *Error) {
	y, err := a.num(0)
	if err != nil {
		return nil, err
	}
	x, err := a.num(1)
	if err != nil {
		return nil, err
	}
	return checkFloat(math.Atan2(y.float(), x.float()), a.pos, "atan2")
}
