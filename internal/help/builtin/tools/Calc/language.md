---
name: Calc/language
kind: section
description: "Calc language — numbers, strings and lists, operators and their precedence, assignment, your own functions and lambdas, ranges, indexing, if, and the common mistakes with the fix for each."
---
How to write a `Calc` expression. Read this before your first call if you
have not used the tool in this run.

## Statements

`expr` holds one or more statements, separated by `;` or by newlines. Each
statement is an expression, an assignment or a function definition. The value
of the **last** statement is the result; every name you assigned or defined
comes back in `assigned`.

```
price = 1249.99
qty = 3
round(price * qty * 1.2, 2)
```

`#` starts a comment that runs to the end of the line. A newline inside `( )`
or `[ ]` does not end a statement, so a long list can span lines.

## Values

- **Numbers.** `42`, `-3`, `0.1`, `2.5e-3`, `1_000_000` (`_` separates
  digits), `0xff`, `0b1010`, `0o17`. Every literal is exact: `0.1` is the
  fraction 1/10, not a binary float. See `Calc/numbers`.
- **Complex numbers.** An `i` suffix on a number: `2i`, `3 - 1.5i`. A bare
  `i` is an ordinary name. Never exact; see `Calc/complex`.
- **Constants.** `pi`, `e`, `tau` (2·pi) and `phi` (the golden ratio). They
  are approximate.
- **Booleans.** `true`, `false`.
- **Strings.** `"text"` in double quotes, with the escapes `\"`, `\\`, `\n`,
  `\t`. Strings are for labels and for `frombase`; there is no string
  arithmetic.
- **Lists.** `[1, 2, 3]`, nested `[[1, 2], [3, 4]]`, mixed `[1, "a", true]`.

## Operators, loosest first

| Level | Operators | Notes |
|---|---|---|
| 1 | `or` | both sides must be booleans |
| 2 | `and` | |
| 3 | `not` | |
| 4 | `==` `!=` `<` `<=` `>` `>=` | do not chain: write `a < b and b < c` |
| 5 | `a:b`, `a:step:b` | a range, both ends included |
| 6 | `+` `-` | |
| 7 | `*` `/` `%` `.*` `./` | `%` is modulo, the sign of the right side |
| 8 | unary `-` `+` | `-2^2` is `-4`, as on paper |
| 9 | `^` `.^` | power, right-associative: `2^3^2` is `2^9` |
| 10 | `f(x)`, `v[i]`, `A[i, j]`, `t.name`, `n!`, `A'` | call, index, a table's column, factorial, transpose |

Parentheses group as usual. A range binds looser than arithmetic, so `1:n+1`
is `1` to `n+1`.

**Lists.** `+` and `-` work element by element, and a number on either side
is applied to every element: `[1, 2, 3] + 1` is `[2, 3, 4]`. `*` multiplies a
list by a number and `/` divides it by one. For anything else — `/` between
two lists, a number divided by a list, a power with a list in it — write the
element-wise operator: `.*`, `./`, `.^`. Two lists combined element by
element must have the same length, two matrices the same shape.

**Matrices.** A list of rows of the same length is a matrix. `*` between two
matrices, or a matrix and a vector, is the matrix product; between two plain
vectors it is refused (write `dot(a, b)` or `.*`). The postfix `'` is the
transpose: `A'`. See `Calc/matrices`.

## Names, assignment and your own functions

- `name = expr` assigns. Names are letters, digits and `_`, starting with a
  letter or `_`, and are case-sensitive.
- `f(x, y) = expr` defines a function; call it as `f(2, 3)`.
- `x -> expr` and `(x, y) -> expr` are lambdas: functions without a name, for
  `map`, `filter`, `reduce`, `sort` and `count`. A lambda can be assigned:
  `sq = x -> x * x`.
- A function may call another one but never itself: there is no recursion.
  Use a closed form, or `reduce` over a range.
