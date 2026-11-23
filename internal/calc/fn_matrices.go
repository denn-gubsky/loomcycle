package calc

// The matrices section. The functions that build or rearrange a matrix
// (zeros through size) and the exact products (dot, cross, kron, trace)
// keep the values they are given, so exact input stays exact; the rest run
// through gonum in float64 (fn_linalg.go).
var matrixFuncs = []builtin{
	{name: "zeros", sig: "zeros(r, c?)", section: SecMatrices, min: 1, max: 2, fn: fnFill(0),
		summary: "an r × c matrix of zeros, or a list of r zeros"},
	{name: "ones", sig: "ones(r, c?)", section: SecMatrices, min: 1, max: 2, fn: fnFill(1),
		summary: "an r × c matrix of ones, or a list of r ones"},
	{name: "identity", sig: "identity(n)", section: SecMatrices, min: 1, max: 1, fn: fnIdentity,
		summary: "the n × n identity matrix"},
	{name: "diag", sig: "diag(x)", section: SecMatrices, min: 1, max: 1, fn: fnDiag,
		summary: "a square matrix with the vector x on its diagonal, or the diagonal of the matrix x"},
	{name: "reshape", sig: "reshape(v, r, c)", section: SecMatrices, min: 3, max: 3, fn: fnReshape,
		summary: "the elements of v, row by row, as an r × c matrix"},
	{name: "hstack", sig: "hstack(A, B, ...)", section: SecMatrices, min: 1, max: -1, fn: fnStack(true),
		summary: "matrices side by side; a vector is a column"},
	{name: "vstack", sig: "vstack(A, B, ...)", section: SecMatrices, min: 1, max: -1, fn: fnStack(false),
		summary: "matrices one above the other; a vector is a row"},
	{name: "linspace", sig: "linspace(a, b, n)", section: SecMatrices, min: 3, max: 3, fn: fnLinspace,
		summary: "n evenly spaced numbers from a to b, both included; exact on exact input"},
	{name: "transpose", sig: "transpose(A)", section: SecMatrices, min: 1, max: 1, fn: fnTranspose,
		summary: "rows become columns, the same as A'; a vector becomes a one-row matrix"},
	{name: "size", sig: "size(x)", section: SecMatrices, min: 1, max: 1, fn: fnSize,
		summary: "the shape: [rows, columns] of a matrix, [n] of a list"},
	{name: "dot", sig: "dot(a, b)", section: SecMatrices, min: 2, max: 2, fn: fnDot,
		summary: "the dot product of two vectors; exact on exact input"},
	{name: "cross", sig: "cross(a, b)", section: SecMatrices, min: 2, max: 2, fn: fnCross,
		summary: "the cross product of two 3-vectors; exact on exact input"},
	{name: "kron", sig: "kron(A, B)", section: SecMatrices, min: 2, max: 2, fn: fnKron,
		summary: "the Kronecker product; exact on exact input"},
	{name: "trace", sig: "trace(A)", section: SecMatrices, min: 1, max: 1, fn: fnTrace,
		summary: "the sum of the diagonal of a square matrix; exact on exact input"},
	{name: "det", sig: "det(A)", section: SecMatrices, min: 1, max: 1, fn: fnDet,
		summary: "the determinant of a square matrix"},
	{name: "matrank", sig: "matrank(A)", section: SecMatrices, min: 1, max: 1, fn: fnMatrank,
		summary: "the rank of a matrix: how many of its rows are independent"},
	{name: "norm", sig: "norm(x, kind?)", section: SecMatrices, min: 1, max: 2, fn: fnNorm,
		summary: `the norm of a vector (2 by default) or a matrix ("fro" by default); kind 1, 2, "inf" or "fro"`},
	{name: "cond", sig: "cond(A)", section: SecMatrices, min: 1, max: 1, fn: fnCond,
		summary: "the condition number in the 2-norm: the largest singular value over the smallest"},
	{name: "inv", sig: "inv(A)", section: SecMatrices, min: 1, max: 1, fn: fnInv,
		summary: "the inverse of a square matrix"},
	{name: "pinv", sig: "pinv(A)", section: SecMatrices, min: 1, max: 1, fn: fnPinv,
		summary: "the Moore-Penrose pseudo-inverse of any matrix"},
	{name: "solve", sig: "solve(A, b)", section: SecMatrices, min: 2, max: 2, fn: fnSolve,
		summary: "x with A*x = b, for a square A; b a vector or a matrix"},
	{name: "lstsq", sig: "lstsq(A, b)", section: SecMatrices, min: 2, max: 2, fn: fnLstsq,
		summary: "the least-squares x making A*x closest to b; the shortest such x"},
	{name: "lu", sig: "lu(A)", section: SecMatrices, min: 1, max: 1, fn: fnLU,
		summary: "[L, U, P] with P*A = L*U, for a square A"},
	{name: "qr", sig: "qr(A)", section: SecMatrices, min: 1, max: 1, fn: fnQR,
		summary: "[Q, R] with A = Q*R, Q orthogonal and R upper triangular"},
	{name: "cholesky", sig: "cholesky(A)", section: SecMatrices, min: 1, max: 1, fn: fnCholesky,
		summary: "L with A = L*L', for a symmetric positive definite A"},
	{name: "eig", sig: "eig(A)", section: SecMatrices, min: 1, max: 1, fn: fnEig,
		summary: "[values, vectors] of a symmetric matrix, values ascending, vectors as columns"},
	{name: "svd", sig: "svd(A)", section: SecMatrices, min: 1, max: 1, fn: fnSVD,
		summary: "[U, s, V] with A = U*diag(s)*V', s descending"},
}

