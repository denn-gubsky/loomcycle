---
name: Calc
description: "Calc tool — evaluate math exactly: arithmetic, rounding, powers, logarithms, trigonometry, combinatorics, primes, lists, unit conversion, statistics, probability distributions, numeric calculus, matrices and linear algebra, complex numbers, finance, dates and tables; the call, the language in brief, exact versus approximate results, and where each group of functions is documented."
---
The `Calc` tool evaluates a math expression and returns the answer, exactly
where it can be exact. Decimals are exact fractions, so `0.1 + 0.2` is `0.3`
and a 30-digit integer keeps all 30 digits.

## When to use it — and when not

- **Use it for every number you compute**: totals, tax, percentages,
  averages, interest, unit conversions, dates, probability, primes. Do not
  work a number out in your head: mental arithmetic is often wrong, and this
  call is cheap.
- **Use it to check** a figure you were given before you rely on it.
- **No symbolic algebra**: for a linear system use `solve(A, b)`, for a root
  of f use `root(f, a, b)`, for a minimum `minimize(f, a, b)`.

## The call

- `expr` (required) — one or more statements separated by `;` or newlines.
  The last one's value is the result.
- `vars` — names bound before evaluation: numbers, strings, booleans,
  lists or tables. A JSON number is exact (`0.1` is 1/10); a string holding
  a number (`"19.99"`) is that number.
- `digits` — significant digits for an approximate or non-terminating
  result; 1 to 1000, default 15.
- `seed` — an integer that makes the random functions repeatable.
- `format` — `json` (default), `markdown` or `csv`: how a table or matrix
  result is written in `text`.
- `from`, `to` — read named tables from, or write the result to, a volume
  file, a Memory key or a Document chunk (`Calc/data`).

No other argument is accepted, and there is no `op`.

## The result

```json result
{"result": 4499.96, "type": "number", "exact": true,
 "assigned": {"subtotal": 3749.97, "tax": 749.99},
 "text": "4499.96"}
```

- `result` — the value as JSON; `text` — the same for reading: quote it
  when you report the number.
- `type` — `number`, `complex`, `string`, `boolean`, `list`, `function`,
  `date`, `datetime`, `table`, `row` or `empty`. A complex result is
  `{"re", "im"}`, a table `{"columns", "rows"}`.
- `exact` — `true` when every number in the result is exact.
- `fraction` — `"1/3"` for a decimal that does not end; `assigned` —
  every name assigned or defined.
- `shape` — `[n]`, `[n, m]` or `[rows, columns]`; `truncated` — the
  output passed 64 KB and was cut.

A refused call returns `{"error", "detail", "hint", "pos"}`; `Calc/errors`
lists every code.

## The language in brief

- Numbers: `42`, `2.5e-3`, `1_000_000`. Constants `pi`, `e`,
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
  `1 / v` or `v ^ 2`, write `.*`, `./`, `.^`. `*` between two matrices is
  the matrix product; between two vectors it is refused.
- A matrix is a list of rows, `[[1, 2], [3, 4]]`; `A'` transposes it,
  `A[i, j]` is an element.
- `if(cond, a, b)` chooses a value; `#` starts a comment.

## Most-used functions

Every argument is required unless marked `?`. A date is a value: wrap a
string, `date("2026-11-23")`.

- `datediff(a, b, unit)` — whole `"days"`, `"weeks"`, `"months"` or
  `"years"` from `a` to `b`; negative when `a` is after `b`.
- `addmonths(d, n)` — `n` months after `d`, clamped to the month's end.
- `workdays(a, b, holidays?)` — Monday-to-Friday days, both ends included.
- `addworkdays(d, n, holidays?)` — the date `n` working days after `d`.
- `isoweek(d)` — the ISO week number; `weekday(d)` is 1 (Monday) to 7.
- `datefmt(d, pattern)` — tokens `YYYY` `YY` `MMMM` `MMM` `MM` `M` `DD` `D`
  `dddd` `ddd` `HH` `mm` `ss`; text goes in `[brackets]`, other letters are
  refused.
