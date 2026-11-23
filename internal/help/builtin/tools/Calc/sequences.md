---
name: Calc/sequences
kind: section
description: "Calc list functions — range, len, sum and product, map, filter and reduce (list and function in either order), sort, unique, count, any and all, zip and reverse, and the random functions with seed."
---
Functions on lists. A function argument is a lambda (`x -> x^2`) or the name
of a function you defined (see `Calc/language`). `length` is not a name
here: write `len`. `size` gives a shape, `[rows, columns]` (see
`Calc/matrices`).

## Building and measuring

### range(a, b, step?)
The list `a`, `a+step`, … up to `b` inclusive; `step` defaults to 1 and may
be negative or a fraction. Same as `a:b` and `a:step:b`.

Example: `range(10, 1, -3)` gives `[10, 7, 4, 1]`.

### len(v)
The number of elements of a list, of characters of a string, or of rows of a
table.

Example: `len([4, 8, 15, 16, 23, 42])` gives `6`.

### sum(v) or sum(a, b, ...)
The sum of a list or of the arguments; `0` for an empty list. Exact on exact
input.

Example: `sum(1:100)` gives `5050`.

### product(v) or product(a, b, ...)
The product of a list or of the arguments; `1` for an empty list. Exact on
exact input.

Example: `product([1.5, 2, 4])` gives `12`.

### reverse(v)
`v` in reverse order. Strings are refused.

Example: `reverse(1:4)` gives `[4, 3, 2, 1]`.

### zip(a, b, ...)
The list of `[a[i], b[i], ...]`, as long as the shortest list.

Example: `zip(["a", "b"], [1, 2])` gives `[["a", 1], ["b", 2]]`.

## Transforming

`map`, `filter`, `reduce`, `count`, `any` and `all` take the list and the
function **in either order**: `map(v, f)` and `map(f, v)` are the same call.

### map(v, f)
The list of `f(x)` for each element `x`.

Example: `map(1:5, x -> x^2)` gives `[1, 4, 9, 16, 25]`.

### filter(v, pred)
The elements `x` of `v` for which `pred(x)` is `true`. `pred` must return a
boolean.

Example: `filter(1:10, x -> x % 3 == 0)` gives `[3, 6, 9]`.

### reduce(v, f, init?)
Folds `v` with `f(acc, x)`, starting from `init`, or from the first element
when `init` is omitted.

Example: `reduce([2, 3, 4], (acc, x) -> acc * 10 + x)` gives `234`.

### sort(v, key?) or sort(t, column, order?)
`v` ascending: all numbers or all strings. With `key`, sorts by `key(x)`, so
lists sort by a field and `x -> -x` sorts descending. Stable. A table sorts
by one column, `"asc"` (the default) or `"desc"`, empty cells last
(`Calc/tables`).

Example: `sort([[3, "c"], [1, "a"]], p -> p[1])` gives `[[1, "a"], [3, "c"]]`.

### unique(v)
`v` without repeats, first occurrences kept, in order.

Example: `unique([3, 1, 3, 2, 1])` gives `[3, 1, 2]`.

## Counting and testing

### count(v, pred_or_value?)
The number of elements, not counting empty table cells; with a function, of
elements where it holds; with a value, of elements equal to it. Of a table:
its rows, or with a function, the rows `r` where it holds.

Example: `count([1, 2, 2, 3, 2], 2)` gives `3`.

### any(v, pred?)
Whether some element is `true`, or satisfies `pred`. `false` for an empty
list.

Example: `any([3, 8, 12], x -> x > 10)` gives `true`.

### all(v, pred?)
Whether every element is `true`, or satisfies `pred`. `true` for an empty
list.

Example: `all([3, 8, 12], x -> x > 5)` gives `false`.

## Random

Without `seed` each call draws new values. Pass `seed` (an integer) to make a
call repeatable: the same expression with the same seed gives the same
result.

### random()
A uniform float in [0, 1).

Example: `random() < 1` gives `true`.

### randint(a, b)
A uniform integer in [`a`, `b`], both ends included. Refused when `a > b`.

Example: `randint(5, 5)` gives `5`.

### randnormal(mu?, sigma?)
A sample from a normal distribution with mean `mu` (default 0) and standard
deviation `sigma` (default 1).

Example: `randnormal(10, 0) == 10` gives `true`.

### shuffle(v)
`v` in a uniformly random order.

Example: `sort(shuffle([3, 1, 2]))` gives `[1, 2, 3]`.

### sample(v, k)
`k` distinct elements of `v` (distinct positions), chosen without
replacement. Refused when `k` is above `len(v)`.

Example: `len(sample(1:100, 5))` gives `5`.

## Examples

The average of a list:

```json
{"expr": "sum(scores) / len(scores)", "vars": {"scores": [82, 91, 77, 88]}}
```

Result: 84.5

Orders over 100, with a 5% discount each, summed:

```json
{"expr": "sum(map(filter(orders, o -> o > 100), o -> o * 0.95))", "vars": {"orders": [80, 120, 250, 99]}}
```

Result: 351.5

The order lines sorted by total, largest first:

```json
{"expr": "sort(zip(skus, qty .* price), p -> -p[2])", "vars": {"skus": ["A1", "B2", "C3"], "qty": [3, 1, 10], "price": [4.5, 30, 1.25]}}
```

Result: [["B2", 30], ["A1", 13.5], ["C3", 12.5]]

Three dice, repeatable with a seed:

```json
{"expr": "[randint(1, 6), randint(1, 6), randint(1, 6)]", "seed": 7}
```

Result: [3, 1, 4]
