---
name: Calc/errors
kind: section
description: "Calc errors — every error code a refused call returns, what it means, and what to change before you call again."
---
A refused call returns `{"error": <code>, "detail": …, "hint": …, "pos": …}`.
`detail` says what went wrong, `hint` (when present) what to write instead,
and `pos` (when present) is the byte offset in `expr` where it went wrong.
Fix the expression and call again; sending the same call again fails the same
way. Never fall back to working the number out yourself: the refusal tells
you what to change.

## Fix the expression

### syntax
The expression is not well formed: a missing `)` or `,`, an operator that
does not exist (`**`), a number run into a name (`2x`), `=` where `==` was
meant, or chained comparisons (`a < b < c`). Read `hint`, fix the spot at
`pos`, and see the common mistakes in `Calc/language`.

### unknown_name
A name that is neither assigned, a var nor a constant. Check the spelling,
assign it first, or pass it in `vars`. `hint` lists close names.

### unknown_function
No built-in or defined function has that name. `hint` names the one to use
(`ln` → `log`, `nCr` → `binomial`) or close names. Define your own with
`f(x) = …` before calling it.

### arity
The function got the wrong number of arguments. `hint` shows its signature.

### type
A value of the wrong kind: a string in arithmetic, a number where a list or
a function is needed, a float where an exact integer is needed (`factor`,
`gcd`, `binomial` …), `*` between two plain vectors, `/` between two lists, a
number divided by a list, or a power of a list, a vector where a matrix is
needed; a date meeting a datetime, or a date in `*`; a complex number
ordered (`<`, `sort`, `min`, statistics) or given to a matrix function. For
lists use `.*`, `./`, `.^`; for an integer argument compute it exactly; for
dates convert with `date(dt)` or `datetime(d)`; for a complex number take
`abs(z)` or `re(z)`.

### index
An index that does not exist: `v[0]`, a negative index, past `len(v)`, or not
a whole number; a table row likewise, or two indexes on a table (write
`t[i].name`). Indexing starts at 1.

### shape
Two lists combined element by element have different lengths, or two
matrices different shapes; a matrix product whose inner sizes differ; a
matrix with rows of different lengths; a function needing a square matrix
given another. `detail` names both shapes. Make them match, transpose one side
with `'`, or index the part you need.

### reserved_name
You used a constant (`pi`, `e`, `tau`, `phi`) or a keyword (`if`, `and` …) as
a name: assigned to it, defined a function or a parameter with it, or passed
it as a var name. Pick another name, e.g. `rate_e`. Built-in function names
are not reserved: `sum = 10` is a variable and `sum([1, 2])` still calls the
built-in.

### recursion
A function calls itself. Use a closed form (`n!`, `fib(n)`), or `reduce` /
`product` over a range.

### bad_var
A var cannot be used: its name is not a valid name, its value is `null`, or
it is an object that is not a table, or a table with a short row, a repeated
or non-string column name, or a list or object in a cell. Pass numbers,
strings, booleans, lists, or a table (`Calc/tables`); send each field of any
other object as its own var.

