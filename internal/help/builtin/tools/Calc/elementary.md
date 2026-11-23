---
name: Calc/elementary
kind: section
description: "Calc elementary functions — rounding, absolute value, min and max, roots, powers, exponentials and logarithms, trigonometric functions in radians, and the special functions (gamma, erf, Bessel)."
---
The everyday functions. Each one given a list as its first argument applies
to every element: `round(prices * 1.2, 2)` rounds each price. An argument
marked `?` is optional. "Exact" means an exact argument gives an exact result
(see `Calc/numbers`); every other function here returns a float.

## Rounding and parts

### round(x, n?)
`x` rounded half away from zero to `n` decimal places (default 0; `n` may be
negative, `round(1234, -2)` is 1200). Exact.

Example: `round(19.995, 2)` gives `20`.

### roundeven(x, n?)
`x` rounded half to even (banker's rounding) to `n` decimal places. Exact.

Example: `roundeven(0.125, 2)` gives `0.12`.

### floor(x, n?)
`x` rounded down to `n` decimal places (default 0). Exact.

Example: `floor(-2.5)` gives `-3`.

### ceil(x, n?)
`x` rounded up to `n` decimal places (default 0). Exact.

Example: `ceil(52 / 12)` gives `5`.

### trunc(x, n?)
`x` rounded toward zero to `n` decimal places (default 0). Exact.

Example: `trunc(2.999, 2)` gives `2.99`.

### abs(x)
The absolute value. Exact.

Example: `abs(-3/4)` gives `0.75`.

### sign(x)
`-1`, `0` or `1`. Exact.

Example: `sign(-4)` gives `-1`.

### clamp(x, lo, hi)
`x` limited to the interval [`lo`, `hi`]. Exact.

Example: `clamp(115, 0, 100)` gives `100`.

### mod(a, b)
Floor modulo: the result has the sign of `b`, as `a % b`. Refused when `b`
is 0. Exact.

Example: `mod(-7, 3)` gives `2`.

### rem(a, b)
Truncated remainder: the result has the sign of `a`. Exact.

Example: `rem(-7, 3)` gives `-1`.

### min(a, b, ...) or min(list)
The smallest of the arguments, or of one list. Refused for an empty list.
Exact.

Example: `min([3.5, 1.25, 2])` gives `1.25`.

### max(a, b, ...) or max(list)
The largest of the arguments, or of one list. Refused for an empty list.
Exact.

Example: `max(3, 9, 2)` gives `9`.

## Powers, roots and logarithms

### pow(x, y)
`x^y`. Exact for an exact `x` and an integer `y`; refused (`domain`) for a
negative `x` with a fractional `y`.

Example: `pow(2/3, 3)` gives `0.296296296296296`.

### sqrt(x)
The square root. Exact when `x` is the square of a fraction; refused for a
negative `x`, whose complex root is `sqrt(x + 0i)` (`Calc/complex`).

Example: `sqrt(16/9)` gives `1.33333333333333`.

### cbrt(x)
The cube root, real for a negative `x`. Use it rather than `x^(1/3)`, which
is refused for a negative `x`.

Example: `cbrt(-27)` gives `-3`.

### hypot(x, y)
`sqrt(x^2 + y^2)` without overflow: a distance.

Example: `hypot(3, 4)` gives `5`.

### exp(x)
`e` to the power `x`. Refused (`overflow`) above about 709.

Example: `exp(0)` gives `1`.

### expm1(x)
`exp(x) - 1`, accurate when `x` is near 0.

Example: `expm1(1e-10)` gives `1.00000000005e-10`.

### log(x, base?)
The natural logarithm, or the logarithm to `base`. `x` must be positive; a
complex argument gives the complex logarithm (`Calc/complex`).
`ln` is not a name here: write `log`.

Example: `log(1024, 2)` gives `10`.

### log10(x)
The base-10 logarithm.

Example: `log10(1000)` gives `3`.

### log2(x)
The base-2 logarithm.

Example: `log2(4096)` gives `12`.

### log1p(x)
`log(1 + x)`, accurate when `x` is near 0.

Example: `log1p(1e-10)` gives `9.9999999995e-11`.

## Trigonometric functions

Angles are in **radians**. Convert degrees with `rad` and back with `deg`.
`arcsin`, `arccos`, `arctan` are not names here: write `asin`, `acos`, `atan`.

### rad(x)
Degrees to radians.

Example: `sin(rad(30))` gives `0.5`.

### deg(x)
Radians to degrees.

Example: `deg(atan2(1, 1))` gives `45`.

### sin(x)
The sine of `x` radians.

Example: `sin(pi / 6)` gives `0.5`.

### cos(x)
The cosine of `x` radians.

Example: `cos(pi)` gives `-1`.

### tan(x)
The tangent of `x` radians.

Example: `tan(pi / 4)` gives `1`.

### sec(x)
The secant, `1/cos(x)`.

Example: `sec(0)` gives `1`.

### csc(x)
The cosecant, `1/sin(x)`.

Example: `csc(pi / 2)` gives `1`.

### cot(x)
The cotangent, `1/tan(x)`.

Example: `cot(pi / 4)` gives `1`.

### asin(x)
The arcsine in radians; `x` must be in [-1, 1].

Example: `deg(asin(0.5))` gives `30`.

### acos(x)
The arccosine in radians; `x` must be in [-1, 1].

Example: `acos(2)` gives `error domain`.

### atan(x)
The arctangent in radians.

Example: `deg(atan(1))` gives `45`.

### atan2(y, x)
The angle of the point (`x`, `y`) in radians, in (-pi, pi]. Note the order:
`y` first.

Example: `deg(atan2(-1, -1))` gives `-135`.

### sinh(x)
The hyperbolic sine.

Example: `sinh(0)` gives `0`.

### cosh(x)
The hyperbolic cosine.

Example: `cosh(0)` gives `1`.

### tanh(x)
The hyperbolic tangent.

Example: `tanh(0)` gives `0`.

### asinh(x)
The inverse hyperbolic sine.

Example: `asinh(0)` gives `0`.

### acosh(x)
The inverse hyperbolic cosine; `x` must be at least 1.

Example: `acosh(1)` gives `0`.

### atanh(x)
The inverse hyperbolic tangent; -1 < `x` < 1.

Example: `atanh(0)` gives `0`.

## Special functions

### gamma(x)
The gamma function; `gamma(n)` is `(n-1)!`. Use `factorial` for an exact
integer result.

Example: `gamma(0.5)^2` gives `3.14159265358979`.

### lgamma(x)
`log |gamma(x)|`, for arguments where `gamma` overflows.

Example: `lgamma(100)` gives `359.134205369575`.

### beta(a, b)
The beta function `gamma(a) gamma(b) / gamma(a + b)`.

Example: `beta(2, 3)` gives `0.0833333333333333`.

### erf(x)
The error function.

Example: `erf(1)` gives `0.842700792949715`.

### erfc(x)
`1 - erf(x)`, accurate for a large `x`.

Example: `erfc(1)` gives `0.157299207050285`.

### erfinv(x)
The inverse of `erf`; -1 < `x` < 1.

Example: `erfinv(0.5)` gives `0.47693627620447`.

### erfcinv(x)
The inverse of `erfc`; 0 < `x` < 2.

Example: `erfcinv(0.5)` gives `0.47693627620447`.

### j0(x)
The Bessel function of the first kind, order 0.

Example: `j0(0)` gives `1`.

### j1(x)
The Bessel function of the first kind, order 1.

Example: `j1(1)` gives `0.440050585744933`.

### jn(n, x)
The Bessel function of the first kind, integer order `n`.

Example: `jn(2, 1)` gives `0.1149034849319`.

### y0(x)
The Bessel function of the second kind, order 0; `x` > 0.

Example: `y0(1)` gives `0.088256964215677`.

### y1(x)
The Bessel function of the second kind, order 1; `x` > 0.

Example: `y1(1)` gives `-0.781212821300289`.

### yn(n, x)
The Bessel function of the second kind, integer order `n`; `x` > 0.

Example: `yn(2, 1)` gives `-1.65068260681625`.

## Examples

Prices with tax, each rounded:

```json
{"expr": "round(prices * 1.2, 2)", "vars": {"prices": [9.99, 24.5, 3.333]}}
```

Result: [11.99, 29.4, 4]

The height of a ramp 4 m long at 15 degrees:

```json
{"expr": "round(4 * sin(rad(15)), 3)"}
```

Result: 1.035

Doubling time at 7% growth a year:

```json
{"expr": "log(2) / log(1.07)"}
```

Result: 10.2447683510587
