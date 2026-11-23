---
name: calculations
description: "Calculation recipes by task, each a Calc call with its result — percent change, tax and discounts, splitting an amount into exact shares, weighted averages, loan payments and interest, growth rates, days and working days to a deadline, checking a table's totals, comparing quantities in different units, mean and standard deviation, temperatures, a loan balance with fv and pv, and totals by group from a table of rows."
aliases: [math, calculator, arithmetic]
---
Recipes for the numbers a task asks for, one `Calc` call each. Put every
figure through `Calc` — never work it out in your head — and quote the
result's `text` in your answer. Pass the figures you were given in `vars`,
so the expression stays readable and nothing is retyped wrong.

Three rules carry most of the recipes:

- `%` is modulo, not percent. 15% of `x` is `x * 15 / 100` (or `x * 0.15`).
- Decimals are exact. Keep money exact and round once, at the end, with
  `round(x, 2)`.
- Finance functions use the spreadsheet sign convention: money you receive is
  positive, money you pay is negative.

The function reference is the `Calc` tool article and its sections (`Calc/finance`, `Calc/dates`, `Calc/units`, `Calc/statistics` and the rest), read with
`Context op=help topic=Calc/<section>`.

## Examples

### Percent change

The change from an old value to a new one, as a percentage of the old.
`pctchange(old, new)` does the same.

```json tool=Calc
{"expr": "round((new - old) / old * 100, 2)", "vars": {"old": 48.2, "new": 53.75}}
```

Result: 11.51

### Taking a discount, then adding tax

Apply the discount to the price, round to cents, then add the tax on the
discounted price and round again. Assign each step, so `assigned` in the
result shows the discounted price and the tax as well as the total.

```json tool=Calc
{"expr": "discounted = round(price * (1 - 15 / 100), 2); tax = round(discounted * 8.25 / 100, 2); discounted + tax", "vars": {"price": 129.99}}
```

Result: 119.61

### Splitting an amount into shares that sum exactly

Round every share but the last, and give the last share whatever is left, so
the shares add up to the total to the cent. Return the list and check its
sum.

```json tool=Calc
{"expr": "s = round(total / n, 2); shares = [s, s, total - s * (n - 1)]; [shares, sum(shares)]", "vars": {"total": 1000, "n": 3}}
```

Result: [[333.33, 333.33, 333.34], 1000]

### A weighted average

`wmean(values, weights)` divides by the sum of the weights, so the weights
need not add up to 1.

```json tool=Calc
{"expr": "wmean(scores, weights)", "vars": {"scores": [88, 92, 79], "weights": [30, 30, 40]}}
```

Result: 85.6

### A loan's monthly payment and total interest

`pmt` takes the rate per period and the number of periods: divide an annual
rate by 12 and count months. The payment comes back negative (you pay it), so
negate it before you round.

```json tool=Calc
{"expr": "payment = round(-pmt(0.065 / 12, 60, 25000), 2); [payment, round(payment * 60 - 25000, 2)]"}
```

Result: [489.15, 4349]

### A growth rate per year (CAGR)

The compound annual growth rate from a start value to an end value over some
years.

```json tool=Calc
{"expr": "round(cagr(50000, 82000, 5) * 100, 2)"}
```

Result: 10.4

### Days and working days to a deadline

Subtracting two dates gives days. `workdays` counts Monday to Friday with
both ends included, less the holidays you list.

```json tool=Calc
{"expr": "start = date(\"2026-11-23\"); due = date(\"2026-12-31\"); [due - start, workdays(start, due, [date(\"2026-12-25\")])]"}
```

Result: [38, 28]

### Checking the totals in a table you were given

Do not trust a total in a table; recompute it. Pass the column and the stated
total, and return both the sum and the difference.

```json tool=Calc
{"expr": "total = sum(column); [total, total - stated]", "vars": {"column": [1250.5, 980.25, 2310, 415.75], "stated": 4965.5}}
```

Result: [4956.5, -9]

The column sums to 4956.5, so the stated 4965.5 is 9 too high. Say so in your
answer.

### Comparing quantities given in different units

Convert to one unit before you compare. Here, a price per kilogram for a
500 g pack at 3.49 and a 1 lb pack at 1.79:

```json tool=Calc
{"expr": "[round(3.49 / 0.5, 2), round(1.79 / convert(1, \"lb\", \"kg\"), 2)]"}
```

Result: [6.98, 3.95]

### The mean and standard deviation of a sample

`stdev` is the sample standard deviation by default (it divides by n − 1);
pass `"population"` for the population one.

```json tool=Calc
{"expr": "[mean(v), round(stdev(v), 4)]", "vars": {"v": [12.1, 11.8, 12.4, 12.0, 11.9]}}
```

Result: [12.04, 0.2302]

### Converting a temperature

`convert` treats `degC` and `degF` as readings, offset included. For a
temperature difference, use `convertdelta`.

```json tool=Calc
{"expr": "[convert(180, \"degC\", \"degF\"), convertdelta(10, \"degC\", \"degF\")]"}
```

Result: [356, 18]

### A loan balance, and how much you can borrow

The balance left on the loan above after 24 of its 60 payments is the future
value of the loan at that point. With the payment negative and the loan
positive, `fv` returns the balance as a negative number: what you still owe.

```json tool=Calc
{"expr": "round(fv(0.065 / 12, 24, -489.15, 25000), 2)"}
```

Result: -15959.95

How much you can borrow if you can pay 450 a month for 48 months at 7% a
year is the present value of those payments:

```json tool=Calc
{"expr": "round(pv(0.07 / 12, 48, -450), 2)"}
```

Result: 18792.09

### Totals by group from rows of data

Pass the rows as an array of objects and let `groupby` add each group; the
sums stay exact. `format: "markdown"` would return the same table as a
Markdown table to show a person (`Calc/tables`).

```json tool=Calc
{"expr": "groupby(t, \"region\", \"total\", g -> sum(g.amount))", "vars": {"t": [{"region": "EU", "amount": 120.10}, {"region": "US", "amount": 300}, {"region": "EU", "amount": 79.95}]}}
```

Result: {"columns": ["region", "total"], "rows": [["EU", 200.05], ["US", 300]]}
