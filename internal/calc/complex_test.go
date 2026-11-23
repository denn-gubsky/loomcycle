package calc

import (
	"context"
	"encoding/json"
	"math/cmplx"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

func TestComplex_SuffixLiteralIsAnImaginaryNumber(t *testing.T) {
	checkTexts(t, []textCase{
		{"2i", "2i"},
		{"1.5i", "1.5i"},
		{"3e2i", "300i"},
		{"1_000i", "1000i"},
		{".5i", "0.5i"},
		{"0i", "0i"},
		{"3 + 2i", "3+2i"},
		{"3 - 2i", "3-2i"},
		{"-2i", "-2i"},
		{"2i.*[1, 2]", "[2i, 4i]"},
	})
	r := mustEval(t, "2i")
	if r.Type != "complex" || r.Exact {
		t.Errorf("2i: type %s exact %v, want complex and not exact", r.Type, r.Exact)
	}
}

// The owner's ruling: only the numeric suffix makes an imaginary number;
// the name i is an ordinary variable, so no existing expression changes.
func TestComplex_BareNameIStaysAnOrdinaryVariable(t *testing.T) {
	checkTexts(t, []textCase{
		{"i = 4; 2*i", "8"},
		{"sum(map([1, 2, 3], i -> i * 2))", "12"},
		{"f(i) = i + 1; f(2)", "3"},
	})
	mustFail(t, "i", codeUnknownName)
}

// A letter run after a number other than a lone i is still the implicit
// multiplication refusal, and unit strings are untouched by the suffix.
func TestLexer_LettersAfterANumberOtherThanALoneIStayRefused(t *testing.T) {
	for _, expr := range []string{"2in", "2pi", "2if", "2ii", "2i2", "2im", "3e2in"} {
		e := mustFail(t, expr, codeSyntax)
		if !strings.Contains(e.Detail, "`"+expr+"` is not a number: no implicit multiplication") {
			t.Errorf("%s: detail %q, want the implicit-multiplication refusal of the whole word", expr, e.Detail)
		}
	}
	mustFail(t, "2 in", codeSyntax)
	// The radix literals read letters as digits; an i there is malformed,
	// as before.
	mustFail(t, "0x1i", codeSyntax)
	checkTexts(t, []textCase{
		{`convert(2, "in", "cm")`, "5.08"},
		{`convert(1, "mi", "in")`, "63360"},
		{"0xff", "255"},
		{"2e3", "2000"},
	})
	mustFail(t, "1e400i", codeOverflow)
}

// A var string is a number only when it is a real literal; "2i" stays the
// string it always was.
func TestComplex_VarStringWithISuffixStaysAString(t *testing.T) {
	r, err := Eval(context.Background(), Request{Expr: "z", Vars: rawVars(t, map[string]any{"z": "2i"})})
	if err != nil {
		t.Fatal(err)
	}
	if r.Type != "string" || r.Text != "2i" {
		t.Errorf("var \"2i\" = %s (%s), want the string 2i", r.Text, r.Type)
	}
}

// Values worked by hand: (1+2i)(3-i) = 3 - i + 6i + 2 = 5+5i;
// (1+2i)/(3-4i) = (1+2i)(3+4i)/25 = (-5+10i)/25.
func TestComplex_ArithmeticGivesTheHandWorkedValues(t *testing.T) {
	checkTexts(t, []textCase{
		{"(1+2i) * (3-1i)", "5+5i"},
		{"(1+2i) / (3-4i)", "-0.2+0.4i"},
		{"(1+2i)^2", "-3+4i"},
		{"(1+1i)^8", "16+0i"},
		{"2i^-1", "-0.5i"},
		{"1i^2", "-1+0i"},
		{"0i^0", "1+0i"},
		{"-(3+2i)", "-3-2i"},
		{"+(3+2i)", "3+2i"},
		{"2i - 2i", "0i"},
		{"pow(1i, 2)", "-1+0i"},
		{"1 / 3 + 1i", "0.333333333333333+1i"},
	})
	mustFail(t, "1 / 0i", codeDivZero)
	mustFail(t, "0i ^ -1", codeDivZero)
	mustFail(t, "(1e200 + 1e200i)^4", codeOverflow)
	e := mustFail(t, "5i % 2", codeType)
	if !strings.Contains(e.Hint, "re(z)") {
		t.Errorf("modulo of a complex: hint %q should point at re/im/abs", e.Hint)
	}
}

// An operation whose imaginary part comes out exactly 0 keeps the complex
// type; re() makes it real again.
func TestComplex_ZeroImaginaryPartStaysComplexUntilRe(t *testing.T) {
	r := mustEval(t, "1i * 1i")
	if r.Type != "complex" || r.Text != "-1+0i" {
		t.Errorf("1i * 1i = %s (%s), want -1+0i, complex", r.Text, r.Type)
	}
	r = mustEval(t, "re(1i * 1i)")
	if r.Type != "number" || r.Text != "-1" {
		t.Errorf("re(1i * 1i) = %s (%s), want the number -1", r.Text, r.Type)
	}
	checkTexts(t, []textCase{
		{"re(3-2i)", "3"}, {"im(3-2i)", "-2"}, {"conj(3-2i)", "3+2i"},
		{"complex(3, -2)", "3-2i"}, {"abs(3+4i)", "5"},
		{"re(7)", "7"}, {"im(7)", "0"}, {"conj(7)", "7"}, {"arg(7)", "0"},
		{"re([1+2i, 3])", "[1, 3]"},
	})
	// re and im of a real number keep it exact.
	if r := mustEval(t, "re(1/3) + im(5)"); !r.Exact || r.Fraction != "1/3" {
		t.Errorf("re(1/3) + im(5): exact %v fraction %q, want exact 1/3", r.Exact, r.Fraction)
	}
}

// Reference values from Python's cmath (CPython 3.12): cmath.sqrt(-4) =
// 2j; cmath.log(-1) = 3.141592653589793j; cmath.exp(1j*pi) =
// (-1+1.2246467991473532e-16j); cmath.asin(2) =
// (1.5707963267948966+1.3169578969248166j); cmath.sin(1j) =
// 1.1752011936438014j; cmath.phase(-1+0j) = pi; cmath.log10(-100) =
// (2+1.3643763538418412j); cmath.cos(1+1j) =
// (0.8337300251311491-0.9888977057628651j). The parts are rounded so the
// last digit, which may differ between platforms, does not decide.
func TestComplex_ElementaryFunctionsMatchPythonCmath(t *testing.T) {
	checkTexts(t, []textCase{
		{"sqrt(-4 + 0i)", "2i"},
		{"round(log(-1 + 0i), 12)", "3.14159265359i"},
		{"round(exp(pi * 1i), 12)", "-1+0i"},
		{"round(asin(2 + 0i), 12)", "1.570796326795+1.316957896925i"},
		{"round(sin(1i), 12)", "1.175201193644i"},
		{"round(arg(-1 + 0i), 12)", "3.14159265359"},
		{"round(log(8 + 0i, 2), 12)", "3+0i"},
		{"round(log10(-100 + 0i), 12)", "2+1.364376353842i"},
		{"round(cos(1+1i), 12)", "0.833730025131-0.988897705763i"},
		{"floor(1.5-1.5i)", "1-2i"},
	})
	mustFail(t, "log(0i)", codeDomain)
}

// A real argument keeps the real path: the answer that is not real is a
// domain refusal whose hint names the complex call.
func TestComplex_RealArgumentOutsideTheRealDomainIsRefusedWithAComplexHint(t *testing.T) {
	for expr, want := range map[string]string{
		"sqrt(-1)":   "sqrt(-1 + 0i)",
		"log(-2)":    "log(-2 + 0i)",
		"log10(-2)":  "log10(-2 + 0i)",
		"asin(2)":    "asin(2 + 0i)",
		"acosh(0.5)": "acosh(0.5 + 0i)",
	} {
		e := mustFail(t, expr, codeDomain)
		if !strings.Contains(e.Hint, want) {
			t.Errorf("%s: hint %q should name %s", expr, e.Hint, want)
		}
	}
	e := mustFail(t, "(-8)^(1/3)", codeDomain)
	if !strings.Contains(e.Hint, "complex") {
		t.Errorf("(-8)^(1/3): hint %q should point at a complex argument", e.Hint)
	}
}

func TestComplex_OrderingIsRefusedAndEqualityComparesValues(t *testing.T) {
	for _, expr := range []string{"1i < 2", "2 > 1i", "1i <= 1i", "1i >= 0", "sort([1i, 2])", "min([1i, 2])", "max(1i, 2)", "median([1i, 2])", "mean([1i])"} {
		e := mustFail(t, expr, codeType)
		if !strings.Contains(e.Hint, "order") && !strings.Contains(e.Hint, "re(z)") {
			t.Errorf("%s: hint %q should say complex numbers have no order", expr, e.Hint)
		}
	}
	checkTexts(t, []textCase{
		{"2i == 2i", "true"},
		{"2i != 2i", "false"},
		{"2 + 0i == 2", "true"},
		{"2 == 2 + 0i", "true"},
		{"1/2 + 0i == 0.5", "true"},
		{"2i == 2", "false"},
		{"[1i, 2] == [1i, 2]", "true"},
		{"sort([3i, 1i], z -> im(z))", "[1i, 3i]"},
	})
}

func TestComplex_ListsAndMatricesWorkElementByElement(t *testing.T) {
	checkTexts(t, []textCase{
		{"[1i, 2] + 1", "[1+1i, 3]"},
		{"[1, 2] * 1i", "[1i, 2i]"},
		{"[1i, 2] .* [2, 3i]", "[2i, 6i]"},
		{"[[1i, 2], [3, 4]] .* 2", "[[2i, 4], [6, 8]]"},
		{"[[1i, 2], [3, 4]]'", "[[1i, 3], [2, 4]]"},
		{"[[1i, 2], [3, 4]][1, 1]", "1i"},
		{"sqrt([-1, -4] + 0i)", "[1i, 2i]"},
		{"sum([1i, 2, 3+1i])", "5+2i"},
		{"product([1i, 1i])", "-1+0i"},
		{"unique([1i, 2i, 1i, 2, 2 + 0i])", "[1i, 2i, 2]"},
		{"count([1i, 1i, 2], 1i)", "2"},
	})
	for _, expr := range []string{"[[1i, 2], [3, 4]] * [[1, 0], [0, 1]]", "det([[1i, 2], [3, 4]])", "inv([[1i]])", "eig([[1i]])"} {
		e := mustFail(t, expr, codeType)
		if !strings.Contains(e.Hint, "element by element") {
			t.Errorf("%s: hint %q should say complex values work element by element", expr, e.Hint)
		}
	}
}

func TestComplex_ResultIsReAndImInJSON(t *testing.T) {
	r := mustEval(t, "z = 3 - 2i; [z, 1]")
	got, err := json.Marshal(r.Result)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `[{"re":3,"im":-2},1]` {
		t.Errorf("result JSON = %s", got)
	}
	a, _ := json.Marshal(r.Assigned["z"])
	if string(a) != `{"re":3,"im":-2}` {
		t.Errorf("assigned z JSON = %s", a)
	}
	if r.Exact {
		t.Error("a list holding a complex number is reported exact")
	}
}

// NumPy's documented example: np.roots([3.2, 2, 1]) is
// [-0.3125+0.46351241j, -0.3125-0.46351241j]; the closed form is
// (-2 ± sqrt(4 - 12.8)) / 6.4. polyroots lists by real part, then
// imaginary part.
func TestPolyroots_MatchesNumPyRoots(t *testing.T) {
	checkTexts(t, []textCase{
		{"round(polyroots([3.2, 2, 1]), 8)", "[-0.3125-0.46351241i, -0.3125+0.46351241i]"},
		{"round(polyroots([1, -3, 2]), 12)", "[1, 2]"},
		{"round(polyroots([1, -6, 11, -6]), 10)", "[1, 2, 3]"},
		{"round(polyroots([1, 0, 1]), 12)", "[-1i, 1i]"},
		{"polyroots([1, 0, 0])", "[0, 0]"},
		{"round(polyroots([1, -1, 0]), 12)", "[0, 1]"},
		{"round(polyroots([0, 0, 1, -1]), 12)", "[1]"},
		{"polyroots([2])", "[]"},
	})
	for alias, name := range map[string]string{"real(1i)": "re", "imag(1i)": "im", "angle(1i)": "arg", "roots([1, 1])": "polyroots"} {
		if e := mustFail(t, alias, codeUnknownFunc); e.Hint != "use "+name {
			t.Errorf("%s: hint %q, want use %s", alias, e.Hint, name)
		}
	}
	mustFail(t, "polyroots([0, 0])", codeDomain)
	mustFail(t, "polyroots([])", codeDomain)
	e := mustFail(t, "polyroots([1, 1i])", codeType)
	if !strings.Contains(e.Detail, "real coefficients") {
		t.Errorf("complex coefficient: detail %q", e.Detail)
	}
	// Degree 501 needs a 501 × 501 companion matrix, over the cap.
	assertFast(t, "polyroots(ones(502))", codeTooLarge)
}

// NumPy's documented example: LA.eig([[1, -1], [1, 1]]) gives the
// eigenvalues 1+1j, 1-1j and the vectors [0.70710678, -0.70710678j] and
// [0.70710678, 0.70710678j] (columns); sorted by imaginary part, 1-1i
// comes first. [[1, 2], [3, 4]] is not symmetric and has the real
// eigenvalues (5 ± sqrt(33)) / 2.
func TestEig_NonSymmetricMatrixMatchesNumPy(t *testing.T) {
	checkTexts(t, []textCase{
		{"round(eig([[1, -1], [1, 1]])[1], 12)", "[1-1i, 1+1i]"},
		{"round(eig([[1, -1], [1, 1]])[2], 8)", "[[0.70710678, 0.70710678], [0.70710678i, -0.70710678i]]"},
		{"round(eig([[1, 2], [3, 4]])[1], 12)", "[-0.372281323269, 5.372281323269]"},
		{"round(eig([[0, -1], [1, 0]])[1], 12)", "[-1i, 1i]"},
	})
	r := mustEval(t, "eig([[1, 2], [3, 4]])")
	if r.Type != "list" || strings.Contains(r.Text, "i") {
		t.Errorf("real eigenvalues of a non-symmetric matrix came back complex: %s", r.Text)
	}
}

// For any square matrix each returned pair satisfies A v = λ v.
func TestEig_EveryPairSatisfiesTheEigenEquation(t *testing.T) {
	a := [][]float64{{4, -2, 1, 0}, {3, 1, -1, 2}, {0, 5, 2, -3}, {1, 0, -4, 1}}
	r, err := Eval(context.Background(), Request{Expr: "eig(A)", Vars: rawVars(t, map[string]any{"A": a})})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(r.Result)
	var out [2]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	var vals []json.RawMessage
	var vecs [][]json.RawMessage
	if err := json.Unmarshal(out[0], &vals); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out[1], &vecs); err != nil {
		t.Fatal(err)
	}
	complexSeen := false
	for k := range vals {
		lambda := jsonComplex(t, vals[k])
		complexSeen = complexSeen || imag(lambda) != 0
		for i := range a {
			var av complex128
			for j := range a {
				av += complex(a[i][j], 0) * jsonComplex(t, vecs[j][k])
			}
			if d := cmplx.Abs(av - lambda*jsonComplex(t, vecs[i][k])); d > 1e-9 {
				t.Errorf("pair %d, row %d: |Av - λv| = %g", k+1, i+1, d)
			}
		}
	}
	if !complexSeen {
		t.Error("the fixture was meant to have complex eigenvalues")
	}
}

