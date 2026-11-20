package calc

import (
	"context"
	"math/big"
	"testing"
)

// TestBaseCost_ChargesForTheDigitsWritten: base(x, r) writes up to 16,384
// digits, so it charges by size as every other big-number result does.
func TestBaseCost_ChargesForTheDigitsWritten(t *testing.T) {
	e := &evaluator{ctx: context.Background(), funcs: builtinIndex, sizes: map[listKey]int{}}
	x := new(big.Int).Lsh(big.NewInt(1), 16000)
	v, err := fnBase(e, args{v: []val{exactInt(x), exactInt64(2)}, pos: 0, name: "base"})
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := v.(string); !ok || len(s) != 16001 {
		t.Fatalf("base result = %T of length %d", v, len(v.(string)))
	}
	if want := 16001 / bitStepCost; e.steps < want {
		t.Errorf("base charged %d steps, want at least %d", e.steps, want)
	}
}
