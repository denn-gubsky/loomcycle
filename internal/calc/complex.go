package calc

import (
	"encoding/json"
	"math"
	"math/big"
	"math/cmplx"
	"sort"
	"strings"

	"gonum.org/v1/gonum/mat"
)

// Complex numbers (RFC GY P2c). A complex value is a pair of float64s, so
// it is never exact. It comes only from an imaginary literal (2i), from
// complex(a, b), or from an operation with a complex operand: a real
// argument always gives a real result or a refusal, so sqrt(-1) is still a
// domain error and no existing expression changes meaning. An operation
// whose result has an imaginary part of exactly 0 stays complex; re(z)
// makes it a real number again.
type cnum complex128

// SecComplex holds the complex-number functions.
const SecComplex = "complex"

// complexArgHint is the refusal hint for a real operation whose answer is
// not a real number.
const complexArgHint = "the answer is not a real number; for the complex value pass a complex argument, e.g. x + 0i"

// complexRealOnlyHint is the hint a function that takes real numbers gives
// when it meets a complex one.
const complexRealOnlyHint = "complex numbers have no order and this function takes real numbers; pass abs(z), re(z) or im(z)"

// complexWiseHint is the hint for a complex element reaching a matrix
// routine: the gonum routines are real.
const complexWiseHint = "the matrix functions and the matrix product take real numbers; + - .* ./ .^ work element by element on complex values"

var complexFuncs = []builtin{
	{name: "complex", sig: "complex(a, b)", section: SecComplex, min: 2, max: 2, fn: fnComplex,
		summary: "the complex number a + b·i from two real numbers"},
	{name: "re", sig: "re(z)", section: SecComplex, min: 1, max: 1, vec: true, fn: complexPart(func(z complex128) float64 { return real(z) }),
		summary: "the real part; a real number is its own real part"},
	{name: "im", sig: "im(z)", section: SecComplex, min: 1, max: 1, vec: true, fn: complexPart(func(z complex128) float64 { return imag(z) }),
		summary: "the imaginary part; 0 for a real number"},
	{name: "conj", sig: "conj(z)", section: SecComplex, min: 1, max: 1, vec: true, fn: fnConj,
		summary: "the complex conjugate; a real number is its own conjugate"},
	{name: "arg", sig: "arg(z)", section: SecComplex, min: 1, max: 1, vec: true, fn: fnArg,
		summary: "the angle of z in radians, in (-pi, pi]"},
	{name: "polyroots", sig: "polyroots(p)", section: SecComplex, min: 1, max: 1, fn: fnPolyroots,
		summary: "the roots of the polynomial with real coefficients p (highest power first), complex where needed"},
}

func isComplex(v val) bool { _, ok := v.(cnum); return ok }

// asComplex reads a real or complex number as a complex128.
func asComplex(v val) (complex128, bool) {
	switch x := v.(type) {
	case cnum:
		return complex128(x), true
	case num:
		return complex(x.float(), 0), true
	}
	return 0, false
}

// complexPair reports two numbers at least one of which is complex: the
// operands arith hands to complexOp.
func complexPair(l, r val) bool {
	_, lok := asComplex(l)
	_, rok := asComplex(r)
	return lok && rok && (isComplex(l) || isComplex(r))
}

// checkComplex refuses a NaN or infinite part, as checkFloat does for a
// real result, and drops the sign of a zero part.
func checkComplex(z complex128, pos int, what string) (val, *Error) {
	re, im := real(z), imag(z)
	if math.IsNaN(re) || math.IsNaN(im) {
		return nil, errf(codeDomain, pos, "%s is undefined", what)
	}
	if math.IsInf(re, 0) || math.IsInf(im, 0) {
		return nil, errf(codeOverflow, pos, "%s overflows a 64-bit float", what)
	}
	if re == 0 {
		re = 0
	}
	if im == 0 {
		im = 0
	}
	return cnum(complex(re, im)), nil
}

// complexOp applies an arithmetic operator to two numbers, one of them
// complex.
func (e *evaluator) complexOp(op string, l, r val, pos int) (val, *Error) {
	if s, ok := elementwise[op]; ok {
		op = s
	}
	a, _ := asComplex(l)
	b, _ := asComplex(r)
	var z complex128
	switch op {
	case "+":
		z = a + b
	case "-":
		z = a - b
	case "*":
		z = a * b
	case "/":
		if b == 0 {
			return nil, errf(codeDivZero, pos, "division by zero")
		}
		z = a / b
	case "^":
		return e.complexPow(a, r, pos)
	case "%":
		return nil, errf(codeType, pos, "%% (modulo) is not defined for complex numbers").withHint("modulo needs real numbers; take re(z), im(z) or abs(z) first")
	default:
		return nil, errf(codeSyntax, pos, "unknown operator %s", op)
	}
	return checkComplex(z, pos, "the result of "+op)
}

