package calc

import (
	"math/cmplx"
)

// Complex matrix products. gonum's matrices are real (its CDense has no
// product), so `*` between operands holding a complex element, and A^n of
// such a matrix, run this loop in complex128: the same shapes, caps, steps
// and gate as the real product, through heavy. The result is complex in
// every element, even one whose imaginary part comes out 0, as for complex
// arithmetic on numbers. Everything else in matrices (inv, solve, det, the
// decompositions) still takes real numbers.

// anyComplex reports a complex element among the operand rows.
func anyComplex(rows [][]val) bool {
	for _, row := range rows {
		for _, x := range row {
			if isComplex(x) {
				return true
			}
		}
	}
	return false
}

// complexRows reads operand rows of real or complex numbers as complex128,
// charging one step per element. The rows' shape was checked by the
// caller (matDims or the vector bound).
func (e *evaluator) complexRows(rows [][]val, what string, pos int) ([][]complex128, *Error) {
	out := make([][]complex128, len(rows))
	for i, row := range rows {
		if err := e.charge(len(row), pos); err != nil {
			return nil, err
		}
		out[i] = make([]complex128, len(row))
		for j, x := range row {
			z, ok := asComplex(x)
			if !ok {
				return nil, errf(codeType, pos, "%s needs numbers, got a %s element", what, typeName(x))
			}
			if cmplx.IsInf(z) {
				return nil, errf(codeOverflow, pos, "%s: an element is too large for a 64-bit float", what)
			}
			out[i][j] = z
		}
	}
	return out, nil
}

// cmul is the complex product a × b, admitted by the caller; the ctx is
// polled once per row of the result.
func (e *evaluator) cmul(a, b [][]complex128, pos int) ([][]complex128, *Error) {
	k, c := len(b), len(b[0])
	out := make([][]complex128, len(a))
	for i, arow := range a {
		if err := e.ctx.Err(); err != nil {
			return nil, ctxErr(err, pos)
		}
		row := make([]complex128, c)
		for p := range k {
			x := arow[p]
			for j, y := range b[p] {
				row[j] += x * y
			}
		}
		out[i] = row
	}
	return out, nil
}

// valOfComplexRows returns a complex matrix as rows of complex values, as a
// vector when vec is set; an element that overflowed is refused.
func (e *evaluator) valOfComplexRows(m [][]complex128, vec bool, what string, pos int) (val, *Error) {
	out := make([]val, 0, len(m))
	for _, row := range m {
		if err := e.charge(len(row), pos); err != nil {
			return nil, err
		}
		vals := make([]val, len(row))
		for j, z := range row {
			if cmplx.IsNaN(z) || cmplx.IsInf(z) {
				// Inf times a zero part gives NaN: either way it overflowed.
				return nil, errf(codeOverflow, pos, "%s overflows a 64-bit float", what)
			}
			v, err := checkComplex(z, pos, what)
			if err != nil {
				return nil, err
			}
			vals[j] = v
		}
		if vec {
			out = append(out, vals...)
		} else {
			out = append(out, vals)
		}
	}
	return out, nil
}

// complexMatMul is `*` of two operands, at least one holding a complex
// element. l and r are the values, for the shape refusal.
func (e *evaluator) complexMatMul(l, r val, lr, rr [][]val, vec bool, pos int) (val, *Error) {
	const what = "the matrix product"
	for _, side := range []val{l, r} {
		if isMatrix(side) {
			if _, _, err := matDims(side, what, pos); err != nil {
				return nil, err
			}
		} else if n := len(side.([]val)); n == 0 {
			return nil, errf(codeType, pos, "%s needs a vector (a list of numbers), got an empty list", what)
		} else if n > MaxMatrixElems {
			return nil, errf(codeTooLarge, pos, "%s: the vector has %d elements; the limit is %d", what, n, MaxMatrixElems)
		}
	}
	ar, ak, br, bc := len(lr), len(lr[0]), len(rr), len(rr[0])
	if ak != br {
		return nil, errf(codeShape, pos, "the matrix product needs the columns of the left to match the rows of the right, got %s and %s", shapeText(l), shapeText(r)).
			withHint("transpose one side with ', or use .* to multiply element by element")
	}
	if ar*bc > MaxMatrixElems {
		return nil, errf(codeTooLarge, pos, "the product would be %d × %d; the limit is %d elements", ar, bc, MaxMatrixElems)
	}
	a, err := e.complexRows(lr, what, pos)
	if err != nil {
		return nil, err
	}
	b, err := e.complexRows(rr, what, pos)
	if err != nil {
		return nil, err
	}
	release, err := e.heavy(ar*ak+br*bc, float64(ar)*float64(ak)*float64(bc), what, pos)
	if err != nil {
		return nil, err
	}
	p, err := e.cmul(a, b, pos)
	release()
	if err != nil {
		return nil, err
	}
	return e.valOfComplexRows(p, vec, what, pos)
}

// complexMatPowOf is A^k of a square matrix holding a complex element: the
// identity for k = 0 once the elements are checked, refused for k < 0 (the
// inverse is a real routine), else complexMatPow.
func (e *evaluator) complexMatPowOf(rows [][]val, k int64, pos int) (val, *Error) {
	if k < 0 {
		return nil, errf(codeType, pos, "a negative matrix power needs the inverse, which takes real numbers, and the matrix has a complex element").
			withHint(complexWiseHint)
	}
	if k == 0 {
		if _, err := e.complexRows(rows, "the matrix power", pos); err != nil {
			return nil, err
		}
		return identityRows(len(rows)), e.charge(len(rows)*len(rows), pos)
	}
	return e.complexMatPow(rows, uint64(k), pos)
}

// complexMatPow is A^k, k >= 1, of a square matrix holding a complex
// element; its products are admitted together before any runs.
func (e *evaluator) complexMatPow(rows [][]val, k uint64, pos int) (val, *Error) {
	const what = "the matrix power"
	n := len(rows)
	a, err := e.complexRows(rows, what, pos)
	if err != nil {
		return nil, err
	}
	release, err := e.heavy(n*n, float64(powProducts(k))*float64(n)*float64(n)*float64(n), what, pos)
	if err != nil {
		return nil, err
	}
	p, err := powBySquaring(a, k, func(x, y [][]complex128) ([][]complex128, *Error) { return e.cmul(x, y, pos) })
	release()
	if err != nil {
		return nil, err
	}
	return e.valOfComplexRows(p, false, what, pos)
}

// fnCtranspose is the conjugate transpose: the transpose with each complex
// element conjugated. A real element is its own conjugate and keeps its
// exactness, so on a real matrix it is the transpose.
func fnCtranspose(e *evaluator, a args) (val, *Error) {
	t, err := fnTranspose(e, a)
	if err != nil {
		return nil, err
	}
	rows := t.([]val)
	out := make([]val, len(rows))
	for i, row := range rows {
		src := row.([]val)
		dst := make([]val, len(src))
		for j, x := range src {
			if z, ok := x.(cnum); ok {
				if x, err = checkComplex(cmplx.Conj(complex128(z)), a.pos, "ctranspose"); err != nil {
					return nil, err
				}
			}
			dst[j] = x
		}
		out[i] = dst
	}
	return out, nil
}
