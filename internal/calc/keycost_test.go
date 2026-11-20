package calc

import (
	"context"
	"testing"
	"time"
)

// TestKeyCost_UniqueChargesNestedAndBigValues: unique serializes every
// nested element and every number's digits, so its step charge must
// follow that work, not the outer length.
func TestKeyCost_UniqueChargesNestedAndBigValues(t *testing.T) {
	for _, expr := range []string{
		// 990 references to one 1,000-element list: ~990k values to key.
		"a = 1:1000; l = map(1:990, i -> a); [unique(l), unique(l)]",
		// 20,000 distinct 16,000-bit numbers: each key is ~4,800 digits.
		"x = 2^16000; l = map(1:20000, i -> x + i); len(unique(l))",
	} {
		start := time.Now()
		_, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err == nil || err.Code != string(codeStepLimit) {
			t.Errorf("%s: err = %v, want step_limit", expr, err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("%s: took %v", expr, d)
		}
	}
	checkTexts(t, []textCase{{"unique([[1, 2], [1, 2], 2^100, 2^100])", "[[1, 2], 1267650600228229401496703205376]"}})
}
