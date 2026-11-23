---
name: Calc/tables
kind: section
description: "Calc tables — a table from vars or CSV text, columns as lists, rows, empty cells, and the table functions: csv, select, where, addcol, sort, head, tail, groupby, pivot, join, leftjoin, totals, describe, isempty; the format field for Markdown or CSV output."
---
A table is named columns of equal length. A cell holds a number, a string, a
boolean or nothing (an **empty** cell). Number cells stay exact, so a column
of prices sums to the cent.

## Making a table

- In `vars`, as columns and rows:
  `{"t": {"columns": ["region", "amount"], "rows": [["EU", 120.5], ["US", 300]]}}`
- In `vars`, as an array of objects: `{"t": [{"region": "EU", "amount": 120.5},
  {"region": "US"}]}`. The columns are the keys in the order they first
  appear; a missing key is an empty cell.
- From CSV text with `csv("…")`; the first line names the columns.

`null` in JSON and a blank CSV field are empty cells. A string holding a
number (`"19.99"`) is that number, as in any var.

## Columns, rows and cells

- `t.amount` is a column: a list, so every list and statistics function takes
  it: `sum(t.amount)`, `t.price .* t.qty`, `round(t.amount * 1.2, 2)`.
- `t["unit price"]` is a column whose name is not a plain word.
- `t[2]` is row 2 (rows start at 1); `t[2].amount` one cell;
  `t[[3, 1]]` a table of rows 3 and 1.
- `len(t)` and `count(t)` count rows; `size(t)` is `[rows, columns]`.

## Empty cells

- **Aggregates skip them**: `sum`, `product`, `mean`, `median`, `mode`,
  `spread`, `geomean`, `harmmean`, `variance`, `stdev`, `quantile`,
  `percentile`, `min`, `max`, and `count(v)`, which counts the cells that are
  not empty. `len` counts every cell.
- **Arithmetic keeps them**: an empty cell plus a number is empty, and so is
  `round` of one, so columns stay aligned.
- **Comparisons with `< <= > >=` are false**, so `where(t, r -> r.qty > 0)`
  drops rows with no `qty`. Test for one with `isempty(x)`.
- Functions that pair or order elements (`wmean`, `covariance`, `rank`,
  `cumsum`) refuse one (`type`): drop those rows first with `where`.

## Rules

`where` and `addcol` take a rule that receives one **row** `r`; read its cells
as `r.qty`. `groupby` and `pivot` take a rule that receives the rows of one
**group** as a table `g`; aggregate its columns: `g -> sum(g.amount)`,
`g -> count(g)`.

## Output

`format` in the call writes a table (or a matrix) result in `text` as
`markdown` or `csv` instead of JSON; `result` is always
`{"columns": [...], "rows": [[...]]}`, and `shape` is `[rows, columns]`.
Ask for `markdown` when you will show the table to a person.

## Functions

### csv(text, sep?)
A table from CSV text. The first line names the columns; quote a field that
holds the separator. A blank line is skipped, so in a one-column table write
an empty cell as `""`. `sep` is one character: `";"`, `"\t"`.

Example: `csv("sku,price\na,19.99\nb,").price` gives `[19.99, null]`.

### select(t, column, ...)
The table with only these columns, in this order. A column argument may also
be a list of names.

Example: `select(csv("a,b,c\n1,2,3"), "c", "a")` gives `{"columns": ["c", "a"], "rows": [[3, 1]]}`.

### where(t, pred)
The rows `r` of `t` where `pred(r)` is true.

Example: `where(csv("sku,qty\na,2\nb,0\nc,5"), r -> r.qty > 0).sku` gives `["a", "c"]`.

### addcol(t, name, rule)
`t` with a column from `rule(r)` per row, from a list as long as `t`, or one
value for every row. A column of that name is replaced in place.

Example: `addcol(csv("price,qty\n2.5,4\n1.2,3"), "total", r -> r.price * r.qty).total` gives `[10, 3.6]`.

### head(v, n?)
The first `n` rows of a table, or elements of a list; default 5.

