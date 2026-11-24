package calc

import (
	"regexp"
	"strings"
	"testing"
)

func TestGrammar_PrecedenceMatchesPaper(t *testing.T) {
	checkTexts(t, []textCase{
		{"1 + 2 * 3", "7"},
		{"(1 + 2) * 3", "9"},
		{"-2^2", "-4"},
		{"(-2)^2", "4"},
		{"2^3^2", "512"},
		{"2^-1", "0.5"},
		{"-3!", "-6"},
		{"2^3!", "64"},
		{"5!", "120"},
		{"10 - 4 - 3", "3"},
		{"24 / 4 / 3", "2"},
		{"2 * 3 % 4", "2"},
		{"1 + 2 == 3", "true"},
		{"not 1 > 2", "true"},
		{"true or false and false", "true"},
		{"(true or false) and false", "false"},
		{"1:3 == [1, 2, 3]", "true"},
		{"1:2+1", "[1, 2, 3]"},
		{"- - 3", "3"},
		{"+3", "3"},
	})
}

func TestGrammar_FloorModTakesTheDivisorSign(t *testing.T) {
	checkTexts(t, []textCase{
		{"7 % 3", "1"},
		{"-7 % 3", "2"},
		{"7 % -3", "-2"},
		{"-7 % -3", "-1"},
		{"mod(-7, 3)", "2"},
		{"rem(-7, 3)", "-1"},
		{"rem(7, -3)", "1"},
		{"5.5 % 2", "1.5"},
		{"-7.0 % 3", "2"},
		{"rem(-7.5, 2)", "-1.5"},
		{"-7 % 3.0", "2"},
		{"7 % -3.0", "-2"},
	})
}

func TestGrammar_NumberLiterals(t *testing.T) {
	checkTexts(t, []textCase{
		{"42", "42"},
		{"1.5", "1.5"},
		{"1e-3", "0.001"},
		{"1E3", "1000"},
		{"1_000_000", "1000000"},
		{"0xff", "255"},
		{"0b1010", "10"},
		{"0o17", "15"},
		{".5", "0.5"},
		{"2.5e+2", "250"},
		{"\"hi\"", "hi"},
		{"true", "true"},
	})
}

func TestGrammar_StatementsReturnTheLastValueAndAssignments(t *testing.T) {
	r := mustEval(t, "x = 2\ny = x * 3; z = y + 1\nz * 2")
	if r.Text != "14" {
		t.Fatalf("text = %q", r.Text)
	}
	for k, want := range map[string]string{"x": "2", "y": "6", "z": "7"} {
		if got := r.Assigned[k]; got == nil || got.(interface{ String() string }).String() != want {
			t.Errorf("Assigned[%s] = %v, want %s", k, got, want)
		}
	}
}

func TestGrammar_NewlinesInsideBracketsContinueTheStatement(t *testing.T) {
	checkTexts(t, []textCase{
		{"sum([1,\n 2,\n 3])", "6"},
		{"max(\n1,\n5\n)", "5"},
		{"# a comment\n1 + 1 # trailing\n", "2"},
		{"[1, 2, 3,]", "[1, 2, 3]"},
	})
}

func TestGrammar_RangesAreInclusive(t *testing.T) {
	checkTexts(t, []textCase{
		{"1:5", "[1, 2, 3, 4, 5]"},
		{"1:2:10", "[1, 3, 5, 7, 9]"},
		{"10:-3:1", "[10, 7, 4, 1]"},
		{"5:1", "[]"},
		{"0:0.25:1", "[0, 0.25, 0.5, 0.75, 1]"},
		{"range(1, 3)", "[1, 2, 3]"},
		{"range(0, 1, 1/3)", "[0, 0.333333333333333, 0.666666666666667, 1]"},
		{"len(0:0.1:1.0)", "11"},
	})
}

