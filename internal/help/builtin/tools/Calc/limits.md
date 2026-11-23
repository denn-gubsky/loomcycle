---
name: Calc/limits
kind: section
description: "Calc limits — the fixed bounds on one call (expression and vars size, steps, time, exact-number size, list size, matrix size, table size, nesting, digits, output) and what to do when you reach each."
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
- **Tables: 100,000 cells** (`list_too_long`), rows times columns; a table
  with no rows counts as one row. A `join` or `pivot` is refused from its
  size before it is built, and every table built costs a step per cell. A
  table inside a list counts its cells toward the list bound.
- **Data by reference** (`from`, `to`): 16 sources, 16 MB read and 16 MB
  written, and 1,000,000 cells read per call; a table read may pass 100,000
  cells, a table built from it may not (`Calc/data`).
- **Matrices: 250,000 elements** (`too_large`) in a matrix a function reads
  or builds, 500 × 500 at most for a square one. The matrix product, the
  solvers and the decompositions are refused before any work when they would
  cost more than a 500 × 500 matrix (rows × columns × the smaller of the two,
  125,000,000). Reading or writing a matrix costs a step per element. Large
  matrix operations from every caller share a pool of one slot per processor
  core; waiting for a slot counts against the 2 seconds (`time_limit`).
- **Numeric methods** (`no_convergence`): `integrate` evaluates its function
  at most 15,000 times, `deriv` 20, `root` and `minimize` 500 each; every
  evaluation costs its steps as well.
- **Nesting: 200 levels** of brackets, lists and lambdas, and **64 levels**
  of one function calling another (`too_deep`). Name the intermediate values.
- **Digits: 1 to 1,000** (`bad_request`). A float shows at most about 17
  significant digits whatever you ask.
- **Output: 64 KB** of rendered text. Past it, `text` is cut, the values that
  did not fit are `null` and `truncated` is `true`. Ask for less — a `sum`,
  the first few elements, or `len` — rather than the whole list.

## List operators that are refused

`/` between two lists is refused (`type`), and so are `*` between two plain
vectors, a number divided by a list (`1 / v`) and a power with a list on
either side (`v ^ 2`, `2 ^ v`): write `.*`, `./` and `.^` for element by
element. Element-wise operands must have the same length, or the same shape
for matrices (`shape`). `+` and `-`, and `*` or `/` by a number, work on lists
directly; `*` between two matrices is the matrix product (`Calc/matrices`).

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
