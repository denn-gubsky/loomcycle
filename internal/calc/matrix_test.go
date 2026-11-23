package calc

import (
	"context"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"
)

func TestMatrix_ProductOrientsVectorsAsColumnsOrRows(t *testing.T) {
	checkTexts(t, []textCase{
		{"[[1, 2], [3, 4]] * [[5, 6], [7, 8]]", "[[19, 22], [43, 50]]"},
		// A vector on the right is a column, the result a vector.
		{"[[1, 2], [3, 4]] * [1, 1]", "[3, 7]"},
		// A vector on the left is a row.
		{"[1, 1] * [[1, 2], [3, 4]]", "[4, 6]"},
		{"[[1, 2, 3]] * [[1], [2], [3]]", "[[14]]"},
		{"[[1], [2]] * [[3, 4]]", "[[3, 4], [6, 8]]"},
		// Scalars still broadcast over a matrix.
		{"2 * [[1, 2], [3, 4]]", "[[2, 4], [6, 8]]"},
		{"[[1, 2], [3, 4]] / 2", "[[0.5, 1], [1.5, 2]]"},
	})
	if r := mustEval(t, "[[1, 2], [3, 4]] * [[5, 6], [7, 8]]"); r.Exact {
		t.Error("a matrix product is computed in floats and must not report exact")
	}
	e := mustFail(t, "[1, 2] * [3, 4]", codeType)
	if !strings.Contains(e.Hint, "dot(a, b)") {
		t.Errorf("vector * vector hint = %q, want it to name dot", e.Hint)
	}
	e = mustFail(t, "[[1, 2, 3]] * [[1, 2]]", codeShape)
	if !strings.Contains(e.Detail, "[1, 3]") || !strings.Contains(e.Detail, "[1, 2]") {
		t.Errorf("product shape error %q does not name both shapes", e.Detail)
	}
	mustFail(t, "[[1, 2], [3, 4]] * [1, 2, 3]", codeShape)
}

func TestMatrix_ElementwiseNeedsTheSameShapeAndNamesBoth(t *testing.T) {
	checkTexts(t, []textCase{
		{"[[1, 2], [3, 4]] + [[10, 20], [30, 40]]", "[[11, 22], [33, 44]]"},
		{"[[1, 2], [3, 4]] .* [[2, 2], [2, 2]]", "[[2, 4], [6, 8]]"},
		{"[[1, 2], [3, 4]] .^ 2", "[[1, 4], [9, 16]]"},
		{"[[4, 6]] ./ [[2, 3]]", "[[2, 2]]"},
	})
	e := mustFail(t, "[[1, 2], [3, 4]] + [[1, 2, 3], [4, 5, 6]]", codeShape)
	if !strings.Contains(e.Detail, "[2, 2]") || !strings.Contains(e.Detail, "[2, 3]") {
		t.Errorf("shape error %q does not name both shapes", e.Detail)
	}
	// A matrix plus a vector is refused, not added row by row.
	mustFail(t, "[[1, 2], [3, 4]] + [1, 2]", codeShape)
}

func TestMatrix_TransposeAndTwoDimensionalIndexing(t *testing.T) {
	checkTexts(t, []textCase{
		{"[[1, 2, 3], [4, 5, 6]]'", "[[1, 4], [2, 5], [3, 6]]"},
		{"A = [[1, 2], [3, 4]]; A''", "[[1, 2], [3, 4]]"},
		{"[1, 2, 3]'", "[[1, 2, 3]]"},
		{"transpose([[1, 2]])", "[[1], [2]]"},
		{"A = [[1, 2, 3], [4, 5, 6]]; A[2, 3]", "6"},
		{"A = [[1, 2, 3], [4, 5, 6]]; A[2, :]", "[4, 5, 6]"},
		{"A = [[1, 2, 3], [4, 5, 6]]; A[:, 2]", "[2, 5]"},
		{"A = [[1, 2, 3], [4, 5, 6]]; A[:, [1, 3]]", "[[1, 3], [4, 6]]"},
		{"A = [[1, 2, 3], [4, 5, 6]]; A[:, :]", "[[1, 2, 3], [4, 5, 6]]"},
		{"v = [10, 20, 30]; v[:]", "[10, 20, 30]"},
		// One-index forms are unchanged.
		{"m = [[1, 2], [3, 4]]; m[2][1]", "3"},
		{"[10, 20, 30][[3, 1]]", "[30, 10]"},
	})
	if r := mustEval(t, "[[1, 2], [3, 4]]'"); !r.Exact {
		t.Error("a transpose rearranges values and must keep them exact")
	}
	mustFail(t, "A = [[1, 2], [3, 4]]; A[3, 1]", codeIndex)
	mustFail(t, "A = [[1, 2], [3, 4]]; A[1, 0]", codeIndex)
	mustFail(t, "v = [1, 2]; v[1, 1]", codeType)
	e := mustFail(t, "'abc'", codeSyntax)
	if !strings.Contains(e.Hint, "double quotes") {
		t.Errorf("single-quoted string hint = %q", e.Hint)
	}
	mustFail(t, "5'", codeType)
}