func TestGrammar_IndexingIsOneBased(t *testing.T) {
	checkTexts(t, []textCase{
		{"v = [10, 20, 30]; v[1]", "10"},
		{"v = [10, 20, 30]; v[3]", "30"},
		{"v = [10, 20, 30]; v[2:3]", "[20, 30]"},
		{"v = [10, 20, 30]; v[[3, 1]]", "[30, 10]"},
		{"[[1, 2], [3, 4]][2][1]", "3"},
	})
	e := mustFail(t, "v = [1, 2]; v[0]", codeIndex)
	if !strings.Contains(e.Hint, "starts at 1") {
		t.Errorf("hint = %q", e.Hint)
	}
	mustFail(t, "[1, 2][3]", codeIndex)
	mustFail(t, "[1, 2][-1]", codeIndex)
	mustFail(t, "[1, 2][1.5]", codeIndex)
}

func TestGrammar_ListsBroadcastAndElementwiseOperators(t *testing.T) {
	checkTexts(t, []textCase{
		{"[1, 2, 3] + 1", "[2, 3, 4]"},
		{"10 - [1, 2]", "[9, 8]"},
		{"[1, 2] * 3", "[3, 6]"},
		{"3 * [1, 2]", "[3, 6]"},
		{"[1, 2] / 4", "[0.25, 0.5]"},
		{"[1, 2] + [10, 20]", "[11, 22]"},
		{"[1, 2] .* [3, 4]", "[3, 8]"},
		{"[1, 2] ./ [4, 8]", "[0.25, 0.25]"},
		{"[1, 2, 3] .^ 2", "[1, 4, 9]"},
		{"2 .^ [1, 2]", "[2, 4]"},
		{"-[1, 2]", "[-1, -2]"},
		{"[5, 7] % 3", "[2, 1]"},
		{"[[1, 2], [3, 4]] * 2", "[[2, 4], [6, 8]]"},
		{"2.*3", "6"},
	})
	mustFail(t, "[1, 2] + [1, 2, 3]", codeShape)
	if e := mustFail(t, "[1, 2] * [3, 4]", codeType); !strings.Contains(e.Hint, ".*") {
		t.Errorf("hint = %q", e.Hint)
	}
	if e := mustFail(t, "[1, 2] ^ 2", codeType); !strings.Contains(e.Hint, ".^") {
		t.Errorf("hint = %q", e.Hint)
	}
	if e := mustFail(t, "1 / [1, 2]", codeType); !strings.Contains(e.Hint, "./") {
		t.Errorf("hint = %q", e.Hint)
	}
}

func TestGrammar_LambdasAndDefinitions(t *testing.T) {
	checkTexts(t, []textCase{
		{"f(x) = x^2 + 1; f(3)", "10"},
		{"g = x -> x * 2; g(5)", "10"},
		{"h = (a, b) -> a + b; h(2, 3)", "5"},
		{"(x -> x + 1)(1)", "2"},
		{"k = () -> 7; k()", "7"},
		{"add(a) = b -> a + b; add(2)(3)", "5"},
		{"f(x) = x + 1; g(x) = f(x) * 2; g(1)", "4"},
		{"map([1, 2, 3], x -> x^2)", "[1, 4, 9]"},
		{"map(x -> x + 1, [1, 2])", "[2, 3]"},
		{"map([1, 4], sqrt)", "[1, 2]"},
	})
	r := mustEval(t, "f(x) = x + 1")
	if r.Type != "function" || r.Text != "function f(x)" {
		t.Errorf("definition result = %q (%s)", r.Text, r.Type)
	}
	mustFail(t, "f(x, y) = x; f(1)", codeArity)
	mustFail(t, "pi = 3", codeReservedName)
	mustFail(t, "f(2) = 3", codeSyntax)
	mustFail(t, "f(x, x) = x", codeSyntax)
}

func TestGrammar_RecursionIsRefused(t *testing.T) {
	for _, expr := range []string{
		"f(n) = if(n == 0, 1, n * f(n - 1))",
		"f = n -> f(n)",
		"f(x) = g(x); g(x) = f(x); f(1)",
		"g = f -> f(f); g(g)",
	} {
		e := mustFail(t, expr, codeRecursion)
		if e.Hint == "" {
			t.Errorf("%q: no hint", expr)
		}
	}
	// A parameter that shadows the function's own name is not a self-call.
	checkTexts(t, []textCase{{"f(f) = f + 1; f(1)", "2"}})
}

