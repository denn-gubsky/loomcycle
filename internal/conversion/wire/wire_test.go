package wire

import (
	"math"
	"testing"
)

// A value past int32 is clamped on the wire, never wrapped into another one.
func TestI32_SaturatesInsteadOfWrapping(t *testing.T) {
	for _, c := range []struct {
		in   int
		want int32
	}{{0, 0}, {42, 42}, {-7, -7}, {math.MaxInt32, math.MaxInt32}, {math.MaxInt32 + 1, math.MaxInt32}, {math.MinInt32 - 1, math.MinInt32}} {
		if got := I32(c.in); got != c.want {
			t.Errorf("I32(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
