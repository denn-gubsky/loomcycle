package calc

import (
	"context"
	"math"
	"strings"
	"testing"
)

type floatCase struct {
	expr string
	want float64
}

// checkFloats compares at 17 digits, where formatting is the identity on a
// float64: the result must be the very float Go's math returns.
func checkFloats(t *testing.T, cases []floatCase) {
	t.Helper()
	for _, c := range cases {
		r, err := Eval(context.Background(), Request{Expr: c.expr, Digits: 17})
		if err != nil {
			t.Errorf("Eval(%q): %v", c.expr, err)
			continue
		}
		if want := formatFloat(c.want, 17); r.Text != want {
			t.Errorf("Eval(%q) = %s, want %s", c.expr, r.Text, want)
		}
		if r.Exact {
			t.Errorf("Eval(%q) reported exact", c.expr)
		}
	}
}

func TestFunctions_Elementary(t *testing.T) {
	checkTexts(t, []textCase{
		{"abs(-3/4)", "0.75"},
		{"abs(-2.5 + sin(0))", "2.5"},
		{"sign(-7)", "-1"},
		{"sign(0)", "0"},
		{"sign(2.5 + sin(0))", "1"},
		{"sqrt(16)", "4"},
		{"sqrt(9/4)", "1.5"},
		{"sqrt(0)", "0"},
		{"pow(2, 10)", "1024"},
		{"pow(4, -1/2)", "0.5"},
		{"hypot(3, 4)", "5"},
		{"log(e)", "1"},
		{"log(8, 2)", "3"},
		{"log(1000, 10)", "3"},
		{"log10(1000)", "3"},
		{"log2(1024)", "10"},
		{"log10(10^400)", "400"},
		{"log(2^16000)", "11090.3548889591"},
		{"min(3, 1, 2)", "1"},
		{"max([3, 1, 2])", "3"},
		{"min(1/3, 0.3)", "0.3"},
		{"clamp(5, 0, 3)", "3"},
		{"clamp(-1, 0, 3)", "0"},
		{"clamp(2, 0, 3)", "2"},
		{"sqrt([1, 4, 9])", "[1, 2, 3]"},
		{"abs([-1, [2, -3]])", "[1, [2, 3]]"},
	})
	checkFloats(t, []floatCase{
		{"sqrt(2)", math.Sqrt(2)},
		{"cbrt(-27)", -3},
		{"cbrt(2)", math.Cbrt(2)},
		{"exp(1.5)", math.Exp(1.5)},
		{"expm1(1e-10)", math.Expm1(1e-10)},
		{"log(10)", math.Log(10)},
		{"log10(2)", math.Log10(2)},
		{"log2(3)", math.Log2(3)},
		{"log1p(1e-10)", math.Log1p(1e-10)},
		{"hypot(1e200, 1e200)", math.Hypot(1e200, 1e200)},
		{"pow(2, 0.5)", math.Pow(2, 0.5)},
	})
	mustFail(t, "sqrt(-1)", codeDomain)
	mustFail(t, "log(0)", codeDomain)
	mustFail(t, "log(-1)", codeDomain)
	mustFail(t, "log(2, 1)", codeDomain)
	mustFail(t, "log1p(-1)", codeDomain)
	mustFail(t, "log10(0)", codeDomain)
	mustFail(t, "exp(1000)", codeOverflow)
	mustFail(t, "clamp(1, 3, 0)", codeDomain)
	mustFail(t, "min([])", codeDomain)
	mustFail(t, "min(\"a\", 1)", codeType)
	mustFail(t, "rem(1, 0)", codeDivZero)
	mustFail(t, "mod(1, 0)", codeDivZero)
	mustFail(t, "round(1, 1.5)", codeDomain)
}

