package calc

import (
	"math"
	"math/big"
	"math/bits"
	"sort"
)

// The integer functions take exact integers only and refuse, rather than
// degrade to a float, when a result would pass the bit cap: an approximate
// prime factor or binomial coefficient is a wrong answer, not a rounded one.
var integerFuncs = []builtin{
	{name: "factorial", sig: "factorial(n)", section: SecIntegers, min: 1, max: 1, vec: true, fn: fnFactorial,
		summary: "n! for an integer n >= 0; also written n!"},
	{name: "binomial", sig: "binomial(n, k)", section: SecIntegers, min: 2, max: 2, vec: true, fn: fnBinomial,
		summary: "n choose k, the number of k-subsets of n items"},
	{name: "permutations", sig: "permutations(n, k)", section: SecIntegers, min: 2, max: 2, vec: true, fn: fnPermutations,
		summary: "n!/(n-k)!, the number of ordered k-arrangements of n items"},
	{name: "gcd", sig: "gcd(a, b, ...) or gcd(list)", section: SecIntegers, min: 1, max: -1, fn: gcdLcm(false),
		summary: "greatest common divisor"},
	{name: "lcm", sig: "lcm(a, b, ...) or lcm(list)", section: SecIntegers, min: 1, max: -1, fn: gcdLcm(true),
		summary: "least common multiple"},
	{name: "isprime", sig: "isprime(n)", section: SecIntegers, min: 1, max: 1, vec: true, fn: fnIsPrime,
		summary: "whether n is prime (certain below 2^64, error under 4^-20 above; n below 2^1024)"},
	{name: "nextprime", sig: "nextprime(n)", section: SecIntegers, min: 1, max: 1, vec: true, fn: fnNextPrime,
		summary: "the smallest prime greater than n"},
	{name: "factor", sig: "factor(n)", section: SecIntegers, min: 1, max: 1, fn: fnFactor,
		summary: "prime factors of n with multiplicity, ascending; |n| below 2^64"},
	{name: "divisors", sig: "divisors(n)", section: SecIntegers, min: 1, max: 1, fn: fnDivisors,
		summary: "all positive divisors of n, ascending; n below 2^64"},
	{name: "modpow", sig: "modpow(b, e, m)", section: SecIntegers, min: 3, max: 3, fn: fnModPow,
		summary: "b^e mod m without the huge intermediate; e < 0 uses the modular inverse"},
	{name: "modinv", sig: "modinv(a, m)", section: SecIntegers, min: 2, max: 2, fn: fnModInv,
		summary: "x with a*x = 1 (mod m)"},
	{name: "fib", sig: "fib(n)", section: SecIntegers, min: 1, max: 1, vec: true, fn: fnFib,
		summary: "the n-th Fibonacci number, fib(0) = 0, fib(1) = 1"},
}

func tooLarge(a args, est float64) *Error {
	return errf(codeTooLarge, a.pos, "%s: the result would have about %.3g bits, over the %d-bit cap", a.name, est, MaxExactBits)
}

// nonNegInt64 returns argument i as an exact integer in [0, 2^63).
func (a args) nonNegInt64(i int) (int64, *Error) {
	v, err := a.integer(i)
	if err != nil {
		return 0, err
	}
	if v.Sign() < 0 {
		return 0, errf(codeDomain, a.pos, "%s: argument %d must not be negative", a.name, i+1)
	}
	if !v.IsInt64() {
		return 0, errf(codeTooLarge, a.pos, "%s: argument %d is too large", a.name, i+1)
	}
	return v.Int64(), nil
}

// lgammaBits is log2 of n! via lgamma, for estimating sizes before work.
func lgammaBits(n float64) float64 {
	lg, _ := math.Lgamma(n + 1)
	return lg / math.Ln2
}

func (e *evaluator) intResult(v *big.Int, a args) (val, *Error) {
	if v.BitLen() > MaxExactBits {
		return nil, tooLarge(a, float64(v.BitLen()))
	}
	if err := e.charge(v.BitLen()/bitStepCost, a.pos); err != nil {
		return nil, err
	}
	return exactInt(v), nil
}

