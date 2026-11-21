package calc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand/v2"
	"strings"
	"testing"
)

func TestNumbers_DecimalLiteralsAreExact(t *testing.T) {
	r := mustEval(t, "0.1 + 0.2")
	if r.Text != "0.3" || !r.Exact || r.Fraction != "" {
		t.Fatalf("0.1 + 0.2 = %q exact=%v fraction=%q", r.Text, r.Exact, r.Fraction)
	}
	checkTexts(t, []textCase{
		{"0.1 + 0.2 == 0.3", "true"},
		{"1.10 * 3", "3.3"},
		{"1/8", "0.125"},
		{"2^100", "1267650600228229401496703205376"},
		{"2^-3", "0.125"},
		{"(2/3)^2", "0.444444444444444"},
		{"1e-30", "0.000000000000000000000000000001"},
	})
}

func TestNumbers_NonTerminatingExactPrintsDigitsWithTheFraction(t *testing.T) {
	cases := []struct{ expr, text, frac string }{
		{"1/3", "0.333333333333333", "1/3"},
		{"2/3", "0.666666666666667", "2/3"},
		{"-2/3", "-0.666666666666667", "-2/3"},
		{"1000/3", "333.333333333333", "1000/3"},
		{"1/7 * 10^-10", "1.42857142857143e-11", "1/70000000000"},
		{"10^25/3", "3.33333333333333e+24", "10000000000000000000000000/3"},
		{"1/30", "0.0333333333333333", "1/30"},
	}
	for _, c := range cases {
		r := mustEval(t, c.expr)
		if r.Text != c.text || r.Fraction != c.frac || !r.Exact {
			t.Errorf("%s = %q (%q, exact=%v), want %q (%q)", c.expr, r.Text, r.Fraction, r.Exact, c.text, c.frac)
		}
	}
	r, err := Eval(context.Background(), Request{Expr: "1/7", Digits: 30})
	if err != nil || r.Text != "0.142857142857142857142857142857" {
		t.Errorf("1/7 at 30 digits = %q, %v", r.Text, err)
	}
}

func TestNumbers_FloatsPrintShortestAtDigits(t *testing.T) {
	r := mustEval(t, "sqrt(2)")
	if r.Text != "1.4142135623731" || r.Exact {
		t.Errorf("sqrt(2) = %q exact=%v", r.Text, r.Exact)
	}
	checkTexts(t, []textCase{
		{"sin(0) + 0.1 + 0.2", "0.3"},
		{"exp(100)", "2.68811714181614e+43"},
		{"exp(-20)", "2.06115362243856e-9"},
		{"cos(pi)", "-1"},
		{"sin(pi)", "1.22464679914735e-16"},
	})
	r, err := Eval(context.Background(), Request{Expr: "pi", Digits: 5})
	if err != nil || r.Text != "3.1416" {
		t.Errorf("pi at 5 digits = %q, %v", r.Text, err)
	}
	r, err = Eval(context.Background(), Request{Expr: "pi", Digits: 40})
	if err != nil || r.Text != "3.141592653589793" {
		t.Errorf("pi at 40 digits = %q, %v (a float has 17 digits at most)", r.Text, err)
	}
}

func TestNumbers_ExactMeetsFloatGivesFloat(t *testing.T) {
	for _, expr := range []string{"1/3 + sin(0)", "2 * pi", "[1, sqrt(2)]", "1 + 0.5^0.5"} {
		if r := mustEval(t, expr); r.Exact {
			t.Errorf("%s reported exact", expr)
		}
	}
	for _, expr := range []string{"sqrt(1/4)", "abs(-1/3)", "[1, 2/3]", "floor(7/2)"} {
		if r := mustEval(t, expr); !r.Exact {
			t.Errorf("%s reported not exact", expr)
		}
	}
	if r := mustEval(t, "\"s\""); r.Exact {
		t.Errorf("a string reported exact")
	}
}