func jsonComplex(t *testing.T, raw json.RawMessage) complex128 {
	t.Helper()
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return complex(f, 0)
	}
	var c struct{ Re, Im float64 }
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("%s is neither a number nor {re, im}: %v", raw, err)
	}
	return complex(c.Re, c.Im)
}

// The general eigen decomposition charges the n³ work before it runs and
// one step per value it returns, vectors included.
func TestEig_GeneralPathChargesWorkAndEveryReturnedValue(t *testing.T) {
	const n = 100
	rows := make([]val, n)
	for i := range n {
		row := make([]val, n)
		for j := range n {
			row[j] = exactInt64(int64((i*7 + j*3) % 11))
		}
		rows[i] = row
	}
	e := &evaluator{ctx: context.Background(), funcs: builtinIndex, sizes: map[listKey]int{}, nextCheck: ctxCheckEvery}
	if _, err := fnEig(e, args{v: []val{rows}, name: "eig"}); err != nil {
		t.Fatal(err)
	}
	// Reading (n²), the work (n³/1000), the values (n) and the vectors (n²).
	if want := n*n + n*n*n/matrixWorkPerStep + n + n*n; e.steps < want {
		t.Errorf("eig of %d × %d charged %d steps, want at least %d", n, n, e.steps, want)
	}
}