func fnFactorial(e *evaluator, a args) (val, *Error) {
	n, err := a.nonNegInt64(0)
	if err != nil {
		if err.Code == string(codeType) {
			err.Hint = "factorial needs an exact integer; use gamma(x + 1) for a float"
		}
		return nil, err
	}
	if est := lgammaBits(float64(n)); est > MaxExactBits {
		return nil, tooLarge(a, est)
	}
	if err := e.charge(int(n), a.pos); err != nil {
		return nil, err
	}
	return e.intResult(new(big.Int).MulRange(1, n), a)
}

func fnBinomial(e *evaluator, a args) (val, *Error) {
	n, err := a.nonNegInt64(0)
	if err != nil {
		return nil, err
	}
	kb, err := a.integer(1)
	if err != nil {
		return nil, err
	}
	if kb.Sign() < 0 || kb.Cmp(big.NewInt(n)) > 0 {
		return exactInt64(0), nil
	}
	k := min(kb.Int64(), n-kb.Int64())
	if est := lgammaBits(float64(n)) - lgammaBits(float64(k)) - lgammaBits(float64(n-k)); est > MaxExactBits {
		return nil, tooLarge(a, est)
	}
	if err := e.charge(int(k), a.pos); err != nil {
		return nil, err
	}
	return e.intResult(new(big.Int).Binomial(n, k), a)
}

func fnPermutations(e *evaluator, a args) (val, *Error) {
	n, err := a.nonNegInt64(0)
	if err != nil {
		return nil, err
	}
	kb, err := a.integer(1)
	if err != nil {
		return nil, err
	}
	if kb.Sign() < 0 || kb.Cmp(big.NewInt(n)) > 0 {
		return exactInt64(0), nil
	}
	k := kb.Int64()
	if k == 0 {
		return exactInt64(1), nil
	}
	if est := lgammaBits(float64(n)) - lgammaBits(float64(n-k)); est > MaxExactBits {
		return nil, tooLarge(a, est)
	}
	if err := e.charge(int(k), a.pos); err != nil {
		return nil, err
	}
	return e.intResult(new(big.Int).MulRange(n-k+1, n), a)
}

func gcdLcm(lcm bool) func(*evaluator, args) (val, *Error) {
	return func(e *evaluator, a args) (val, *Error) {
		items := a.v
		if len(items) == 1 {
			if l, ok := items[0].([]val); ok {
				items = l
			}
		}
		if len(items) == 0 {
			return nil, errf(codeDomain, a.pos, "%s of an empty list is undefined", a.name)
		}
		if err := e.charge(len(items), a.pos); err != nil {
			return nil, err
		}
		sub := args{v: items, pos: a.pos, name: a.name}
		acc, err := sub.integer(0)
		if err != nil {
			return nil, err
		}
		acc = new(big.Int).Abs(acc)
		for i := 1; i < len(items); i++ {
			x, err := sub.integer(i)
			if err != nil {
				return nil, err
			}
			x = new(big.Int).Abs(x)
			g := new(big.Int).GCD(nil, nil, acc, x)
			if !lcm {
				acc = g
				continue
			}
			if g.Sign() == 0 {
				acc = big.NewInt(0)
				continue
			}
			acc = new(big.Int).Mul(new(big.Int).Quo(acc, g), x)
			if acc.BitLen() > MaxExactBits {
				return nil, tooLarge(a, float64(acc.BitLen()))
			}
		}
		return e.intResult(acc, a)
	}
}

func primeArg(a args, maxBits int) (*big.Int, *Error) {
	n, err := a.integer(0)
	if err != nil {
		return nil, err
	}
	if n.BitLen() > maxBits {
		return nil, errf(codeTooLarge, a.pos, "%s: n has %d bits; the limit is %d", a.name, n.BitLen(), maxBits)
	}
	return n, nil
}

func fnIsPrime(e *evaluator, a args) (val, *Error) {
	n, err := primeArg(a, maxPrimeBits)
	if err != nil {
		return nil, err
	}
	if err := e.charge(1+n.BitLen()/64, a.pos); err != nil {
		return nil, err
	}
	return n.Sign() > 0 && n.ProbablyPrime(20), nil
}

