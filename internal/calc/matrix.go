package calc

import (
	"fmt"
	"math"
	"strings"

	"gonum.org/v1/gonum/mat"
)

// Matrices are not a separate value: a list of equal-length lists of
// numbers is a matrix wherever a matrix operation meets it, and a list of
// numbers is a vector. The construction and rearranging functions (zeros,
// diag, reshape, hstack, transpose, ...) keep the values they are given, so
// exact numbers stay exact; everything computed through gonum (the matrix
// product, solving, the decompositions) is float64, so its result is never
// exact.

// isMatrix is the cheap structural test the operators dispatch on: a
// non-empty list whose elements are all lists. denseOf checks the rest.
func isMatrix(v val) bool {
	l, ok := v.([]val)
	if !ok || len(l) == 0 {
		return false
	}
	for _, x := range l {
		if _, ok := x.([]val); !ok {
			return false
		}
	}
	return true
}

// matDims returns the rows and columns of a matrix value, refusing a ragged
// one or one over MaxMatrixElems.
func matDims(v val, what string, pos int) (r, c int, err *Error) {
	l, ok := v.([]val)
	if !ok || len(l) == 0 {
		return 0, 0, errf(codeType, pos, "%s needs a matrix (a list of equal-length lists of numbers), got %s", what, describeVal(v))
	}
	first, ok := l[0].([]val)
	if !ok {
		return 0, 0, errf(codeType, pos, "%s needs a matrix (a list of equal-length lists of numbers), got a list of numbers", what).
			withHint("write a matrix as rows, e.g. [[1, 2], [3, 4]]")
	}
	r, c = len(l), len(first)
	if c == 0 {
		return 0, 0, errf(codeShape, pos, "%s: the matrix has empty rows", what)
	}
	for i, x := range l {
		row, ok := x.([]val)
		if !ok || len(row) != c {
			return 0, 0, errf(codeShape, pos, "%s: row %d has %s, row 1 has %d; a matrix needs rows of the same length", what, i+1, lenText(x), c)
		}
	}
	if r*c > MaxMatrixElems {
		return 0, 0, errf(codeTooLarge, pos, "%s: the matrix is %d × %d, %d elements; the limit is %d", what, r, c, r*c, MaxMatrixElems)
	}
	return r, c, nil
}

func describeVal(v val) string {
	if l, ok := v.([]val); ok && len(l) == 0 {
		return "an empty list"
	}
	return "a " + typeName(v)
}

func lenText(x val) string {
	if l, ok := x.([]val); ok {
		return fmt.Sprintf("%d elements", len(l))
	}
	return "a " + typeName(x) + " in place of a row"
}

// denseOf reads a matrix value into a gonum matrix, charging one step per
// element.
func (e *evaluator) denseOf(v val, what string, pos int) (*mat.Dense, *Error) {
	r, c, err := matDims(v, what, pos)
	if err != nil {
		return nil, err
	}
	if err := e.charge(r*c, pos); err != nil {
		return nil, err
	}
	data := make([]float64, 0, r*c)
	for _, row := range v.([]val) {
		for _, x := range row.([]val) {
			n, ok := x.(num)
			if !ok {
				return nil, realElementErr(what, "a matrix", x, pos)
			}
			f := n.float()
			if math.IsInf(f, 0) {
				return nil, errf(codeOverflow, pos, "%s: an element is too large for a 64-bit float", what)
			}
			data = append(data, f)
		}
	}
	return mat.NewDense(r, c, data), nil
}

// realElementErr refuses an element that is not a real number; a complex
// one says that the routines are real (complexWiseHint).
func realElementErr(what, kind string, x val, pos int) *Error {
	err := errf(codeType, pos, "%s needs %s of numbers, got a %s element", what, kind, typeName(x))
	if isComplex(x) {
		err.Hint = complexWiseHint
	}
	return err
}

// vectorOf reads a list of numbers as floats, charging one step per element.
func (e *evaluator) vectorOf(v val, what string, pos int) ([]float64, *Error) {
	l, ok := v.([]val)
	if !ok || len(l) == 0 {
		return nil, errf(codeType, pos, "%s needs a vector (a list of numbers), got %s", what, describeVal(v))
	}
	if len(l) > MaxMatrixElems {
		return nil, errf(codeTooLarge, pos, "%s: the vector has %d elements; the limit is %d", what, len(l), MaxMatrixElems)
	}
	if err := e.charge(len(l), pos); err != nil {
		return nil, err
	}
	out := make([]float64, len(l))
	for i, x := range l {
		n, ok := x.(num)
		if !ok {
			return nil, realElementErr(what, "a vector", x, pos)
		}
		out[i] = n.float()
		if math.IsInf(out[i], 0) {
			return nil, errf(codeOverflow, pos, "%s: an element is too large for a 64-bit float", what)
		}
	}
	return out, nil
}

