---
name: Calc/complex
kind: section
description: "Calc complex numbers — the imaginary literal 2i, complex(a, b), real and imaginary parts, conjugate, modulus and angle, complex arithmetic and the elementary functions on complex arguments, the roots of a polynomial (polyroots), and how complex results are written and returned."
---
A complex number is written with an **`i` suffix on a number**: `2i`,
`1.5i`, `3e2i`, so `3 + 2i` and `3 - 2i`. The `1` is needed: `1i`, never a
bare `i`. **`i` alone is an ordinary name**: `i = 4; 2*i` is `8`, and a lambda
may call its parameter `i`. `complex(a, b)` builds `a + b·i` from two real
numbers.

A complex number is two 64-bit floats, so it is **never exact**; `type` is
`complex`. `text` writes it `3+2i`, `3-2i`, `2i`, with the imaginary part
always shown (`-1+0i`); `result` holds it as `{"re": 3, "im": 2}`. A list may
mix real and complex numbers.

## Real in, real out

A real argument keeps its real meaning: `sqrt(-1)` and `log(-2)` are still
refused (`domain`). Ask for the complex answer by passing a complex
argument: `sqrt(-1 + 0i)` is `1i`. An operation on a complex value gives a
complex value, even when its imaginary part comes out 0: `1i * 1i` is
`-1+0i`. `re(z)` turns it back into a real number.

## What takes complex numbers

- `+ - * / ^` and `.* ./ .^`. An integer power is computed by repeated
  multiplication, so `(1+2i)^2` is exactly `-3+4i`; any other power is the
  principal value.
- `sqrt exp log log10 log2`, `sin cos tan asin acos atan`,
  `sinh cosh tanh asinh acosh atanh` and `pow`: the principal value, as in
  Python's `cmath`. `log(z, base)` takes a complex `z` or base.
- `abs(z)` is the modulus; `round`, `floor`, `ceil`, `trunc` and
  `roundeven` round each part.
- `==` and `!=` compare values: `2 + 0i == 2` is `true`.
- `sum`, `product`, `unique`, `count`, indexing, transpose `'`, and lists or
  matrices combined element by element.
- The matrix product `*` and the power `A^n` (`n` ≥ 0) of matrices with
  complex elements, and `ctranspose(A)`, the conjugate transpose (`'` does
  not conjugate). See `Calc/matrices`.
- `polyval(p, x)` with complex coefficients or a complex `x`.

## What refuses them

Complex numbers have **no order**: `<`, `>`, `sort`, `min`, `max`,
`median` and every statistics function refuse one (`type`); compare
`abs(z)` or `re(z)`, or sort with a key: `sort(v, z -> abs(z))`. `%` refuses
one too. The other matrix functions (`inv`, `solve`, `det`, the
decompositions) and a negative power `A^-n` work in real numbers and refuse
a complex element. `eig` of a real matrix does return complex eigenvalues
(`Calc/matrices`).

## Functions

### complex(a, b)
The complex number `a + b·i`; `a` and `b` are real numbers.

Example: `complex(3, -2)` gives `3-2i`.

### re(z)
The real part. A real number is its own real part and keeps its exactness.

Example: `re(3-2i)` gives `3`.

### im(z)
The imaginary part; an exact `0` for a real number.

Example: `im(3-2i)` gives `-2`.

### conj(z)
The complex conjugate: the imaginary part negated. `z * conj(z)` is
`abs(z)^2`.

Example: `conj(3-2i)` gives `3+2i`.

### arg(z)
The angle of `z` in radians, in (−π, π]: `atan2(im(z), re(z))`. `0` for a
positive real number, `pi` for a negative one.

Example: `round(arg(1+1i), 10)` gives `0.7853981634`.

### polyroots(p)
The roots of the polynomial with real coefficients `p`, **highest power
first** as in `polyval` (`[1, -3, 2]` is x² − 3x + 2), as the eigenvalues of
its companion matrix (NumPy's `roots`). A degree-n polynomial has n roots,
repeated roots repeated. A root whose imaginary part is 0 is a real number,
the others complex; they are listed by real part, then imaginary part.
Leading zeros are dropped and trailing ones are roots at 0. Floats: round
for display, as the roots carry rounding in the last digits. Degree 500 at
most; a large one waits for a matrix slot like any matrix routine
(`Calc/limits`).

Example: `round(polyroots([1, 0, 1]), 12)` gives `[-1i, 1i]`.

Example: `round(polyroots([1, -6, 11, -6]), 10)` gives `[1, 2, 3]`.

## Examples

The roots of 3.2x² + 2x + 1:

```json
{"expr": "round(polyroots([3.2, 2, 1]), 6)"}
```

Result: [-0.3125-0.463512i, -0.3125+0.463512i]

Euler's identity, rounded:

```json
{"expr": "round(exp(pi * 1i) + 1, 12)"}
```

Result: 0i

A matrix power in complex numbers:

```json
{"expr": "R = [[1i, 0], [0, -1i]]; R^2 * [1, 2]"}
```

Result: [-1+0i, -2+0i]

An impedance in ohms, 50 Ω with an inductor of 0.1 H at 60 Hz, as modulus and
phase in degrees:

```json
{"expr": "z = 50 + 2*pi*60*0.1*1i; [round(abs(z), 3), round(deg(arg(z)), 2)]"}
```

Result: [62.62, 37.02]
