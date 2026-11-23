package calc

import (
	"math"
	"math/big"
)

// elementwise maps the dotted operators to their scalar meaning.
var elementwise = map[string]string{".*": "*", "./": "/", ".^": "^"}

// arith applies an arithmetic operator, broadcasting over lists. `*`
// between two lists is the matrix product (see matMul); `/` by a list and
// `^` with a list operand are refused, and the dotted forms say
// element-wise explicitly.
func (e *evaluator) arith(op string, l, r val, pos int) (val, *Error) {
	ln, lok := l.(num)
	rn, rok := r.(num)
	if lok && rok {
		if s, ok := elementwise[op]; ok {
			op = s
		}
		return e.numOp(op, ln, rn, pos)
	}
	if complexPair(l, r) {
		return e.complexOp(op, l, r, pos)
	}
	ll, lList := l.([]val)
	rl, rList := r.([]val)
	// An empty table cell stays empty, so t.price .* t.qty keeps its rows
	// aligned and sum() then skips the gap.
	if !lList && !rList && (isEmpty(l) || isEmpty(r)) {
		return empty, nil
	}
	if !lList && !rList && (isTime(l) || isTime(r)) {
		return e.timeArith(op, l, r, pos)
	}
	// A list of dates broadcasts like a list of numbers: d + [0, 7, 14].
	// A list of complex numbers broadcasts the same way.
	lok, rok = lok || isTime(l) || isComplex(l) || isEmpty(l), rok || isTime(r) || isComplex(r) || isEmpty(r)
	if !(lList || lok) || !(rList || rok) {
		return nil, errf(codeType, pos, "cannot apply %s to a %s and a %s", op, typeName(l), typeName(r))
	}
	switch op {
	case "*":
		if lList && rList {
			return e.matMul(l, r, pos)
		}
	case "/":
		if rList {
			return nil, errf(codeType, pos, "`/` by a list is not defined").withHint("use `./` to divide element by element")
		}
	case "^":
		return nil, errf(codeType, pos, "`^` on a list is not defined").withHint("use `.^` to raise element by element")
	}
	switch {
	case lList && rList:
		if isMatrix(l) || isMatrix(r) {
			// Name both shapes here, at the top: the element-wise recursion
			// below would only see the lengths of one pair of rows.
			if ls, rs := shape(l), shape(r); !equalInts(ls, rs) {
				return nil, errf(codeShape, pos, "%s needs the same shape on both sides, got %s and %s", op, shapeText(l), shapeText(r))
			}
		}
		if len(ll) != len(rl) {
			return nil, errf(codeShape, pos, "%s needs lists of the same length, got %d and %d", op, len(ll), len(rl))
		}
		out := make([]val, len(ll))
		for i := range ll {
			if err := e.charge(1, pos); err != nil {
				return nil, err
			}
			v, err := e.arith(op, ll[i], rl[i], pos)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case lList:
		return e.broadcast(op, ll, r, false, pos)
	default:
		return e.broadcast(op, rl, l, true, pos)
	}
}

func (e *evaluator) broadcast(op string, l []val, s val, scalarLeft bool, pos int) (val, *Error) {
	out := make([]val, len(l))
	for i, x := range l {
		if err := e.charge(1, pos); err != nil {
			return nil, err
		}
		var v val
		var err *Error
		if scalarLeft {
			v, err = e.arith(op, s, x, pos)
		} else {
			v, err = e.arith(op, x, s, pos)
		}
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (e *evaluator) numOp(op string, a, b num, pos int) (val, *Error) {
	if op == "^" {
		return e.pow(a, b, pos)
	}
	if (op == "/" || op == "%") && b.isZero() {
		return nil, errf(codeDivZero, pos, "division by zero")
	}
	if a.exact() && b.exact() {
		r := new(big.Rat)
		switch op {
		case "+":
			r.Add(a.r, b.r)
		case "-":
			r.Sub(a.r, b.r)
		case "*":
			r.Mul(a.r, b.r)
		case "/":
			r.Quo(a.r, b.r)
		case "%":
			r = floorMod(a.r, b.r)
		default:
			return nil, errf(codeSyntax, pos, "unknown operator %s", op)
		}
		return e.exactResult(r, pos)
	}
	x, y := a.float(), b.float()
	var f float64
	switch op {
	case "+":
		f = x + y
	case "-":
		f = x - y
	case "*":
		f = x * y
	case "/":
		f = x / y
	case "%":
		f = math.Mod(x, y)
		if f != 0 && (f < 0) != (y < 0) {
			f += y
		}
	default:
		return nil, errf(codeSyntax, pos, "unknown operator %s", op)
	}
	return checkFloat(f, pos, "the result of "+op)
}

// floorMod is a - b*floor(a/b): the result takes the sign of the divisor.
func floorMod(a, b *big.Rat) *big.Rat {
	q := new(big.Rat).Quo(a, b)
	fl := ratFloor(q)
	return new(big.Rat).Sub(a, new(big.Rat).Mul(b, new(big.Rat).SetInt(fl)))
}

// ratFloor is floor(r) as an integer.
func ratFloor(r *big.Rat) *big.Int {
	// big.Int.Div is Euclidean; with the always-positive denominator of a
	// big.Rat that is floor division.
	return new(big.Int).Div(r.Num(), r.Denom())
}

// exactResult keeps r exact when it fits the bit cap; otherwise plain
// arithmetic degrades to a float and the response reports not exact.
// Bigger results charge more steps, which bounds the memory a long list of
// huge numbers could take.
func (e *evaluator) exactResult(r *big.Rat, pos int) (val, *Error) {
	bits := ratBits(r)
	if err := e.charge(bits/bitStepCost, pos); err != nil {
		return nil, err
	}
	if bits <= MaxExactBits {
		return exactNum(r), nil
	}
	f, _ := r.Float64()
	return checkFloat(f, pos, "the exact result (over the bit cap)")
}

// log2Abs estimates log2|x| for a positive-size integer.
func log2Abs(x *big.Int) float64 {
	if x.BitLen() <= 1000 {
		f, _ := new(big.Float).SetInt(x).Float64()
		return math.Log2(math.Abs(f))
	}
	return float64(x.BitLen())
}

// pow keeps exactness for an exact base and an exact integer exponent. The
// result size is estimated from the operands before anything is computed,
// so 9^9^9 is refused at once instead of allocating a 1.2-gigabit integer;
// an over-cap result that still fits a float64 comes back as a float.
func (e *evaluator) pow(a, b num, pos int) (val, *Error) {
	if a.isZero() && b.sign() < 0 {
		return nil, errf(codeDivZero, pos, "division by zero: 0 to a negative power")
	}
	if a.exact() && b.exact() && b.r.IsInt() {
		exp := b.r.Num()
		base := a.r
		switch {
		case exp.Sign() == 0:
			return exactInt64(1), nil
		case base.Sign() == 0:
			return exactInt64(0), nil
		case base.IsInt() && base.Num().CmpAbs(big.NewInt(1)) == 0:
			if base.Sign() < 0 && exp.Bit(0) == 1 {
				return exactInt64(-1), nil
			}
			return exactInt64(1), nil
		}
		est := math.Inf(1)
		if exp.BitLen() < 63 {
			est = math.Abs(float64(exp.Int64())) * (log2Abs(base.Num()) + log2Abs(base.Denom()))
		}
		if est > MaxExactBits {
			f := math.Pow(a.float(), b.float())
			if math.IsInf(f, 0) || math.IsNaN(f) {
				return nil, errf(codeTooLarge, pos, "the power has about %.3g bits, over the %d-bit cap, and overflows a float too", est, MaxExactBits)
			}
			return checkFloat(f, pos, "the power")
		}
		if err := e.charge(int(est)/bitStepCost, pos); err != nil {
			return nil, err
		}
		ae := new(big.Int).Abs(exp)
		n := new(big.Int).Exp(base.Num(), ae, nil)
		d := new(big.Int).Exp(base.Denom(), ae, nil)
		if exp.Sign() < 0 {
			n, d = d, n
		}
		return e.exactResult(new(big.Rat).SetFrac(n, d), pos)
	}
	return checkFloat(math.Pow(a.float(), b.float()), pos, "the power")
}

// compare implements == != < <= > >=. Equality works on any two values
// (different types are unequal, but a date meets a datetime only after an
// explicit conversion); ordering needs two numbers, two strings, two dates
// or two datetimes.
func (e *evaluator) compare(op string, l, r val, pos int) (val, *Error) {
	if op == "==" || op == "!=" {
		eq, err := e.equal(l, r, pos)
		if err != nil {
			return nil, err
		}
		return eq == (op == "=="), nil
	}
	if isEmpty(l) || isEmpty(r) {
		// An empty cell is neither above nor below anything, so a rule like
		// r -> r.qty > 0 drops the row; test for it with isempty().
		return false, nil
	}
	if isComplex(l) || isComplex(r) {
		return nil, errf(codeType, pos, "complex numbers cannot be ordered with %s", op).
			withHint("only == and != compare complex numbers; compare abs(z), re(z) or im(z) instead")
	}
	var c int
	switch lv := l.(type) {
	case num:
		rv, ok := r.(num)
		if !ok {
			return nil, errf(codeType, pos, "cannot order a number and a %s", typeName(r))
		}
		c = cmpNum(lv, rv)
	case string:
		rv, ok := r.(string)
		if !ok {
			return nil, errf(codeType, pos, "cannot order a string and a %s", typeName(r))
		}
		c = cmpString(lv, rv)
	case dateVal, timeVal:
		var err *Error
		if c, err = cmpTime(l, r, pos); err != nil {
			return nil, err
		}
	default:
		return nil, errf(codeType, pos, "%s cannot be ordered with %s", typeName(l), op).
			withHint("to compare lists element by element use map or zip, e.g. all(map(zip(a, b), p -> p[1] < p[2]))")
	}
	switch op {
	case "<":
		return c < 0, nil
	case "<=":
		return c <= 0, nil
	case ">":
		return c > 0, nil
	}
	return c >= 0, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (e *evaluator) equal(l, r val, pos int) (bool, *Error) {
	if err := e.charge(1, pos); err != nil {
		return false, err
	}
	if isComplex(l) || isComplex(r) {
		return complexEqual(l, r), nil
	}
	switch lv := l.(type) {
	case num:
		rv, ok := r.(num)
		return ok && cmpNum(lv, rv) == 0, nil
	case string:
		rv, ok := r.(string)
		return ok && lv == rv, nil
	case bool:
		rv, ok := r.(bool)
		return ok && lv == rv, nil
	case emptyVal:
		return isEmpty(r), nil
	case *tableVal:
		rv, ok := r.(*tableVal)
		if !ok || !equalStrings(lv.names, rv.names) || lv.rows() != rv.rows() {
			return false, nil
		}
		for j := range lv.cols {
			if eq, err := e.equal(lv.cols[j], rv.cols[j], pos); err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	case rowVal:
		rv, ok := r.(rowVal)
		if !ok || !equalStrings(lv.t.names, rv.t.names) {
			return false, nil
		}
		return e.equal(lv.t.cellsOf(lv.i), rv.t.cellsOf(rv.i), pos)
	case dateVal, timeVal:
		if !isTime(r) {
			return false, nil
		}
		c, err := cmpTime(l, r, pos)
		return c == 0, err
	case []val:
		rv, ok := r.([]val)
		if !ok || len(lv) != len(rv) {
			return false, nil
		}
		for i := range lv {
			eq, err := e.equal(lv[i], rv[i], pos)
			if err != nil || !eq {
				return false, err
			}
		}
		return true, nil
	}
	return false, errf(codeType, pos, "functions cannot be compared")
}

// evalRange builds start:end or start:step:end, inclusive. The length is
// computed exactly and checked before a single element is allocated.
func (e *evaluator) evalRange(n *rangeN, sc *scope) (val, *Error) {
	parts := []node{n.start, n.end}
	if n.step != nil {
		parts = append(parts, n.step)
	}
	vals := make([]num, len(parts))
	for i, p := range parts {
		v, err := e.eval(p, sc)
		if err != nil {
			return nil, err
		}
		x, ok := v.(num)
		if !ok {
			return nil, errf(codeType, n.p, "range bounds must be numbers, got a %s", typeName(v))
		}
		vals[i] = x
	}
	step := exactInt64(1)
	if len(vals) == 3 {
		step = vals[2]
	}
	return e.makeRange(vals[0], step, vals[1], n.p)
}

func (e *evaluator) makeRange(start, step, end num, pos int) (val, *Error) {
	if step.isZero() {
		return nil, errf(codeDomain, pos, "a range step cannot be zero")
	}
	span := new(big.Rat).Sub(end.rat(), start.rat())
	q := new(big.Rat).Quo(span, step.rat())
	if q.Sign() < 0 {
		return []val{}, nil
	}
	cnt := ratFloor(q)
	cnt.Add(cnt, big.NewInt(1))
	if !cnt.IsInt64() || cnt.Int64() > MaxListLen {
		return nil, errf(codeListTooLong, pos, "the range has %s elements, over the %d-element limit", cnt.String(), MaxListLen)
	}
	count := int(cnt.Int64())
	if err := e.charge(count, pos); err != nil {
		return nil, err
	}
	out := make([]val, count)
	if start.exact() && step.exact() {
		for i := range out {
			if err := e.poll(i, pos); err != nil {
				return nil, err
			}
			r := new(big.Rat).Mul(step.r, new(big.Rat).SetInt64(int64(i)))
			r.Add(r, start.r)
			v, err := e.exactResult(r, pos)
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	}
	s, d := start.float(), step.float()
	for i := range out {
		// start + i*step, not a running sum, so the error does not grow.
		v, err := checkFloat(s+float64(i)*d, pos, "a range element")
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
