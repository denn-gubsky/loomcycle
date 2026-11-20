package calc

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// BenchmarkArithmeticChain measures plain exact arithmetic, to decide later
// whether a small-integer fast path is worth having.
func BenchmarkArithmeticChain(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		expr := "1" + strings.Repeat(" + 2 * 3 - 4 / 2", n)
		b.Run("ops="+strconv.Itoa(n*4), func(b *testing.B) {
			ctx := context.Background()
			for b.Loop() {
				if _, err := Eval(ctx, Request{Expr: expr}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkFloatChain(b *testing.B) {
	expr := "x = 0.5 + sin(0)" + strings.Repeat("; x = x * 1.0001 + 0.25", 250) + "; x"
	ctx := context.Background()
	for b.Loop() {
		if _, err := Eval(ctx, Request{Expr: expr}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSumRange(b *testing.B) {
	ctx := context.Background()
	for b.Loop() {
		if _, err := Eval(ctx, Request{Expr: "sum(1:100000)"}); err != nil {
			b.Fatal(err)
		}
	}
}