func TestGrammar_ImplicitMultiplicationIsRefusedWithAHint(t *testing.T) {
	cases := map[string]string{
		"2x":          "write `2*x`",
		"x = 1; 2x":   "write `2*x`",
		"3pi":         "write `3*pi`",
		"2(3)":        "`*`",
		"x = 2; x(3)": "`x*(...)`",
		"2 x":         "`*`",
		"(1)(2)":      "`*`",
	}
	for expr, hint := range cases {
		r, err := Eval(t.Context(), Request{Expr: expr})
		if err == nil {
			t.Errorf("%q = %q, want an error", expr, r.Text)
			continue
		}
		if !strings.Contains(err.Hint, hint) {
			t.Errorf("%q: hint %q does not contain %q", expr, err.Hint, hint)
		}
	}
}

func TestGrammar_CommonSlipsGetTeachingHints(t *testing.T) {
	cases := []struct {
		expr string
		code errCode
		hint string
	}{
		{"2 ** 3", codeSyntax, "power is `^`"},
		{"if(1 = 1, 2, 3)", codeSyntax, "use `==`"},
		{"1 = 2", codeSyntax, "use `==`"},
		{"sqr(4)", codeUnknownFunc, "sqrt"},
		{"ln(2)", codeUnknownFunc, "use log"},
		// Names a model brought from other tools in the Calc help measurement.
		{"powmod(3, 2, 7)", codeUnknownFunc, "use modpow"},
		{"correl([1, 2], [2, 4])", codeUnknownFunc, "use correlation"},
		{"corr([1, 2], [2, 4])", codeUnknownFunc, "use correlation"},
		{"pearson([1, 2], [2, 4])", codeUnknownFunc, "use correlation"},
		{"seq(1, 3)", codeUnknownFunc, "use range"},
		{"networkdays(1, 2)", codeUnknownFunc, "use workdays"},
		{"workday(1, 2)", codeUnknownFunc, "use addworkdays"},
		{"edate(1, 2)", codeUnknownFunc, "use addmonths"},
		// Names from R, SciPy, Excel and NumPy seen in the second measurement.
		{"normal_cdf(1)", codeUnknownFunc, "use normcdf"},
		{"normalcdf(1)", codeUnknownFunc, "use normcdf"},
		{"norm_cdf(1)", codeUnknownFunc, "use normcdf"},
		{"pnorm(1)", codeUnknownFunc, "use normcdf"},
		{"normal_inv(0.9)", codeUnknownFunc, "use norminv"},
		{"norm_ppf(0.9)", codeUnknownFunc, "use norminv"},
		{"qnorm(0.9)", codeUnknownFunc, "use norminv"},
		{"normal_pdf(1)", codeUnknownFunc, "use normpdf"},
		{"dnorm(1)", codeUnknownFunc, "use normpdf"},
		{"poisson_cdf(2, 3)", codeUnknownFunc, "use poisscdf"},
		{"ppois(2, 3)", codeUnknownFunc, "use poisscdf"},
		{"poisson_pdf(2, 3)", codeUnknownFunc, "use poisspdf"},
		{"dpois(2, 3)", codeUnknownFunc, "use poisspdf"},
		{"binom_cdf(2, 5, 0.5)", codeUnknownFunc, "use binomcdf"},
		{"pbinom(2, 5, 0.5)", codeUnknownFunc, "use binomcdf"},
		{"invT(0.9, 5)", codeUnknownFunc, "use tinv"},
		{"t_inv(0.9, 5)", codeUnknownFunc, "use tinv"},
		{"qt(0.9, 5)", codeUnknownFunc, "use tinv"},
		{"tppf(0.9, 5)", codeUnknownFunc, "use tinv"},
		{"invchisq(0.9, 5)", codeUnknownFunc, "use chi2inv"},
		{"chi2_inv(0.9, 5)", codeUnknownFunc, "use chi2inv"},
		{"qchisq(0.9, 5)", codeUnknownFunc, "use chi2inv"},
		{"inverse([[1, 2], [3, 4]])", codeUnknownFunc, "use inv"},
		{"tr([[1, 2], [3, 4]])", codeUnknownFunc, "use trace"},
		{"rank([[1, 2], [3, 4]])", codeType, "matrank(A)"},
		{"length([1, 2])", codeUnknownFunc, "use len"},
		{"size_of([1, 2])", codeUnknownFunc, "use len"},
		{"factors(12)", codeUnknownFunc, "use factor"},
		{"days_between(1, 2)", codeUnknownFunc, "use datediff"},
		{"daysbetween(1, 2)", codeUnknownFunc, "use datediff"},
		{"pvariance([1, 2, 3])", codeUnknownFunc, `use variance(x, "population")`},
		{"pstdev([1, 2, 3])", codeUnknownFunc, `use stdev(x, "population")`},
		{"list(1, 2, 3)", codeUnknownFunc, "[1, 2, 3]"},
		// A bare i or j is an ordinary name; the imaginary unit is 1i.
		{"i", codeUnknownName, "write 1i"},
		{"2 * i + 1", codeUnknownName, "write 1i"},
		{"j", codeUnknownName, "write 1i"},
		{"convert(100, \"F\", \"C\")", codeUnknownUnit, "degF"},
		{"convert(100, \"degF\", \"C\")", codeUnknownUnit, "degC"},
		{"sqrt(1, 2)", codeArity, "sqrt(x)"},
		{"1 < 2 < 3", codeSyntax, "and"},
		{"if(1, 2, 3)", codeType, "compare"},
		{"xx + 1", codeUnknownName, "assign"},
		{"x = 1; xy", codeUnknownName, "did you mean"},
	}
	for _, c := range cases {
		e := mustFail(t, c.expr, c.code)
		if !strings.Contains(e.Hint, c.hint) {
			t.Errorf("%q: hint %q does not contain %q", c.expr, e.Hint, c.hint)
		}
	}
}

