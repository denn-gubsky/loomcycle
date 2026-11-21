---
name: Calc/numbers
kind: section
description: "Calc numbers — exact fractions and approximate floats, what makes a result approximate, digits (significant digits), the fraction field, the rounding functions, and how to compute money."
---
`Calc` keeps every number **exact** unless something forces it to be
approximate. Knowing which is which tells you whether a result can be trusted
to the last digit.

## Exact numbers

Every literal you write is an exact fraction: `0.1` is 1/10, `1/3` is 1/3,
`2^200` is the whole 61-digit integer. `+`, `-`, `*`, `/`, `%`, an integer
power, `abs`, `min`, `max`, `sum`, `product`, the rounding functions and the
integer functions keep a result exact. So `0.1 + 0.2` is exactly `0.3`, and
`1/3 * 3` is exactly `1`.

An exact result is written out in full when its decimal expansion ends
(`1/8` is `0.125`). When it does not (`1/3`), `text` is rounded to `digits`
significant digits and `fraction` holds the exact value, `"1/3"`.

## Approximate numbers

These give a 64-bit float, about 16 significant digits:

- `sqrt` and `cbrt`, unless the root is exact (`sqrt(16/9)` is exactly `4/3`),
- a power whose exponent is not an integer (`2^0.5`),
- `exp`, `log` and every logarithm, every trigonometric and special function,
- the constants `pi`, `e`, `tau`, `phi`,
- `random` and `randnormal`.

Anything computed from an approximate number is approximate, and stays so:
rounding a float does not make it exact. `exact` in the result is `true` only
when every number in it is exact.

An approximate result can be off in the last digit or two: `sqrt(2)^2 == 2`
is `false`. Compare floats with a tolerance — `abs(a - b) < 1e-9` — never with
`==`.

## digits

`digits` sets how many **significant** digits `text` and `result` show for an
approximate number or a non-terminating fraction. The default is 15; the
range is 1 to 1000. It is not decimal places: `digits: 3` writes `100/3` as
`33.3` and `123456/7` as `17600`.

- For an exact fraction, more digits are real digits: `1/7` with `digits: 30`
  shows 30 correct digits.
- A float holds about 17 significant digits; asking for more shows no more.
- A terminating exact number is always written in full, whatever `digits`
  says. To fix the decimal places, round it.

## Rounding

`round(x, n)` rounds half away from zero to `n` decimal places, `roundeven`
rounds half to even (banker's rounding), `floor` rounds down, `ceil` up and
`trunc` toward zero. Each takes `n` (default 0) and keeps an exact number
exact. They are documented in `Calc/elementary`.

## Money

1. Write amounts as decimals — `19.99`, or as strings in `vars`
   (`"19.99"`) when they come from text. Both are exact.
2. Do the arithmetic exactly; round **once**, at the point the money rule says
   (per line, per invoice), with `round(x, 2)`.
3. Never put a float in a money calculation: `sqrt`, `exp`, `log` or a
   fractional power make the result approximate. Compound interest with an
   integer number of periods stays exact: `(1 + 0.05/12)^120`.
4. For a rate in percent, divide by 100: `amount * rate / 100`.

## Examples

Exact decimal arithmetic:

```json
{"expr": "0.1 + 0.2"}
```

Result: 0.3

A non-terminating fraction, shown to 5 significant digits; `fraction` holds `"22/7"`:

```json
{"expr": "22/7", "digits": 5}
```

Result: 3.1429

Monthly compound interest over ten years, rounded once at the end:

```json
{"expr": "round(principal * (1 + rate / 100 / 12)^(12 * years), 2)", "vars": {"principal": 1000, "rate": 5, "years": 10}}
```

Result: 1647.01

Amounts read from text, passed as strings so they stay exact:

```json
{"expr": "round(sum(lines) * 1.0825, 2)", "vars": {"lines": ["19.99", "5.01", "0.25"]}}
```

Result: 27.33

A square root is approximate:

```json
{"expr": "sqrt(2)^2 == 2"}
```

Result: false