// count reads argument i as a dimension: a positive integer. It is bounded
// only so that rows × columns cannot overflow; newRows refuses a result
// over MaxMatrixElems as too_large.
func (a args) count(i int) (int, *Error) {
	n, err := a.small(i, 1<<31)
	if err == nil && n < 1 {
		err = errf(codeDomain, a.pos, "%s: argument %d must be at least 1", a.name, i+1)
	}
	return int(n), err
}

// newRows builds an r × c matrix of values from at(i, j), charging one step
// per element after checking MaxMatrixElems.
func (e *evaluator) newRows(r, c int, a args, at func(i, j int) val) (val, *Error) {
	if r*c > MaxMatrixElems {
		return nil, errf(codeTooLarge, a.pos, "%s: %d × %d is %d elements; the limit is %d", a.name, r, c, r*c, MaxMatrixElems)
	}
	if err := e.charge(r*c, a.pos); err != nil {
		return nil, err
	}
	rows := make([]val, r)
	for i := range r {
		row := make([]val, c)
		for j := range c {
			row[j] = at(i, j)
		}
		rows[i] = row
	}
	return rows, nil
}

func fnFill(v int64) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		x := exactInt64(v)
		r, err := a.count(0)
		if err != nil {
			return nil, err
		}
		if len(a.v) == 1 {
			out, err := e.newRows(1, r, a, func(int, int) val { return x })
			if err != nil {
				return nil, err
			}
			return out.([]val)[0], nil
		}
		c, err := a.count(1)
		if err != nil {
			return nil, err
		}
		return e.newRows(r, c, a, func(int, int) val { return x })
	}
}

func fnIdentity(e *evaluator, a args) (val, *Error) {
	n, err := a.count(0)
	if err != nil {
		return nil, err
	}
	one, zero := exactInt64(1), exactInt64(0)
	return e.newRows(n, n, a, func(i, j int) val {
		if i == j {
			return one
		}
		return zero
	})
}

// numsOf reads a vector argument as numbers, kept as they are.
func numsOf(v val, a args) ([]num, *Error) {
	l, ok := v.([]val)
	if !ok || len(l) == 0 {
		return nil, errf(codeType, a.pos, "%s needs a vector (a list of numbers), got %s", a.name, describeVal(v))
	}
	return numbersOf(l, a)
}

func fnDiag(e *evaluator, a args) (val, *Error) {
	if isMatrix(a.v[0]) {
		r, c, err := matDims(a.v[0], a.name, a.pos)
		if err != nil {
			return nil, err
		}
		rows := a.v[0].([]val)
		out := make([]val, min(r, c))
		for i := range out {
			out[i] = rows[i].([]val)[i]
		}
		return out, e.charge(len(out), a.pos)
	}
	xs, err := numsOf(a.v[0], a)
	if err != nil {
		return nil, err
	}
	zero := exactInt64(0)
	return e.newRows(len(xs), len(xs), a, func(i, j int) val {
		if i == j {
			return xs[i]
		}
		return zero
	})
}

// flatten returns the elements of a vector, or of a matrix row by row.
func flatten(v val, a args) ([]val, *Error) {
	if isMatrix(v) {
		if _, _, err := matDims(v, a.name, a.pos); err != nil {
			return nil, err
		}
		var out []val
		for _, row := range v.([]val) {
			out = append(out, row.([]val)...)
		}
		return out, nil
	}
	l, ok := v.([]val)
	if !ok {
		return nil, errf(codeType, a.pos, "%s: argument 1 must be a list, got a %s", a.name, typeName(v))
	}
	return l, nil
}