// An alias hint must name a function that exists, and an alias must not be
// a function itself (it would never be reached, and its hint would lie).
func TestGrammar_AliasHintsNameARealFunction(t *testing.T) {
	fns := map[string]bool{}
	for _, f := range Functions() {
		fns[f.Name] = true
	}
	for from, to := range aliases {
		if !fns[to] {
			t.Errorf("alias %s points at %s, which is not a function", from, to)
		}
		if fns[from] {
			t.Errorf("alias %s is itself a function", from)
		}
	}
	// A hint written as a call form must call a function that exists, and
	// its name must not be a function or a plain alias.
	callIn := regexp.MustCompile(`([a-z][a-z0-9_]*)\(`)
	for from, hint := range aliasHints {
		if fns[from] {
			t.Errorf("alias %s is itself a function", from)
		}
		if _, dup := aliases[from]; dup {
			t.Errorf("%s is in both aliases and aliasHints", from)
		}
		for _, m := range callIn.FindAllStringSubmatch(hint, -1) {
			if !fns[m[1]] {
				t.Errorf("alias %s: hint %q calls %s, which is not a function", from, hint, m[1])
			}
		}
	}
}

func TestGrammar_ErrorPositionPointsAtTheProblem(t *testing.T) {
	e := mustFail(t, "1 + 2x", codeSyntax)
	if e.Pos != 4 {
		t.Errorf("pos = %d, want 4", e.Pos)
	}
	e = mustFail(t, "1 + nosuch(2)", codeUnknownFunc)
	if e.Pos != 4 {
		t.Errorf("pos = %d, want 4", e.Pos)
	}
}

func TestGrammar_IfIsLazyAndBooleansShortCircuit(t *testing.T) {
	checkTexts(t, []textCase{
		{"if(true, 1, 1/0)", "1"},
		{"if(false, 1/0, 2)", "2"},
		{"x = 0; x != 0 and 1/x > 2", "false"},
		{"x = 0; x == 0 or 1/x > 2", "true"},
	})
	mustFail(t, "1 and true", codeType)
	mustFail(t, "if(true, 1)", codeArity)
}

func TestGrammar_ComparisonsAreExact(t *testing.T) {
	checkTexts(t, []textCase{
		{"0.1 + 0.2 == 0.3", "true"},
		{"1/3 * 3 == 1", "true"},
		{"sqrt(2)^2 == 2", "false"},
		{"0.5 == sqrt(0.25)", "true"},
		{"\"a\" < \"b\"", "true"},
		{"[1, 2] == [1, 2]", "true"},
		{"[1, 2] != [1, 3]", "true"},
		{"1 == \"1\"", "false"},
		{"2 >= 2", "true"},
	})
	mustFail(t, "[1] < [2]", codeType)
	mustFail(t, "sin == cos", codeType)
}

