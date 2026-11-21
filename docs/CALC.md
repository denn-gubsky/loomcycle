# Calc — exact arithmetic for agents

`Calc` is a built-in tool that evaluates a math expression and returns the
answer, exactly where it can be exact. It exists because a model's mental
arithmetic is wrong often enough to matter: a total, a tax, an interest
payment or a count of working days that is off by a little is worse than
useless, because it looks right. With Calc the model writes the formula and
the runtime computes it.

Every agent has Calc unless its definition removes it (see
[Removing Calc from an agent](#removing-calc-from-an-agent)). It is pure: no
state, no I/O, no configuration, no provider key.

- [How an agent uses it](#how-an-agent-uses-it)
- [The language in brief](#the-language-in-brief)
- [Exact or approximate](#exact-or-approximate)
- [The sections](#the-sections)
- [The limits](#the-limits)
- [Calc outside a run: the MCP `calc` meta-tool](#calc-outside-a-run-the-mcp-calc-meta-tool)
- [Removing Calc from an agent](#removing-calc-from-an-agent)
- [The help a model reads](#the-help-a-model-reads)

## How an agent uses it

One call, one expression. The input is:

| Field | |
|---|---|
| `expr` (required) | One or more statements separated by `;` or newlines. The last one's value is the result. |
| `vars` | Names bound before evaluation: numbers, strings, booleans or lists. A JSON number is exact (`0.1` is 1/10); a string holding a number (`"19.99"`, `"1_000"`) is that number. |
| `digits` | Significant digits for an approximate result or a fraction that does not terminate: 1 to 1000, default 15. |
| `seed` | An integer that makes the random functions repeatable. |

No other argument is accepted. Pass the figures from the conversation in
`vars`, so the expression stays readable and nothing is retyped wrong:

```json
{"expr": "round(sum(hours .* rates), 2)", "vars": {"hours": [7.5, 8, 6.25], "rates": [42, 42, 55.5]}}
```

The result:

```json
{"result": 997.88, "type": "number", "exact": true, "text": "997.88"}
```

- `result` is the value as JSON and `text` the same written for reading; the
  model should quote `text`.
- `type` is `number`, `string`, `boolean`, `list`, `function`, `date` or
  `datetime`.
- `exact` is `true` when every number in the result is exact.
- `fraction` holds the exact value of an exact result whose decimals do not
  end (`"1/3"`); `assigned` every name the expression assigned or defined;
  `shape` a list's dimensions; `truncated` is `true` when the output passed
  64 KB and was cut.

A refused call returns `{"error", "detail", "hint", "pos"}` with a code from a
closed set (`syntax`, `unknown_function`, `arity`, `domain`, `step_limit`, …).
A malformed expression is a `validation` error (fix it and resend); a domain
error or a limit is a `business` error. `/metrics` counts refusals in
`loomcycle_calc_refusals_total{error}`.

## The language in brief

- Numbers `42`, `-3.5`, `2.5e-3`, `1_000_000`, `0xff`; constants `pi`, `e`,
  `tau`, `phi`; booleans, strings `"text"`, lists `[1, 2, 3]`.
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
- Variables and functions have separate namespaces: a variable named `units`
  does not hide the function `units(...)`.

## Exact or approximate

Literals, `+ - * /`, integer powers, rounding and the integer functions keep
a result exact: `0.1 + 0.2` is `0.3`, and `2^100` is
`1267650600228229401496703205376` with every digit. Square roots (unless the
root is exact), fractional powers, `exp`, `log`, trigonometry, the constants
and the random functions give a 64-bit float, about 16 significant digits, and
everything computed from one stays approximate: `sqrt(2)^2 == 2` is `false`.
The `exact` field says which a result is. For money, keep it exact and round
once at the end with `round(x, 2)`.

## The sections

The functions are grouped into sections, each with its own help article
(read with `Context op=help topic=Calc/<section>`). Every example below is one
the help shows and a test runs.

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
quantiles, ranks, correlation, regression. `wmean([90, 75, 60], [0.5, 0.3,
0.2])` gives `79.5`; `variance([2, 4, 4, 4, 5, 5, 7, 9], "population")` gives
`4`.

**`Calc/finance`** — `pmt`, `fv`, `pv`, `nper`, `rate`, `npv`, `irr`,
compound growth, CAGR and percentages, with the spreadsheet sign convention
(money paid out is negative; `rate` is per period). `round(pmt(0.08/12, 10,
10000), 2)` gives `-1037.03`; `round(pctchange(1840, 2125), 1)` gives `15.5`.

**`Calc/dates`** — dates and datetimes, day arithmetic, months and years,
working days (Monday to Friday, both ends included, less holidays), day-count
bases, `xnpv`/`xirr`, time zones (the zone database is embedded) and
formatting. `date(2026, 11, 20) + 41` gives `2026-12-31`;
`addmonths(date("2026-01-31"), 1)` gives `2026-02-28`.

Two more articles cover the whole language rather than a group of functions:
`Calc/language` (statements, operators and precedence, functions and lambdas,
ranges, indexing, common mistakes) and `Calc/numbers` (exact and approximate
numbers, `digits`, `fraction`, rounding, money). `Calc/errors` lists every
error code and what to change, and `Calc/limits` the bounds.

## The limits

Every call is bounded, and no setting raises the bounds: 16 KB of expression,
1 MB of `vars`, 1,000,000 steps, 2 seconds, exact numbers of 16,384 bits
(about 4,900 digits), 1,000,000 list elements in one value, 200 levels of
nesting and 64 of one function calling another, 1 to 1,000 digits, and 64 KB
of output (past it the result is cut and `truncated` is `true`). Reaching a
bound refuses the call with its own code (`step_limit`, `too_large`, …), so a
model learns to prefer a closed form: `n * (n + 1) / 2` costs a handful of
steps where `sum(1:n)` costs `n`. `Calc/limits` has the details.

Calc evaluates numbers. It does not solve equations or simplify formulas.

## Calc outside a run: the MCP `calc` meta-tool

The MCP server (`/v1/_mcp`, and `loomcycle mcp`) offers `calc`: the same
input, result and refusals as the in-run tool, for a client with no run — for
example, Claude Code checking a figure. Its description is Calc's own, with
the help pointer rewritten to the `context` meta-tool. Every token may call
it, an isolated `substrate:user` token included, except a token limited to
certain agents (which may call only the run tools). Each call takes one unit
of the token's run rate; past it the call is a retryable failure that says
when to retry. See [MCP_SERVER.md](MCP_SERVER.md).

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
The help is in the runtime's help corpus, read with `Context op=help`:

- `Calc` — the call, the result, the language in brief, exact versus
  approximate, and the list of sections.
- `Calc/<section>` — one article per section above, plus `Calc/language`,
  `Calc/numbers`, `Calc/errors` and `Calc/limits`. Each function has an entry
  with its signature and an example.
- `calculations` (also found as `math`, `calculator`, `arithmetic`) —
  recipes by task: percent change, tax and discounts, splitting an amount
  into shares that sum exactly, weighted averages, a loan's payment and
  interest, growth rates, days and working days to a deadline, checking the
  totals in a table, comparing quantities in different units, mean and
  standard deviation, temperatures, and a loan balance with `fv` and `pv`.

A test runs every example in these articles through the tool and compares the
result to the digit, so the help cannot drift from what Calc does. A refused
call's error also names the help topic to read next.