### column
A table has no column of that name (`hint` lists close names), a table
function would make two columns of one name (a `pivot` value equal to the row
column's name, `addcol` is fine: it replaces), a column has no name, or a
table has no columns. Check the spelling against the table's `columns`.

### bad_data
A source named in `from` is not a table or value in its format: invalid JSON,
broken CSV quoting, no Markdown table in a chunk, two columns of one name, or
text that is not UTF-8. Fix the source, or name one in the format you meant
(`Calc/data`).

### unknown_unit
A unit string `convert` or `convertdelta` cannot read: a name not in the
table, a wrong case (`Km`, `mb` — units are case-sensitive), a prefix on a
unit that takes none (`kmi`), or a malformed compound (`m2`, `km//h`). Also a
dimension name `units` does not know. `hint` lists close names; see
`Calc/units`.

### dimension
The two units of a conversion measure different things (`kg` to `m`, `lb` to
`lbf`), or `convert` met `degC` or `degF` inside a compound. `detail` names
both dimensions. Pick a unit of the same dimension; for a temperature rate or
difference use `convertdelta`.

### bad_request
The arguments are not the documented shape: `expr` missing or not a string,
`digits` outside 1 to 1000, an argument the tool does not take.

### empty
`expr` holds no statement. Write an expression.

### bad_date
A date, time or zone the tool cannot read: anything but ISO 8601
(`"2026-11-20"`, `"2026-11-20T14:30:00Z"`), an ambiguous `11/12/2026`, an
unknown zone name (use an IANA name such as `"Europe/Kyiv"`), or in a `datefmt`
pattern an unclosed `[` or a token it does not have (`%A`, `EEEE`, `yyyy`; a
letter meant as text goes in `[brackets]`). `hint` shows the form to write. See
`Calc/dates`.

## Fix the mathematics

### domain
The function is not defined there: `sqrt` or `log` of a negative number, a
fractional power of a negative number, `asin(2)`, `min([])`, `modinv` of
numbers that are not coprime, a sample `variance` of one value, a day the
month lacks (`date(2026, 2, 30)`), an unknown `datediff` unit or `yearfrac`
basis, an `irr`, `rate` or `xirr` with no rate that solves the flows, a singular
matrix to `inv` or `solve` (use `pinv` or `lstsq`), `cholesky` of one that is not positive definite, a distribution
parameter out of range (`normcdf(x, 0, 0)`, `norminv(1)`), a `root` bracket
whose ends have the same sign, `interp` outside its table. A real argument
never gives a complex answer: write `sqrt(-1 + 0i)` for one
(`Calc/complex`). Check the inputs; for the finance functions, check the
signs.

### no_convergence
A numeric method did not settle within its evaluations of the function:
`integrate` over a singularity it cannot integrate (`1/x` from 0), `deriv`
at a jump or a kink it cannot resolve, `root` or `minimize` on a function too
rough to close in on. Split the interval at the trouble point, narrow it, or
check that the answer exists.

### division_by_zero
A division, `%`, `mod` or `rem` by zero, or zero to a negative power. Guard
it: `if(d == 0, 0, n / d)`.

### overflow
A float result is too large for 64 bits: `exp(1000)`, `gamma(200)`. Work in
logarithms (`lgamma`, `log`) or keep the computation exact. For dates: a
result outside the years 1 to 9999.

### too_large
An exact number would pass 16,384 bits, or an argument is over a function's
cap (`isprime` and `nextprime` near 2^1024, `factor` and `divisors` at 2^64,
`modpow` at 4,096 bits), or a matrix over 250,000 elements or a matrix
operation over the work of a 500 × 500 matrix. Use a float on purpose (`2.0^x` is still exact;
`exp(x * log(2))` is not) or a smaller problem.

## Make the work smaller

### list_too_long
A list, or all the lists nested in one value, would hold more than 1,000,000
elements, or a table more than 100,000 cells (a `join` or `pivot` is checked
before it is built). Shrink the range, `select` fewer columns, filter rows
first, or split the work.

### step_limit
The evaluation needed more than 1,000,000 steps. Use a closed form (`sum(1:n)`
is `n*(n+1)/2`), smaller ranges, or several calls.

### time_limit
The evaluation ran past 2 seconds. Same remedies as `step_limit`; huge exact
numbers are the usual cause. A large matrix operation may also have waited
for a free slot while other calls ran theirs: resend it.

### too_deep
Brackets nest more than 200 levels deep, or functions call each other more
than 64 deep. Flatten the expression with intermediate names.

### expr_too_long
`expr` is over 16 KB. Pass the data in `vars` and keep `expr` for the
formula.

### vars_too_large
The vars total over 1 MB. Pass less data, or split it over several calls.

### canceled
The run stopped while the call was evaluating. Call again if the run
continues.

## Examples

A float where an exact integer is needed:

```json
{"expr": "binomial(10.5, 2)"}
```

Result: error type

Guarding a division:

```json
{"expr": "if(visits == 0, 0, round(100 * orders / visits, 1))", "vars": {"orders": 0, "visits": 0}}
```

Result: 0