func TestNumbers_ArithmeticPastTheBitCapBecomesAFloat(t *testing.T) {
	r := mustEval(t, "a = 3^6000; (a + 1) / a")
	if r.Exact || r.Text != "1" {
		t.Errorf("(a+1)/a = %q exact=%v, want a float 1", r.Text, r.Exact)
	}
	mustFail(t, "2^16000 * 2^16000", codeOverflow)
}

func TestNumbers_SummingDecimalPricesStaysExact(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	prices := make([]string, 1000)
	want := new(big.Rat)
	for i := range prices {
		cents := rng.Int64N(100000)
		prices[i] = fmt.Sprintf("%d.%02d", cents/100, cents%100)
		r, _ := new(big.Rat).SetString(prices[i])
		want.Add(want, r)
	}
	vars := rawVars(t, map[string]any{"prices": prices})
	r, err := Eval(context.Background(), Request{Expr: "sum(prices)", Vars: vars})
	if err != nil {
		t.Fatal(err)
	}
	if got := exactOf(t, r); got.Cmp(want) != 0 {
		t.Fatalf("sum = %q, want %s", r.Text, want.FloatString(2))
	}
}

// exactOf reads an exact response back into a big.Rat.
func exactOf(t *testing.T, r Response) *big.Rat {
	t.Helper()
	s := r.Text
	if r.Fraction != "" {
		s = r.Fraction
	}
	v, ok := new(big.Rat).SetString(s)
	if !ok || !r.Exact {
		t.Fatalf("not an exact result: %+v", r)
	}
	return v
}

func TestNumbers_ExactArithmeticAgreesWithMathBig(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	randRat := func() *big.Rat {
		n := rng.Int64N(2_000_001) - 1_000_000
		d := rng.Int64N(999) + 1
		return big.NewRat(n, d)
	}
	for i := 0; i < 500; i++ {
		a, b := randRat(), randRat()
		ops := []struct {
			op string
			f  func(z, x, y *big.Rat) *big.Rat
		}{{"+", (*big.Rat).Add}, {"-", (*big.Rat).Sub}, {"*", (*big.Rat).Mul}, {"/", (*big.Rat).Quo}}
		for _, o := range ops {
			if o.op == "/" && b.Sign() == 0 {
				continue
			}
			expr := fmt.Sprintf("(%s) %s (%s)", a.RatString(), o.op, b.RatString())
			got := exactOf(t, mustEval(t, expr))
			if want := o.f(new(big.Rat), a, b); got.Cmp(want) != 0 {
				t.Fatalf("%s = %s, want %s", expr, got.RatString(), want.RatString())
			}
		}
		k := rng.IntN(9) - 4
		if a.Sign() == 0 && k < 0 {
			continue
		}
		expr := fmt.Sprintf("(%s)^%d", a.RatString(), k)
		want := big.NewRat(1, 1)
		for j := 0; j < abs(k); j++ {
			want.Mul(want, a)
		}
		if k < 0 {
			want.Inv(want)
		}
		if got := exactOf(t, mustEval(t, expr)); got.Cmp(want) != 0 {
			t.Fatalf("%s = %s, want %s", expr, got.RatString(), want.RatString())
		}
	}
}

func TestNumbers_RoundingKeepsExactnessAndTieRules(t *testing.T) {
	checkTexts(t, []textCase{
		{"round(2.5)", "3"},
		{"round(-2.5)", "-3"},
		{"round(0.5)", "1"},
		{"roundeven(2.5)", "2"},
		{"roundeven(3.5)", "4"},
		{"roundeven(-2.5)", "-2"},
		{"round(1.005, 2)", "1.01"},
		{"roundeven(0.125, 2)", "0.12"},
		{"roundeven(0.135, 2)", "0.14"},
		{"round(1234, -2)", "1200"},
		{"round(1250, -2)", "1300"},
		{"floor(-1.5)", "-2"},
		{"ceil(-1.5)", "-1"},
		{"trunc(-1.7)", "-1"},
		{"floor(3.14159, 2)", "3.14"},
		{"ceil(3.14159, 3)", "3.142"},
		{"trunc(-3.14159, 1)", "-3.1"},
		{"round(2/3, 4)", "0.6667"},
		{"round([1.25, 2.35], 1)", "[1.3, 2.4]"},
		// A float rounds through its exact binary value: 2.675 as a float
		// is 2.67499999...
		{"round(2.675 + sin(0), 2)", "2.67"},
	})
	if r := mustEval(t, "round(2/3, 4)"); !r.Exact {
		t.Errorf("round of exact input is not exact")
	}
	if r := mustEval(t, "round(sqrt(2), 2)"); r.Exact || r.Text != "1.41" {
		t.Errorf("round(sqrt(2), 2) = %q exact=%v", r.Text, r.Exact)
	}
}