func TestMatrix_BuildingFunctionsKeepExactValues(t *testing.T) {
	checkTexts(t, []textCase{
		{"zeros(2, 3)", "[[0, 0, 0], [0, 0, 0]]"},
		{"ones(3)", "[1, 1, 1]"},
		{"identity(3)", "[[1, 0, 0], [0, 1, 0], [0, 0, 1]]"},
		{"diag([1/3, 2])", "[[0.333333333333333, 0], [0, 2]]"},
		{"diag([[1, 2, 3], [4, 5, 6]])", "[1, 5]"},
		{"reshape([[1, 2], [3, 4]], 1, 4)", "[[1, 2, 3, 4]]"},
		{"hstack([[1, 2], [3, 4]], [5, 6])", "[[1, 2, 5], [3, 4, 6]]"},
		{"vstack([[1, 2]], [3, 4], [[5, 6]])", "[[1, 2], [3, 4], [5, 6]]"},
		{"linspace(1, 2, 3)", "[1, 1.5, 2]"},
		{"linspace(5, 9, 1)", "[5]"},
		{"size(zeros(4, 2))", "[4, 2]"},
		{"size([1, 2, 3])", "[3]"},
		{"dot([1/2, 1/3], [2, 3])", "2"},
		{"cross([1, 2, 3], [4, 5, 6])", "[-3, 6, -3]"},
		{"kron([[1, 2]], [[1], [10]])", "[[1, 2], [10, 20]]"},
		{"trace([[1/2, 9], [9, 1/2]])", "1"},
	})
	for _, expr := range []string{"diag([1/3, 2])", "dot([1/2, 1/3], [2, 3])", "kron([[1/3]], [[3]])", "linspace(0, 1, 4)"} {
		if r := mustEval(t, expr); !r.Exact {
			t.Errorf("%s: not exact", expr)
		}
	}
	mustFail(t, "reshape(1:6, 4, 2)", codeShape)
	mustFail(t, "hstack([[1], [2]], [[1]])", codeShape)
	mustFail(t, "vstack([1, 2], [1, 2, 3])", codeShape)
	mustFail(t, "cross([1, 2], [3, 4])", codeShape)
	mustFail(t, "dot([1, 2], [3])", codeShape)
	mustFail(t, "trace([[1, 2, 3]])", codeShape)
	mustFail(t, "det([[1, 2], [3]])", codeShape)
	mustFail(t, "zeros(0)", codeDomain)
	mustFail(t, "det([1, 2])", codeType)
}

// residual evaluates expr, which must give a number, and fails the test
// unless it is below tol.
func residual(t *testing.T, expr string, tol float64) {
	t.Helper()
	r := mustEval(t, expr)
	f, err := strconv.ParseFloat(r.Text, 64)
	if err != nil {
		t.Fatalf("%s gave %q, not a number", expr, r.Text)
	}
	if f > tol {
		t.Errorf("%s = %g, want under %g", expr, f, tol)
	}
}