func TestFunctions_Trigonometric(t *testing.T) {
	checkFloats(t, []floatCase{
		{"sin(1)", math.Sin(1)},
		{"cos(1)", math.Cos(1)},
		{"tan(1)", math.Tan(1)},
		{"asin(0.5)", math.Asin(0.5)},
		{"acos(0.5)", math.Acos(0.5)},
		{"atan(2)", math.Atan(2)},
		{"atan2(1, -1)", math.Atan2(1, -1)},
		{"sec(1)", 1 / math.Cos(1)},
		{"csc(1)", 1 / math.Sin(1)},
		{"cot(1)", 1 / math.Tan(1)},
		{"sinh(1)", math.Sinh(1)},
		{"cosh(1)", math.Cosh(1)},
		{"tanh(1)", math.Tanh(1)},
		{"asinh(1)", math.Asinh(1)},
		{"acosh(2)", math.Acosh(2)},
		{"atanh(0.5)", math.Atanh(0.5)},
		{"deg(pi)", 180},
		{"rad(180)", math.Pi},
	})
	mustFail(t, "asin(2)", codeDomain)
	mustFail(t, "acos(-1.5)", codeDomain)
	mustFail(t, "acosh(0.5)", codeDomain)
	mustFail(t, "atanh(1)", codeDomain)
	mustFail(t, "csc(0)", codeDivZero)
	mustFail(t, "cot(0)", codeDivZero)
}

func TestFunctions_Special(t *testing.T) {
	lg, _ := math.Lgamma(100.5)
	checkFloats(t, []floatCase{
		{"gamma(5)", 24},
		{"gamma(0.5)", math.Gamma(0.5)},
		{"gamma(-1.5)", math.Gamma(-1.5)},
		{"lgamma(100.5)", lg},
		{"beta(2, 3)", 1.0 / 12},
		{"erf(0.5)", math.Erf(0.5)},
		{"erfc(0.5)", math.Erfc(0.5)},
		{"erfinv(0.5)", math.Erfinv(0.5)},
		{"erfcinv(0.5)", math.Erfcinv(0.5)},
		{"j0(2)", math.J0(2)},
		{"j1(2)", math.J1(2)},
		{"jn(3, 2)", math.Jn(3, 2)},
		{"y0(2)", math.Y0(2)},
		{"y1(2)", math.Y1(2)},
		{"yn(3, 2)", math.Yn(3, 2)},
	})
	checkTexts(t, []textCase{
		{"jn(0, [0, 0])", "[1, 1]"},
		{"beta(-0.5, 0.5)", "0"},
	})
	mustFail(t, "gamma(0)", codeDomain)
	mustFail(t, "gamma(-2)", codeDomain)
	mustFail(t, "lgamma(-3)", codeDomain)
	mustFail(t, "gamma(200)", codeOverflow)
	mustFail(t, "beta(0, 1)", codeDomain)
	mustFail(t, "erfinv(1)", codeDomain)
	mustFail(t, "erfcinv(0)", codeDomain)
	mustFail(t, "y0(0)", codeDomain)
	mustFail(t, "yn(2, -1)", codeDomain)
	mustFail(t, "jn(1.5, 2)", codeDomain)
}

