package calc

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// errorCases reaches every code in ErrorCodes() at least once.
var errorCases = map[errCode]Request{
	codeSyntax:         {Expr: "1 +"},
	codeUnknownName:    {Expr: "nosuch + 1"},
	codeUnknownFunc:    {Expr: "nosuch(1)"},
	codeArity:          {Expr: "sqrt()"},
	codeType:           {Expr: "1 + true"},
	codeDomain:         {Expr: "sqrt(-1)"},
	codeDivZero:        {Expr: "1/0"},
	codeOverflow:       {Expr: "exp(1000)"},
	codeTooLarge:       {Expr: "9^9^9"},
	codeIndex:          {Expr: "[1][0]"},
	codeShape:          {Expr: "[1] + [1, 2]"},
	codeRecursion:      {Expr: "f(x) = f(x)"},
	codeReservedName:   {Expr: "pi = 3"},
	codeBadRequest:     {Expr: "1", Digits: -1},
	codeBadVar:         {Expr: "x", Vars: map[string]json.RawMessage{"x": json.RawMessage(`{}`)}},
	codeExprTooLong:    {Expr: strings.Repeat("1", MaxExprBytes+1)},
	codeVarsTooLarge:   {Expr: "1", Vars: map[string]json.RawMessage{"x": json.RawMessage(`"` + strings.Repeat("a", MaxVarsBytes) + `"`)}},
	codeStepLimit:      {Expr: "sort(1:70000)"},
	codeListTooLong:    {Expr: "1:2000000"},
	codeTooDeep:        {Expr: strings.Repeat("(", MaxDepth+1) + "1" + strings.Repeat(")", MaxDepth+1)},
	codeEmptyStatement: {Expr: ""},
	codeBadDate:        {Expr: `date("11/12/2026")`},
	codeUnknownUnit:    {Expr: `convert(1, "furlong", "m")`},
	codeDimension:      {Expr: `convert(1, "kg", "m")`},
	codeNoConvergence:  {Expr: "integrate(x -> 1/x, 0, 1)"},
	codeColumn:         {Expr: `t = csv("a,b\n1,2"); t.c`},
	codeBadData:        {Expr: "t", Data: map[string]Data{"t": {Bytes: []byte("{"), Format: FormatJSON}}},
	// time_limit and canceled need a context; see their own tests.
}

func TestErrorCodes_EachDeclaredCodeIsReachable(t *testing.T) {
	covered := map[string]bool{string(codeTimeLimit): true, string(codeCanceled): true}
	for code, req := range errorCases {
		_, err := Eval(context.Background(), req)
		if err == nil || err.Code != string(code) {
			t.Errorf("%s: got %v", code, err)
			continue
		}
		if err.Detail == "" {
			t.Errorf("%s: empty detail", code)
		}
		covered[string(code)] = true
	}
	var missing []string
	for _, c := range ErrorCodes() {
		if !covered[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		t.Errorf("declared codes with no reaching case: %v", missing)
	}
	if !sort.StringsAreSorted(ErrorCodes()) || len(ErrorCodes()) != len(allCodes) {
		t.Errorf("ErrorCodes() = %v", ErrorCodes())
	}
}

// TestErrorCodes_EveryProducedCodeIsDeclared runs every expression in the
// fuzz seed corpus and the error cases; any error must carry a declared
// code. FuzzEval checks the same on random input.
func TestErrorCodes_EveryProducedCodeIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, c := range ErrorCodes() {
		declared[c] = true
	}
	for _, expr := range fuzzSeeds {
		if _, err := Eval(context.Background(), Request{Expr: expr}); err != nil && !declared[err.Code] {
			t.Errorf("%q produced undeclared code %q", expr, err.Code)
		}
	}
}

func assertFast(t *testing.T, expr string, code errCode) {
	t.Helper()
	start := time.Now()
	mustFail(t, expr, code)
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Errorf("%s took %v; it must be refused before the work", expr, d)
	}
}

