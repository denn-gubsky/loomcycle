package calc

import (
	"math/big"
	"math/bits"
)

// Exact matrix products. When every element of both operands of `*` (or of
// A in A^n) is exact, the product is computed exactly, within two caps;
// past either it is computed in floats, as before, and reports not exact:
//
//   - maxExactMatSide: no side (rows, columns, the inner dimension) over 50,
//     so a product is at most 125,000 multiply-adds of big integers;
//   - the result must fit MaxExactBits per element, estimated from the
//     operands' sizes before any work;
//   - maxExactMatSteps: the estimated cost, bit-size aware, within a
//     quarter of the call's step budget.
//
// The arithmetic is on integers: each matrix is held as integers over one
// common denominator, so a product is n³ integer multiply-adds with no
// greatest-common-divisor reduction until the last step, one per element.
const (
	maxExactMatSide  = 50
	maxExactMatSteps = MaxSteps / 4
	// exactMulDiv and exactMulBitCost: an exact multiply-add of an a-bit
	// by a b-bit integer is charged (1 + a·b / exactMulBitCost) /
	// exactMulDiv steps. A step stands for about 2 µs (MaxSteps in
	// MaxWall); measured on an M-series laptop, a 50 × 50 product takes
	// about 65 ns per multiply-add on small integers, about 1.1 µs on
	// 1,024-bit ones and about 23 µs on 4,096-bit ones, so the charge
	// (0.06, 2 and 17 steps) stays above the time.
	exactMulDiv     = 16
	exactMulBitCost = 1 << 16
)

// ratMatrix is an exact matrix as integers over one positive denominator.
type ratMatrix struct {
	ints [][]*big.Int
	den  *big.Int
	bits int // the largest bit length among ints
}

// exactRowsOf reads rows as a ratMatrix; ok is false, with no error, when
// the exact path does not apply: an element that is not an exact number,
// rows of different lengths, a side over maxExactMatSide, or a common
// denominator over MaxExactBits. The float path then gives any refusal.
func (e *evaluator) exactRowsOf(rows [][]val, pos int) (*ratMatrix, bool, *Error) {
	if len(rows) == 0 || len(rows) > maxExactMatSide || len(rows[0]) == 0 || len(rows[0]) > maxExactMatSide {
		return nil, false, nil
	}
	c := len(rows[0])
	den := big.NewInt(1)
	g := new(big.Int)
	for _, row := range rows {
		if len(row) != c {
			return nil, false, nil
		}
		for _, x := range row {
			n, ok := x.(num)
			if !ok || !n.exact() {
				return nil, false, nil
			}
			if err := e.charge(1+ratBits(n.r)/bitStepCost, pos); err != nil {
				return nil, false, err
			}
			if q := n.r.Denom(); !q.IsInt64() || q.Int64() != 1 {
				g.GCD(nil, nil, den, q)
				den.Mul(den.Quo(den, g), q)
				if den.BitLen() > MaxExactBits {
					return nil, false, nil
				}
			}
		}
	}
	m := &ratMatrix{ints: make([][]*big.Int, len(rows)), den: den}
	for i, row := range rows {
		m.ints[i] = make([]*big.Int, c)
		for j, x := range row {
			r := x.(num).r
			v := new(big.Int).Quo(den, r.Denom())
			v.Mul(v, r.Num())
			m.ints[i][j] = v
			m.bits = max(m.bits, v.BitLen())
		}
	}
	return m, true, nil
}

// exactMulCost is the steps an r × k by k × c product of integers of aBits
// and bBits bits is charged, as a float so that it cannot wrap.
func exactMulCost(r, k, c, aBits, bBits int) float64 {
	return float64(r) * float64(k) * float64(c) * (1 + float64(aBits)*float64(bBits)/exactMulBitCost) / exactMulDiv
}

// productBits bounds the bits of an element of a product with inner
// dimension k: k terms each under 2^(aBits+bBits).
func productBits(aBits, bBits, k int) int { return aBits + bBits + bits.Len(uint(k)) }

