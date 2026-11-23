package calc

import (
	"math"

	"gonum.org/v1/gonum/floats"
	"gonum.org/v1/gonum/mat"
)

// The linear-algebra functions of the matrices section, in float64 through
// gonum/mat. Every one checks the shapes it needs and admits its work with
// heavy (the size cap, the steps, the gate) before gonum runs, and runs
// gonum under guard.

// machEps is the float64 machine epsilon, for the rank and pseudo-inverse
// tolerances (NumPy's defaults: max(rows, columns) × eps × the largest
// singular value).
const machEps = 0x1p-52

// symTol is how far apart a[i][j] and a[j][i] may be, relative to the
// largest element, for a matrix to count as symmetric: rounding in a matrix
// computed as B'*B must not make it non-symmetric.
const symTol = 1e-10

func singularErr(a args) *Error {
	return errf(codeDomain, a.pos, "%s: the matrix is singular, or so close to it (condition number over 1e16) that the answer would be noise", a.name).
		withHint("pinv(A) or lstsq(A, b) give the least-squares answer for a singular matrix")
}

// squareOf reads argument i as a square matrix.
func (e *evaluator) squareOf(a args, i int) (*mat.Dense, int, *Error) {
	m, err := e.denseOf(a.v[i], a.name, a.pos)
	if err != nil {
		return nil, 0, err
	}
	r, c := m.Dims()
	if r != c {
		return nil, 0, notSquare(a, r, c)
	}
	return m, r, nil
}

// run admits work on m (see heavy), then runs f under guard. A gonum
// routine cannot be interrupted, so it is the size cap heavy checks, not
// the clock, that bounds how long f takes.
func (e *evaluator) run(m *mat.Dense, work float64, a args, f func() *Error) *Error {
	r, c := m.Dims()
	release, err := e.heavy(r*c, work, a.name, a.pos)
	if err != nil {
		return err
	}
	defer release()
	return guard(a.name, a.pos, f)
}

func fnDet(e *evaluator, a args) (val, *Error) {
	m, n, err := e.squareOf(a, 0)
	if err != nil {
		return nil, err
	}
	var d float64
	if err := e.run(m, cubicWork(n, n), a, func() *Error { d = mat.Det(m); return nil }); err != nil {
		return nil, err
	}
	return checkFloat(d, a.pos, "the determinant")
}

// svdOf factorizes m. SVDNone, the values alone, takes about a third of
// the time of SVDThin, which also forms U and V.
func svdOf(m *mat.Dense, kind mat.SVDKind, a args) (*mat.SVD, *Error) {
	var s mat.SVD
	if !s.Factorize(m, kind) {
		return nil, errf(codeDomain, a.pos, "%s: the singular value decomposition did not converge", a.name)
	}
	return &s, nil
}

// singularValues runs a thin SVD of argument 0 and returns its singular
// values, largest first.
func (e *evaluator) singularValues(a args) ([]float64, int, int, *Error) {
	m, err := e.denseOf(a.v[0], a.name, a.pos)
	if err != nil {
		return nil, 0, 0, err
	}
	r, c := m.Dims()
	var s []float64
	err = e.run(m, cubicWork(r, c), a, func() *Error {
		svd, err := svdOf(m, mat.SVDNone, a)
		if err == nil {
			s = svd.Values(nil)
		}
		return err
	})
	return s, r, c, err
}

func fnMatrank(e *evaluator, a args) (val, *Error) {
	s, r, c, err := e.singularValues(a)
	if err != nil {
		return nil, err
	}
	tol := float64(max(r, c)) * machEps * s[0]
	rank := 0
	for _, v := range s {
		if v > tol {
			rank++
		}
	}
	return exactInt64(int64(rank)), nil
}

func fnCond(e *evaluator, a args) (val, *Error) {
	s, r, c, err := e.singularValues(a)
	if err != nil {
		return nil, err
	}
	// Below the rank tolerance the smallest singular value is rounding
	// noise, and so would the quotient be.
	if s[len(s)-1] <= float64(max(r, c))*machEps*s[0] {
		return nil, errf(codeDomain, a.pos, "cond: the matrix is singular to working precision; its condition number is infinite").
			withHint("matrank(A) shows how many rows are independent")
	}
	return checkFloat(s[0]/s[len(s)-1], a.pos, "the condition number")
}

