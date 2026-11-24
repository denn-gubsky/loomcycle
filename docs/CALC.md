# Calc — exact arithmetic for agents

`Calc` is a built-in tool that evaluates a math expression and returns the
answer, exactly where it can be exact. It exists because a model's mental
arithmetic is wrong often enough to matter: a total, a tax, an interest
payment or a count of working days that is off by a little is worse than
useless, because it looks right. With Calc the model writes the formula and
the runtime computes it.

Every agent has Calc unless its definition removes it (see
[Removing Calc from an agent](#removing-calc-from-an-agent)). It needs no
configuration and no provider key, and it keeps no state between calls. It
touches nothing outside the call, with one exception: inside a run, `from`
and `to` read a stored table and write a result through the agent's own
tools (see [Data by reference](#data-by-reference-from-and-to)).

- [How an agent uses it](#how-an-agent-uses-it)
- [The language in brief](#the-language-in-brief)
- [Exact or approximate](#exact-or-approximate)
- [The sections](#the-sections)
- [Matrices and linear algebra](#matrices-and-linear-algebra)
- [Distributions and numeric calculus](#distributions-and-numeric-calculus)
- [Complex numbers](#complex-numbers)
- [Tables and `format`](#tables-and-format)
- [Data by reference: `from` and `to`](#data-by-reference-from-and-to)
- [The limits](#the-limits)
- [When a call is refused](#when-a-call-is-refused)
- [Calc outside a run: MCP, HTTP and gRPC](#calc-outside-a-run-mcp-http-and-grpc)
- [Removing Calc from an agent](#removing-calc-from-an-agent)
- [The help a model reads](#the-help-a-model-reads)

## How an agent uses it

One call, one expression. The input is:

| Field | |
|---|---|
| `expr` (required) | One or more statements separated by `;` or newlines. The last one's value is the result. |
| `vars` | Names bound before evaluation: numbers, strings, booleans, lists or tables. A JSON number is exact (`0.1` is 1/10); a string holding a number (`"19.99"`, `"1_000"`) is that number. |
| `digits` | Significant digits for an approximate result or a fraction that does not terminate: 1 to 1000, default 15. |
| `seed` | An integer that makes the random functions repeatable. |
| `format` | `json` (the default), `markdown` or `csv`: how a table or matrix result is written in `text`, and in which format `to` writes it. |
| `from` | Inside a run only: names bound to stored data — a volume file, a Memory value or a Document chunk. |
| `to` | Inside a run only: a place to write the result to instead of returning it. |

No other argument is accepted, and there is no `op`. Pass the figures from
the conversation in `vars`, so the expression stays readable and nothing is
retyped wrong:

```json
{"expr": "round(sum(hours .* rates), 2)", "vars": {"hours": [7.5, 8, 6.25], "rates": [42, 42, 55.5]}}
```

The result:

```json
{"result": 997.88, "type": "number", "exact": true, "text": "997.88"}
```

- `result` is the value as JSON and `text` the same written for reading; the
  model should quote `text`.
- `type` is `number`, `complex`, `string`, `boolean`, `list`, `function`,
  `date`, `datetime`, `table`, `row` or `empty`. A complex result is
  `{"re", "im"}`, a table `{"columns", "rows"}`.
- `exact` is `true` when every number in the result is exact.
- `fraction` holds the exact value of an exact result whose decimals do not
  end (`"1/3"`); `assigned` every name the expression assigned or defined;
  `shape` a list's dimensions (`[n]`, or `[n, m]` for a matrix) or a table's
  `[rows, columns]`; `truncated` is `true` when the output passed 64 KB and
  was cut.

A refused call returns `{"error", "detail", "hint", "pos"}` with a code from a
closed set; see [When a call is refused](#when-a-call-is-refused).

## The language in brief

- Numbers `42`, `-3.5`, `2.5e-3`, `1_000_000`, `0xff`; constants `pi`, `e`,
  `tau`, `phi`; booleans, strings `"text"`, lists `[1, 2, 3]`. A number with
  an `i` suffix is imaginary: `3 + 2i`. The name `i` on its own stays an
  ordinary variable.
- `+ - * /`; power is `^` (`2^10`), never `**`. **`%` is modulo**, not percent:
  15% of `x` is `x * 15 / 100`. `n!` is the factorial. No implicit
  multiplication: `2*x`, not `2x`.
- Comparison `== != < <= > >=`, logic `and or not`; `if(cond, a, b)`.
- `name = expr` assigns, `f(x) = expr` defines a function, `x -> expr` is a
  lambda; no recursion.
- `a:b` and `a:step:b` are ranges with both ends included. **Indexing starts
  at 1.**
- On lists, `+` and `-` work element by element and `v * 2` scales; between
  two lists write `.*`, `./`, `.^`.
- A matrix is a list of rows, `[[1, 2], [3, 4]]`. `*` between two matrices is
  the matrix product, `A'` the transpose, `A[i, j]` an element.
- A table's column is `t.price` (or `t["unit price"]`), a row `t[3]`, a cell
  `t[3].price`.
- Variables and functions have separate namespaces: a variable named `units`
  does not hide the function `units(...)`.

## Exact or approximate

Literals, `+ - * /`, integer powers, rounding and the integer functions keep
a result exact: `0.1 + 0.2` is `0.3`, and `2^100` is
`1267650600228229401496703205376` with every digit. Square roots (unless the
root is exact), fractional powers, `exp`, `log`, trigonometry, the constants
and the random functions give a 64-bit float, about 16 significant digits, and
everything computed from one stays approximate: `sqrt(2)^2 == 2` is `false`.

The newer sections follow the same rule. Building and rearranging a matrix,
`dot`, `cross`, `kron`, `trace`, the matrix product `*` and the power `A^n`
(`n` ≥ 1) keep exact numbers exact; the product and the power do so up to 50
rows or columns a side and while the result stays within the exact-number
cap, past which they are floats. The solvers and the decompositions work in
floats, even on whole numbers. The distributions and the numeric calculus
always give floats (the binomial probabilities are computed exactly on an
exact `p` and rounded once, so they are the nearest float to the true
value). A complex number is two floats and is never exact. Table cells
holding numbers stay exact, so a column of prices sums to the cent.

The `exact` field says which a result is. For money, keep it exact and round
once at the end with `round(x, 2)`.

## The sections

The functions are grouped into sections, each with its own help article
(read with `Context op=help topic=Calc/<section>`). Every example in this
list is one the help shows and a test runs.

**`Calc/elementary`** — rounding, `abs`, `min`/`max`, roots, powers,
logarithms, trigonometry in radians (`rad`/`deg` convert), gamma, erf,
Bessel. `round(19.995, 2)` gives `20`; `sin(rad(30))` gives `0.5`.

**`Calc/integers`** — factorial, binomial, Fibonacci, gcd/lcm, primes,
factoring, modular arithmetic, bits and number bases. `binomial(49, 6)` gives
`13983816`; `factor(360)` gives `[2, 2, 2, 3, 3, 5]`.

**`Calc/sequences`** — `range`, `sum`, `map`/`filter`/`reduce`, `sort`,
`unique`, `count`, `zip`, and the random functions with `seed`.
`sum(1:100)` gives `5050`; `filter(1:10, x -> x % 3 == 0)` gives `[3, 6, 9]`.

**`Calc/units`** — `convert` and `convertdelta` between units of length,
mass, time, temperature, area, volume, speed, force, pressure, energy, power,
angle, frequency and data size, with SI and binary prefixes and compound
units. `convert(5, "mi", "km")` gives `8.04672`; `convert([32, 212], "degF",
"degC")` gives `[0, 100]`.

**`Calc/statistics`** — mean, median, mode, weighted and geometric means,
variance and standard deviation (sample by default, or population),
quantiles, ranks, correlation, regression; of a matrix, `mean`, `median`,
`variance` and `stdev` work column by column. `wmean([90, 75, 60], [0.5,
0.3, 0.2])` gives `79.5`; `variance([2, 4, 4, 4, 5, 5, 7, 9], "population")`
gives `4`.

**`Calc/finance`** — `pmt`, `fv`, `pv`, `nper`, `rate`, `npv`, `irr`,
compound growth, CAGR and percentages, with the spreadsheet sign convention
(money paid out is negative; `rate` is per period). `round(pmt(0.08/12, 10,
10000), 2)` gives `-1037.03`; `round(pctchange(1840, 2125), 1)` gives `15.5`.

**`Calc/dates`** — dates and datetimes, day arithmetic, months and years,
working days (Monday to Friday, both ends included, less holidays), day-count
bases, `xnpv`/`xirr`, time zones (the zone database is embedded) and
formatting. `date(2026, 11, 20) + 41` gives `2026-12-31`;
`addmonths(date("2026-01-31"), 1)` gives `2026-02-28`.

Six more sections have a part of this guide each, below:
**`Calc/matrices`**, **`Calc/distributions`**, **`Calc/calculus`**,
**`Calc/complex`**, **`Calc/tables`** and **`Calc/data`**.

Two more articles cover the whole language rather than a group of functions:
`Calc/language` (statements, operators and precedence, functions and lambdas,
ranges, indexing, common mistakes) and `Calc/numbers` (exact and approximate
numbers, `digits`, `fraction`, rounding, money). `Calc/errors` lists every
error code and what to change, and `Calc/limits` the bounds.

## Matrices and linear algebra

A matrix is a list of rows of the same length, `[[1, 2], [3, 4]]`, and a
list of numbers is a vector. There is no separate matrix type or syntax: a
matrix function or operator reads the list it is given. `Calc/matrices` has
every function.

- **Operators.** `A * B` is the matrix product; a vector on the right is a
  column, on the left a row. `*` between two vectors is refused: write
  `dot(a, b)`, or `.*` element by element. `A'` is the transpose. `+`, `-`
  and the dotted operators work element by element on two matrices of the
  same shape, and a number applies to every element; a matrix plus a vector
  is refused (`shape`) rather than guessed at.
- **Power.** `A^n` is the matrix power of a square matrix and an integer
  `n`: `A^0` is the identity and `A^-2` is `inv(A)^2` (refused for a
  singular `A`). There is no fractional matrix power; `.^` raises each
  element. `[[1, 1], [1, 0]]^10` gives `[[89, 55], [55, 34]]`.
- **Indexing.** `A[i, j]` is an element, `A[i, :]` a row, `A[:, j]` a column,
  from 1; a list of indexes picks several, `A[[1, 3], :]`.
- **Building and properties.** `zeros`, `ones`, `identity`, `diag`,
  `reshape`, `hstack`, `vstack`, `linspace`, `size`; `dot`, `cross`, `kron`,
  `trace`, `det`, `matrank` (the rank of a matrix: `rank` is the statistics
  ranking), `norm`, `cond`.
- **Solving.** `inv`, `pinv`, `solve(A, b)` and `lstsq(A, b)` for least
  squares. A singular matrix to `inv` or `solve` is refused (`domain`), with a
  pointer to `pinv` or `lstsq`.
- **Decompositions.** `lu`, `qr`, `cholesky`, `eig` and `svd` each return a
  list of their factors, taken apart by index (`lu(A)` is `[L, U, P]`).
  `eig` of a symmetric matrix gives real eigenvalues in ascending order; of
  any other square matrix it may give complex ones.

`solve([[2, 1], [1, 3]], [3, 5])` gives `[0.8, 1.4]`; `det([[4, 3], [6,
3]])` gives `-6`; `eig([[2, 1], [1, 2]])[1]` gives `[1, 3]`.

**Size and the shared pool.** A matrix a function reads or builds holds at
most 250,000 elements (500 × 500 for a square one), and the product, the
power, the solvers and the decompositions are refused before any work when
they would cost more than one 500 × 500 matrix (`A^n` counts each product it
takes). The check comes first because the linear-algebra routines cannot be
interrupted once they start: the size cap is what bounds their time. A
matrix operation over 10,000 elements, or over 1,000,000 multiply-adds, also
takes a slot of a pool shared by every caller, one slot per processor core.
Waiting for a slot counts against the call's 2 seconds; a call that runs out
of time there is refused `time_limit`, and resending it is the remedy.

## Distributions and numeric calculus

**`Calc/distributions`** — the normal, t, chi-squared, F, exponential and
uniform distributions each have a density (`…pdf`), a cumulative probability
(`…cdf`, the probability of a value at most `x`) and its inverse (`…inv`, the
quantile); the binomial and Poisson distributions have `…pdf` (exactly `k`)
and `…cdf` (at most `k`). The names follow the calculator convention:
`normcdf`, `tinv`, `chi2inv`, `fcdf`, `expcdf` (which takes the rate λ),
`unifinv`, `binompdf`, `poisscdf`. The point comes first, so a list of points
gives a list. A parameter out of range is refused (`domain`).
`round(norminv(0.975), 4)` gives `1.96`; `binomcdf(6, 10, 0.5)` gives
`0.828125`. For the upper tail take `1 - …cdf(x)`.

**`Calc/calculus`** — `integrate(f, a, b)`, `deriv(f, x)`, `root(f, a, b)`
(Brent's method on a bracket whose ends differ in sign) and
`minimize(f, a, b)` take a function first: a lambda, a function the
expression defined, or a built-in by name. They work in floats, to about 10
significant digits (a minimum's position to about 8). `polyfit(x, y, deg)`,
`polyval(p, x)` and `interp(x, xs, ys)` (linear, never extrapolating) round
out the section; a polynomial is its coefficients, highest power first.
`round(integrate(x -> x^2, 0, 3), 10)` gives `9`;
`round(root(x -> x^2 - 2, 0, 2), 10)` gives `1.4142135624`.

Each numeric method evaluates its function a bounded number of times, and
every evaluation costs its steps. A method that does not settle — an integral
over a singularity, a derivative at a jump, a root of a function too rough to
close in on — is refused with `no_convergence`, never answered with a guess.
Split or narrow the interval and call again.

## Complex numbers

A complex number is written with an `i` suffix on a number — `2i`, `1.5i`,
`3e2i`, so `3 + 2i` — or built with `complex(a, b)`. The `1` in `1i` is
needed: `i` on its own is an ordinary name, so `i = 4; 2*i` is still `8` and
no expression written before complex numbers changed meaning. An unassigned
`i` or `j` is refused with a hint to write `1i`.

- A complex value is two 64-bit floats: `type` is `complex`, it is never
  exact, `text` writes it `3+2i` (the imaginary part always shown, `-1+0i`)
  and `result` holds `{"re": 3, "im": 2}`.
- **Real in, real out.** A real argument keeps its real meaning: `sqrt(-1)`
  and `log(-2)` are still refused (`domain`), with a hint to pass a complex
  argument. `sqrt(-1 + 0i)` gives `1i`. A complex result stays complex even
  when its imaginary part is 0; `re(z)` makes it real.
- Arithmetic, the element-wise operators, the elementary and trigonometric
  functions (principal values), `abs` (the modulus) and rounding (each part)
  take complex arguments; `re`, `im`, `conj` and `arg` take one apart.
  `polyroots(p)` gives the roots of a polynomial, highest power first:
  `round(polyroots([1, 0, 1]), 12)` gives `[-1i, 1i]`; `polyval(p, x)` takes
  complex coefficients or a complex `x`.
- Complex numbers have no order: `<`, `>`, `min`, `max`, `median`, the
  statistics functions, `%` and a plain `sort` refuse one (`type`); compare
  `abs(z)` or `re(z)`, or sort with a key, `sort(v, z -> abs(z))`.
- The matrix product `*`, the power `A^n` (`n` ≥ 0) and `ctranspose(A)`, the
  conjugate transpose, take complex elements. The other matrix functions
  (`inv`, `solve`, `det`, the decompositions) and `A^-n` work in real numbers
  and refuse a complex element, but `eig` of a real matrix returns complex
  eigenvalues.

`Calc/complex` has every function.

## Tables and `format`

A table is named columns of equal length. A cell holds a number, a string, a
boolean or nothing (an **empty** cell), and number cells stay exact. A table
comes from `vars` — as `{"columns": [...], "rows": [[...], ...]}` or as an
array of objects, whose keys become the columns — from CSV text with
`csv("…")`, or, inside a run, from a stored file with `from`.

- `t.amount` is a column, a list, so every list and statistics function
  takes it: `sum(t.amount)`, `t.price .* t.qty`. `t["unit price"]` reaches a
  column whose name is not a plain word; `t[2]` is a row and `t[2].amount` a
  cell.
- **Empty cells** are skipped by the aggregates (`sum`, `mean`, `count`,
  `min` …), kept through arithmetic so the columns stay aligned, and make
  `< <= > >=` false; `isempty(x)` tests for one. `len` counts every cell.
- **Functions:** `select`, `where`, `addcol`, `sort`, `head`, `tail`,
  `groupby`, `pivot`, `join`, `leftjoin`, `totals`, `describe`. `where` and
  `addcol` take a rule that receives one row (`r -> r.qty > 0`); `groupby`
  and `pivot` take a rule that receives one group as a table
  (`g -> sum(g.amount)`). A column name that does not exist is refused
  (`column`), with the close names in the hint.

```json
{"expr": "groupby(csv(\"region,amount\\nEU,120\\nUS,300\\nEU,80\"), \"region\", \"total\", g -> sum(g.amount))"}
```

gives the table `{"columns": ["region", "total"], "rows": [["EU", 200], ["US",
300]]}`.

**`format`** — `json` (the default), `markdown` or `csv` — changes how a table
or a rectangular matrix is written in `text`; `result` is the same JSON in
every format, and `shape` is `[rows, columns]`. Ask for `markdown` when the
table will be shown to a person. A table built in a call holds at most
100,000 cells, and building one costs a step per cell. `Calc/tables` has
every function.

## Data by reference: `from` and `to`

A table of thousands of rows should not pass through the model twice. Inside
a run, `from` names where the data is stored and binds it to a name, as a
`vars` entry would be; `to` names where the result goes, and the call writes
it there instead of returning it:

```json
{"expr": "groupby(t, \"region\", \"total\", g -> sum(g.amount))", "from": {"t": "volume:sales.csv"}, "to": "volume:by-region.csv"}
```

With `to`, the call returns where the result went, the format, `type`,
`shape`, `exact` and the size in `bytes`, with a `text` line saying so
("wrote a table of 2 rows and 2 columns, 27 bytes of csv, to
volume:by-region.csv") — never the content. Writing replaces what is there:
the whole file, the Memory value, or the chunk's whole body.

| Reference | What it names | Reading needs | Writing needs |
|---|---|---|---|
| `volume:<path>` | a file on the agent's default volume | `Read` | `Write` |
| `volume:<name>:<path>` | a file on the volume `<name>` | `Read` | `Write` |
| `memory:<scope>/<key>` | a Memory value; scope `agent`, `user` or `tenant` | `Memory` | `Memory` |
| `document:<scope>/<chunk_id>` | a Document chunk's body | `Document` | `Document` |

**Calc reaches nothing the agent could not reach itself.** The tool in the
table must be in the run's effective tools, and that tool's pre-hooks are
asked before anything is read or written. Each store then applies its own
checks: the volume's bindings, path fence and read-only flag; Memory's scope
grant, its read-only scopes and the per-document access rules; the Document
tool's scope. `to` is judged before any `from` is read, so a call that could
not store its result reads nothing. A refused reference names the reference
and what was missing and carries no data. A workspace reference is refused:
copy the file onto a volume first.

**Formats read.** CSV (`.csv`) and TSV (`.tsv`, `.tab`) with a header row;
JSON (`.json`: a table, an array of objects, an array, an array of arrays, or
a single value); Markdown (`.md`, and every `document:` chunk): the first pipe
table in the text. With no known extension (a Memory key), the first bytes
decide. A field that reads as a number is that exact number; anything else
stays a string, and text is never evaluated as an expression. Files must be
UTF-8.

**Formats written.** `format` chooses `json`, `markdown` or `csv`; without
it, the target's extension, then Markdown for a `document:` chunk and JSON
for anything else. A `format` the name contradicts (`csv` to `report.json`)
and a `.tsv` target are refused. A table or a matrix is written in any of the
three; any other result only as JSON.

**Limits.** At most 16 sources per call, 16 MB read over all of them and
16 MB written, and 1,000,000 cells read in all. A file's size is checked
before it is read, and a larger one is refused, never cut. A table read this
way may pass the 100,000-cell bound of a table built inline, but a table the
expression builds from it (`select`, `where`, `sort` …) is held to 100,000
cells again, so reduce first. Reading costs a step per cell on top of the
expression's own steps, and parsing a source takes a slot of the shared pool
(see [Matrices](#matrices-and-linear-algebra)). A name may come from `vars`
or from `from`, not both.

**Outside a run** there is no agent whose stores `from` and `to` would reach.
The MCP `calc` meta-tool's schema leaves them out, the client libraries do
not offer them, and `POST /v1/_calc` and the `Calc` gRPC RPC refuse a call
that carries either (`bad_request`, a validation refusal). The tool refuses
them too when it finds no run, so no path around the transports' check
reaches a store. `Calc/data` has the details.

## The limits

Every call is bounded, and no setting raises the bounds. Reaching one refuses
the call with its own code, except the output bound, which cuts the result.

| Bound | Limit | Code |
|---|---|---|
| Expression | 16 KB | `expr_too_long` |
| `vars` | 1 MB in total; 32 levels of nesting in one var | `vars_too_large`, `bad_var` |
| Steps | 1,000,000 per call | `step_limit` |
| Time | 2 seconds per call, including any wait for a slot of the shared pool | `time_limit` |
| Exact numbers | 16,384 bits, about 4,900 digits | `too_large` |
| Lists | 1,000,000 elements in one value, nested lists included | `list_too_long` |
| Tables built in a call | 100,000 cells | `list_too_long` |
| Matrices | 250,000 elements; work of at most one 500 × 500 matrix per product, solve or decomposition | `too_large` |
| Numeric methods | `integrate` 15,000 evaluations of its function, `deriv` 20, `root` and `minimize` 500 each | `no_convergence` |
| Data by reference | 16 sources, 16 MB read, 16 MB written, 1,000,000 cells read | `bad_request`, `too_large`, `list_too_long` |
| Nesting | 200 levels of brackets, 64 of one function calling another | `too_deep` |
| Digits | 1 to 1,000 | `bad_request` |
| Output | 64 KB of rendered text; past it the result is cut and `truncated` is `true` | — |

A model learns to prefer a closed form: `n * (n + 1) / 2` costs a handful of
steps where `sum(1:n)` costs `n`. `Calc/limits` has the details, including
the caps on single functions (`isprime`, `factor`, `modpow`, `round`).

Calc evaluates numbers. It does not solve equations symbolically or simplify
formulas; `root` and `solve` find numeric answers.

## When a call is refused

A refused call returns `{"error", "detail", "hint", "pos"}`: the code,
what went wrong, what to write instead when there is something to write, and
the byte offset in `expr` when the error has a place. The codes are a closed
set in three categories, which every transport carries:

| Category | Codes | What the model does |
|---|---|---|
| `validation` — the call is malformed | `syntax`, `unknown_name`, `unknown_function`, `arity`, `type`, `index`, `shape`, `reserved_name`, `recursion`, `bad_var`, `bad_request`, `expr_too_long`, `vars_too_large`, `bad_date`, `unknown_unit`, `dimension`, `column`, `bad_data` | Fix the call and send it again. |
| `business` — the mathematics or a limit | `domain`, `division_by_zero`, `overflow`, `too_large`, `too_deep`, `list_too_long`, `step_limit`, `time_limit`, `empty`, `no_convergence` | Change the inputs or make the work smaller. |
| `transient` | `canceled` | The run stopped mid-call; call again if it continues. |

The codes added with the newer sections: `no_convergence` (a numeric method
did not settle: split or narrow the interval), `column` (a table has no
column of that name, or a table function would make two of one name; the
hint lists close names) and `bad_data` (a source named in `from` is not
valid in its format: broken JSON or CSV quoting, no Markdown table, two
columns of one name, or not UTF-8). A singular matrix, a distribution
parameter out of range and a `root` bracket whose ends share a sign are
`domain`; a matrix or a matrix operation over its cap is `too_large`; a
complex number where an order is needed is `type`.

Sending the same call again fails the same way, except `time_limit` after a
wait for the shared pool, where resending is the remedy. `Calc/errors` lists
every code and what to change; `/metrics` counts refusals in
`loomcycle_calc_refusals_total{error}`.

## Calc outside a run: MCP, HTTP and gRPC

The MCP server (`/v1/_mcp`, and `loomcycle mcp`) offers `calc`: the same
evaluator, result and refusals as the in-run tool, for a client with no run —
for example, Claude Code checking a figure. Its description is Calc's own,
with the help pointer rewritten to the `context` meta-tool and the sentence on
`from` and `to` dropped. Every token may call it, an isolated
`substrate:user` token included, except a token limited to certain agents
(which may call only the run tools). Each call takes one unit of the token's
run rate; past it the call is a retryable failure that says when to retry.
See [MCP_SERVER.md](MCP_SERVER.md).

The same evaluator is `POST /v1/_calc` over HTTP and the `Calc` RPC over gRPC,
with the same callers and the same run-rate charge. Both take `expr`, `vars`,
`digits`, `seed` and `format`. Over HTTP the body is the tool's input and a
200 its result; a refusal carries the tool's error code in `code`, with
`detail`, `hint`, `pos` and its category, and is `400` for a validation
refusal, `422` for a business one and `503` for a transient one (`429` with
`Retry-After` past the rate, `403` for a token limited to certain agents).
Over gRPC `input_json` and `result_json` carry the same JSON, so the proto
does not change when the tool gains a field, and a refusal is
`INVALID_ARGUMENT`, `FAILED_PRECONDITION` or `UNAVAILABLE` with the code as
the `ErrorInfo` reason (`RESOURCE_EXHAUSTED` past the rate,
`PERMISSION_DENIED` for a restricted token). On every transport outside a
run, `from` and `to` are refused (`bad_request`): there is no agent whose
stores they would reach.

```bash
curl -s -X POST "$LOOMCYCLE/v1/_calc" -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -d '{"expr": "0.1 + 0.2"}'
```

The client libraries wrap both: `calc(input)` on the TypeScript client
(`@loomcycle/client`, over HTTP) and `calc(expr, vars=..., digits=...,
seed=..., format=...)` on the Python client (over gRPC). Each returns the
tool's result and raises `CalcError` for a refusal, with the tool's code,
hint, position and category. A number in `result` has been through the
language's JSON decoder, so read `text` for the exact digits of a long
decimal (and, in TypeScript, of an integer past 2^53). See
[the TypeScript README](../adapters/ts/README.md#calc) and
[the Python README](../adapters/python/README.md#calc).

## Removing Calc from an agent

Calc is added to every agent when a run resolves its tools; it is never
written into a stored definition, so no content hash changed when it became a
default. An entry `-Calc` in an agent's `tools` removes it:

```yaml
agents:
  classifier:
    tools: [Read, -Calc]
```

The same `-Calc` works in an AgentDef overlay and in `register_agent`. A fork
keeps its lineage root's removals, and a tenant's version of a static agent
takes the static agent's removals. Listings show what a run holds:
`loomcycle agents list`, `list_agents` and `GET /v1/_library/agents` report
Calc in `effective_tools`, and `Context op=agents` counts it.

Remove it from a single-purpose agent that never calculates (an extractor, a
judge, a classifier). The cost of keeping it is its description and schema in
every model call: about **320–330 input tokens per call** (a bytes/4
estimate), cached along with the other tool definitions where the provider
caches the prompt. Leave it on any agent that converses, plans or answers
questions: a wrong number costs more than the tokens.

## The help a model reads

The Calc description tells the model when to use it and where its help is.
The help is in the runtime's help corpus, read with `Context op=help`. This
guide names the functions only by group; the help is the full reference.

- `Calc` — the call, the result, the language in brief, exact versus
  approximate, and the list of sections with each one's main functions. A
  measurement of how models use the help found they read almost only this
  article, so it names the functions they need most and the names other
  tools use for them.
- `Calc/<section>` — one article per section: `Calc/elementary`,
  `Calc/integers`, `Calc/sequences`, `Calc/units`, `Calc/statistics`,
  `Calc/distributions`, `Calc/calculus`, `Calc/matrices`, `Calc/complex`,
  `Calc/finance`, `Calc/tables`, `Calc/data` and `Calc/dates`, plus
  `Calc/language`, `Calc/numbers`, `Calc/errors` and `Calc/limits`. Each
  function has an entry with its signature and an example.
- `calculations` (also found as `math`, `calculator`, `arithmetic`) —
  recipes by task: percent change, tax and discounts, splitting an amount
  into shares that sum exactly, weighted averages, a loan's payment and
  interest, growth rates, days and working days to a deadline, checking the
  totals in a table, comparing quantities in different units, mean and
  standard deviation, temperatures, and a loan balance with `fv` and `pv`.

A test runs every example in these articles through the tool and compares the
result to the digit, so the help cannot drift from what Calc does. A refused
call's error also names the help topic to read next, and a name another tool
uses (`pnorm`, `qnorm`, `pvariance`, `days_between`) is refused with a hint
naming the Calc function.
