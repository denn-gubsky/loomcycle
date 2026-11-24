---
name: Calc/matrices
kind: section
description: "Calc matrices and linear algebra — a matrix as a list of rows, the matrix product, transpose A', indexing A[i, j], building matrices, dot and cross products, determinant, rank, norms, inverse, solving and least squares, the LU, QR, Cholesky, eigen and singular value decompositions, and statistics by column."
---
A matrix is a list of rows of the same length, `[[1, 2], [3, 4]]`; a list of
numbers is a vector. There is no separate matrix type.

Building, rearranging and the products (`zeros` to `size`, `dot`, `cross`,
`kron`, `trace`, `*` and `A^n` for `n` ≥ 1) keep exact numbers exact; `*` and
`A^n` only up to 50 per side and while the result stays within the exact
cap, past which they are floats. Solving and the decompositions are
computed in 64-bit floats, so the result is not exact even for whole
numbers; round it for display.

## Operators and indexing

- `A * B` is the matrix product. A vector on the right is a column and the
  result a vector; a vector on the left is a row. `*` between two vectors is
  refused: write `dot(a, b)`, or `.*` for element by element.
- `A^n` is the matrix power of a square matrix and an integer `n`: `A^0`
  is the identity, `A^-2` is `inv(A)^2` (refused for a singular `A`). There
  is no fractional power; `.^` raises each element, and a vector takes only
  `.^`.
- `A'` is the transpose (also `transpose(A)`), never conjugating; a
  vector `v'` is the one-row matrix `[v]`. `ctranspose(A)` conjugates too.
- With a complex element, `*` and `A^n` (`n` ≥ 0) compute in complex
  numbers; the other matrix functions refuse one (`Calc/complex`).
- `+`, `-`, `.*`, `./`, `.^` work element by element on two matrices of the
  same shape, and a number applies to every element: `2 * A`, `A + 1`. A
  matrix plus a vector is refused (`shape`).
- `A[i, j]` is one element, `A[i, :]` row `i`, `A[:, j]` column `j`, from 1.
  A list of indexes picks several: `A[[1, 3], :]`.

## Building

### zeros(r, c?)
An `r` × `c` matrix of zeros; with one argument a list of `r` zeros.

Example: `zeros(2, 3)` gives `[[0, 0, 0], [0, 0, 0]]`.

### ones(r, c?)
The same, filled with ones.

Example: `ones(3)` gives `[1, 1, 1]`.

### identity(n)
The `n` × `n` identity matrix.

Example: `identity(2)` gives `[[1, 0], [0, 1]]`.

### diag(x)
For a vector, the square matrix with it on the diagonal; for a matrix, its
diagonal.

Example: `diag([[1, 2], [3, 4]])` gives `[1, 4]`.

### reshape(v, r, c)
The elements of `v` (a matrix is read row by row) as an `r` × `c` matrix,
filled row by row. The count must match.

Example: `reshape(1:6, 2, 3)` gives `[[1, 2, 3], [4, 5, 6]]`.

### hstack(A, B, ...)
Matrices side by side; the row counts must match. A vector is a column.

Example: `hstack([[1], [2]], [3, 4])` gives `[[1, 3], [2, 4]]`.

### vstack(A, B, ...)
Matrices one above the other; the column counts must match. A vector is a
row.

Example: `vstack([1, 2], [[3, 4]])` gives `[[1, 2], [3, 4]]`.

### linspace(a, b, n)
`n` evenly spaced numbers from `a` to `b`, both included. Exact on exact
input.

Example: `linspace(0, 1, 5)` gives `[0, 0.25, 0.5, 0.75, 1]`.

### transpose(A)
Rows become columns; the same as `A'`.

Example: `transpose([[1, 2, 3], [4, 5, 6]])` gives `[[1, 4], [2, 5], [3, 6]]`.

### ctranspose(A)
The conjugate transpose: `A'` with each complex element conjugated. On a
real matrix it is `A'`.

Example: `ctranspose([[1 + 2i, 3]])` gives `[[1-2i], [3]]`.

### size(x)
The shape: `[rows, columns]` of a matrix, `[n]` of a list.

Example: `size([[1, 2, 3], [4, 5, 6]])` gives `[2, 3]`.

## Products and measures

### dot(a, b)
The dot product of two vectors of the same length. Exact on exact input.

Example: `dot([1, 2, 3], [4, 5, 6])` gives `32`.

### cross(a, b)
The cross product of two vectors of 3 elements. Exact on exact input.

Example: `cross([1, 0, 0], [0, 1, 0])` gives `[0, 0, 1]`.

### kron(A, B)
The Kronecker product: every element of `A` times the whole of `B`. Exact on
exact input.

Example: `kron([[1, 2]], [[1], [10]])` gives `[[1, 2], [10, 20]]`.

### trace(A)
The sum of the diagonal of a square matrix. Exact on exact input.

Example: `trace([[1, 2], [3, 4]])` gives `5`.

### det(A)
The determinant of a square matrix. A float.

Example: `det([[4, 3], [6, 3]])` gives `-6`.

### matrank(A)
The rank: how many rows are independent, counting singular values above
NumPy's tolerance. `rank` is the statistics function that ranks values.