// maxComplexIntPow bounds an integer exponent computed by squaring: past
// 2^53 an exponent is no longer an integer a float64 can tell apart.
const maxComplexIntPow = 1 << 53

// complexPow raises a to the power r. An integer exponent is computed by
// repeated squaring, so (1+2i)^2 is exactly -3+4i rather than the
// exp(2·log z) value cmplx.Pow gives, which carries rounding into the zero
// digits; any other exponent is the principal value.
func (e *evaluator) complexPow(a complex128, r val, pos int) (val, *Error) {
	b, _ := asComplex(r)
	if a == 0 {
		switch {
		case b == 0:
			return cnum(1), nil
		case real(b) < 0:
			return nil, errf(codeDivZero, pos, "division by zero: 0 to a negative power")
		case real(b) == 0:
			return nil, errf(codeDomain, pos, "0 to a purely imaginary power is undefined")
		}
		return cnum(0), nil
	}
	if n, ok := r.(num); ok {
		if k, ok := smallInt(n, maxComplexIntPow); ok {
			neg := k < 0
			if neg {
				k = -k
			}
			z, sq := complex(1, 0), a
			for ; k > 0; k >>= 1 {
				if err := e.charge(1, pos); err != nil {
					return nil, err
				}
				if k&1 == 1 {
					z *= sq
				}
				sq *= sq
			}
			if neg {
				z = 1 / z
			}
			return checkComplex(z, pos, "the power")
		}
	}
	return checkComplex(cmplx.Pow(a, b), pos, "the power")
}

// complexEqual compares a complex value with a real or complex one by
// value: 2 + 0i equals 2. A real exact operand is compared through its
// nearest float, the precision a complex part has.
func complexEqual(l, r val) bool {
	a, lok := asComplex(l)
	b, rok := asComplex(r)
	if !lok || !rok {
		return false
	}
	if n, ok := l.(num); ok {
		return imag(b) == 0 && cmpNum(n, floatNum(real(b))) == 0
	}
	if n, ok := r.(num); ok {
		return imag(a) == 0 && cmpNum(floatNum(real(a)), n) == 0
	}
	return a == b
}

// complexMath holds the functions that take a complex argument, applied
// when one is complex; a real argument still takes the real path.
var complexMath = map[string]func(complex128) complex128{
	"sqrt": cmplx.Sqrt, "exp": cmplx.Exp, "log": cmplx.Log, "log10": cmplx.Log10,
	"log2": func(z complex128) complex128 { return cmplx.Log(z) / math.Ln2 },
	"sin":  cmplx.Sin, "cos": cmplx.Cos, "tan": cmplx.Tan,
	"asin": cmplx.Asin, "acos": cmplx.Acos, "atan": cmplx.Atan,
	"sinh": cmplx.Sinh, "cosh": cmplx.Cosh, "tanh": cmplx.Tanh,
	"asinh": cmplx.Asinh, "acosh": cmplx.Acosh, "atanh": cmplx.Atanh,
}

// complexRounders are the rounding functions that round each part of a
// complex number.
var complexRounders = map[string]bool{"round": true, "roundeven": true, "floor": true, "ceil": true, "trunc": true}

// complexCall applies b to arguments holding a complex number when b takes
// one; handled reports whether it did. Every other function meets the
// complex value in its own argument check and refuses it (see
// complexHint).
func (e *evaluator) complexCall(b *builtin, argv []val, pos int) (v val, handled bool, err *Error) {
	if len(argv) == 0 || !(isComplex(argv[0]) || len(argv) > 1 && isComplex(argv[1])) {
		return nil, false, nil
	}
	z, zok := asComplex(argv[0])
	switch {
	case b.name == "abs" && isComplex(argv[0]):
		f := cmplx.Abs(z)
		n, err := checkFloat(f, pos, "abs")
		return n, true, err
	case b.name == "pow" && zok:
		v, err := e.arith("^", argv[0], argv[1], pos)
		return v, true, err
	case b.name == "log" && zok && len(argv) == 2:
		v, err := complexLog(z, argv[1], pos)
		return v, true, err
	case complexRounders[b.name] && isComplex(argv[0]):
		v, err := e.roundComplex(b, z, argv[1:], pos)
		return v, true, err
	case complexMath[b.name] != nil && isComplex(argv[0]):
		if z == 0 && strings.HasPrefix(b.name, "log") {
			return nil, true, errf(codeDomain, pos, "%s(0) is undefined", b.name).withHint("the logarithm of 0 is minus infinity")
		}
		v, err := checkComplex(complexMath[b.name](z), pos, b.name)
		return v, true, err
	}
	return nil, false, nil
}

