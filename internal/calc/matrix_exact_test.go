package calc

import (
	"context"
	"testing"
	"time"
)

func TestExactMatrixProduct_ExactOperandsGiveAnExactProduct(t *testing.T) {
	checkTexts(t, []textCase{
		{"[[1, 2], [3, 4]]^5", "[[1069, 1558], [2337, 3406]]"},
		{"[[1/2, 1/3], [1/4, 1/5]] * [[2, 3], [4, 5]] == [[7/3, 19/6], [13/10, 7/4]]", "true"},
		{"[[1/2, 1/3]] * [3, 6]", "[3.5]"},
		{"[1/3, 1] * [[3, 0], [0, 1/7]]", "[1, 0.142857142857143]"},
		// F(100), past the 2^53 a float holds exactly.
		{"([[1, 1], [1, 0]]^99)[1, 1]", "354224848179261915075"},
		{"[[1/2, 0], [0, 1/3]]^4 == [[1/16, 0], [0, 1/81]]", "true"},
		// 2^16000 fits the exact cap and overflows a float.
		{"([[2^8000]] * [[2^8000]])[1, 1] == 2^16000", "true"},
	})
	for _, expr := range []string{
		"[[1, 2], [3, 4]]^5",
		"[[1/2, 1/3], [1/4, 1/5]] * [[2, 3], [4, 5]]",
		"[[1, 2], [3, 4]] * [1, 1]",
		"ones(50, 50) * ones(50, 50)",
		"[[1, 1], [1, 0]]^0",
	} {
		if r := mustEval(t, expr); !r.Exact {
			t.Errorf("%s: not exact", expr)
		}
	}
}

// Past a cap the product is computed in floats, as before the exact path,
// and reports not exact.
func TestExactMatrixProduct_PastACapFallsBackToFloats(t *testing.T) {
	for _, expr := range []string{
		// A float operand.
		"[[sqrt(2), 1]] * [[1], [1]]",
		// A side over 50.
		"ones(51, 2) * ones(2, 2)",
		"ones(2, 51) * ones(51, 2)",
		// A negative power goes through the float inverse.
		"[[2, 0], [0, 4]]^-1",
		// Elements of 4,001 bits: within the bit cap, over the cost cap.
		"x = (2^2000 + 1) / 2^2000; A = map(1:50, i -> map(1:50, j -> x)); A * A",
		// A^k whose elements would pass the bit cap (A^k = A here).
		"[[1/2, 1/2], [1/2, 1/2]]^5000",
	} {
		r, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if r.Exact {
			t.Errorf("%s: reported exact", expr)
		}
	}
	// Past the bit cap, the float path refuses what overflows a float.
	mustFail(t, "[[2^9000]] * [[2^9000]]", codeOverflow)
}

// The exact work is charged from the operands' sizes before it runs:
// repeated big products run out of steps, they do not run on.
func TestExactMatrixProduct_WorkIsChargedFromTheSizes(t *testing.T) {
	e := &evaluator{ctx: context.Background(), nextCheck: ctxCheckEvery}
	rows := make([][]val, 50)
	for i := range rows {
		rows[i] = make([]val, 50)
		for j := range rows[i] {
			rows[i][j] = exactInt64(int64(i + j))
		}
	}
	if _, ok, err := e.exactMatMul(rows, rows, false, 0); !ok || err != nil {
		t.Fatalf("50 × 50 small integers: ok %v, err %v", ok, err)
	}
	if e.steps < 50*50*50/exactMulDiv {
		t.Errorf("a 50 × 50 exact product charged %d steps, want at least %d", e.steps, 50*50*50/exactMulDiv)
	}
	_, err := evalWithin(context.Background(), Request{Expr: "A = ones(50, 50); map(1:200, k -> A * A)"}, time.Minute)
	if err == nil || err.Code != string(codeStepLimit) {
		t.Fatalf("200 exact 50 × 50 products: err = %v, want step_limit", err)
	}
}