func TestFunctions_Integers(t *testing.T) {
	checkTexts(t, []textCase{
		{"factorial(0)", "1"},
		{"factorial(20)", "2432902008176640000"},
		{"25!", "15511210043330985984000000"},
		{"binomial(5, 2)", "10"},
		{"binomial(50, 25)", "126410606437752"},
		{"binomial(5, 7)", "0"},
		{"binomial(5, -1)", "0"},
		{"permutations(5, 2)", "20"},
		{"permutations(5, 0)", "1"},
		{"gcd(12, 18)", "6"},
		{"gcd(12, -18, 8)", "2"},
		{"gcd([0, 0])", "0"},
		{"lcm(4, 6)", "12"},
		{"lcm([2, 3, 4])", "12"},
		{"lcm(0, 5)", "0"},
		{"isprime(97)", "true"},
		{"isprime(1)", "false"},
		{"isprime(-7)", "false"},
		{"isprime(2^127 - 1)", "true"},
		{"isprime([2, 4, 5])", "[true, false, true]"},
		{"nextprime(1)", "2"},
		{"nextprime(2)", "3"},
		{"nextprime(13)", "17"},
		{"nextprime(2^64)", "18446744073709551629"},
		{"factor(360)", "[2, 2, 2, 3, 3, 5]"},
		{"factor(-12)", "[-1, 2, 2, 3]"},
		{"factor(1)", "[]"},
		{"factor(97)", "[97]"},
		// two 32-bit primes: Pollard's rho, not trial division
		{"factor(4294967291 * 4294967279)", "[4294967279, 4294967291]"},
		{"factor(2^64 - 1)", "[3, 5, 17, 257, 641, 65537, 6700417]"},
		{"divisors(12)", "[1, 2, 3, 4, 6, 12]"},
		{"divisors(1)", "[1]"},
		{"len(divisors(720720))", "240"},
		{"modpow(2, 10, 1000)", "24"},
		{"modpow(3, -1, 7)", "5"},
		{"modpow(2, 0, 1)", "0"},
		{"modinv(3, 7)", "5"},
		{"modinv(-3, 7)", "2"},
		{"fib(0)", "0"},
		{"fib(10)", "55"},
		{"fib(100)", "354224848179261915075"},
		{"fib(-8)", "-21"},
		{"fib(-7)", "13"},
	})
	if e := mustFail(t, "factorial(2.5)", codeType); !strings.Contains(e.Hint, "integer") {
		t.Errorf("hint = %q", e.Hint)
	}
	if e := mustFail(t, "factorial(sqrt(4) + sin(0))", codeType); !strings.Contains(e.Hint, "gamma") {
		t.Errorf("hint = %q", e.Hint)
	}
	mustFail(t, "factorial(-1)", codeDomain)
	mustFail(t, "binomial(-1, 1)", codeDomain)
	mustFail(t, "gcd([])", codeDomain)
	mustFail(t, "factor(0)", codeDomain)
	mustFail(t, "factor(2^64)", codeTooLarge)
	mustFail(t, "modpow(2, 3, 0)", codeDomain)
	mustFail(t, "modpow(2, -1, 4)", codeDomain)
	mustFail(t, "modpow(2, 2^5000, 7)", codeTooLarge)
	mustFail(t, "modinv(2, 4)", codeDomain)
	mustFail(t, "modinv(2, 0)", codeDomain)
	mustFail(t, "isprime(2^1024)", codeTooLarge)
	mustFail(t, "fib(100000)", codeTooLarge)
	mustFail(t, "binomial(10^6, 5*10^5)", codeTooLarge)
	mustFail(t, "lcm(2^9000, 2^9000 - 1)", codeTooLarge)
}

func TestFunctions_BitsAndBases(t *testing.T) {
	checkTexts(t, []textCase{
		{"bitand(12, 10)", "8"},
		{"bitor(12, 10)", "14"},
		{"bitxor(12, 10)", "6"},
		{"bitnot(0)", "-1"},
		{"bitand(-1, 255)", "255"},
		{"shl(1, 10)", "1024"},
		{"shr(1024, 3)", "128"},
		{"shr(-5, 1)", "-3"},
		{"base(255, 16)", "ff"},
		{"base(-10, 2)", "-1010"},
		{"base([1, 2], 2)", `["1", "10"]`},
		{`frombase("ff", 16)`, "255"},
		{`frombase("FF", 16)`, "255"},
		{`frombase("z", 36)`, "35"},
		{`frombase(base(12345, 7), 7)`, "12345"},
	})
	mustFail(t, "shl(1, 20000)", codeTooLarge)
	mustFail(t, "shl(1, -1)", codeDomain)
	mustFail(t, "base(10, 1)", codeDomain)
	mustFail(t, "base(10, 37)", codeDomain)
	mustFail(t, `frombase("12", 2)`, codeDomain)
	mustFail(t, `frombase("1_0", 2)`, codeDomain)
	mustFail(t, `frombase(10, 2)`, codeType)
	mustFail(t, "bitand(1.5, 1)", codeType)
}