// normKind reads norm's optional kind: 1, 2, "inf" or "fro".
func normKind(a args, matrix bool) (string, *Error) {
	if len(a.v) < 2 {
		if matrix {
			return "fro", nil
		}
		return "2", nil
	}
	switch k := a.v[1].(type) {
	case num:
		if n, ok := smallInt(k, 2); ok && (n == 1 || n == 2) {
			return map[int64]string{1: "1", 2: "2"}[n], nil
		}
	case string:
		if k == "inf" || k == "fro" {
			return k, nil
		}
	}
	return "", errf(codeDomain, a.pos, "norm: kind must be 1, 2, \"inf\" or \"fro\"").
		withHint("norm(v) is the length of a vector; norm(A) the Frobenius norm of a matrix")
}

func fnNorm(e *evaluator, a args) (val, *Error) {
	if !isMatrix(a.v[0]) {
		kind, err := normKind(a, false)
		if err != nil {
			return nil, err
		}
		xs, err := e.vectorOf(a.v[0], a.name, a.pos)
		if err != nil {
			return nil, err
		}
		l := map[string]float64{"1": 1, "2": 2, "fro": 2, "inf": math.Inf(1)}[kind]
		return checkFloat(floats.Norm(xs, l), a.pos, "the norm")
	}
	kind, err := normKind(a, true)
	if err != nil {
		return nil, err
	}
	if kind == "2" {
		s, _, _, err := e.singularValues(a)
		if err != nil {
			return nil, err
		}
		return checkFloat(s[0], a.pos, "the norm")
	}
	m, err := e.denseOf(a.v[0], a.name, a.pos)
	if err != nil {
		return nil, err
	}
	r, c := m.Dims()
	// gonum's Norm(m, 2) of a matrix is the Frobenius norm.
	l := map[string]float64{"1": 1, "fro": 2, "inf": math.Inf(1)}[kind]
	var n float64
	if err := e.run(m, float64(r*c), a, func() *Error { n = mat.Norm(m, l); return nil }); err != nil {
		return nil, err
	}
	return checkFloat(n, a.pos, "the norm")
}