func TestLimits_HugeResultsAreRefusedBeforeWork(t *testing.T) {
	assertFast(t, "9^9^9", codeTooLarge)
	assertFast(t, "factorial(10^6)", codeTooLarge)
	assertFast(t, "factorial(10^18)", codeTooLarge)
	assertFast(t, "(10^6)!", codeTooLarge)
	assertFast(t, "fib(10^9)", codeTooLarge)
	assertFast(t, "binomial(10^9, 5*10^8)", codeTooLarge)
	assertFast(t, "permutations(10^9, 10^6)", codeTooLarge)
	assertFast(t, "shl(1, 10^9)", codeTooLarge)
	assertFast(t, "1e99999999", codeTooLarge)
	assertFast(t, "1"+strings.Repeat("0", 5000), codeTooLarge)
	assertFast(t, "0x"+strings.Repeat("f", 5000), codeTooLarge)
	assertFast(t, "1:10^12", codeListTooLong)
	assertFast(t, "range(0, 1, 10^-9)", codeListTooLong)
	// Over the bit cap but within float range: a float, not an error.
	if r := mustEval(t, "1.0001^100000"); r.Exact {
		t.Errorf("1.0001^100000 reported exact")
	}
}

func TestLimits_StepLimitStopsLongWork(t *testing.T) {
	for _, expr := range []string{
		"sort(1:70000)",
		"sum(map(1:900000, x -> x * 2))",
		"f(x) = x + 1; reduce(1:600000, (a, b) -> f(a))",
		// Bits charge steps: a long list of huge numbers runs out of steps
		// before it can hold gigabytes.
		"x = 2^16000; (1:40000) * x",
	} {
		_, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err == nil || err.Code != string(codeStepLimit) {
			t.Errorf("%s: err = %v, want step_limit", expr, err)
		}
	}
}

func TestLimits_DeadlineStopsEvaluation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Eval(ctx, Request{Expr: "s = 0; sum(map(1:300000, x -> x/3)) + sum(1:300000)"})
	if err == nil || err.Code != string(codeTimeLimit) {
		t.Fatalf("err = %v, want time_limit", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("deadline honoured after %v", d)
	}
}

func TestLimits_CancelStopsEvaluationMidway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(5*time.Millisecond, cancel)
	defer timer.Stop()
	start := time.Now()
	_, err := Eval(ctx, Request{Expr: "sum(map(1:300000, x -> x/3)) + sum(1:300000)"})
	if err == nil || err.Code != string(codeCanceled) {
		t.Fatalf("err = %v, want canceled", err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("cancel honoured after %v", d)
	}
	done, stop := context.WithCancel(context.Background())
	stop()
	if _, err := Eval(done, Request{Expr: "1"}); err == nil || err.Code != string(codeCanceled) {
		t.Errorf("pre-canceled ctx: err = %v", err)
	}
}

func TestLimits_LongOutputIsTruncatedWithAFlag(t *testing.T) {
	r := mustEval(t, "v = 1:100000; v")
	if !r.Truncated || r.Result != nil || len(r.Text) > MaxOutputBytes {
		t.Fatalf("truncated=%v result-nil=%v len=%d", r.Truncated, r.Result == nil, len(r.Text))
	}
	if r.Assigned["v"] != nil {
		t.Errorf("an assigned value past the budget must be null")
	}
	if len(r.Shape) != 1 || r.Shape[0] != 100000 {
		t.Errorf("shape = %v", r.Shape)
	}
	if r := mustEval(t, "len(1:100000)"); r.Truncated {
		t.Errorf("a short result was flagged truncated")
	}
}

func TestLimits_DeepNestingIsRefused(t *testing.T) {
	for _, expr := range []string{
		strings.Repeat("-", MaxDepth+5) + "1",
		strings.Repeat("[", MaxDepth+5) + strings.Repeat("]", MaxDepth+5),
		strings.Repeat("2^", MaxDepth+5) + "1",
		strings.Repeat("not ", MaxDepth+5) + "true",
		strings.Repeat("x -> ", MaxDepth+5) + "1",
	} {
		mustFail(t, expr, codeTooDeep)
	}
	// A chain of distinct functions deeper than MaxCallDepth.
	var b strings.Builder
	b.WriteString("f0(x) = x\n")
	for i := 1; i <= MaxCallDepth+1; i++ {
		b.WriteString("f" + strconv.Itoa(i) + "(x) = f" + strconv.Itoa(i-1) + "(x)\n")
	}
	b.WriteString("f" + strconv.Itoa(MaxCallDepth+1) + "(1)")
	mustFail(t, b.String(), codeTooDeep)
}

func TestLimits_DigitsAreBounded(t *testing.T) {
	for _, d := range []int{-1, MaxDigits + 1} {
		if _, err := Eval(context.Background(), Request{Expr: "1", Digits: d}); err == nil || err.Code != string(codeBadRequest) {
			t.Errorf("digits %d: err = %v", d, err)
		}
	}
}