func fnNextPrime(e *evaluator, a args) (val, *Error) {
	n, err := primeArg(a, maxPrimeBits-1)
	if err != nil {
		return nil, err
	}
	two := big.NewInt(2)
	if n.Cmp(two) < 0 {
		return exactInt64(2), nil
	}
	c := new(big.Int).Add(n, big.NewInt(1))
	if c.Bit(0) == 0 && c.Cmp(two) != 0 {
		c.Add(c, big.NewInt(1))
	}
	for {
		if err := e.charge(1+c.BitLen()/64, a.pos); err != nil {
			return nil, err
		}
		if c.ProbablyPrime(20) {
			return exactInt(new(big.Int).Set(c)), nil
		}
		c.Add(c, two)
	}
}

// factorArg returns |n| as a uint64 and its sign; factor and divisors work
// in machine words, where Pollard's rho is fast and ProbablyPrime(0) exact.
func factorArg(a args) (uint64, int, *Error) {
	n, err := a.integer(0)
	if err != nil {
		return 0, 0, err
	}
	if n.Sign() == 0 {
		return 0, 0, errf(codeDomain, a.pos, "%s(0) is undefined", a.name)
	}
	m := new(big.Int).Abs(n)
	if !m.IsUint64() {
		return 0, 0, errf(codeTooLarge, a.pos, "%s handles |n| below 2^64; n has %d bits", a.name, m.BitLen())
	}
	return m.Uint64(), n.Sign(), nil
}

func fnFactor(e *evaluator, a args) (val, *Error) {
	m, sign, err := factorArg(a)
	if err != nil {
		return nil, err
	}
	fs, err := e.factorize(m, a.pos)
	if err != nil {
		return nil, err
	}
	out := make([]val, 0, len(fs)+1)
	if sign < 0 {
		out = append(out, exactInt64(-1))
	}
	for _, f := range fs {
		out = append(out, exactInt(new(big.Int).SetUint64(f)))
	}
	return out, nil
}

func fnDivisors(e *evaluator, a args) (val, *Error) {
	m, _, err := factorArg(a)
	if err != nil {
		return nil, err
	}
	fs, err := e.factorize(m, a.pos)
	if err != nil {
		return nil, err
	}
	divs := []uint64{1}
	for i := 0; i < len(fs); {
		p, k := fs[i], 0
		for i < len(fs) && fs[i] == p {
			i++
			k++
		}
		n := len(divs)
		pk := uint64(1)
		for j := 0; j < k; j++ {
			pk *= p
			for _, d := range divs[:n] {
				divs = append(divs, d*pk)
			}
		}
		if err := e.charge(len(divs), a.pos); err != nil {
			return nil, err
		}
	}
	sort.Slice(divs, func(i, j int) bool { return divs[i] < divs[j] })
	out := make([]val, len(divs))
	for i, d := range divs {
		out[i] = exactInt(new(big.Int).SetUint64(d))
	}
	return out, nil
}

// factorize returns the prime factors of n >= 1, ascending, with
// multiplicity: trial division by small primes, then Pollard–Brent rho.
func (e *evaluator) factorize(n uint64, pos int) ([]uint64, *Error) {
	var fs []uint64
	for _, p := range []uint64{2, 3, 5, 7, 11, 13, 17, 19, 23, 29, 31, 37} {
		for n%p == 0 {
			fs = append(fs, p)
			n /= p
		}
	}
	stack := []uint64{}
	if n > 1 {
		stack = append(stack, n)
	}
	for len(stack) > 0 {
		m := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if new(big.Int).SetUint64(m).ProbablyPrime(0) {
			fs = append(fs, m)
			continue
		}
		d, err := e.rho(m, pos)
		if err != nil {
			return nil, err
		}
		stack = append(stack, d, m/d)
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i] < fs[j] })
	return fs, nil
}

func mulMod(a, b, m uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	return bits.Rem64(hi, lo, m)
}