func TestMatrix_SolversAndDecompositionsSatisfyTheirIdentities(t *testing.T) {
	const a = "A = [[4, 1, 2], [1, 3, 0], [2, 0, 5]]; "   // symmetric positive definite
	const g = "G = [[2, -1, 0], [1, 1, 3], [0, 4, -2]]; " // general, invertible
	const tall = "T = [[1, 2, 3], [4, 5, 6], [7, 8, 10], [1, 0, 1]]; "
	for _, expr := range []string{
		g + "norm(inv(G) * G - identity(3))",
		g + "b = [1, 2, 3]; norm(solve(G, b) - inv(G) * b)",
		g + "b = [1, 2, 3]; norm(G * solve(G, b) - b)",
		g + "B = [[1, 0], [0, 1], [1, 1]]; norm(G * solve(G, B) - B)",
		g + "r = lu(G); norm(r[1] * r[2] - r[3] * G)",
		// Pivoting here is a 3-cycle, not its own inverse, so P versus P'
		// is told apart.
		"C = [[1, 2, 3], [4, 5, 6], [7, 8, 10]]; r = lu(C); norm(r[1] * r[2] - r[3] * C)",
		g + "r = lu(G); norm(r[1] - [[1, 0, 0], [r[1][2, 1], 1, 0], [r[1][3, 1], r[1][3, 2], 1]])",
		tall + "r = qr(T); norm(r[1] * r[2] - T)",
		tall + "r = qr(T); norm(r[1]' * r[1] - identity(4))",
		a + "L = cholesky(A); norm(L * L' - A)",
		a + "r = eig(A); V = r[2]; norm(A * V - V * diag(r[1]))",
		a + "r = eig(A); norm(r[2]' * r[2] - identity(3))",
		tall + "r = svd(T); norm(r[1] * diag(r[2]) * r[3]' - T)",
		tall + "norm(T * pinv(T) * T - T)",
		// The least-squares x satisfies the normal equations T'(Tx - b) = 0.
		tall + "b = [1, 2, 3, 4]; x = lstsq(T, b); norm(T' * (T * x - b))",
	} {
		residual(t, expr, 1e-9)
	}
	checkTexts(t, []textCase{
		{"det([[4, 3], [6, 3]])", "-6"},
		{"matrank([[1, 2, 3], [2, 4, 6], [1, 0, 1]])", "2"},
		{"matrank(zeros(2, 2))", "0"},
		{"norm([3, 4])", "5"},
		{"norm([3, -4], 1)", "7"},
		{`norm([3, -4], "inf")`, "4"},
		{"norm([[1, 2], [3, 4]], 1)", "6"},
		{`norm([[1, 2], [3, 4]], "inf")`, "7"},
		{"norm([[3, 0], [0, 4]])", "5"},
		{"norm([[3, 0], [0, 4]], 2)", "4"},
		{"cond([[1, 0], [0, 4]])", "4"},
		{"eig([[2, 1], [1, 2]])[1]", "[1, 3]"},
		{"svd([[3, 0], [0, -4]])[2]", "[4, 3]"},
		{"lu([[0, 1], [1, 1]])[3]", "[[0, 1], [1, 0]]"},
	})
}

func TestMatrix_SingularAndUnsuitableMatricesAreRefused(t *testing.T) {
	for _, expr := range []string{
		"inv([[1, 2], [2, 4]])",
		"solve([[1, 2], [2, 4]], [1, 2])",
		"cond([[1, 2], [2, 4]])",
		"cholesky([[1, 2], [2, 1]])", // symmetric, not positive definite
		"cholesky([[1, 2], [3, 4]])", // not symmetric
		`norm([1, 2], "max")`,
	} {
		mustFail(t, expr, codeDomain)
	}
	// The pseudo-inverse and least squares do take a singular matrix.
	checkTexts(t, []textCase{{"pinv([[1, 2], [2, 4]])", "[[0.04, 0.08], [0.08, 0.16]]"}})
	for _, expr := range []string{"inv([[1, 2, 3]])", "solve([[1, 2], [3, 4]], [1, 2, 3])", "qr([[1, 2, 3]])", "lu([[1, 2]])"} {
		mustFail(t, expr, codeShape)
	}
	e := mustFail(t, "rank([[1, 2], [2, 4]])", codeType)
	if !strings.Contains(e.Hint, "matrank") {
		t.Errorf("rank of a matrix: hint %q should name matrank", e.Hint)
	}
}

func TestMatrix_StatisticsWorkColumnByColumn(t *testing.T) {
	checkTexts(t, []textCase{
		{"mean([[1, 10], [2, 20], [3, 60]])", "[2, 30]"},
		{"median([[1, 10], [2, 20], [9, 60]])", "[2, 20]"},
		{`variance([[2, 1], [4, 1], [4, 1], [4, 1], [5, 1], [5, 1], [7, 1], [9, 1]], "population")`, "[4, 0]"},
		{"stdev([[1, 5], [3, 5]])", "[1.4142135623731, 0]"},
		// Rows already add element by element, so sum is per column too.
		{"sum([[1, 10], [2, 20]])", "[3, 30]"},
		// One-dimensional data is unchanged.
		{"mean([1, 2, 3, 4])", "2.5"},
		{"mean(1, 2)", "1.5"},
	})
	mustFail(t, "mean([[1, 2], [3]])", codeShape)
}

