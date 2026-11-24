package calc

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Hand-computed: (1+i)·1 + 2·i = 1+3i, (1+i)·i + 2·1 = 1+i, i·i = -1, i·1 = i.
func TestComplexMatrixProduct_MatchesHandComputedValues(t *testing.T) {
	checkTexts(t, []textCase{
		{"[[1 + 1i, 2], [0, 1i]] * [[1, 1i], [1i, 1]]", "[[1+3i, 1+1i], [-1+0i, 1i]]"},
		{"[[1i, 0], [0, 2]] * [1, 1i]", "[1i, 2i]"},
		{"[1i, 1] * [[1, 0], [0, 1]]", "[1i, 1+0i]"},
		{"[[1, 2]] * [[1i], [1i]]", "[[3i]]"},
		// The Pauli matrix Y squares to the identity.
		{"[[0, -1i], [1i, 0]]^2", "[[1+0i, 0i], [0i, 1+0i]]"},
		{"[[1i]]^3", "[[-1i]]"},
		{"[[1 + 1i, 2], [3, 4]]^0", "[[1, 0], [0, 1]]"},
		{"A = [[1 + 1i, 2], [3, 1i]]; A^3 == A * A * A", "true"},
		// ' stays the plain transpose; ctranspose also conjugates.
		{"[[1 + 2i, 3]]'", "[[1+2i], [3]]"},
		{"ctranspose([[1 + 2i, 3], [4i, 5]])", "[[1-2i, -4i], [3, 5]]"},
		{"ctranspose([1i, 2])", "[[-1i, 2]]"},
		{"ctranspose([[1, 2], [3, 4]]) == [[1, 2], [3, 4]]'", "true"},
	})
	if r := mustEval(t, "ctranspose([[1/3, 2]])"); !r.Exact {
		t.Error("ctranspose of a real matrix rearranges values and must keep them exact")
	}
}

func TestComplexMatrixProduct_OtherMatrixRoutinesStillRefuseComplex(t *testing.T) {
	for _, expr := range []string{"inv([[1i]])", "det([[1i, 0], [0, 1]])", "solve([[1i]], [1])", "[[1i, 0], [0, 1]]^-1"} {
		e := mustFail(t, expr, codeType)
		if !strings.Contains(e.Hint, "real") {
			t.Errorf("%s: hint %q does not say the routine is real", expr, e.Hint)
		}
	}
	mustFail(t, "[[1i, 2]] * [[1, 2]]", codeShape)
	mustFail(t, "[[1i, 2], [3]] * [[1], [2]]", codeShape)
	mustFail(t, `[[1i, "a"]] * [1, 2]`, codeType)
	mustFail(t, "[[1i]] * []", codeType)
	mustFail(t, "[[1i, 1]]^2", codeShape)
	mustFail(t, "[[1e308 + 1e308i, 1e308], [1, 1]]^2", codeOverflow)
	mustFail(t, "ctranspose(5)", codeType)
}

// The complex product's work is bounded from the shapes before it runs,
// like the real one.
func TestComplexMatrixProduct_WorkIsBoundedFromTheShapeBeforeWork(t *testing.T) {
	big := "r = ones(300); A = map(r, x -> r * 1i); "
	for _, expr := range []string{big + "A^15", "r = ones(501); A = map(r, x -> r * 1i); A * A"} {
		start := time.Now()
		_, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err == nil || err.Code != string(codeTooLarge) {
			t.Fatalf("%s: err = %v, want too_large", expr, err)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s took %v; it must be refused before the products run", expr, d)
		}
	}
	r, err := evalWithin(context.Background(), Request{Expr: big + "B = A * A; B[1, 1]"}, time.Minute)
	if err != nil || r.Text != "-300+0i" {
		t.Fatalf("300 × 300 complex product: %q, %v", r.Text, err)
	}
}
