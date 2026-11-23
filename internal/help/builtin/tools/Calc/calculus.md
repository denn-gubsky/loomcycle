---
name: Calc/calculus
kind: section
description: "Calc numeric calculus — definite integrals, derivatives at a point, roots of an equation in a bracket, the minimum of a function on an interval, least-squares polynomial fits, evaluating a polynomial, and linear interpolation in a table."
---
`integrate`, `deriv`, `root` and `minimize` take a **function** first: a
lambda (`x -> x^2 - 2`), a function you defined (`f(x) = …` then `f`), or a
built-in by name (`sin`, `exp`). It must take one number and return one
number.

They work in 64-bit floats and give a float: an integral or a root is
accurate to about 10 significant digits, a minimum's position to about 8 (a
function is flat near its minimum). Round the result for display. Every
evaluation of the function costs its steps, and each method evaluates it a
bounded number of times (`Calc/limits`); one that does not settle is refused
with `no_convergence`, never answered with a guess. An error inside the
function (`1/0`, `log(-1)`) is returned as it is.

The polynomial functions write a polynomial as its coefficients, **highest
power first**: `[2, 0, -1]` is 2x² − 1 (NumPy's and MATLAB's order).

## Functions of a function

### integrate(f, a, b)
The integral of `f` from `a` to `b`, by adaptive Gauss-Kronrod quadrature.
The bounds are finite; `b` below `a` gives the negative. The ends are never
evaluated, so an integrable singularity there (`1 / sqrt(x)` from 0) works;
one inside the interval should be split at: integrate each side.

Example: `integrate(x -> x^2, 0, 3)` gives `9`.

### deriv(f, x)
The derivative of `f` at `x`, from central differences refined by Richardson
extrapolation. `f` must be defined on both sides of `x`. At a kink it gives
the mean of the two slopes (`deriv(abs, 0)` is 0); at a jump it is refused.

Example: `deriv(x -> x^3, 2)` gives `12`.

### root(f, a, b)
An `x` between `a` and `b` where `f(x)` is 0, by Brent's method. `f(a)` and
`f(b)` must have **opposite signs** (or one be 0), so the interval brackets a
root; otherwise `domain`. With several roots inside, one of them. To solve
`g(x) = c`, find the root of `x -> g(x) - c`.

Example: `root(x -> x^2 - 2, 0, 2)` gives `1.41421356237309`.

### minimize(f, a, b)
The `x` between `a` and `b` where `f` is smallest, by Brent's method (MATLAB's
`fminbnd`). It finds a **local** minimum: with several dips, narrow the
interval around the one you want. A minimum at an end is returned exactly.
For the smallest value itself, take `f(minimize(f, a, b))`; for a maximum,
minimize `x -> -f(x)`.

Example: `minimize(x -> (x - 2)^2 + 1, 0, 5)` gives `2`.

## Polynomials and tables

### polyfit(x, y, deg)
The coefficients of the degree-`deg` polynomial nearest the points
`(x[i], y[i])` by least squares, highest power first. It needs at least
`deg + 1` points with that many distinct `x`; a fit too badly conditioned
for floats (a high degree over a wide `x` range) is refused, so lower the
degree or centre `x` first. `polyfit(x, y, 1)` is the line `linreg` gives.

Example: `round(polyfit([0, 1, 2, 3], [1, 3, 7, 13], 2), 10)` gives `[1, 1, 1]`.

### polyval(p, x)
The polynomial with coefficients `p` at `x`, or at each element of a list
`x`. Exact on exact input.

Example: `polyval([2, 0, -1], 3)` gives `17`.

### interp(x, xs, ys)
Linear interpolation: the value at `x` on the broken line through the points
`(xs[i], ys[i])`. `xs` must be strictly increasing, and `x` (a number or a
list) within `xs[1]` to the last `xs`; outside, it is refused, since
extrapolating is a different question. Exact on exact input.

Example: `interp(2.5, [1, 2, 3], [10, 20, 40])` gives `30`.

## Examples

The area under the standard normal curve within one standard deviation:

```json
{"expr": "round(integrate(normpdf, -1, 1), 6)"}
```

Result: 0.682689

The interest rate per period at which 36 payments of 300 repay 9,500:

```json
{"expr": "round(root(r -> 300 * (1 - (1 + r)^-36) / r - 9500, 0.0001, 0.1), 6)"}
```

Result: 0.007104

A fitted quadratic, then its value at a new point:

```json
{"expr": "p = polyfit(t, h, 2); round(polyval(p, 2.5), 3)", "vars": {"t": [0, 1, 2, 3, 4], "h": [2.0, 6.9, 9.8, 10.9, 9.8]}}
```

Result: 10.585

A table lookup between rows:

```json
{"expr": "interp(17, [0, 10, 20, 30], [1.0, 1.31, 1.75, 2.29])"}
```

Result: 1.618

An integral that does not exist (1/x near 0) is refused:

```json
{"expr": "integrate(x -> 1 / x, 0, 1)"}
```

Result: error no_convergence
