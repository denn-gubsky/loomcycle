---
name: Calc/finance
kind: section
description: "Calc finance — loan payments, future and present value, number of periods and the rate of an annuity (pmt, fv, pv, nper, rate), net present value and internal rate of return, compound growth, CAGR and percentages, with the spreadsheet sign convention."
---
The finance functions follow the spreadsheet conventions (Excel,
LibreOffice). Read these two rules before any call:

- **Signs: money paid out is negative, money received is positive.** A loan
  you receive is `+250000`; the payment you make on it comes back negative.
  Money you invest is negative; what you get back is positive.
- **`rate` is the rate per period, and `n` counts periods.** For monthly
  payments at 6% a year, pass `0.06/12` and the number of months. A rate of
  -100% or less is refused.

`when` is `0` (the default) for payments at the end of each period, `1` for
payments at the beginning. `future` and `present` default to 0. `pmt`, `fv`,
`pv` and `npv` stay **exact** when every input is exact and `n` is a whole
number (round once with `round(x, 2)`); `nper`, `rate`, `irr` and `cagr`
give a float.

## Examples — a loan and an investment

A loan: 250,000 at 6% a year over 30 years, paid monthly. The monthly
payment and the total interest:

```json
{"expr": "p = pmt(0.06/12, 360, 250000); [round(p, 2), round(-p * 360 - 250000, 2)]"}
```

Result: [-1498.88, 289595.47]

An investment: 5,000 now and 200 a month for 10 years at 7% a year. What
it is worth at the end:

```json
{"expr": "round(fv(0.07/12, 120, -200, -5000), 2)"}
```

Result: 44665.27

## Annuities

### pmt(rate, n, present, future?, when?)
The payment per period that takes a balance of `present` to `future` in `n`
periods: a loan's instalment (`future` 0), or the saving that reaches a target
(`present` 0, `future` the target).

Example: `round(pmt(0.08/12, 10, 10000), 2)` gives `-1037.03`.

### fv(rate, n, payment, present?, when?)
The value after `n` periods of `payment`, starting from `present`. Deposits
are negative, so the value you will have comes back positive.

Example: `round(fv(0.06/12, 10, -200, -500, 1), 2)` gives `2581.4`.

### pv(rate, n, payment, future?, when?)
The value today of `n` payments (and a final `future` amount): what a stream
of payments is worth now.

Example: `round(pv(0.08/12, 240, 500), 2)` gives `-59777.15`.

### nper(rate, payment, present, future?, when?)
The number of periods `payment` takes to go from `present` to `future`;
usually not a whole number. Refused when the payment never gets there (check
the signs).

Example: `round(nper(0.01, -100, -1000, 10000, 1), 2)` gives `59.67`.

### rate(n, payment, present, future?, when?, guess?)
The rate per period at which `n` payments take `present` to `future`
(Excel's `RATE`). Found by Newton's method from `guess` (default 0.1); when
no rate solves it, or the search does not converge, the call is refused, never
answered approximately. Multiply by 12 for an annual rate of monthly periods.

Example: `round(rate(48, -200, 8000) * 12, 4)` gives `0.0924`.

## Cash flows

### npv(rate, flows)
The net present value of `flows`, one per period. **As in Excel, the first
flow is discounted by one period**: `npv` assumes it comes at the end of the
first period. For an outlay made today, add it outside the call:
`outlay + npv(rate, returns)`.

Example: `round(npv(0.1, [-10000, 3000, 4200, 6800]), 2)` gives `1188.44`.

### irr(flows, guess?)
The internal rate of return: the rate per period at which the flows' value
is zero, **the first flow at time 0** (unlike `npv`). Needs at least one
negative and one positive flow. Newton's method from `guess` (default 0.1),
then a bisection search; flows that have no such rate, or where neither
converges, are refused, never approximated. A float.

Example: `round(irr([-70000, 12000, 15000, 18000, 21000, 26000]), 4)` gives `0.0866`.

## Growth and percentages

### compound(principal, rate, periods, per_year?)
`principal * (1 + rate)^periods`: `rate` per period. With `per_year`, `rate`
is an annual rate compounded `per_year` times a year and `periods` is in
years: `principal * (1 + rate/per_year)^(periods * per_year)`. Exact on exact
input with a whole number of compounding periods.

Example: `round(compound(1000, 0.05, 10, 12), 2)` gives `1647.01`.

### cagr(start, end, years)
The compound annual growth rate, as a fraction: `(end/start)^(1/years) - 1`.
`start` must be above zero. A float.

Example: `round(cagr(1840, 2125, 3) * 100, 2)` gives `4.92`.

### pct(part, whole)
`part` as a percentage of `whole`. Exact on exact input.

Example: `pct(45, 180)` gives `25`.

### pctchange(old, new)
The change from `old` to `new` as a percentage of `old`; negative for a
fall. Exact on exact input.

Example: `round(pctchange(1840, 2125), 1)` gives `15.5`.

## Examples — cash flows

Comparing two projects by net present value at 8%, with the outlay today:

```json
{"expr": "a = -50000 + npv(0.08, [15000, 18000, 20000, 22000]); b = -60000 + npv(0.08, [30000, 25000, 20000]); [round(a, 2), round(b, 2)]"}
```

Result: [11368.29, 5087.89]

A variable named `rate` beside a call to `rate`: the annual rate a lender
charges on a 20,000 car loan at 470 a month for four years:

```json
{"expr": "rate = rate(48, -470, 20000); round(rate * 12 * 100, 2)"}
```

Result: 6.03

A series with no sign change has no rate of return:

```json
{"expr": "irr([1000, 2000, 3000])"}
```

Result: error domain
