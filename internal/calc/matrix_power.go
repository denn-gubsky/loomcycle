package calc

import (
	"math"
	"math/bits"

	"gonum.org/v1/gonum/mat"
)

// maxMatPowExp bounds |n| in A^n: past 2^53 an exponent written as a float
// is no longer an integer a float64 can tell apart, as for complexPow.
const maxMatPowExp = 1 << 53

// powProducts is how many matrix products A^k takes by repeated squaring:
// a squaring per bit after the highest, and a multiplication per further
// set bit. A^1 takes none.
func powProducts(k uint64) int {
	return bits.Len64(k) - 1 + bits.OnesCount64(k) - 1
}

// powBySquaring raises base to k >= 1 with powProducts(k) calls of mul.
func powBySquaring[T any](base T, k uint64, mul func(T, T) (T, *Error)) (T, *Error) {
	var acc T
	have := false
	for {
		if k&1 == 1 {
			if have {
				var err *Error
				if acc, err = mul(acc, base); err != nil {
					return acc, err
				}
			} else {
				acc, have = base, true
			}
		}
		k >>= 1
		if k == 0 {
			return acc, nil
		}
		var err *Error
		if base, err = mul(base, base); err != nil {
			return acc, err
		}
	}
}

// matPow is A^n, `^` with a matrix on the left: a square matrix and an
// integer n. n = 0 is the identity, n > 0 repeated squaring, n < 0 the
// inverse raised to |n|. An exact matrix to n > 0 is computed exactly within
// the caps of exactMatPow; a matrix holding a complex element in complex
// arithmetic; a negative power goes through the float inverse.
// The float work (the products, and the inverse) is admitted before any of
// it runs, as one routine of heavy.
func (e *evaluator) matPow(l, r val, pos int) (val, *Error) {
	k, err := matPowExponent(r, pos)
	if err != nil {
		return nil, err
	}
	rows, cols, err := matDims(l, "the matrix power", pos)
	if err != nil {
		return nil, err
	}
	if rows != cols {
		return nil, errf(codeShape, pos, "the matrix power needs a square matrix, got %d × %d", rows, cols).
			withHint("use .^ to raise element by element")
	}
	rs := productRows(l, true, false)
	if anyComplex(rs) {
		return e.complexMatPowOf(rs, k, pos)
	}
	if k > 0 {
		if v, ok, err := e.exactMatPow(rs, uint64(k), pos); ok || err != nil {
			return v, err
		}
	}
	m, err := e.denseOf(l, "the matrix power", pos)
	if err != nil {
		return nil, err
	}
	if k == 0 {
		return identityRows(rows), e.charge(rows*rows, pos)
	}
	return e.floatMatPow(m, rows, k, pos)
}

// matPowExponent reads the exponent of A^n: an integer, written exactly or
// as a whole float, of at most maxMatPowExp.
func matPowExponent(r val, pos int) (int64, *Error) {
	n, ok := r.(num)
	if !ok {
		return 0, errf(codeType, pos, "the matrix power A^n needs an integer n, got a %s", typeName(r)).
			withHint("use .^ to raise element by element")
	}
	if k, ok := smallInt(n, maxMatPowExp); ok {
		return k, nil
	}
	if n.exact() && n.r.IsInt() || !n.exact() && n.f == math.Trunc(n.f) {
		return 0, errf(codeTooLarge, pos, "the matrix power's exponent is over 2^53")
	}
	return 0, errf(codeDomain, pos, "the matrix power A^n needs an integer n, got %s", fmtArg(n)).
		withHint("there is no fractional matrix power; use .^ to raise each element")
}

// identityRows is the exact n × n identity.
func identityRows(n int) val {
	one, zero := exactInt64(1), exactInt64(0)
	rows := make([]val, n)
	for i := range rows {
		row := make([]val, n)
		for j := range row {
			row[j] = zero
		}
		row[i] = one
		rows[i] = row
	}
	return rows
}

// floatMatPow is A^k in float64 through gonum. The work, powProducts(|k|)
// products of n³ and for a negative k an inverse, is admitted before any of
// it runs; an element that overflows on the way is refused, never returned.
func (e *evaluator) floatMatPow(m *mat.Dense, n int, k int64, pos int) (val, *Error) {
	uk := uint64(k)
	if k < 0 {
		uk = uint64(-k)
	}
	cube := float64(n) * float64(n) * float64(n)
	work := float64(powProducts(uk)) * cube
	if k < 0 {
		work += cube
	}
	release, err := e.heavy(n*n, work, "the matrix power", pos)
	if err != nil {
		return nil, err
	}
	var out *mat.Dense
	err = guard("the matrix power", pos, func() *Error {
		base := m
		if k < 0 {
			var inv mat.Dense
			if inv.Inverse(m) != nil {
				return errf(codeDomain, pos, "a negative matrix power needs the inverse, and the matrix is singular, or so close to it (condition number over 1e16) that the answer would be noise")
			}
			base = &inv
		}
		var perr *Error
		out, perr = powBySquaring(base, uk, func(x, y *mat.Dense) (*mat.Dense, *Error) {
			var p mat.Dense
			p.Mul(x, y)
			return &p, nil
		})
		return perr
	})
	release()
	if err != nil {
		return nil, err
	}
	for _, f := range out.RawMatrix().Data {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			// Inf times a zero gives NaN, so either way an element overflowed.
			return nil, errf(codeOverflow, pos, "the matrix power overflows a 64-bit float")
		}
	}
	return e.valOfDense(out, "the matrix power", pos)
}