- Variables and functions have separate names. A name followed by `(` is a
  call: it calls your function of that name if you defined one (`f(x) = …`,
  or a variable holding a lambda), otherwise the built-in. A name without `(`
  is your variable. So `rate`, `sum`, `count`, `min` or `log` are fine
  variable names, in `expr` and in `vars`: `sum = 10; sum([1, 2]) + sum`
  gives 13. A variable holding a number does not hide the built-in function.
- Defining your own function with a built-in's name replaces the built-in for
  calls, also inside your function's own body: `sum(x) = sum(x) * 2` is a
  self-call and refused as recursion. Give yours another name.
- You cannot assign to a constant (`pi`, `e`, `tau`, `phi`) or a keyword
  (`and`, `or`, `not`, `true`, `false`, `if`), or use one as a parameter: a
  silently changed `e` would give wrong results with no error.

## Ranges and indexing

- `1:5` is `[1, 2, 3, 4, 5]`; `0:0.25:1` steps by 0.25; `10:-3:1` counts
  down. `range(a, b, step?)` is the same.
- **Indexing starts at 1**: `v[1]` is the first element, `v[len(v)]` the
  last. There is no index 0 and no negative index.
- A nested list is indexed one level at a time, `m[2][1]`, or as a matrix,
  `m[2, 1]`. A lone `:` takes every row or column: `m[2, :]` is row 2,
  `m[:, 1]` column 1.
- Strings cannot be indexed.
- A table's column is `t.price` (or `t["unit price"]`), its row `t[2]`, a
  cell `t[2].price`; see `Calc/tables`.

## Choosing a value

### if(cond, a, b)

Returns `a` when `cond` is `true`, else `b`; only the chosen branch is
evaluated, so the other may hold an expression that would fail. `cond` must be
a boolean: compare, do not pass a number.

Example: `if(1249.99 * 3 > 3000, "free shipping", "pay shipping")` gives `free shipping`.

## Common mistakes

| You wrote | Write instead | Why |
|---|---|---|
| `2**10` | `2^10` | power is `^`; `**` is refused |
| `20% * price` or `price * 20%` | `price * 0.2` or `price * 20 / 100` | `%` is modulo, not percent |
| `v[0]` | `v[1]` | indexing starts at 1 |
| `[1, 2] * [3, 4]` | `[1, 2] .* [3, 4]` or `dot([1, 2], [3, 4])` | `*` between two vectors is refused |
| `'text'` | `"text"` | strings use double quotes; `'` is the transpose |
| `1 / v`, `v ^ 2` | `1 ./ v`, `v .^ 2` | the element-wise forms |
| `if(x = 1, …)` | `if(x == 1, …)` | `=` assigns, `==` compares |
| `2x`, `3(x + 1)` | `2*x`, `3*(x + 1)` | there is no implicit multiplication |
| `ln(x)`, `nCr(n, k)` | `log(x)`, `binomial(n, k)` | the names are the ones in the table |
| `f(n) = n * f(n - 1)` | `product(1:n)` or `n!` | no recursion |

## Examples

Several statements; the last one is the result:

```json
{"expr": "subtotal = 1249.99 * 3; tax = round(subtotal * 0.2, 2); subtotal + tax"}
```

Result: 4499.96

Your own function and a lambda:

```json
{"expr": "area(r) = pi * r^2; map([1, 2, 3], r -> round(area(r), 2))"}
```

Result: [3.14, 12.57, 28.27]

Data passed in `vars`, filtered with a lambda:

```json
{"expr": "sum(filter(amounts, a -> a > 100))", "vars": {"amounts": [120, 80, 310.5, 99.99]}}
```

Result: 430.5

A variable named like a built-in function; the call still takes the built-in:

```json
{"expr": "rate = 0.05; sum = 1000; count = 3; sum * (1 + rate) ^ count + sum([1, 2])"}
```

Result: 1160.625

`%` is modulo, so a percentage is written as a fraction:

```json
{"expr": "price * 15 / 100", "vars": {"price": 84}}
```

Result: 12.6

Indexing starts at 1:

```json
{"expr": "v = [10, 20, 30]; v[0]"}
```

Result: error index