func TestGrammar_StringsAndEscapes(t *testing.T) {
	checkTexts(t, []textCase{
		{`"a\"b"`, `a"b`},
		{`len("héllo")`, "5"},
		{`["x", "y"]`, `["x", "y"]`},
	})
	mustFail(t, `"open`, codeSyntax)
	mustFail(t, `"bad \q"`, codeSyntax)
	mustFail(t, "1 $ 2", codeSyntax)
	mustFail(t, "", codeEmptyStatement)
	mustFail(t, " ; \n ", codeEmptyStatement)
}

// Variables and functions live in separate namespaces: a name followed by
// `(` is a call, which takes the user's function of that name if there is
// one and the built-in otherwise; a bare name is the user's variable.
func TestGrammar_VariableNamedLikeABuiltinKeepsTheBuiltinCallable(t *testing.T) {
	checkTexts(t, []textCase{
		{"sum = 10; sum", "10"},
		{"count = 3; count + 1", "4"},
		{"min = 2; max = 5; max - min", "3"},
		{"log = 7; log * 2", "14"},
		{"sum = 10; sum([1, 2]) + sum", "13"},
		{"count = 3; count([1, 2, 3, 4], x -> x > 1) * count", "9"},
		{"min = 2; min([5, min, 9])", "2"},
		{"log = 100; log(log, 10)", "2"},
		{"sum = [1, 2, 3]; sum(sum)", "6"},
		{"rate = 0.05; rate * 2", "0.1"},
		// A parameter named like a built-in does not hide the built-in call.
		{"f(sum) = sum([sum, 1]); f(4)", "5"},
	})
	// A name that is both a variable and a built-in is suggested once.
	if e := mustFail(t, "sum = 1; sun", codeUnknownName); strings.Count(e.Hint, "sum") != 1 {
		t.Errorf("hint = %q, want one sum", e.Hint)
	}
	r := mustEval(t, "sum = 10; total = sum([1, 2]) + sum")
	if _, ok := r.Assigned["sum"]; !ok || len(r.Assigned) != 2 {
		t.Errorf("assigned = %v, want sum and total", r.Assigned)
	}
}

func TestGrammar_UserFunctionShadowsTheBuiltinForCalls(t *testing.T) {
	checkTexts(t, []textCase{
		{"sum(a, b) = a * b; sum(3, 4)", "12"},
		{"count = x -> x + 100; count(1)", "101"},
		{"log(x) = x - 1; map([1, 2], log)", "[0, 1]"},
		// Defined after use in the same expression: the call before the
		// definition took the built-in, the one after takes the user's.
		{"a = sum([1, 2]); sum(x) = 0; a + sum(5)", "3"},
	})
}

func TestGrammar_UserFunctionNamedLikeABuiltinCallingThatNameIsRecursion(t *testing.T) {
	for _, expr := range []string{
		"sum(x) = sum(x) * 2",
		"sum = x -> sum(x) * 2",
	} {
		e := mustFail(t, expr, codeRecursion)
		if !strings.Contains(e.Hint, "built-in sum") {
			t.Errorf("%q: hint = %q, want it to name the shadowed built-in", expr, e.Hint)
		}
	}
}

func TestGrammar_KeywordsAndConstantsStayReserved(t *testing.T) {
	for _, name := range []string{"if", "pi", "e", "tau", "phi"} {
		for _, expr := range []string{name + " = 3", name + "(x) = x", "f(" + name + ") = 1"} {
			mustFail(t, expr, codeReservedName)
		}
	}
	// The other keywords are operators or literals: they cannot even parse
	// as a name, so any refusal is right; an evaluation is not.
	for _, name := range []string{"true", "false", "and", "or", "not"} {
		if _, err := evalText(t, name+" = 3"); err == nil {
			t.Errorf("Eval(%q) succeeded, want a refusal", name+" = 3")
		}
	}
}
