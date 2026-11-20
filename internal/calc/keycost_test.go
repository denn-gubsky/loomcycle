package calc

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestKeyCost_StringsChargeAtLeastOneStepAndKeyBytesAreCapped: a short
// string's key must cost more than the element visit (its quoted bytes
// are written), and one unique call may not build more than maxKeyBytes
// of keys however few steps they cost.
func TestKeyCost_StringsChargeAtLeastOneStepAndKeyBytesAreCapped(t *testing.T) {
	short := make([]string, 110_000)
	for i := range short {
		short[i] = "s" + strconv.FormatInt(int64(i), 36) // a letter first: a numeric string would decode as a number
	}
	long := strings.Repeat("x", 900_000)
	enc := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cases := []struct {
		expr string
		vars map[string]json.RawMessage
		code errCode
	}{
		// 110k distinct short strings, keyed six times: one step per
		// element would be 660k, under the limit.
		{"[" + strings.TrimSuffix(strings.Repeat("len(unique(s)), ", 6), ", ") + "]",
			map[string]json.RawMessage{"s": enc(short)}, codeStepLimit},
		// 20 copies of a 900 KB string: 18 MB of keys for ~35k steps.
		{"len(unique([" + strings.TrimSuffix(strings.Repeat("l, ", 20), ", ") + "]))",
			map[string]json.RawMessage{"l": enc(long)}, codeTooLarge},
	}
	// No wall-clock assertion: the minute-long wall is the bound. Work the
	// charge missed would run past it and end in time_limit, not the code
	// wanted, so the code check alone catches it. A 10 s assertion here
	// failed under -race on a loaded machine while the charge was right.
	for _, c := range cases {
		_, err := evalWithin(context.Background(), Request{Expr: c.expr, Vars: c.vars}, time.Minute)
		if err == nil || err.Code != string(c.code) {
			t.Errorf("%.60s: err = %v, want %s", c.expr, err, c.code)
		}
	}
}

// TestKeyCost_UniqueChargesNestedAndBigValues: unique serializes every
// nested element and every number's digits, so its step charge must
// follow that work, not the outer length.
func TestKeyCost_UniqueChargesNestedAndBigValues(t *testing.T) {
	for _, expr := range []string{
		// 990 references to one 1,000-element list: ~990k values to key.
		"a = 1:1000; l = map(1:990, i -> a); [unique(l), unique(l)]",
		// 3,000 distinct 16,000-bit numbers keyed ten times: each key is
		// ~4,800 digits (14.5 MB a call, under maxKeyBytes), so the steps
		// must run out.
		"x = 2^16000; l = map(1:3000, i -> x + i); [" + strings.TrimSuffix(strings.Repeat("len(unique(l)), ", 10), ", ") + "]",
	} {
		// The minute-long wall is the bound, as above.
		_, err := evalWithin(context.Background(), Request{Expr: expr}, time.Minute)
		if err == nil || err.Code != string(codeStepLimit) {
			t.Errorf("%s: err = %v, want step_limit", expr, err)
		}
	}
	checkTexts(t, []textCase{{"unique([[1, 2], [1, 2], 2^100, 2^100])", "[[1, 2], 1267650600228229401496703205376]"}})
}