func complexLog(z complex128, base val, pos int) (val, *Error) {
	bz, ok := asComplex(base)
	if !ok {
		return nil, errf(codeType, pos, "log: the base must be a number, got a %s", typeName(base))
	}
	if z == 0 {
		return nil, errf(codeDomain, pos, "log(0) is undefined").withHint("the logarithm of 0 is minus infinity")
	}
	if bz == 0 || bz == 1 {
		return nil, errf(codeDomain, pos, "log base %s is not allowed", formatComplex(bz, 6)).withHint("the base must not be 0 or 1")
	}
	return checkComplex(cmplx.Log(z)/cmplx.Log(bz), pos, "log")
}

// roundComplex rounds each part of z with the real rounding function b.
func (e *evaluator) roundComplex(b *builtin, z complex128, rest []val, pos int) (val, *Error) {
	var parts [2]float64
	for i, p := range []float64{real(z), imag(z)} {
		v, err := b.fn(e, args{v: append([]val{floatNum(p)}, rest...), pos: pos, name: b.name})
		if err != nil {
			return nil, err
		}
		parts[i] = v.(num).float()
	}
	return checkComplex(complex(parts[0], parts[1]), pos, b.name)
}

// complexHint gives a type refusal caused by a complex argument a hint
// that says so. A refusal with its own hint keeps it.
func complexHint(err *Error) {
	if err.Code == string(codeType) && err.Hint == "" && strings.Contains(err.Detail, "complex") {
		err.Hint = complexRealOnlyHint
	}
}

// realArg is the refusal hint for a real argument outside a function's
// real domain: the same call with a complex argument has an answer.
func realArgHint(name string, x num) string {
	return "for the complex value write " + name + "(" + fmtArg(x) + " + 0i)"
}

func fnComplex(_ *evaluator, a args) (val, *Error) {
	re, err := a.num(0)
	if err != nil {
		return nil, err
	}
	im, err := a.num(1)
	if err != nil {
		return nil, err
	}
	return checkComplex(complex(re.float(), im.float()), a.pos, "complex")
}

// complexPart is re or im. A real number keeps its exactness: re(x) is x
// and im(x) is an exact 0.
func complexPart(part func(complex128) float64) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		if z, ok := a.v[0].(cnum); ok {
			return checkFloat(part(complex128(z)), a.pos, a.name)
		}
		x, err := a.num(0)
		if err != nil {
			return nil, err
		}
		if a.name == "im" {
			return exactInt64(0), nil
		}
		return x, nil
	}
}

func fnConj(_ *evaluator, a args) (val, *Error) {
	if z, ok := a.v[0].(cnum); ok {
		return checkComplex(cmplx.Conj(complex128(z)), a.pos, "conj")
	}
	return a.num(0)
}

func fnArg(_ *evaluator, a args) (val, *Error) {
	if z, ok := a.v[0].(cnum); ok {
		return checkFloat(cmplx.Phase(complex128(z)), a.pos, "arg")
	}
	x, err := a.num(0)
	if err != nil {
		return nil, err
	}
	if x.sign() < 0 {
		return floatNum(math.Pi), nil
	}
	return exactInt64(0), nil
}

// complexOrReal is a computed eigenvalue or root as a value: a real number
// when its imaginary part is exactly 0, which is how LAPACK reports a real
// eigenvalue, else a complex one.
func complexOrReal(z complex128, pos int, what string) (val, *Error) {
	if imag(z) == 0 {
		n, err := checkFloat(real(z), pos, what)
		return n, err
	}
	return checkComplex(z, pos, what)
}