func TestFunctions_Sequences(t *testing.T) {
	checkTexts(t, []textCase{
		{"len([1, 2, 3])", "3"},
		{"len([])", "0"},
		{"filter(1:10, x -> x % 2 == 0)", "[2, 4, 6, 8, 10]"},
		{"filter(x -> x > 1, [1, 2, 3])", "[2, 3]"},
		{"reduce(1:5, (a, b) -> a * b)", "120"},
		{"reduce(1:5, (a, b) -> a + b, 100)", "115"},
		{"reduce([], (a, b) -> a + b, 0)", "0"},
		{"sort([3, 1, 2])", "[1, 2, 3]"},
		{"sort([1/3, 0.3, -1])", "[-1, 0.3, 0.333333333333333]"},
		{`sort(["b", "a"])`, `["a", "b"]`},
		{"sort([[2, \"b\"], [1, \"a\"]], p -> p[1])", `[[1, "a"], [2, "b"]]`},
		{"sort([3, 1, 2], x -> -x)", "[3, 2, 1]"},
		{"reverse([1, 2, 3])", "[3, 2, 1]"},
		{"unique([1, 2, 1, 3, 2])", "[1, 2, 3]"},
		{"unique([0.5, 1/2, [1], [1]])", "[0.5, [1]]"},
		{"count([1, 2, 3])", "3"},
		{"count([1, 2, 3], x -> x > 1)", "2"},
		{"count([1, 2, 1], 1)", "2"},
		{"any([false, true])", "true"},
		{"any([])", "false"},
		{"all([true, true])", "true"},
		{"all([])", "true"},
		{"any(1:5, x -> x > 4)", "true"},
		{"all(1:5, x -> x > 4)", "false"},
		{"zip([1, 2, 3], [4, 5])", "[[1, 4], [2, 5]]"},
		{"sum([1, 2, 3])", "6"},
		{"sum(1, 2, 3)", "6"},
		{"sum([])", "0"},
		{"sum([[1, 2], [3, 4]])", "[4, 6]"},
		{"product([1, 2, 3, 4])", "24"},
		{"product([])", "1"},
		{"sum(1:100)", "5050"},
		{"sum(map(1:10, k -> 1/k))", "2.92896825396825"},
	})
	mustFail(t, "map([1], 2)", codeType)
	mustFail(t, "map(1, x -> x)", codeType)
	mustFail(t, "filter([1], x -> x)", codeType)
	mustFail(t, "reduce([], (a, b) -> a)", codeDomain)
	mustFail(t, "sort([1, \"a\"])", codeType)
	mustFail(t, "any([1])", codeType)
	mustFail(t, "sum([\"a\"])", codeType)
	mustFail(t, "len(1)", codeType)
	mustFail(t, "unique([sin])", codeType)
	mustFail(t, "range(1, 2, 0)", codeDomain)
	mustFail(t, "map([1, 2], (a, b) -> a)", codeArity)
}