// The element cap and the size checks come before any conversion or work:
// a 501 × 501 matrix built for a handful of steps is refused at once.
func TestMatrix_OverTheCapIsRefusedBeforeWork(t *testing.T) {
	big := "r = ones(501); A = map(r, x -> r); "
	for _, op := range []string{"inv(A)", "det(A)", "svd(A)", "eig(A)", "A * A", "lstsq(A, r)", "transpose(A)", "mean(A)"} {
		assertFast(t, big+op, codeTooLarge)
	}
	assertFast(t, "zeros(501, 500)", codeTooLarge)
	assertFast(t, "ones(250001)", codeTooLarge)
	assertFast(t, "identity(501)", codeTooLarge)
	assertFast(t, "kron(ones(1, 600), ones(600, 1))", codeTooLarge)
	assertFast(t, "r = ones(600); qr(map(r, x -> [1]))", codeTooLarge)
	// A tall matrix under the element cap is fine for least squares.
	r := mustEval(t, "x = linspace(0, 1, 2000); y = 3 * x + 1; round(lstsq(hstack(ones(2000), x), y), 9)")
	if r.Text != "[1, 3]" {
		t.Errorf("tall least squares = %s, want [1, 3]", r.Text)
	}
}

// heavy refuses work over MaxMatrixWork before charging or running it. The
// element cap keeps every operation today at or under it (r·k·c is at most
// 250,000^1.5); this pins the guard for an operation whose work formula
// could outgrow the elements.
func TestMatrix_HeavyRefusesWorkOverTheCap(t *testing.T) {
	e := &evaluator{ctx: context.Background(), nextCheck: ctxCheckEvery}
	if _, err := e.heavy(10, MaxMatrixWork+1, "the test", 0); err == nil || err.Code != string(codeTooLarge) {
		t.Fatalf("err = %v, want too_large", err)
	}
	if e.steps != 0 {
		t.Errorf("a refused operation charged %d steps", e.steps)
	}
	release, err := e.heavy(10, MaxMatrixWork, "the test", 0)
	if err != nil {
		t.Fatalf("work at the cap: %v", err)
	}
	release()
}

// A gonum panic is a refusal, never a crash.
func TestMatrix_GuardTurnsAPanicIntoARefusal(t *testing.T) {
	err := guard("the test", 3, func() *Error { panic("boom") })
	if err == nil || err.Code != string(codeDomain) || err.Pos != 3 {
		t.Fatalf("err = %v, want domain at 3", err)
	}
}

func TestMatrix_GateMakesALargeOperationWaitAndASmallOneNot(t *testing.T) {
	gate := semaphore.NewWeighted(1)
	if !gate.TryAcquire(1) {
		t.Fatal("fresh gate is full")
	}
	// 101 × 101 = 10,201 elements, over the gate threshold.
	big := Request{Expr: "inv(identity(101) * 2)[1, 1]", Gate: gate}
	start := time.Now()
	_, err := evalWithin(context.Background(), big, 300*time.Millisecond)
	if err == nil || err.Code != string(codeTimeLimit) {
		t.Fatalf("large operation with the gate held: err = %v, want time_limit", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("waiting for the gate took %v; it must end at the call's own wall", d)
	}
	small := Request{Expr: "inv(identity(3) * 2)[1, 1]", Gate: gate}
	r, err := evalWithin(context.Background(), small, time.Minute)
	if err != nil || r.Text != "0.5" {
		t.Fatalf("small operation with the gate held: %q, %v", r.Text, err)
	}
	gate.Release(1)
	r, err = evalWithin(context.Background(), big, time.Minute)
	if err != nil || r.Text != "0.5" {
		t.Fatalf("large operation with the gate free: %q, %v", r.Text, err)
	}
	// The slot was given back.
	if !gate.TryAcquire(1) {
		t.Error("the large operation did not release its slot")
	}
}

func TestNewGate_HasOneSlotPerProcessor(t *testing.T) {
	n := int64(runtime.GOMAXPROCS(0))
	g := NewGate()
	if !g.TryAcquire(n) {
		t.Fatalf("the gate has fewer than %d slots", n)
	}
	if g.TryAcquire(1) {
		t.Errorf("the gate has more than %d slots", n)
	}
}
