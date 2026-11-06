package main

import (
	"testing"
	"time"
)

// TestLimitsReloadInterval_ReadsTheKnob: unset is a minute, "0" is off, a Go
// duration of at least a second is taken, and anything else is refused.
func TestLimitsReloadInterval_ReadsTheKnob(t *testing.T) {
	for _, c := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"", time.Minute, false},
		{"0", 0, false},
		{"30s", 30 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"100ms", 0, true},
		{"-1m", 0, true},
		{"60", 0, true},
		{"soon", 0, true},
	} {
		got, err := limitsReloadInterval(c.in)
		if (err != nil) != c.bad || got != c.want {
			t.Errorf("limitsReloadInterval(%q) = %v, %v; want %v, bad=%v", c.in, got, err, c.want, c.bad)
		}
	}
}
