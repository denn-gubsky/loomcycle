package calc

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMatrixPower_IntegerPowersOfASquareMatrix(t *testing.T) {
	checkTexts(t, []textCase{
		// Fibonacci: [[1, 1], [1, 0]]^n holds F(n+1), F(n), F(n-1).
		{"[[1, 1], [1, 0]]^10", "[[89, 55], [55, 34]]"},
		{"[[1, 1], [1, 0]]^1", "[[1, 1], [1, 0]]"},
		{"[[2, 0], [0, 3]]^3", "[[8, 0], [0, 27]]"},
		// n = 0 is the identity, of the matrix's size.
		{"[[5, 6, 7], [1, 2, 3], [0, 0, 1]]^0", "[[1, 0, 0], [0, 1, 0], [0, 0, 1]]"},
		// A negative power is the inverse raised to |n|.
		{"round([[2, 0], [0, 4]]^-2, 12)", "[[0.25, 0], [0, 0.0625]]"},
		{"round([[1, 1], [0, 1]]^-3, 12)", "[[1, -3], [0, 1]]"},
		// The element-wise power is unchanged.
		{"[[1, 2], [3, 4]] .^ 2", "[[1, 4], [9, 16]]"},
		{"A = [[1, 2], [3, 4]]; A^2 == A * A", "true"},
		{"A = [[1, 2], [3, 4]]; A^3 == A * A * A", "true"},
		// An exponent written as a float that is a whole number counts.
		{"[[1, 1], [1, 0]]^2.0", "[[2, 1], [1, 1]]"},
	})
}

func TestMatrixPower_RefusesWhatHasNoMatrixPower(t *testing.T) {
	e := mustFail(t, "[[1, 2], [3, 4]]^0.5", codeDomain)
	if !strings.Contains(e.Hint, "fractional") || !strings.Contains(e.Hint, ".^") {
		t.Errorf("fractional power hint = %q, want it to say there is no fractional power and name .^", e.Hint)
	}
	e = mustFail(t, "[[1, 2, 3], [4, 5, 6]]^2", codeShape)
	if !strings.Contains(e.Detail, "2 × 3") {
		t.Errorf("non-square detail = %q, want the shape", e.Detail)
	}
	// A vector has no matrix power: the element-wise hint stays.
	e = mustFail(t, "[1, 2, 3]^2", codeType)
	if !strings.Contains(e.Hint, ".^") {
		t.Errorf("vector ^ hint = %q, want .^", e.Hint)
	}
	mustFail(t, "2^[[1, 2], [3, 4]]", codeType)
	mustFail(t, "[[1, 2], [3, 4]]^[[1, 2], [3, 4]]", codeType)
	mustFail(t, `[[1, 2], [3, 4]]^"2"`, codeType)
	mustFail(t, "[[1, 2], [3, 4]]^(1 + 1i)", codeType)
	// Singular: no negative power, as no inverse.
	mustFail(t, "[[1, 2], [2, 4]]^-1", codeDomain)
	mustFail(t, `[["a", 2], [3, 4]]^2`, codeType)
	mustFail(t, "[[1, 2], [3]]^2", codeShape)
	// An exponent past 2^53 is not an integer a float can tell apart.
	mustFail(t, "[[1, 0], [0, 1]]^(2^60)", codeTooLarge)
	// The float path overflows loudly, never returns Inf or NaN.
	mustFail(t, "[[1e200, 1e200], [1e200, 1e200]]^2", codeOverflow)
	mustFail(t, "[[1e10, 1], [1, 0.5]]^200", codeOverflow)
}

// The work is ~log2(n) products of n³ each, checked from the shape and the
// exponent before any of them runs: a 300 × 300 matrix squared is under the
// cap, its 15th power (six products) and its -7th (an inverse and four
// products) over it, and so is the cube of a 500 × 500 one.
func TestMatrixPower_WorkIsBoundedFromTheShapeBeforeWork(t *testing.T) {
	big := "r = ones(300); A = map(r, x -> r); "
	for _, expr := range []string{big + "A^15", big + "A^-7", "r = ones(500); A = map(r, x -> r); A^3"} {
		start := time.Now()
		_, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err == nil || err.Code != string(codeTooLarge) {
			t.Fatalf("%s: err = %v, want too_large", expr, err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s took %v; it must be refused before the products run", expr, d)
		}
	}
	// 300^3 · 1 product is well under the cap.
	r, err := evalWithin(context.Background(), Request{Expr: big + "B = A^2; B[1, 1]"}, time.Minute)
	if err != nil || r.Text != "300" {
		t.Fatalf("300 × 300 squared: %q, %v", r.Text, err)
	}
}

func TestMatrixPower_ProductCount(t *testing.T) {
	for _, c := range []struct {
		k    uint64
		want int
	}{{1, 0}, {2, 1}, {3, 2}, {4, 2}, {5, 3}, {10, 4}, {1 << 20, 20}, {1<<20 + 1, 21}} {
		if got := powProducts(c.k); got != c.want {
			t.Errorf("powProducts(%d) = %d, want %d", c.k, got, c.want)
		}
	}
}