// A polynomial over the gate's threshold waits for a slot like any heavy
// matrix routine.
func TestPolyroots_LargeDegreeWaitsForTheGate(t *testing.T) {
	gate := semaphore.NewWeighted(1)
	if !gate.TryAcquire(1) {
		t.Fatal("fresh gate is full")
	}
	// Degree 101: a 101 × 101 companion matrix, 10,201 elements.
	big := Request{Expr: "size(polyroots(ones(102)))", Gate: gate}
	if _, err := evalWithin(context.Background(), big, 300*time.Millisecond); err == nil || err.Code != string(codeTimeLimit) {
		t.Fatalf("polyroots with the gate held: err = %v, want time_limit", err)
	}
	gate.Release(1)
	r, err := evalWithin(context.Background(), big, time.Minute)
	if err != nil || r.Text != "[101]" {
		t.Fatalf("polyroots with the gate free: %q, %v", r.Text, err)
	}
}

// Complex values are charged like real ones: a long complex broadcast
// reaches the step limit.
func TestComplex_LongBroadcastReachesTheStepLimit(t *testing.T) {
	_, err := evalWithin(context.Background(), Request{Expr: "v = (1:400000) * 1i; w = v .* v; w + 1i"}, time.Minute)
	if err == nil || err.Code != string(codeStepLimit) {
		t.Fatalf("err = %v, want step_limit", err)
	}
}
