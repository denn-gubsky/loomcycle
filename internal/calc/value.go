package calc

import (
	"math"
	"math/big"
)

// A value is one of: num, string, bool, []val (a list), *fnVal, dateVal,
// timeVal (see datetime.go). Lists are
// never mutated after they are built; every operation returns a new one.
type val any

// num is exact when r is non-nil and a float64 otherwise. The *big.Rat is
// never mutated after construction, so nums may be shared freely. A float
// num is always finite: every float result passes checkFloat.
type num struct {
	r *big.Rat
	f float64
}

func exactNum(r *big.Rat) num         { return num{r: r} }
func exactInt(i *big.Int) num         { return num{r: new(big.Rat).SetInt(i)} }
func exactInt64(i int64) num          { return num{r: new(big.Rat).SetInt64(i)} }
func floatNum(f float64) num          { return num{f: f} }
func (n num) exact() bool             { return n.r != nil }
func (n num) sign() int               { return signOf(n) }
func (n num) isZero() bool            { return n.sign() == 0 }
func ratFromFloat(f float64) *big.Rat { return new(big.Rat).SetFloat64(f) }

// ratBits is the size MaxExactBits caps: numerator plus denominator bits,
// with an integer's denominator of 1 counting nothing, so 2^16383 fits.
func ratBits(r *big.Rat) int {
	if r.IsInt() {
		return r.Num().BitLen()
	}
	return r.Num().BitLen() + r.Denom().BitLen()
}

func signOf(n num) int {
	if n.r != nil {
		return n.r.Sign()
	}
	switch {
	case n.f > 0:
		return 1
	case n.f < 0:
		return -1
	}
	return 0
}

// float returns the nearest float64; an exact value too large for float64
// comes back as ±Inf, which checkFloat refuses where it matters.
func (n num) float() float64 {
	if n.r == nil {
		return n.f
	}
	f, _ := n.r.Float64()
	return f
}

// rat returns the exact value of n. A float converts exactly (every finite
// float64 is a dyadic rational); this is only for comparison and rounding
// internals, never to make a float result exact again.
func (n num) rat() *big.Rat {
	if n.r != nil {
		return n.r
	}
	return ratFromFloat(n.f)
}

// checkFloat refuses NaN and ±Inf: the evaluator never hands either back.
func checkFloat(f float64, pos int, what string) (num, *Error) {
	if math.IsNaN(f) {
		return num{}, errf(codeDomain, pos, "%s is undefined (not a real number)", what).withHint("there are no complex numbers in this version")
	}
	if math.IsInf(f, 0) {
		return num{}, errf(codeOverflow, pos, "%s overflows a 64-bit float", what)
	}
	if f == 0 {
		f = 0 // drop the sign of -0
	}
	return floatNum(f), nil
}

// cmpNum compares exactly, even between a float and an exact value: 0.1
// typed in is 1/10, and a float 0.1 is not equal to it.
func cmpNum(a, b num) int {
	if a.r == nil && b.r == nil {
		switch {
		case a.f < b.f:
			return -1
		case a.f > b.f:
			return 1
		}
		return 0
	}
	return a.rat().Cmp(b.rat())
}

// intOf returns n as a big.Int when n is an exact integer.
func intOf(n num) (*big.Int, bool) {
	if n.r == nil || !n.r.IsInt() {
		return nil, false
	}
	return n.r.Num(), true
}

// smallInt returns n as an int when n is integral (exact or float) and
// within ±lim.
func smallInt(n num, lim int64) (int64, bool) {
	if n.r != nil {
		if !n.r.IsInt() || !n.r.Num().IsInt64() {
			return 0, false
		}
		v := n.r.Num().Int64()
		return v, v >= -lim && v <= lim
	}
	if n.f != math.Trunc(n.f) || math.Abs(n.f) > float64(lim) {
		return 0, false
	}
	return int64(n.f), true
}

type fnVal struct {
	name    string // "" for a lambda
	params  []string
	body    node
	env     *scope
	builtin *builtin
}

type scope struct {
	vars   map[string]val
	parent *scope
}

func (s *scope) lookup(name string) (val, bool) {
	for sc := s; sc != nil; sc = sc.parent {
		if v, ok := sc.vars[name]; ok {
			return v, true
		}
	}
	return nil, false
}

func typeName(v val) string {
	switch v.(type) {
	case num:
		return "number"
	case string:
		return "string"
	case bool:
		return "boolean"
	case []val:
		return "list"
	case *fnVal:
		return "function"
	case dateVal:
		return "date"
	case timeVal:
		return "datetime"
	}
	return "unknown"
}
