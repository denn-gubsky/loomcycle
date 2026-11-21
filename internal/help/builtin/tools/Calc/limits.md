---
name: Calc/limits
kind: section
description: "Calc limits — the fixed bounds on one call (expression and vars size, steps, time, exact-number size, list size, nesting, digits, output) and what to do when you reach each."
---
Every call is bounded, so one expression cannot run away. The bounds are
fixed; no setting raises them. Reaching one refuses the call with the code in
brackets (see `Calc/errors`), except the output bound, which cuts the result.

## The bounds

- **Expression: 16 KB** (`expr_too_long`). Keep the formula in `expr` and the
  data in `vars`.
- **Vars: 1 MB** in total, names included (`vars_too_large`); a var nests at
  most 32 levels (`bad_var`). Split the data over several calls.
- **Steps: 1,000,000** per call (`step_limit`). A step is one operation, one
  function call, or one list element touched by `map`, `filter`, `reduce`,
  `sort`, a range or an element-wise operator; very large exact numbers cost
  extra. Prefer a closed form: `n*(n+1)/2` costs a handful of steps where
  `sum(1:n)` costs `n`.
- **Time: 2 seconds** per call (`time_limit`). Same remedies as steps.
- **Exact numbers: 16,384 bits**, numerator and denominator together — about
  4,900 decimal digits (`too_large`). A power, factorial or Fibonacci number
  over it is refused before it is computed. A float result over about
  1.8e308 is `overflow`.
- **Function caps** (`too_large`): `isprime` takes `n` below 2^1024,
  `nextprime` below 2^1023, `factor` and `divisors` `|n|` below 2^64, and
  `modpow` an exponent and a modulus of at most 4,096 bits. `round` and its
  siblings take at most 1,000 decimal places.
- **Lists: 1,000,000 elements** (`list_too_long`) — and that is the **total**
  for one value: a list of 1,000 lists of 1,001 elements is over it. A range
  longer than the limit is refused before it is built.
- **Nesting: 200 levels** of brackets, lists and lambdas, and **64 levels**
  of one function calling another (`too_deep`). Name the intermediate values.
- **Digits: 1 to 1,000** (`bad_request`). A float shows at most about 17
  significant digits whatever you ask.
- **Output: 64 KB** of rendered text. Past it, `text` is cut, the values that
  did not fit are `null` and `truncated` is `true`. Ask for less — a `sum`,
  the first few elements, or `len` — rather than the whole list.

## List operators that are refused

`*` and `/` between two lists are refused (`type`), and so are a number
divided by a list (`1 / v`) and a power with a list on either side (`v ^ 2`,
`2 ^ v`): write `.*`, `./` and `.^` for element by element. Element-wise
operands must have the same length (`shape`). `+` and `-`, and `*` or `/` by a
number, work on lists directly.

## Examples

A sum written as a closed form stays far inside the step limit:

```json
{"expr": "n = 10^9; n * (n + 1) / 2"}
```

Result: 500000000500000000

A range over the list limit is refused before it is built:

```json
{"expr": "sum(1:2000000)"}
```

Result: error list_too_long

The same sum by its closed form:

```json
{"expr": "n = 2000000; n * (n + 1) / 2"}
```

Result: 2000001000000
