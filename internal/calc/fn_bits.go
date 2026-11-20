package calc

import (
	"math"
	"math/big"
	"strings"
)

// Bitwise functions treat negative integers as infinite two's complement,
// as math/big does: bitnot(x) = -x - 1.
var bitFuncs = []builtin{
	{name: "bitand", sig: "bitand(a, b)", section: SecBits, min: 2, max: 2, vec: true, fn: bitwise((*big.Int).And), summary: "bitwise and"},
	{name: "bitor", sig: "bitor(a, b)", section: SecBits, min: 2, max: 2, vec: true, fn: bitwise((*big.Int).Or), summary: "bitwise or"},
	{name: "bitxor", sig: "bitxor(a, b)", section: SecBits, min: 2, max: 2, vec: true, fn: bitwise((*big.Int).Xor), summary: "bitwise exclusive or"},
	{name: "bitnot", sig: "bitnot(a)", section: SecBits, min: 1, max: 1, vec: true, fn: fnBitNot, summary: "bitwise not, -a - 1"},
	{name: "shl", sig: "shl(a, n)", section: SecBits, min: 2, max: 2, vec: true, fn: shift(true), summary: "a shifted left n bits, a * 2^n"},
	{name: "shr", sig: "shr(a, n)", section: SecBits, min: 2, max: 2, vec: true, fn: shift(false), summary: "a shifted right n bits, floor(a / 2^n)"},
	{name: "base", sig: "base(x, radix)", section: SecBits, min: 2, max: 2, vec: true, fn: fnBase, summary: "the integer x written in radix 2..36, as a string"},
	{name: "frombase", sig: "frombase(s, radix)", section: SecBits, min: 2, max: 2, fn: fnFromBase, summary: "the integer a string of radix 2..36 digits stands for"},
}

func bitwise(op func(z, x, y *big.Int) *big.Int) func(*evaluator, args) (val, *Error) {
	return func(_ *evaluator, a args) (val, *Error) {
		x, err := a.integer(0)
		if err != nil {
			return nil, err
		}
		y, err := a.integer(1)
		if err != nil {
			return nil, err
		}
		return exactInt(op(new(big.Int), x, y)), nil
	}
}

func fnBitNot(_ *evaluator, a args) (val, *Error) {
	x, err := a.integer(0)
	if err != nil {
		return nil, err
	}
	return exactInt(new(big.Int).Not(x)), nil
}

func shift(left bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		x, err := a.integer(0)
		if err != nil {
			return nil, err
		}
		n, err := a.small(1, math.MaxInt32)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, errf(codeDomain, a.pos, "%s: the shift count must not be negative", a.name).
				withHint("use the other shift: shl for left, shr for right")
		}
		if !left {
			return exactInt(new(big.Int).Rsh(x, uint(n))), nil
		}
		if est := x.BitLen() + int(n); est > MaxExactBits {
			return nil, tooLarge(a, float64(est))
		}
		return e.intResult(new(big.Int).Lsh(x, uint(n)), a)
	}
}

func radixArg(a args, i int) (int, *Error) {
	r, err := a.small(i, 36)
	if err != nil || r < 2 {
		return 0, errf(codeDomain, a.pos, "%s: the radix must be an integer from 2 to 36", a.name)
	}
	return int(r), nil
}

func fnBase(_ *evaluator, a args) (val, *Error) {
	x, err := a.integer(0)
	if err != nil {
		return nil, err
	}
	r, err := radixArg(a, 1)
	if err != nil {
		return nil, err
	}
	return x.Text(r), nil
}

func fnFromBase(e *evaluator, a args) (val, *Error) {
	s, ok := a.v[0].(string)
	if !ok {
		return nil, errf(codeType, a.pos, "frombase: argument 1 must be a string, got a %s", typeName(a.v[0])).
			withHint(`quote the digits: frombase("ff", 16)`)
	}
	r, err := radixArg(a, 1)
	if err != nil {
		return nil, err
	}
	if float64(len(s))*math.Log2(float64(r)) > MaxExactBits+8 {
		return nil, tooLarge(a, float64(len(s))*math.Log2(float64(r)))
	}
	v, ok := new(big.Int).SetString(strings.ToLower(s), r)
	if !ok || strings.Contains(s, "_") {
		return nil, errf(codeDomain, a.pos, "frombase: %q is not a base-%d number", s, r)
	}
	return e.intResult(v, a)
}