func TestNumbers_VarsAreExactAndStringsMayCarryNumbers(t *testing.T) {
	vars := map[string]any{
		"a":    0.1,
		"b":    "0.2",
		"name": "widget",
		"v":    []any{1, "2.5", []any{3}},
		"ok":   true,
		"big":  rawNumber("123456789012345678901234567890"),
		"neg":  "-1_000",
	}
	raw := rawVars(t, vars)
	r, err := Eval(context.Background(), Request{Expr: "a + b == 0.3 and name == \"widget\" and v[2] == 5/2 and ok and neg == -1000 and big + 1 == 123456789012345678901234567891", Vars: raw})
	if err != nil || r.Text != "true" {
		t.Fatalf("vars check = %q, %v", r.Text, err)
	}
	r, err = Eval(context.Background(), Request{Expr: "v", Vars: raw})
	if err != nil || r.Text != "[1, 2.5, [3]]" || len(r.Shape) != 1 {
		t.Fatalf("v = %q shape=%v, %v", r.Text, r.Shape, err)
	}
}

type rawNumber string

func (n rawNumber) MarshalJSON() ([]byte, error) { return []byte(n), nil }

func TestNumbers_BadVarsAreRefused(t *testing.T) {
	cases := []struct {
		vars map[string]string
		code errCode
	}{
		{map[string]string{"x": `{"a": 1}`}, codeBadVar},
		{map[string]string{"x": `null`}, codeBadVar},
		{map[string]string{"x": `[1,`}, codeBadVar},
		{map[string]string{"x": `1 2`}, codeBadVar},
		{map[string]string{"1x": `1`}, codeBadVar},
		{map[string]string{"pi": `1`}, codeReservedName},
		{map[string]string{"e": `1`}, codeReservedName},
		{map[string]string{"if": `1`}, codeReservedName},
		{map[string]string{"x": strings.Repeat("[", 40) + strings.Repeat("]", 40)}, codeBadVar},
		{map[string]string{"x": `1e9999999`}, codeTooLarge},
	}
	for _, c := range cases {
		req := Request{Expr: "1", Vars: map[string]json.RawMessage{}}
		for k, v := range c.vars {
			req.Vars[k] = json.RawMessage(v)
		}
		_, err := Eval(context.Background(), req)
		if err == nil || err.Code != string(c.code) {
			t.Errorf("vars %v: err = %v, want %s", c.vars, err, c.code)
		}
	}
}

func TestNumbers_VarNamedLikeABuiltinIsAVariable(t *testing.T) {
	req := Request{
		Expr: "sum(sum) / count + max",
		Vars: rawVars(t, map[string]any{"sum": []int{1, 2, 3}, "count": 2, "max": 1}),
	}
	r, err := Eval(context.Background(), req)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if r.Text != "4" {
		t.Errorf("text = %q, want 4", r.Text)
	}
}

func TestNumbers_ShapeDescribesNestedLists(t *testing.T) {
	cases := map[string][]int{
		"[1, 2, 3]":                {3},
		"[[1, 2], [3, 4], [5, 6]]": {3, 2},
		"[[1], [2, 3]]":            {2},
		"[]":                       {0},
		"[[[1, 2]], [[3, 4]]]":     {2, 1, 2},
		"1":                        nil,
	}
	for expr, want := range cases {
		r := mustEval(t, expr)
		if fmt.Sprint(r.Shape) != fmt.Sprint(want) {
			t.Errorf("shape(%s) = %v, want %v", expr, r.Shape, want)
		}
	}
}