func TestFunctions_RandomIsReproducibleWithASeed(t *testing.T) {
	seed := int64(42)
	expr := "[random(), randint(1, 6), randnormal(), shuffle(1:5), sample(1:10, 3)]"
	a, err := Eval(context.Background(), Request{Expr: expr, Seed: &seed})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Eval(context.Background(), Request{Expr: expr, Seed: &seed})
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != b.Text {
		t.Fatalf("seeded runs differ: %s vs %s", a.Text, b.Text)
	}
	checks := []string{
		"r = random(); r >= 0 and r < 1",
		"all(map(1:200, i -> randint(-2, 2) >= -2 and randint(-2, 2) <= 2))",
		"sort(shuffle(1:20)) == 1:20",
		"s = sample(1:50, 50); sort(s) == 1:50",
		"len(unique(sample(1:100, 30))) == 30",
		"x = randnormal(10, 0); x == 10",
		"randint(2^70, 2^70) == 2^70",
	}
	for _, c := range checks {
		r, err := Eval(context.Background(), Request{Expr: c, Seed: &seed})
		if err != nil || r.Text != "true" {
			t.Errorf("%s = %q, %v", c, r.Text, err)
		}
	}
	if r := mustEval(t, "randint(1, 6)"); !r.Exact {
		t.Errorf("randint is not exact")
	}
	mustFail(t, "randint(5, 1)", codeDomain)
	mustFail(t, "randint(0, 2^64)", codeTooLarge)
	mustFail(t, "randnormal(0, -1)", codeDomain)
	mustFail(t, "sample([1, 2], 3)", codeDomain)
}

func TestFunctions_TableIsCompleteAndConsistent(t *testing.T) {
	fs := Functions()
	seen := map[string]bool{}
	for _, f := range fs {
		if seen[f.Name] {
			t.Errorf("%s listed twice", f.Name)
		}
		seen[f.Name] = true
		if f.Signature == "" || f.Summary == "" || f.Section == "" {
			t.Errorf("%s: missing signature, summary or section: %+v", f.Name, f)
		}
		if !strings.HasPrefix(f.Signature, f.Name+"(") {
			t.Errorf("%s: signature %q does not start with the name", f.Name, f.Signature)
		}
		b := builtinIndex[f.Name]
		if b == nil || b.fn == nil && f.Name != "if" {
			t.Errorf("%s: no implementation", f.Name)
		}
	}
	// The functions this PR promises, by section.
	want := map[string]string{
		SecElementary: "abs sign sqrt cbrt exp expm1 log log10 log2 log1p pow hypot floor ceil round roundeven trunc mod rem min max clamp",
		SecTrig:       "sin cos tan asin acos atan atan2 sec csc cot sinh cosh tanh asinh acosh atanh deg rad",
		SecSpecial:    "gamma lgamma beta erf erfc erfinv erfcinv j0 j1 jn y0 y1 yn",
		SecIntegers:   "factorial binomial permutations gcd lcm isprime nextprime factor divisors modpow modinv fib",
		SecBits:       "bitand bitor bitxor bitnot shl shr base frombase",
		SecSequences:  "range map filter reduce sort reverse unique count any all zip sum product len",
		SecRandom:     "random randint randnormal shuffle sample",
		SecControl:    "if",
		SecStatistics: "mean median mode spread geomean harmmean wmean variance stdev zscore quantile percentile rank cumsum cumprod diff covariance correlation linreg",
		SecFinance:    "pmt fv pv nper periodrate npv irr compound cagr pct pctchange",
	}
	got := map[string][]string{}
	for _, f := range fs {
		got[f.Section] = append(got[f.Section], f.Name)
	}
	for sec, names := range want {
		for _, n := range strings.Fields(names) {
			if !contains(got[sec], n) {
				t.Errorf("section %s is missing %s", sec, n)
			}
		}
		if len(got[sec]) != len(strings.Fields(names)) {
			t.Errorf("section %s has %v, want %s", sec, got[sec], names)
		}
	}
	if len(got) != len(want) {
		t.Errorf("sections = %v", got)
	}
}

func TestFunctions_ArityErrorsShowTheSignature(t *testing.T) {
	for _, f := range Functions() {
		b := builtinIndex[f.Name]
		if b.max < 0 || f.Name == "if" {
			continue
		}
		args := strings.TrimSuffix(strings.Repeat("1, ", b.max+1), ", ")
		e := mustFail(t, f.Name+"("+args+")", codeArity)
		if e.Hint != f.Signature {
			t.Errorf("%s: hint %q, want %q", f.Name, e.Hint, f.Signature)
		}
	}
}
