package config

import (
	"strings"
	"testing"
	"time"
)

// A parked interactive run waits an hour for its operator unless the operator
// says otherwise: a Go duration, 0 to wait forever. A value that is not one is
// refused at load rather than read as the default.
func TestInteractiveIdleTimeout_Env(t *testing.T) {
	for _, c := range []struct {
		env  string
		want time.Duration
	}{
		{"", time.Hour},
		{"30m", 30 * time.Minute},
		{"0", 0},
		{"0s", 0},
	} {
		t.Setenv("LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT", c.env)
		cfg, err := loadYAMLString(t, "channels: {}\n")
		if err != nil {
			t.Fatalf("%q: %v", c.env, err)
		}
		if got := cfg.Env.InteractiveIdleTimeout; got != c.want {
			t.Errorf("%q: InteractiveIdleTimeout = %v, want %v", c.env, got, c.want)
		}
	}
	for _, bad := range []string{"60", "an hour", "-5m"} {
		t.Setenv("LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT", bad)
		if _, err := loadYAMLString(t, "channels: {}\n"); err == nil || !strings.Contains(err.Error(), "LOOMCYCLE_INTERACTIVE_IDLE_TIMEOUT") {
			t.Errorf("%q: err = %v, want a refusal naming the knob", bad, err)
		}
	}
}