func gcd64(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// rho finds a non-trivial factor of the odd composite n (no factor below
// 41). Each iteration is one step; a semiprime of two 32-bit primes needs
// about 2^16.
func (e *evaluator) rho(n uint64, pos int) (uint64, *Error) {
	for c := uint64(1); ; c++ {
		y, r, q := uint64(2), uint64(1), uint64(1)
		var g, x, ys uint64
		f := func(v uint64) uint64 {
			// v*v + c reduced mod n; the add is carried so an n near 2^64
			// cannot wrap.
			lo, carry := bits.Add64(mulMod(v, v, n), c, 0)
			return bits.Rem64(carry, lo, n)
		}
		for g = 1; g == 1; r *= 2 {
			x = y
			for i := uint64(0); i < r; i++ {
				y = f(y)
			}
			for k := uint64(0); k < r && g == 1; k += 128 {
				if err := e.charge(1, pos); err != nil {
					return 0, err
				}
				ys = y
				for i := uint64(0); i < min(128, r-k); i++ {
					y = f(y)
					diff := x - y
					if y > x {
						diff = y - x
					}
					q = mulMod(q, diff, n)
				}
				g = gcd64(q, n)
			}
		}
		if g == n {
			for g = 1; g == 1; {
				ys = f(ys)
				diff := x - ys
				if ys > x {
					diff = ys - x
				}
				g = gcd64(diff, n)
			}
		}
		if g != n {
			return g, nil
		}
	}
}

func fnModPow(e *evaluator, a args) (val, *Error) {
	b, err := a.integer(0)
	if err != nil {
		return nil, err
	}
	x, err := a.integer(1)
	if err != nil {
		return nil, err
	}
	m, err := a.integer(2)
	if err != nil {
		return nil, err
	}
	if m.Sign() <= 0 {
		return nil, errf(codeDomain, a.pos, "modpow: the modulus must be positive")
	}
	if m.BitLen() > maxModBits || x.BitLen() > maxModBits {
		return nil, errf(codeTooLarge, a.pos, "modpow: the modulus and exponent are limited to %d bits", maxModBits)
	}
	if err := e.charge(1+x.BitLen()*m.BitLen()/4096, a.pos); err != nil {
		return nil, err
	}
	base := new(big.Int).Mod(b, m)
	if x.Sign() < 0 {
		inv := new(big.Int).ModInverse(base, m)
		if inv == nil {
			return nil, errf(codeDomain, a.pos, "modpow: %s has no inverse mod %s, so a negative exponent is undefined", b, m)
		}
		base = inv
		x = new(big.Int).Neg(x)
	}
	return exactInt(new(big.Int).Exp(base, x, m)), nil
}

func fnModInv(_ *evaluator, a args) (val, *Error) {
	x, err := a.integer(0)
	if err != nil {
		return nil, err
	}
	m, err := a.integer(1)
	if err != nil {
		return nil, err
	}
	if m.Sign() <= 0 {
		return nil, errf(codeDomain, a.pos, "modinv: the modulus must be positive")
	}
	if m.Cmp(big.NewInt(1)) == 0 {
		return exactInt64(0), nil
	}
	inv := new(big.Int).ModInverse(new(big.Int).Mod(x, m), m)
	if inv == nil {
		return nil, errf(codeDomain, a.pos, "modinv: %s and %s are not coprime, so there is no inverse", x, m)
	}
	return exactInt(inv), nil
}

// fib uses fib(-n) = (-1)^(n+1) fib(n) for negative n.
func fnFib(e *evaluator, a args) (val, *Error) {
	nb, err := a.integer(0)
	if err != nil {
		return nil, err
	}
	if !nb.IsInt64() {
		return nil, tooLarge(a, math.Inf(1))
	}
	n := nb.Int64()
	neg := n < 0
	if neg {
		n = -n
	}
	if est := float64(n) * math.Log2(math.Phi); est > MaxExactBits {
		return nil, tooLarge(a, est)
	}
	if err := e.charge(int(n), a.pos); err != nil {
		return nil, err
	}
	x, y := big.NewInt(0), big.NewInt(1)
	for i := int64(0); i < n; i++ {
		x, y = y, x.Add(x, y)
	}
	if neg && n%2 == 0 {
		x.Neg(x)
	}
	return exactInt(x), nil
}
