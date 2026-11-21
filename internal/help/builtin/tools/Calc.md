---
name: Calc
description: "Calc tool — evaluate math exactly: arithmetic, rounding, powers, logarithms, trigonometry, combinatorics, primes, lists, unit conversion, statistics and finance; the call, the language in brief, exact versus approximate results, and where each group of functions is documented."
---
The `Calc` tool evaluates a math expression and returns the answer, exactly
where it can be exact. Decimals are exact fractions, so `0.1 + 0.2` is `0.3`
and a 30-digit integer keeps all 30 digits.

## When to use it — and when not

- **Use it for every number you compute**: totals, tax, discounts,
  percentages, averages, interest, unit prices, unit conversions,
  counting and probability, primes. Do not work a number out in
  your head and do not estimate: a model's mental arithmetic is wrong often
  enough to matter, and this call is cheap.
- **Use it to check** a figure you were given before you rely on it.
- **There are no date functions.** Write the formula from the operators.
- **Not for algebra.** It evaluates numbers; it does not solve equations or
  simplify formulas.

## The call

- `expr` (required) — one or more statements separated by `;` or newlines.
  The last one's value is the result.
- `vars` — names bound before evaluation: numbers, strings, booleans or
  lists. A JSON number is exact (`0.1` is 1/10). A string holding a number
  (`"19.99"`, `"1_000"`) is that number; any other string stays a string.
- `digits` — significant digits for an approximate result or a fraction that
  does not terminate; 1 to 1000, default 15.
- `seed` — an integer that makes the random functions repeatable.

No other argument is accepted, and there is no `op`.

## The result

```json result
{"result": 4499.96, "type": "number", "exact": true,
 "assigned": {"subtotal": 3749.97, "tax": 749.99},
 "text": "4499.96"}
```

- `result` — the value as JSON; `text` — the same, written for reading.
  Quote `text` when you report the number.
- `type` — `number`, `string`, `boolean`, `list` or `function`.
- `exact` — `true` when every number in the result is exact.
- `fraction` — for an exact result whose decimals do not end, the exact
  value: `"1/3"`.
- `assigned` — every name the expression assigned or defined.
- `shape` — for a list: `[n]`, or `[n, m]` for n lists of m.
- `truncated` — `true` when the output passed 64 KB and was cut.

A refused call returns `{"error", "detail", "hint", "pos"}`; `Calc/errors`
lists every code.

## The language in brief

- Numbers: `42`, `-3.5`, `2.5e-3`, `1_000_000`, `0xff`. Constants `pi`, `e`,
  `tau`, `phi`. Booleans `true`, `false`. Strings `"text"`. Lists
  `[1, 2, 3]`.
- Arithmetic: `+ - * /`. Power is `^` (`2^10`), never `**`. `%` is
  **modulo** (`7 % 3` is `1`), never percent: 15% of x is `x * 15 / 100`.
- `n!` is the factorial. There is no implicit multiplication: `2*x`, not `2x`.
- Comparison `== != < <= > >=`, logic `and or not`. `=` assigns, `==`
  compares.
- `name = expr` assigns; `f(x) = expr` defines a function; `x -> expr` is a
  lambda. No recursion.
- `a:b` and `a:step:b` are ranges, both ends included: `1:5`.
- **Indexing starts at 1**: `v[1]` is the first element.
- On lists, `+` and `-` work element by element and a number applies to
  every element; `v * 2` and `v / 2` scale a list. Between two lists, and for
  `1 / v` or `v ^ 2`, write `.*`, `./`, `.^`; `*` between lists is refused.
- `if(cond, a, b)` chooses a value.
- `#` starts a comment.

`Calc/language` has the full grammar and a list of common mistakes.

## Exact or approximate

Literals, `+ - * /`, integer powers, rounding and the integer functions keep
a result exact. `sqrt` (unless the root is exact), fractional powers, `exp`,
`log`, trigonometry, the constants and the random functions give a 64-bit
float, about 16 significant digits, and everything computed from one stays
approximate. `exact` in the result tells you which you got. For money, keep
it exact and round once with `round(x, 2)`. Details in `Calc/numbers`.

## Sections

Read the section for the functions you need, as `Context op=help
topic=Calc/<section>`:

- `Calc/language` — statements, operators and precedence, functions and
  lambdas, ranges, indexing, `if`, common mistakes.
- `Calc/numbers` — exact and approximate numbers, `digits`, `fraction`,
  rounding, money.
- `Calc/elementary` — rounding, `abs`, `min`/`max`, roots, powers,
  logarithms, trigonometry (radians), gamma, erf, Bessel.
- `Calc/integers` — factorial, binomial, Fibonacci, gcd/lcm, primes,
  factoring, modular arithmetic, bits and number bases.
- `Calc/sequences` — `range`, `sum`, `map`/`filter`/`reduce`, `sort`,
  `unique`, `count`, `zip`, and the random functions with `seed`.
- `Calc/units` — `convert` between units of length, mass, time,
  temperature, volume, energy, data size and more; prefixes and compounds.
- `Calc/statistics` — mean, median, mode, variance and standard deviation
  (sample or population), quantiles, ranks, correlation, regression.
- `Calc/finance` — loan payments, future and present value, NPV, IRR,
  compound growth, percentages; the sign convention.
- `Calc/errors` — every error code and what to change.
- `Calc/limits` — the bounds on one call.

## Examples

An order total with tax rounded to cents:

```json
{"expr": "subtotal = 1249.99 * 3; tax = round(subtotal * 0.2, 2); subtotal + tax"}
```

Result: 4499.96

Data from the conversation, passed in `vars`:

```json
{"expr": "round(sum(hours .* rates), 2)", "vars": {"hours": [7.5, 8, 6.25], "rates": [42, 42, 55.5]}}
```

Result: 997.88

A percentage change:

```json
{"expr": "round((new - old) / old * 100, 1)", "vars": {"old": 1840, "new": 2125}}
```

Result: 15.5

A fraction that does not terminate, with more digits:

```json
{"expr": "1/7", "digits": 30}
```

Result: 0.142857142857142857142857142857

Exact integers of any size:

```json
{"expr": "2^100"}
```

Result: 1267650600228229401496703205376