// valOfDense turns a gonum result back into a list of rows, charging one
// step per element; a NaN or an infinity is refused, never returned.
func (e *evaluator) valOfDense(m mat.Matrix, what string, pos int) (val, *Error) {
	r, c := m.Dims()
	if err := e.charge(r*c, pos); err != nil {
		return nil, err
	}
	rows := make([]val, r)
	for i := range r {
		row := make([]val, c)
		for j := range c {
			n, err := checkFloat(m.At(i, j), pos, what)
			if err != nil {
				return nil, err
			}
			row[j] = n
		}
		rows[i] = row
	}
	return rows, nil
}

func (e *evaluator) valOfFloats(xs []float64, what string, pos int) (val, *Error) {
	if err := e.charge(len(xs), pos); err != nil {
		return nil, err
	}
	out := make([]val, len(xs))
	for i, x := range xs {
		n, err := checkFloat(x, pos, what)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

// heavy admits one matrix routine of the given input size and work
// (multiply-adds, rows × columns × the inner dimension): it refuses work
// over MaxMatrixWork before anything runs, charges its steps, and, when the
// routine is large, takes a slot of the process-wide gate, waiting under
// the evaluation's own deadline. The caller runs the routine and then calls
// release.
func (e *evaluator) heavy(elems int, work float64, what string, pos int) (release func(), err *Error) {
	if work > MaxMatrixWork {
		return nil, errf(codeTooLarge, pos, "%s would take about %.3g multiply-adds; the limit is %d, the work of a 500 × 500 matrix", what, work, MaxMatrixWork).
			withHint("work on a smaller matrix, or split the problem")
	}
	if err := e.charge(int(work/matrixWorkPerStep), pos); err != nil {
		return nil, err
	}
	if e.gate == nil || elems <= gateElems && work <= gateWork {
		return func() {}, nil
	}
	if err := e.gate.Acquire(e.ctx, 1); err != nil {
		return nil, ctxErr(err, pos)
	}
	return func() { e.gate.Release(1) }, nil
}

// cubicWork is rows × columns × the smaller of the two: the order of the
// work of an inverse, a solve or a decomposition.
func cubicWork(r, c int) float64 { return float64(r) * float64(c) * float64(min(r, c)) }

// guard runs a gonum routine and turns a panic into a refusal. Every shape
// a routine needs is checked before it is called, so this is a backstop: a
// gonum panic must never take down the process serving the call.
func guard(what string, pos int, f func() *Error) (err *Error) {
	defer func() {
		if r := recover(); r != nil {
			err = errf(codeDomain, pos, "%s cannot be computed for this matrix (%v)", what, r)
		}
	}()
	return f()
}

func shapeText(v val) string {
	s := shape(v)
	parts := make([]string, len(s))
	for i, n := range s {
		parts[i] = fmt.Sprint(n)
	}
	if len(parts) == 0 {
		return "a " + typeName(v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// matMul is `*` between two lists: matrix × matrix, matrix × vector (the
// vector a column, the result a vector) or vector × matrix (the vector a
// row). Two plain vectors are refused: whether a dot product or an outer
// product was meant cannot be told.
func (e *evaluator) matMul(l, r val, pos int) (val, *Error) {
	lm, rm := isMatrix(l), isMatrix(r)
	if !lm && !rm {
		return nil, errf(codeType, pos, "`*` of two vectors is ambiguous").
			withHint("use dot(a, b) for the dot product, `.*` to multiply element by element")
	}
	a, err := e.operand(l, lm, true, pos)
	if err != nil {
		return nil, err
	}
	b, err := e.operand(r, rm, false, pos)
	if err != nil {
		return nil, err
	}
	ar, ac := a.Dims()
	br, bc := b.Dims()
	if ac != br {
		return nil, errf(codeShape, pos, "the matrix product needs the columns of the left to match the rows of the right, got %s and %s", shapeText(l), shapeText(r)).
			withHint("transpose one side with ', or use .* to multiply element by element")
	}
	if ar*bc > MaxMatrixElems {
		return nil, errf(codeTooLarge, pos, "the product would be %d × %d; the limit is %d elements", ar, bc, MaxMatrixElems)
	}
	release, err := e.heavy(ar*ac+br*bc, float64(ar)*float64(ac)*float64(bc), "the matrix product", pos)
	if err != nil {
		return nil, err
	}
	var out mat.Dense
	err = guard("the matrix product", pos, func() *Error { out.Mul(a, b); return nil })
	release()
	if err != nil {
		return nil, err
	}
	if !lm || !rm {
		// A vector in gives a vector out.
		return e.valOfFloats(out.RawMatrix().Data, "the matrix product", pos)
	}
	return e.valOfDense(&out, "the matrix product", pos)
}

// operand reads one side of a matrix product; a vector is a column on the
// right and a row on the left.
func (e *evaluator) operand(v val, isMat, left bool, pos int) (*mat.Dense, *Error) {
	if isMat {
		return e.denseOf(v, "the matrix product", pos)
	}
	xs, err := e.vectorOf(v, "the matrix product", pos)
	if err != nil {
		return nil, err
	}
	if left {
		return mat.NewDense(1, len(xs), xs), nil
	}
	return mat.NewDense(len(xs), 1, xs), nil
}
