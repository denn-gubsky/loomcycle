package calc

import (
	"context"
	"testing"
	"time"
)

// fuzzSeeds covers every grammar form and every function section; the
// fuzzers mutate from here. testdata/fuzz holds the corpus entries the
// local runs found interesting.
var fuzzSeeds = []string{
	"1 + 2 * 3", "-2^2", "2^3^2", "5!", "-7 % 3", "0.1 + 0.2 == 0.3",
	"1_000.5e-3", "0xff + 0b101", "\"a\\\"b\"", "[1, 2][1]", "1:2:10",
	"v = [1, 2, 3]; v .* v ./ 2 .^ 1", "f(x) = x^2; f(3)", "g = (a, b) -> a + b; g(1, 2)",
	"map(1:5, x -> x^2)", "filter(1:9, x -> x % 2 == 0)", "reduce(1:5, (a, b) -> a * b, 1)",
	"if(1 < 2, 3, 1/0)", "not true or false and true", "sort([3, 1, 2], x -> -x)",
	"unique([1, 1, 2])", "zip([1], [2])", "any([true])", "count([1, 2], 1)",
	"sum(1:10)", "product([1, 2])", "len(\"ab\")", "range(0, 1, 1/4)",
	"sqrt(2) + cbrt(8) + exp(1) + log(10, 2) + log10(5) + log2(5) + log1p(1) + expm1(1)",
	"round(2.5) + roundeven(2.5) + floor(-1.5, 1) + ceil(1.25, 1) + trunc(-1.7)",
	"abs(-1) + sign(-2) + pow(2, 3) + hypot(3, 4) + mod(7, 3) + rem(-7, 3) + clamp(5, 0, 3)",
	"min(1, 2) + max([1, 2])",
	"sin(1) + cos(1) + tan(1) + asin(0.5) + acos(0.5) + atan(1) + atan2(1, 2)",
	"sec(1) + csc(1) + cot(1) + sinh(1) + cosh(1) + tanh(1) + asinh(1) + acosh(2) + atanh(0.5) + deg(1) + rad(1)",
	"gamma(5) + lgamma(5) + beta(2, 3) + erf(1) + erfc(1) + erfinv(0.5) + erfcinv(0.5)",
	"j0(1) + j1(1) + jn(2, 1) + y0(1) + y1(1) + yn(2, 1)",
	"factorial(10) + binomial(10, 3) + permutations(5, 2) + gcd(4, 6) + lcm(4, 6)",
	"[isprime(97), nextprime(100), factor(360), divisors(12), modpow(2, 10, 7), modinv(3, 7), fib(20)]",
	"[bitand(6, 3), bitor(6, 3), bitxor(6, 3), bitnot(6), shl(1, 4), shr(16, 2), base(255, 16), frombase(\"ff\", 16)]",
	"[random(), randint(1, 6), randnormal(0, 1), shuffle([1, 2]), sample([1, 2, 3], 2)]",
	"9^9^9", "2x", "2 ** 3", "if(x = 1, 2, 3)", "f(x) = f(x)", "[1][0]", "sqrt(-1)", "1/0",
	"factorial(10^6)", "((((1))))", "x -> y -> x + y", "# comment\n1",
}

func FuzzParse(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > MaxExprBytes {
			return
		}
		stmts, err := parse(src)
		if err == nil && len(stmts) == 0 {
			t.Fatalf("parse(%q) returned no statements and no error", src)
		}
		if err != nil && (err.Pos < -1 || err.Pos > len(src)) {
			t.Fatalf("parse(%q): position %d outside the input", src, err.Pos)
		}
	})
}

func FuzzEval(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	declared := map[string]bool{}
	for _, c := range ErrorCodes() {
		declared[c] = true
	}
	f.Fuzz(func(t *testing.T, src string) {
		seed := int64(1)
		start := time.Now()
		r, err := Eval(context.Background(), Request{Expr: src, Seed: &seed})
		if d := time.Since(start); d > MaxWall+time.Second {
			t.Fatalf("Eval(%q) took %v", src, d)
		}
		if err != nil {
			if !declared[err.Code] {
				t.Fatalf("Eval(%q): undeclared code %q", src, err.Code)
			}
			return
		}
		if len(r.Text) > MaxOutputBytes {
			t.Fatalf("Eval(%q): text of %d bytes", src, len(r.Text))
		}
		if r.Type == "" {
			t.Fatalf("Eval(%q): empty type", src)
		}
	})
}