// sortComplex orders computed eigenvalues or roots by real part, then by
// imaginary part, so the same input always lists them the same way (LAPACK
// returns them in no particular order). It returns the permutation.
func sortComplex(zs []complex128) []int {
	idx := make([]int, len(zs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool {
		a, b := zs[idx[i]], zs[idx[j]]
		if real(a) != real(b) {
			return real(a) < real(b)
		}
		return imag(a) < imag(b)
	})
	return idx
}

// fnPolyroots finds the roots of a polynomial as the eigenvalues of its
// companion matrix, NumPy's method. Leading zeros are dropped; trailing
// zeros are roots at 0, found without the matrix.
func fnPolyroots(e *evaluator, a args) (val, *Error) {
	p, err := numList(a, 0)
	if err != nil {
		return nil, err
	}
	if err := e.charge(len(p), a.pos); err != nil {
		return nil, err
	}
	for len(p) > 0 && p[0].isZero() {
		p = p[1:]
	}
	if len(p) == 0 {
		return nil, errf(codeDomain, a.pos, "polyroots: every coefficient is 0, so every number is a root").
			withHint("[1, -3, 2] is x^2 - 3x + 2: highest power first")
	}
	zeros := 0
	for len(p) > 1 && p[len(p)-1].isZero() {
		p = p[:len(p)-1]
		zeros++
	}
	n := len(p) - 1
	if n*n > MaxMatrixElems {
		return nil, errf(codeTooLarge, a.pos, "polyroots: degree %d needs a %d × %d companion matrix; the limit is %d elements", n, n, n, MaxMatrixElems)
	}
	roots := make([]complex128, 0, n+zeros)
	if n > 0 {
		lead := p[0].float()
		c := mat.NewDense(n, n, nil)
		for j := range n {
			f := -p[j+1].float() / lead
			if math.IsInf(f, 0) || math.IsNaN(f) || math.IsInf(lead, 0) {
				return nil, errf(codeOverflow, a.pos, "polyroots: a coefficient ratio overflows a 64-bit float")
			}
			c.Set(0, j, f)
		}
		for i := 1; i < n; i++ {
			c.Set(i, i-1, 1)
		}
		err = e.run(c, cubicWork(n, n), a, func() *Error {
			var eg mat.Eigen
			if !eg.Factorize(c, mat.EigenNone) {
				return errf(codeNoConvergence, a.pos, "polyroots: the eigenvalue iteration did not converge")
			}
			roots = append(roots, eg.Values(nil)...)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for range zeros {
		roots = append(roots, 0)
	}
	return e.valOfComplex(roots, sortComplex(roots), "a root", a.pos)
}

// valOfComplex returns zs in the order idx as a list, each value real or
// complex (complexOrReal), charging one step per value.
func (e *evaluator) valOfComplex(zs []complex128, idx []int, what string, pos int) (val, *Error) {
	if err := e.charge(len(zs), pos); err != nil {
		return nil, err
	}
	out := make([]val, len(zs))
	for i, k := range idx {
		v, err := complexOrReal(zs[k], pos, what)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// eigGeneral is eig of a square matrix that is not symmetric: eigenvalues
// and right eigenvectors through gonum's Eigen (LAPACK dgeev), complex
// where the matrix has complex eigenvalues. The values are sorted by
// sortComplex, and the vectors' columns follow them; each vector has
// Euclidean norm 1.
func (e *evaluator) eigGeneral(m *mat.Dense, n int, a args) (val, *Error) {
	var vals []complex128
	var vecs mat.CDense
	err := e.run(m, cubicWork(n, n), a, func() *Error {
		var eg mat.Eigen
		if !eg.Factorize(m, mat.EigenRight) {
			return errf(codeNoConvergence, a.pos, "eig: the eigenvalue iteration did not converge")
		}
		vals = eg.Values(nil)
		eg.VectorsTo(&vecs)
		return nil
	})
	if err != nil {
		return nil, err
	}
	idx := sortComplex(vals)
	vv, err := e.valOfComplex(vals, idx, "an eigenvalue", a.pos)
	if err != nil {
		return nil, err
	}
	if err := e.charge(n*n, a.pos); err != nil {
		return nil, err
	}
	rows := make([]val, n)
	for i := range n {
		row := make([]val, n)
		for j, k := range idx {
			v, err := complexOrReal(vecs.At(i, k), a.pos, "an eigenvector")
			if err != nil {
				return nil, err
			}
			row[j] = v
		}
		rows[i] = row
	}
	return []val{vv, rows}, nil
}

// formatComplex renders z as 3+2i, 3-2i, 2i or 3+0i: each part as a float
// to digits significant digits (at most 17), the imaginary part always
// written, and with the 1 of 1i kept, so the text reads back as the same
// value (i alone is an ordinary name).
func formatComplex(z complex128, digits int) string {
	d := min(digits, 17)
	re, im := real(z), imag(z)
	ims := formatFloat(math.Abs(im), d) + "i"
	sign := "+"
	if im < 0 {
		sign = "-"
	}
	if re == 0 {
		if sign == "-" {
			return "-" + ims
		}
		return ims
	}
	return formatFloat(re, d) + sign + ims
}

// complexJSON is a complex result in Response.Result: both parts as
// numbers, so a client never has to parse "3+2i".
type complexJSON struct {
	Re json.Number `json:"re"`
	Im json.Number `json:"im"`
}

func toComplexJSON(z complex128, digits int) complexJSON {
	d := min(digits, 17)
	return complexJSON{Re: json.Number(formatFloat(real(z), d)), Im: json.Number(formatFloat(imag(z), d))}
}

// complexKey is the valueKey text of z: a complex number with an imaginary
// part of 0 shares the key of the real number it equals.
func complexKey(z complex128) string {
	re := new(big.Rat).SetFloat64(real(z)).RatString()
	if imag(z) == 0 {
		return "n" + re
	}
	return "c" + re + "," + new(big.Rat).SetFloat64(imag(z)).RatString()
}