func fnReshape(e *evaluator, a args) (val, *Error) {
	xs, err := flatten(a.v[0], a)
	if err != nil {
		return nil, err
	}
	r, err := a.count(1)
	if err != nil {
		return nil, err
	}
	c, err := a.count(2)
	if err != nil {
		return nil, err
	}
	if r*c != len(xs) {
		return nil, errf(codeShape, a.pos, "reshape: %d elements do not make %d × %d (%d)", len(xs), r, c, r*c)
	}
	return e.newRows(r, c, a, func(i, j int) val { return xs[i*c+j] })
}

// blockRows reads one hstack/vstack argument as rows of values: a matrix as
// it is, a vector as a column (hstack) or a row (vstack).
func blockRows(v val, column bool, a args) ([][]val, *Error) {
	if isMatrix(v) {
		r, _, err := matDims(v, a.name, a.pos)
		if err != nil {
			return nil, err
		}
		out := make([][]val, r)
		for i, row := range v.([]val) {
			out[i] = row.([]val)
		}
		return out, nil
	}
	l, ok := v.([]val)
	if !ok || len(l) == 0 {
		return nil, errf(codeType, a.pos, "%s needs matrices or vectors, got %s", a.name, describeVal(v))
	}
	if !column {
		return [][]val{l}, nil
	}
	out := make([][]val, len(l))
	for i, x := range l {
		out[i] = []val{x}
	}
	return out, nil
}

func fnStack(horizontal bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		var out [][]val
		elems := 0
		for i, v := range a.v {
			b, err := blockRows(v, horizontal, a)
			if err != nil {
				return nil, err
			}
			elems += len(b) * len(b[0])
			if elems > MaxMatrixElems {
				return nil, errf(codeTooLarge, a.pos, "%s: the result would hold over %d elements", a.name, MaxMatrixElems)
			}
			if err := e.charge(len(b)*len(b[0]), a.pos); err != nil {
				return nil, err
			}
			if out, err = stackBlock(out, b, horizontal, i, a); err != nil {
				return nil, err
			}
		}
		rows := make([]val, len(out))
		for i, r := range out {
			rows[i] = r
		}
		return rows, nil
	}
}

// stackBlock appends block b (argument i) to the rows so far.
func stackBlock(out, b [][]val, horizontal bool, i int, a args) ([][]val, *Error) {
	if out == nil {
		out = make([][]val, len(b))
		for k, row := range b {
			out[k] = append([]val(nil), row...)
		}
		return out, nil
	}
	if horizontal {
		if len(b) != len(out) {
			return nil, errf(codeShape, a.pos, "hstack: argument %d has %d rows, the ones before have %d", i+1, len(b), len(out))
		}
		for k := range out {
			out[k] = append(out[k], b[k]...)
		}
		return out, nil
	}
	if len(b[0]) != len(out[0]) {
		return nil, errf(codeShape, a.pos, "vstack: argument %d has %d columns, the ones before have %d", i+1, len(b[0]), len(out[0]))
	}
	for _, row := range b {
		out = append(out, append([]val(nil), row...))
	}
	return out, nil
}

func fnLinspace(e *evaluator, a args) (val, *Error) {
	lo, err := a.num(0)
	if err != nil {
		return nil, err
	}
	hi, err := a.num(1)
	if err != nil {
		return nil, err
	}
	n, err := a.small(2, MaxListLen)
	if err != nil {
		return nil, err
	}
	if n < 1 {
		return nil, errf(codeDomain, a.pos, "linspace: n must be at least 1, got %d", n)
	}
	if err := e.charge(int(n), a.pos); err != nil {
		return nil, err
	}
	if n == 1 {
		return []val{lo}, nil
	}
	o := &chain{e: e, pos: a.pos}
	step := o.div(o.sub(hi, lo), exactInt64(n-1))
	out := make([]val, n)
	for i := range out {
		if err := e.poll(i, a.pos); err != nil {
			return nil, err
		}
		// lo + i*step, not a running sum, so the error does not grow; the
		// last one is hi itself, so the end is exact even for floats.
		out[i] = o.add(lo, o.mul(step, exactInt64(int64(i))))
	}
	out[n-1] = hi
	if o.err != nil {
		return nil, o.err
	}
	return out, nil
}