Example: `matrank([[1, 2], [2, 4]])` gives `1`.

### norm(x, kind?)
A vector's length by default (`kind` 2); `1` is the sum of absolute values,
`"inf"` the largest. For a matrix the default is `"fro"`, the square root of
the sum of squares; `1` is the largest column sum, `"inf"` the largest row
sum, `2` the largest singular value.

Example: `norm([3, 4])` gives `5`.

### cond(A)
The condition number: the largest singular value over the smallest. Above
about 1e12, answers computed from `A` lose most of their digits. A singular
matrix is refused.

Example: `cond([[1, 0], [0, 4]])` gives `4`.

## Solving

### inv(A)
The inverse of a square matrix. A singular matrix, or one so close to
singular that the answer would be noise (condition number over 1e16), is
refused (`domain`). For `A*x = b`, `solve` is more accurate.

Example: `inv([[4, 7], [2, 6]])` gives `[[0.6, -0.7], [-0.2, 0.4]]`.

### pinv(A)
The Moore-Penrose pseudo-inverse, for any matrix, singular or not square.

Example: `pinv([[1, 2], [2, 4]])` gives `[[0.04, 0.08], [0.08, 0.16]]`.

### solve(A, b)
The `x` with `A*x = b`, for a square `A`. `b` is a vector, or a matrix for
several right-hand sides. A singular `A` is refused (`domain`).

Example: `solve([[2, 1], [1, 3]], [3, 5])` gives `[0.8, 1.4]`.

### lstsq(A, b)
The least-squares `x`, making `A*x` as close to `b` as it can be, for any
`A`; when many `x` do equally well, the shortest. A straight-line fit is
`lstsq(hstack(ones(n), x), y)`.

Example: `round(lstsq([[1, 1], [1, 2], [1, 3]], [1, 2, 2]), 6)` gives `[0.666667, 0.5]`.

## Decompositions

Each returns a list of its factors; take them apart by index.

### lu(A)
`[L, U, P]` for a square `A`: `L` lower triangular with ones on the
diagonal, `U` upper triangular, `P` a permutation, and `P*A = L*U`.

Example: `lu([[0, 1], [1, 1]])[3]` gives `[[0, 1], [1, 0]]`.

### qr(A)
`[Q, R]` with `A = Q*R`, `Q` orthogonal (`Q'*Q` is the identity) and `R`
upper triangular, for a matrix with at least as many rows as columns.

Example: `r = qr([[3, 4], [4, 3]]); round(r[1] * r[2], 9)` gives `[[3, 4], [4, 3]]`.

### cholesky(A)
`L`, lower triangular, with `A = L*L'`, for a symmetric positive definite
`A`; anything else is refused (`domain`).

Example: `cholesky([[4, 2], [2, 2]])` gives `[[2, 0], [1, 1]]`.

### eig(A)
`[values, vectors]`: the eigenvalues, and the eigenvectors as the columns of
a matrix, so `A*v = λ*v` for column `v`, each of length 1. A symmetric matrix
has real eigenvalues, listed ascending. Any other square matrix may have
complex ones (`Calc/complex`), in conjugate pairs: a value whose imaginary
part is 0 is a real number, and the values are listed by real part, then
imaginary part, each column of vectors following its value.

Example: `eig([[2, 1], [1, 2]])[1]` gives `[1, 3]`.

Example: `round(eig([[0, -1], [1, 0]])[1], 12)` gives `[-1i, 1i]`.

### svd(A)
`[U, s, V]` with `A = U*diag(s)*V'`: the singular values `s` largest first,
`U` and `V` with orthonormal columns.

Example: `svd([[3, 0], [0, -4]])[2]` gives `[4, 3]`.

## Statistics by column

`mean`, `median`, `variance` and `stdev` of a matrix work column by column
and return one value per column; `sum` and `product` of a matrix add or
multiply its rows, which is also per column. Of a vector they work as in
`Calc/statistics`.

## Limits

A matrix a function reads or builds holds at most 250,000 elements
(`too_large`). The product, the solvers and the decompositions are refused,
before any work, when they would cost more than a 500 × 500 matrix: rows ×
columns × the smaller of the two, at most 125,000,000; `A^n` counts each of
its products, about two per doubling of `n`. Reading and writing a
matrix costs a step per element, so one call handles a few matrices of that
size at most. See `Calc/limits`.

## Examples

Solve a system of two equations, `2x + y = 3` and `x + 3y = 5`:

```json
{"expr": "solve([[2, 1], [1, 3]], [3, 5])"}
```

Result: [0.8, 1.4]

A matrix from `vars`, its transpose times itself:

```json
{"expr": "A' * A", "vars": {"A": [[1, 2], [3, 4], [5, 6]]}}
```

Result: [[35, 44], [44, 56]]

Column means of a table of measurements:

```json
{"expr": "mean(m)", "vars": {"m": [[1, 10], [2, 20], [3, 60]]}}
```

Result: [2, 30]

Fibonacci numbers from a matrix power, exact past 2^53:

```json
{"expr": "([[1, 1], [1, 0]]^99)[1, 1]"}
```

Result: 354224848179261915075

`*` between two vectors is refused:

```json
{"expr": "[1, 2] * [3, 4]"}
```

Result: error type