Example: `head(10:20, 3)` gives `[10, 11, 12]`.

### tail(v, n?)
The last `n` rows of a table, or elements of a list; default 5.

Example: `tail(csv("n\n1\n2\n3"), 2).n` gives `[2, 3]`.

### groupby(t, keys, name, rule, ...)
One row per distinct value of `keys` (a column name, or a list of them), in
the order each first appears, then one column per `name, rule` pair, where
`rule(g)` gets the group's rows. An empty key is a group of its own. Sort the
result to order it.

Example: `groupby(csv("region,amount\nEU,120\nUS,300\nEU,80"), "region", "total", g -> sum(g.amount), "orders", g -> count(g))` gives `{"columns": ["region", "total", "orders"], "rows": [["EU", 200, 2], ["US", 300, 1]]}`.

### pivot(t, rows, columns, rule)
A row per value of the `rows` column and a column per value of the `columns`
column, each cell `rule(g)` of the rows that have both; empty where none do.
The new columns are named by the values' text.

Example: `pivot(csv("m,p,n\njan,a,1\njan,b,2\nfeb,a,3\njan,a,4"), "m", "p", g -> sum(g.n))` gives `{"columns": ["m", "a", "b"], "rows": [["jan", 5, 2], ["feb", 3, null]]}`.

### join(a, b, key)
The rows of `a` and `b` whose `key` column (or list of columns, present in
both) match: each row of `a` once per matching row of `b`, in `a`'s order.
`b`'s other columns follow `a`'s; a name both have gets `_2` on `b`'s side.
Numbers match by value (`1` and `1.0`); an empty key matches nothing.

Example: `join(csv("id,name\n1,ann\n2,bob"), csv("id,score\n2,7\n1,5"), "id").score` gives `[5, 7]`.

### leftjoin(a, b, key)
Like `join`, but every row of `a` is kept; with no match, `b`'s columns are
empty.

Example: `leftjoin(csv("id,name\n1,ann\n3,cy"), csv("id,score\n1,5"), "id").score` gives `[5, null]`.

### totals(t)
`t` with one more row: the sum of each column whose cells are all numbers,
empty elsewhere, and `"total"` in the first column when it is not a number
column. Exact on exact input.

Example: `totals(csv("item,cost\na,0.10\nb,0.20"))[3]` gives `{"item": "total", "cost": 0.3}`.

### describe(t)
A summary of each column whose cells are all numbers: rows `count` (cells
that are not empty), `empty`, `mean`, `stdev` (sample; empty under two
values), `min`, `q1`, `median`, `q3` (`quantile` at 0.25 and 0.75) and
`max`, beside a `stat` column naming them.

Example: `describe(csv("x,y\n1,a\n2,b\n,c\n5,d")).x` gives `[3, 1, 2.66666666666667, 2.08166599946613, 1, 1.5, 2, 3.5, 5]`.

### isempty(x)
Whether `x` is an empty cell.

Example: `map(csv("a,b\n1,x\n,y").a, isempty)` gives `[false, true]`.

## Examples

Revenue by region from data in `vars`, largest first:

```json
{"expr": "s = groupby(t, \"region\", \"revenue\", g -> sum(g.price .* g.qty)); sort(s, \"revenue\", \"desc\")", "vars": {"t": [{"region": "EU", "price": 19.99, "qty": 3}, {"region": "US", "price": 5, "qty": 10}, {"region": "EU", "price": 0.5, "qty": 2}]}}
```

Result: {"columns": ["region", "revenue"], "rows": [["EU", 60.97], ["US", 50]]}

The share of each row in the total, rounded:

```json
{"expr": "addcol(t, \"share\", round(t.amount / sum(t.amount) * 100, 1))", "vars": {"t": {"columns": ["item", "amount"], "rows": [["rent", 1200], ["food", 450], ["other", null]]}}}
```

Result: {"columns": ["item", "amount", "share"], "rows": [["rent", 1200, 72.7], ["food", 450, 27.3], ["other", null, null]]}
