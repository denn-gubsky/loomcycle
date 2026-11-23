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
	"[convert(5, \"mi\", \"km\"), convertdelta(1, \"degC/s\", \"K/min\"), units(), units(\"angle\")]",
	"factorial(10^6)", "((((1))))", "x -> y -> x + y", "# comment\n1",
	`d = date("2026-01-31"); [addmonths(d, 1), eomonth(d, 1), d + 30, date(2026, 3, 1) - d, weekday(d), datefmt(d, "D MMM YYYY")]`,
	`t = tz(datetime("2026-03-29T00:30:00Z"), "Europe/Kyiv"); [t + 3600, hour(t), datediff(date(t), date("2027-01-01"), "months")]`,
	`[workdays(date("2026-11-01"), date("2026-11-30")), addworkdays(date("2026-11-20"), 5), yearfrac(date("2026-01-01"), date("2026-07-01"), "act/act")]`,
	"A = [[2, 1], [1, 3]]; [A * A', A[1, :], A[:, 2], A * [1, 2], [1, 2] * A, A .* A, size(A)]",
	"A = [[4, 1], [1, 3]]; [inv(A), det(A), solve(A, [1, 2]), lstsq(A, [1, 2]), pinv(A), matrank(A), cond(A), norm(A, 2)]",
	"A = [[4, 1], [1, 3]]; [lu(A), qr(A), cholesky(A), eig(A), svd(A), trace(A), kron(A, A)]",
	"[zeros(2, 2), ones(2), identity(2), diag([1, 2]), reshape(1:4, 2, 2), hstack([1], [2]), vstack([1], [2]), linspace(0, 1, 3)]",
	"[dot([1, 2], [3, 4]), cross([1, 0, 0], [0, 1, 0]), mean([[1, 2], [3, 4]]), [1, 2]']",
	"[normpdf(1), normcdf(1, 0, 2), norminv(0.9), tpdf(1, 3), tcdf(1, 3), tinv(0.9, 3), chi2pdf(1, 2), chi2cdf(1, 2), chi2inv(0.5, 2)]",
	"[fpdf(1, 2, 3), fcdf(1, 2, 3), finv(0.5, 2, 3), exppdf(1, 2), expcdf(1, 2), expinv(0.5, 2), unifpdf(1, 0, 2), unifcdf(1, 0, 2), unifinv(0.5, 0, 2)]",
	"[binompdf(3, 10, 0.3), binomcdf(3, 10, 0.3), poisspdf(2, 4), poisscdf(2, 4), norminv(1), binompdf(11, 10, 0.5)]",
	"[integrate(x -> x^2, 0, 1), integrate(sin, 1, 0), deriv(exp, 1), root(x -> x^2 - 2, 0, 2), minimize(cos, 0, 6)]",
	"f(x) = 1 / x; [integrate(f, 0, 1), deriv(x -> if(x > 0, 1, 0), 0), root(f, 1, 2), minimize(x -> x, 1, 1)]",
	"p = polyfit([0, 1, 2, 3], [1, 2, 5, 10], 2); [p, polyval(p, [4, 5]), interp([1.5, 2], [1, 2, 3], [1, 4, 9])]",
	"z = 3 + 2i; [z * conj(z), z / 1.5i, z^3, z^0.5, -z, abs(z), arg(z), re(z), im(z), complex(1, -1), 2i == 2i, 1e3i]",
	"[sqrt(-1 + 0i), exp(pi * 1i), log(-1 + 0i, 2), sin(1i), acosh(0.5 + 0i), round(1.25 - 2.5i, 1), pow(1i, 1i), 0i^0]",
	"i = 4; [2*i, 2i, 2in, 2pi, .5i, 2.i, 0x1i, 2e3i, [1i, 2] .* [2, 3i], sum([1i, 2]), unique([1i, 1i])]",
	"[polyroots([1, 0, 1]), polyroots([3.2, 2, 1]), eig([[1, -1], [1, 1]]), eig([[0, 1], [0, 0]]), 1i < 2, sort([1i])]",
	`t = csv("r,m,a\nEU,jan,1.5\nUS,jan,\nEU,feb,2"); [t.a, t[2], t[2].a, t["r"], t[[3, 1]], size(t), sum(t.a), t.a * 2, count(t.a)]`,
	`t = csv("r,m,a\nEU,jan,1.5\nUS,jan,\nEU,feb,2"); [where(t, r -> r.a > 1), addcol(t, "b", r -> r.a * 2), sort(t, "a", "desc"), head(t, 1), tail(t)]`,
	`t = csv("r,m,a\nEU,jan,1.5\nUS,jan,\nEU,feb,2"); [groupby(t, ["r", "m"], "n", count, "s", g -> sum(g.a)), pivot(t, "r", "m", g -> mean(g.a)), totals(t), describe(t)]`,
	`a = csv("k;x\n1;\"p;q\"\n2;y", ";"); b = csv("k,z\n1,5\n1,6\n,7"); [join(a, b, "k"), leftjoin(a, b, ["k"]), isempty(b[3].k), t.x, a.]`,
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