func fnTranspose(e *evaluator, a args) (val, *Error) {
	v := a.v[0]
	if !isMatrix(v) {
		xs, ok := v.([]val)
		if !ok || len(xs) == 0 {
			return nil, errf(codeType, a.pos, "transpose needs a matrix or a vector, got %s", describeVal(v)).
				withHint("`'` is the transpose, written after a matrix: A'; for feet use convert(x, \"ft\", \"m\")")
		}
		return []val{xs}, e.charge(len(xs), a.pos)
	}
	r, c, err := matDims(v, a.name, a.pos)
	if err != nil {
		return nil, err
	}
	rows := v.([]val)
	return e.newRows(c, r, a, func(i, j int) val { return rows[j].([]val)[i] })
}

func fnSize(_ *evaluator, a args) (val, *Error) {
	if t, ok := a.v[0].(*tableVal); ok {
		return []val{exactInt64(int64(t.rows())), exactInt64(int64(len(t.names)))}, nil
	}
	if _, ok := a.v[0].([]val); !ok {
		return nil, errf(codeType, a.pos, "size needs a list or a matrix, got a %s", typeName(a.v[0])).
			withHint("len(s) counts the characters of a string")
	}
	s := shape(a.v[0])
	out := make([]val, len(s))
	for i, n := range s {
		out[i] = exactInt64(int64(n))
	}
	return out, nil
}

// pairOf reads two vectors of the same length, kept exact.
func pairOf(a args) ([]num, []num, *Error) {
	x, err := numsOf(a.v[0], a)
	if err != nil {
		return nil, nil, err
	}
	y, err := numsOf(a.v[1], a)
	if err != nil {
		return nil, nil, err
	}
	if len(x) != len(y) {
		return nil, nil, errf(codeShape, a.pos, "%s needs vectors of the same length, got %d and %d", a.name, len(x), len(y))
	}
	return x, y, nil
}

func fnDot(e *evaluator, a args) (val, *Error) {
	x, y, err := pairOf(a)
	if err != nil {
		return nil, err
	}
	if err := e.charge(len(x), a.pos); err != nil {
		return nil, err
	}
	o := &chain{e: e, pos: a.pos}
	s := exactInt64(0)
	for i := range x {
		s = o.add(s, o.mul(x[i], y[i]))
	}
	return o.result(s)
}

func fnCross(e *evaluator, a args) (val, *Error) {
	x, y, err := pairOf(a)
	if err != nil {
		return nil, err
	}
	if len(x) != 3 {
		return nil, errf(codeShape, a.pos, "cross needs two vectors of 3 elements, got %d", len(x))
	}
	o := &chain{e: e, pos: a.pos}
	out := []val{
		o.sub(o.mul(x[1], y[2]), o.mul(x[2], y[1])),
		o.sub(o.mul(x[2], y[0]), o.mul(x[0], y[2])),
		o.sub(o.mul(x[0], y[1]), o.mul(x[1], y[0])),
	}
	if o.err != nil {
		return nil, o.err
	}
	return out, nil
}

// exactMatrix reads a matrix of numbers kept as they are, charging one
// step per element.
func (e *evaluator) exactMatrix(v val, a args) ([][]num, *Error) {
	r, c, err := matDims(v, a.name, a.pos)
	if err != nil {
		return nil, err
	}
	if err := e.charge(r*c, a.pos); err != nil {
		return nil, err
	}
	rows := v.([]val)
	out := make([][]num, len(rows))
	for i, r := range rows {
		ns, err := numbersOf(r.([]val), a)
		if err != nil {
			return nil, err
		}
		out[i] = ns
	}
	return out, nil
}

func fnKron(e *evaluator, a args) (val, *Error) {
	x, err := e.exactMatrix(a.v[0], a)
	if err != nil {
		return nil, err
	}
	y, err := e.exactMatrix(a.v[1], a)
	if err != nil {
		return nil, err
	}
	br, bc := len(y), len(y[0])
	o := &chain{e: e, pos: a.pos}
	out, err := e.newRows(len(x)*br, len(x[0])*bc, a, func(i, j int) val {
		return o.mul(x[i/br][j/bc], y[i%br][j%bc])
	})
	if err != nil {
		return nil, err
	}
	if o.err != nil {
		return nil, o.err
	}
	return out, nil
}

func fnTrace(e *evaluator, a args) (val, *Error) {
	x, err := e.exactMatrix(a.v[0], a)
	if err != nil {
		return nil, err
	}
	if len(x) != len(x[0]) {
		return nil, notSquare(a, len(x), len(x[0]))
	}
	o := &chain{e: e, pos: a.pos}
	s := exactInt64(0)
	for i := range x {
		s = o.add(s, x[i][i])
	}
	return o.result(s)
}

func notSquare(a args, r, c int) *Error {
	return errf(codeShape, a.pos, "%s needs a square matrix, got %d × %d", a.name, r, c)
}