func fnInv(e *evaluator, a args) (val, *Error) {
	m, n, err := e.squareOf(a, 0)
	if err != nil {
		return nil, err
	}
	var out mat.Dense
	err = e.run(m, cubicWork(n, n), a, func() *Error {
		if out.Inverse(m) != nil {
			return singularErr(a)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e.valOfDense(&out, "the inverse", a.pos)
}

// pinvOf is V × diag(1/s) × U' over the singular values above NumPy's
// tolerance; the ones below it count as zero.
func pinvOf(m *mat.Dense, a args) (*mat.Dense, *Error) {
	svd, err := svdOf(m, mat.SVDThin, a)
	if err != nil {
		return nil, err
	}
	var u, v mat.Dense
	svd.UTo(&u)
	svd.VTo(&v)
	s := svd.Values(nil)
	r, c := m.Dims()
	tol := float64(max(r, c)) * machEps * s[0]
	for j, sv := range s {
		scale := 0.0
		if sv > tol {
			scale = 1 / sv
		}
		for i := range c {
			v.Set(i, j, v.At(i, j)*scale)
		}
	}
	var out mat.Dense
	out.Mul(&v, u.T())
	return &out, nil
}

func fnPinv(e *evaluator, a args) (val, *Error) {
	m, err := e.denseOf(a.v[0], a.name, a.pos)
	if err != nil {
		return nil, err
	}
	r, c := m.Dims()
	var out *mat.Dense
	err = e.run(m, cubicWork(r, c), a, func() *Error {
		var err *Error
		out, err = pinvOf(m, a)
		return err
	})
	if err != nil {
		return nil, err
	}
	return e.valOfDense(out, "the pseudo-inverse", a.pos)
}

// rhs reads the b of solve and lstsq: a vector (a column) or a matrix,
// with as many rows as A.
func (e *evaluator) rhs(a args, rows int) (b *mat.Dense, vec bool, err *Error) {
	if isMatrix(a.v[1]) {
		b, err = e.denseOf(a.v[1], a.name, a.pos)
	} else {
		var xs []float64
		if xs, err = e.vectorOf(a.v[1], a.name, a.pos); err == nil {
			b, vec = mat.NewDense(len(xs), 1, xs), true
		}
	}
	if err != nil {
		return nil, false, err
	}
	if br, _ := b.Dims(); br != rows {
		return nil, false, errf(codeShape, a.pos, "%s: A has %d rows but b has %d", a.name, rows, br)
	}
	return b, vec, nil
}

// solution renders x as a vector when b was one.
func (e *evaluator) solution(x *mat.Dense, vec bool, pos int) (val, *Error) {
	if vec {
		return e.valOfFloats(mat.Col(nil, 0, x), "the solution", pos)
	}
	return e.valOfDense(x, "the solution", pos)
}

func fnSolve(e *evaluator, a args) (val, *Error) {
	m, n, err := e.squareOf(a, 0)
	if err != nil {
		return nil, err
	}
	b, vec, err := e.rhs(a, n)
	if err != nil {
		return nil, err
	}
	_, p := b.Dims()
	var x mat.Dense
	err = e.run(m, max(cubicWork(n, n), float64(n*n*p)), a, func() *Error {
		var lu mat.LU
		lu.Factorize(m)
		if lu.Det() == 0 || lu.Cond() > mat.ConditionTolerance || lu.SolveTo(&x, false, b) != nil {
			return singularErr(a)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e.solution(&x, vec, a.pos)
}

func fnLstsq(e *evaluator, a args) (val, *Error) {
	m, err := e.denseOf(a.v[0], a.name, a.pos)
	if err != nil {
		return nil, err
	}
	r, c := m.Dims()
	b, vec, err := e.rhs(a, r)
	if err != nil {
		return nil, err
	}
	_, p := b.Dims()
	var x mat.Dense
	err = e.run(m, max(cubicWork(r, c), float64(r*c*p)), a, func() *Error {
		pinv, err := pinvOf(m, a)
		if err == nil {
			x.Mul(pinv, b)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return e.solution(&x, vec, a.pos)
}

// factors renders a decomposition as the list of its factors.
func (e *evaluator) factors(pos int, what string, ms ...mat.Matrix) (val, *Error) {
	out := make([]val, len(ms))
	for i, m := range ms {
		v, err := e.valOfDense(m, what, pos)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func fnLU(e *evaluator, a args) (val, *Error) {
	m, n, err := e.squareOf(a, 0)
	if err != nil {
		return nil, err
	}
	var l, u mat.TriDense
	var piv []int
	err = e.run(m, cubicWork(n, n), a, func() *Error {
		var lu mat.LU
		lu.Factorize(m)
		lu.LTo(&l)
		lu.UTo(&u)
		piv = lu.RowPivots(nil)
		return nil
	})
	if err != nil {
		return nil, err
	}
	lv, err := e.factors(a.pos, "the LU factors", &l, &u)
	if err != nil {
		return nil, err
	}
	// gonum factors A = P0*L*U with P0 the pivots applied to I; the P
	// returned is its transpose, so P*A = L*U. Its 0s and 1s are exact.
	one, zero := exactInt64(1), exactInt64(0)
	p, err := e.newRows(n, n, a, func(i, j int) val {
		if piv[j] == i {
			return one
		}
		return zero
	})
	if err != nil {
		return nil, err
	}
	return append(lv.([]val), p), nil
}

func fnQR(e *evaluator, a args) (val, *Error) {
	m, err := e.denseOf(a.v[0], a.name, a.pos)
	if err != nil {
		return nil, err
	}
	r, c := m.Dims()
	if r < c {
		return nil, errf(codeShape, a.pos, "qr needs at least as many rows as columns, got %d × %d", r, c).
			withHint("factor the transpose, qr(A')")
	}
	if r*r > MaxMatrixElems {
		// Q is r × r, however few the columns.
		return nil, errf(codeTooLarge, a.pos, "qr: Q would be %d × %d; the limit is %d elements", r, r, MaxMatrixElems).
			withHint("for a tall matrix, lstsq(A, b) solves the least-squares problem without forming Q")
	}
	var q, rr mat.Dense
	err = e.run(m, float64(r)*float64(r)*float64(c), a, func() *Error {
		var qr mat.QR
		qr.Factorize(m)
		qr.QTo(&q)
		qr.RTo(&rr)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e.factors(a.pos, "the QR factors", &q, &rr)
}

// symmetricOf reads argument 0 as a symmetric matrix, to within symTol.
func (e *evaluator) symmetricOf(a args) (*mat.SymDense, *mat.Dense, *Error) {
	m, n, err := e.squareOf(a, 0)
	if err != nil {
		return nil, nil, err
	}
	if i, j, ok := asymmetry(m, n); ok {
		return nil, nil, errf(codeDomain, a.pos, "%s needs a symmetric matrix; element [%d, %d] differs from [%d, %d]", a.name, i+1, j+1, j+1, i+1)
	}
	return mat.NewSymDense(n, m.RawMatrix().Data), m, nil
}

// asymmetry returns the first element [i, j] that differs from [j, i] by
// more than symTol relative to the largest element, if there is one.
func asymmetry(m *mat.Dense, n int) (i, j int, found bool) {
	big := 0.0
	for i := range n {
		for j := range n {
			big = max(big, math.Abs(m.At(i, j)))
		}
	}
	for i := range n {
		for j := i + 1; j < n; j++ {
			if math.Abs(m.At(i, j)-m.At(j, i)) > symTol*big {
				return i, j, true
			}
		}
	}
	return 0, 0, false
}

func fnCholesky(e *evaluator, a args) (val, *Error) {
	sym, m, err := e.symmetricOf(a)
	if err != nil {
		return nil, err
	}
	n := sym.SymmetricDim()
	var l mat.TriDense
	err = e.run(m, cubicWork(n, n), a, func() *Error {
		var ch mat.Cholesky
		if !ch.Factorize(sym) {
			return errf(codeDomain, a.pos, "cholesky: the matrix is not positive definite").
				withHint("cholesky needs a symmetric matrix with every eigenvalue above zero; check eig(A)")
		}
		ch.LTo(&l)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return e.valOfDense(&l, "the Cholesky factor", a.pos)
}

// fnEig takes the symmetric path (real eigenvalues, ascending) when the
// matrix is symmetric to within symTol, and the general one (eigGeneral,
// complex where needed) otherwise.
func fnEig(e *evaluator, a args) (val, *Error) {
	m, n, err := e.squareOf(a, 0)
	if err != nil {
		return nil, err
	}
	if _, _, asym := asymmetry(m, n); asym {
		return e.eigGeneral(m, n, a)
	}
	sym := mat.NewSymDense(n, m.RawMatrix().Data)
	var vals []float64
	var vecs mat.Dense
	err = e.run(m, cubicWork(n, n), a, func() *Error {
		var es mat.EigenSym
		if !es.Factorize(sym, true) {
			return errf(codeDomain, a.pos, "eig: the eigen decomposition did not converge")
		}
		vals = es.Values(nil)
		es.VectorsTo(&vecs)
		return nil
	})
	if err != nil {
		return nil, err
	}
	vv, err := e.valOfFloats(vals, "an eigenvalue", a.pos)
	if err != nil {
		return nil, err
	}
	vm, err := e.valOfDense(&vecs, "an eigenvector", a.pos)
	if err != nil {
		return nil, err
	}
	return []val{vv, vm}, nil
}

func fnSVD(e *evaluator, a args) (val, *Error) {
	m, err := e.denseOf(a.v[0], a.name, a.pos)
	if err != nil {
		return nil, err
	}
	r, c := m.Dims()
	var u, v mat.Dense
	var s []float64
	err = e.run(m, cubicWork(r, c), a, func() *Error {
		svd, err := svdOf(m, mat.SVDThin, a)
		if err != nil {
			return err
		}
		svd.UTo(&u)
		svd.VTo(&v)
		s = svd.Values(nil)
		return nil
	})
	if err != nil {
		return nil, err
	}
	uv, err := e.valOfDense(&u, "the SVD", a.pos)
	if err != nil {
		return nil, err
	}
	sv, err := e.valOfFloats(s, "the SVD", a.pos)
	if err != nil {
		return nil, err
	}
	vv, err := e.valOfDense(&v, "the SVD", a.pos)
	if err != nil {
		return nil, err
	}
	return []val{uv, sv, vv}, nil
}