// mulRat is the exact product a × b, already admitted and charged by the
// caller. The ctx is polled once per output element.
func (e *evaluator) mulRat(a, b *ratMatrix, pos int) (*ratMatrix, *Error) {
	r, k, c := len(a.ints), len(b.ints), len(b.ints[0])
	out := &ratMatrix{ints: make([][]*big.Int, r), den: new(big.Int).Mul(a.den, b.den)}
	t := new(big.Int)
	for i := range r {
		out.ints[i] = make([]*big.Int, c)
		for j := range c {
			if err := e.poll(i*c+j, pos); err != nil {
				return nil, err
			}
			s := new(big.Int)
			for p := range k {
				s.Add(s, t.Mul(a.ints[i][p], b.ints[p][j]))
			}
			out.ints[i][j] = s
			out.bits = max(out.bits, s.BitLen())
		}
	}
	return out, nil
}

// valOfRat turns an exact matrix back into rows of exact numbers, each
// reduced to lowest terms; as a vector when vec is set (one row or one
// column).
func (e *evaluator) valOfRat(m *ratMatrix, vec bool, pos int) (val, *Error) {
	out := make([]val, 0, len(m.ints))
	for _, row := range m.ints {
		vals := make([]val, len(row))
		for j, x := range row {
			v, err := e.exactResult(new(big.Rat).SetFrac(x, m.den), pos)
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

// exactMatMul is the exact matrix product of the operand rows lr × rr, when
// it applies (see exactRowsOf and the caps above); ok false means the caller
// computes it in floats.
func (e *evaluator) exactMatMul(lr, rr [][]val, vec bool, pos int) (val, bool, *Error) {
	a, ok, err := e.exactRowsOf(lr, pos)
	if !ok || err != nil {
		return nil, false, err
	}
	b, ok, err := e.exactRowsOf(rr, pos)
	if !ok || err != nil {
		return nil, false, err
	}
	r, k, c := len(a.ints), len(b.ints), len(b.ints[0])
	if len(a.ints[0]) != k {
		return nil, false, nil // the float path names both shapes
	}
	if productBits(a.bits, b.bits, k)+a.den.BitLen()+b.den.BitLen() > MaxExactBits {
		return nil, false, nil
	}
	cost := exactMulCost(r, k, c, a.bits, b.bits)
	if cost > maxExactMatSteps {
		return nil, false, nil
	}
	if err := e.charge(int(cost), pos); err != nil {
		return nil, false, err
	}
	p, err := e.mulRat(a, b, pos)
	if err != nil {
		return nil, false, err
	}
	v, err := e.valOfRat(p, vec, pos)
	return v, true, err
}

// exactMatPow is A^k for k >= 1 in exact arithmetic, when every element of
// A is exact and the result fits: the elements of A^k, held as integers
// over den^k, are under k·(bits + log2 n) bits. The cost charged is every
// product at that final size, an upper bound. ok false means the caller
// computes it in floats.
func (e *evaluator) exactMatPow(rows [][]val, k uint64, pos int) (val, bool, *Error) {
	a, ok, err := e.exactRowsOf(rows, pos)
	if !ok || err != nil {
		return nil, false, err
	}
	n := len(a.ints)
	if k > MaxExactBits {
		return nil, false, nil
	}
	finalBits := int(k) * (a.bits + bits.Len(uint(n)))
	if finalBits+int(k)*a.den.BitLen() > MaxExactBits {
		return nil, false, nil
	}
	cost := float64(powProducts(k)) * exactMulCost(n, n, n, finalBits, finalBits)
	if cost > maxExactMatSteps {
		return nil, false, nil
	}
	if err := e.charge(int(cost), pos); err != nil {
		return nil, false, err
	}
	p, err := powBySquaring(a, k, func(x, y *ratMatrix) (*ratMatrix, *Error) { return e.mulRat(x, y, pos) })
	if err != nil {
		return nil, false, err
	}
	v, err := e.valOfRat(p, false, pos)
	return v, true, err
}

// productRows views the operands of `*` as rows: a matrix as its rows, a
// vector as one row on the left or one column on the right.
func productRows(v val, isMat, left bool) [][]val {
	l := v.([]val)
	if isMat {
		out := make([][]val, len(l))
		for i, row := range l {
			out[i], _ = row.([]val)
		}
		return out
	}
	if left {
		return [][]val{l}
	}
	out := make([][]val, len(l))
	for i, x := range l {
		out[i] = []val{x}
	}
	return out
}
