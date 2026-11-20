package calc

import (
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
	mustFail(t, "sqrt = 3", codeReservedName)
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