- `convert(value, from, to)` — case-sensitive unit symbols: `"kg"` `"lb"`
  `"oz"` `"gal"` (US) `"L"` `"GiB"` `"MB"` `"d"` `"s"`. Never type a
  conversion factor yourself.
- `log(x, base?)` (natural by default), `modpow(b, e, m)`,
  `correlation(x, y)`, `range(a, b, step?)`.

## Names from other tools

`ln` → `log`, `powmod` → `modpow`, `correl` → `correlation`, `seq` →
`range`, `networkdays` → `workdays`, `edate` → `addmonths`, `pnorm` →
`normcdf`, `qnorm` → `norminv`. `datefmt`
refuses `%A`, `%V` and `EEEE`: the weekday name is `dddd`, the week number
`isoweek(d)`. An unknown name's error names the Calc one.

## Exact or approximate

Literals, `+ - * /`, integer powers, rounding and the integer functions keep
a result exact. `sqrt` (unless the root is exact), fractional powers, `exp`,
`log`, trigonometry and the constants give a 64-bit float (about 16
significant digits), and so does everything computed from one; `exact` says
which. For money, round once with `round(x, 2)`. See `Calc/numbers`.

## Sections

Read one as `Context op=help topic=Calc/<section>`; each line names its
main functions:

- `Calc/language` — grammar, operators, ranges, common mistakes.
- `Calc/numbers` — exact and approximate, `digits`, rounding, money.
- `Calc/elementary` — `round floor sqrt exp log sin atan2 gamma erf`
  (radians).
- `Calc/integers` — `factorial binomial gcd isprime factor modpow modinv`.
- `Calc/sequences` — `range len sum map filter reduce sort unique`, random
  with `seed`.
- `Calc/units` — `convert units`.
- `Calc/statistics` — `mean median stdev variance quantile percentile
  correlation linreg`.
- `Calc/distributions` — `normcdf normpdf norminv tcdf tinv chi2cdf chi2inv
  binompdf binomcdf poisspdf poisscdf`.
- `Calc/calculus` — `integrate deriv root minimize polyfit polyval interp`.
  A polynomial is its coefficient list, highest power first: `[1, -3, 2]`
  is x² − 3x + 2.
- `Calc/matrices` — `solve inv det trace matrank eig lstsq`.
- `Calc/complex` — `1i re im conj arg abs polyroots`.
- `Calc/finance` — `pmt pv fv rate npv irr compound cagr`.
- `Calc/tables` — `csv select where groupby pivot join describe`.
- `Calc/data` — `from` and `to`: stored tables.
- `Calc/dates` — dates, months, working days, time zones, `datefmt`.
- `Calc/errors` — every error code and what to change.
- `Calc/limits` — the bounds on one call.

Recipes by task (tax, loans, deadlines, totals):
`Context op=help topic=calculations`.

## Examples

An order total with tax rounded to cents:

```json
{"expr": "subtotal = 1249.99 * 3; tax = round(subtotal * 0.2, 2); subtotal + tax"}
```

Result: 4499.96

Numbers from the conversation, in `vars`:

```json
{"expr": "round(sum(hours .* rates), 2)", "vars": {"hours": [7.5, 8, 6.25], "rates": [42, 42, 55.5]}}
```

Result: 997.88

A table given in the conversation: pass it to `csv()` instead of copying
rows by hand:

```json
{"expr": "t = csv(\"region,amount\\nEU,120\\nUS,300\\nEU,80\"); sum(where(t, r -> r.region == \"EU\").amount)"}
```

Result: 200

Days until a date (earlier date first):

```json
{"expr": "datediff(date(\"2026-11-23\"), date(\"2026-12-25\"), \"days\")"}
```

Result: 32

A deadline 10 working days out, and its ISO week:

```json
{"expr": "due = addworkdays(date(\"2026-11-23\"), 10); [due, isoweek(due)]"}
```

Result: [2026-12-07, 50]

A unit conversion:

```json
{"expr": "round(convert(500, \"g\", \"oz\"), 2)"}
```

Result: 17.64
